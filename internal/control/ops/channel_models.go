package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
)

// Upstream model discovery (GET /api/admin/channels/{id}/upstream-models).
//
// Why this exists: the channel editor used to ask operators to type the
// upstream model name by hand. Provider catalogues change (DeepSeek retired
// deepseek-chat and now answers with deepseek-flash / deepseek-v4-pro), so a
// hand-typed name rots silently — the row looks fine and only breaks at call
// time with a 502 from the upstream. Reading the vendor's own /v1/models with
// the channel's key turns that guess into a lookup.
//
// The endpoint is read-only on purpose: registering a platform model also
// decides its price rule and listing state, which is a separate, auditable
// write. This call only answers "what does the upstream actually offer, and
// which of those names does this channel already point at".

const (
	// upstreamModelsTimeout bounds the discovery call. Generous enough for a
	// cross-border provider, short enough that an operator does not stare at a
	// spinner while a dead base_url hangs.
	upstreamModelsTimeout = 10 * time.Second
	// upstreamModelsMaxBody caps the response. A catalogue is a few KB; a
	// proxy returning an HTML error page must not be read into memory.
	upstreamModelsMaxBody = 1 << 20
)

// upstreamModelItem is one row of the provider catalogue plus the platform's
// view of it. The three state fields answer different questions and are not
// interchangeable:
//
//	Registered    — a platform model with this exact name exists (t_model)
//	PlatformModel — some platform model maps to this upstream name
//	                (t_channel.upstream_model_name_map), may differ from ID
type upstreamModelItem struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
	MaxOutput     int64  `json:"max_output_tokens,omitempty"`
	Registered    bool   `json:"registered"`
	PlatformModel string `json:"platform_model,omitempty"`
}

type upstreamModelsResponse struct {
	Items []upstreamModelItem `json:"items"`
}

// ChannelUpstreamModels lists the models the channel's upstream advertises.
func (s *Service) ChannelUpstreamModels(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if s.Box == nil {
		// Without CHANNEL_KEY_KEK the stored envelope cannot be opened, so the
		// endpoint would be guessing. Fail loudly instead.
		mw.WriteError(w, r, apierr.New(apierr.CodeConfigUnavailable).
			WithMessage("渠道密钥未配置解密密钥（CHANNEL_KEY_KEK），无法向上游查询模型列表"))
		return
	}

	ctx := r.Context()
	var (
		baseURL  string
		envelope string
		modelMap string
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT base_url, upstream_key_enc, COALESCE(upstream_model_name_map, '')
		FROM t_channel
		WHERE id = ? AND deleted_at IS NULL`, id).Scan(&baseURL, &envelope, &modelMap)
	if errors.Is(err, sql.ErrNoRows) {
		mw.WriteError(w, r, apierr.New(apierr.CodeChannelNotFound))
		return
	}
	if err != nil {
		s.logError(ctx, "ops_channel_upstream_models_load_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	plain, err := s.Box.Open(envelope)
	if err != nil {
		// A broken envelope is an operator-visible fact, not a 500: the key must
		// be re-entered before this channel can call anything either.
		s.logError(ctx, "ops_channel_upstream_models_open_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeUpstreamKeyUnavailable).
			WithMessage("渠道密钥无法解密，请重新填写上游密钥"))
		return
	}

	items, apiErr := fetchUpstreamModels(ctx, baseURL, string(plain))
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	annotateUpstreamModels(ctx, s.DB, items, decodeModelMap(modelMap))
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	_ = mw.WriteJSON(w, http.StatusOK, upstreamModelsResponse{Items: items})
}

// fetchUpstreamModels calls the provider's OpenAI-compatible /v1/models.
func fetchUpstreamModels(ctx context.Context, baseURL, key string) ([]upstreamModelItem, *apierr.APIError) {
	target, err := modelsEndpoint(baseURL)
	if err != nil {
		return nil, apierr.New(apierr.CodeUpstreamAllFailed).
			WithMessage("渠道接入地址无效，无法查询上游模型列表：" + err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, apierr.New(apierr.CodeUpstreamAllFailed).
			WithMessage("无法构造上游模型列表请求：" + err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{
		Timeout: upstreamModelsTimeout,
		Transport: &http.Transport{
			// Same egress policy as the dispatcher: an ambient HTTPS_PROXY in
			// the control-plane process must not silently reroute provider
			// traffic (dispatch.go documents the same decision).
			Proxy:                 nil,
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   upstreamModelsTimeout,
			ResponseHeaderTimeout: upstreamModelsTimeout,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, upstreamModelsTransportError(err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, upstreamModelsMaxBody+1))
	if readErr != nil {
		return nil, apierr.New(apierr.CodeUpstreamAllFailed).
			WithMessage("读取上游模型列表失败：" + readErr.Error())
	}
	if len(body) > upstreamModelsMaxBody {
		return nil, apierr.New(apierr.CodeUpstreamInvalidResponse).
			WithMessage("上游模型列表超出 1MB，疑似返回了非模型数据")
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// The single most likely cause, so say it instead of "上游异常".
		return nil, apierr.New(apierr.CodeUpstreamAuthFailed).
			WithMessage(fmt.Sprintf("上游拒绝了这把密钥（HTTP %d），请确认密钥有效且未过期", resp.StatusCode))
	case resp.StatusCode >= 300:
		return nil, apierr.New(apierr.CodeUpstreamAllFailed).
			WithMessage(fmt.Sprintf("上游返回 HTTP %d，无法读取模型列表：%s",
				resp.StatusCode, modelListSnippet(string(body), 120)))
	}

	items, parseErr := parseUpstreamModels(body)
	if parseErr != nil {
		return nil, apierr.New(apierr.CodeUpstreamInvalidResponse).
			WithMessage("上游模型列表格式无法识别：" + parseErr.Error())
	}
	return items, nil
}

func upstreamModelsTransportError(err error) *apierr.APIError {
	msg := err.Error()
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return apierr.New(apierr.CodeUpstreamTimeout).
			WithMessage(fmt.Sprintf("查询上游模型列表超时（%s）", upstreamModelsTimeout))
	}
	return apierr.New(apierr.CodeUpstreamAllFailed).
		WithMessage("无法连接上游查询模型列表：" + modelListSnippet(msg, 120))
}

// parseUpstreamModels accepts the OpenAI shape ({"data":[{"id":…}]}) and the
// flatter {"models":[…]} some gateways return. Unknown extra fields are
// ignored on purpose: providers add metadata faster than we can model it.
func parseUpstreamModels(body []byte) ([]upstreamModelItem, error) {
	var envelope struct {
		Data   []upstreamModelItem `json:"data"`
		Models []upstreamModelItem `json:"models"`
	}
	if err := jsonx.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	rows := envelope.Data
	if len(rows) == 0 {
		rows = envelope.Models
	}
	out := make([]upstreamModelItem, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		id := strings.TrimSpace(row.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		row.ID = id
		row.Name = strings.TrimSpace(row.Name)
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil, errors.New("没有解析到任何模型 id")
	}
	return out, nil
}

// annotateUpstreamModels marks which upstream names the platform already knows.
func annotateUpstreamModels(ctx context.Context, db *sql.DB, items []upstreamModelItem, modelMap map[string]string) {
	if len(items) == 0 {
		return
	}
	// Reverse the channel map: upstream name -> platform model name.
	reverse := make(map[string]string, len(modelMap))
	for platformModel, upstreamName := range modelMap {
		reverse[upstreamName] = platformModel
	}

	placeholders := make([]string, 0, len(items))
	args := make([]any, 0, len(items))
	for _, item := range items {
		placeholders = append(placeholders, "?")
		args = append(args, item.ID)
	}
	registered := make(map[string]struct{}, len(items))
	rows, err := db.QueryContext(ctx, `
		SELECT model_name FROM t_model
		WHERE deleted_at IS NULL AND model_name IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if scanErr := rows.Scan(&name); scanErr == nil {
				registered[name] = struct{}{}
			}
		}
	}
	for i := range items {
		if _, ok := registered[items[i].ID]; ok {
			items[i].Registered = true
		}
		if platformModel, ok := reverse[items[i].ID]; ok {
			items[i].PlatformModel = platformModel
		}
	}
}

// modelsEndpoint mirrors dispatch.upstreamURL so the discovery call hits the
// same URL the gateway would: /v1 is appended only when the base lacks it.
func modelsEndpoint(base string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return "", errors.New("接入地址为空")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("接入地址格式不正确")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("接入地址只支持 http 或 https")
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base + "/models", nil
}

func decodeModelMap(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := make(map[string]string)
	if err := jsonx.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// modelListSnippet is truncateRunes plus whitespace collapsing, so an HTML
// error page does not paste newlines into a one-line message.
func modelListSnippet(s string, limit int) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), limit)
}

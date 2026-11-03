// Package model implements the console model catalog (PRD §5.4 / F8).
//
// The catalog is a read-only projection of operator configuration. It shows
// LISTED, DOMESTIC models only, in the fixed order
// (recommend_weight DESC, published_at DESC), and prices come from
// t_price_rule without the key's group ratio: the plaza publishes the standard
// price, not the viewer's discounted price (PRD F1-5 / T12 hard constraint 7).
package model

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/control/units"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// Service owns the catalog queries.
type Service struct {
	DB     *sql.DB
	Now    clock.Clock
	Logger *obslog.Logger
}

// New wires the catalog service.
func New(db *sql.DB, now clock.Clock, logger *obslog.Logger) *Service {
	if now == nil {
		now = clock.Real{}
	}
	return &Service{DB: db, Now: now, Logger: logger}
}

func (s *Service) logError(ctx context.Context, msg string) {
	if s.Logger != nil {
		s.Logger.Error(ctx, msg, obslog.Fields{Stage: "control", Status: http.StatusInternalServerError})
	}
}

type capabilityView struct {
	Stream   *bool `json:"stream"`
	ToolCall *bool `json:"tool_call"`
	Vision   *bool `json:"vision"`
	JSONMode *bool `json:"json_mode"`
}

type pricingView struct {
	InPriceMicroPerK       int64   `json:"in_price_micro_per_k"`
	OutPriceMicroPerK      int64   `json:"out_price_micro_per_k"`
	PerCallPriceMicro      int64   `json:"per_call_price_micro"`
	InPriceCreditsPerK     string  `json:"in_price_credits_per_k"`
	OutPriceCreditsPerK    string  `json:"out_price_credits_per_k"`
	PerCallCredits         string  `json:"per_call_credits_per_call"`
	InPriceCNYPerK         string  `json:"in_price_cny_per_k"`
	OutPriceCNYPerK        string  `json:"out_price_cny_per_k"`
	PerCallCNY             string  `json:"per_call_cny_per_call"`
	CacheBillingEnabled    bool    `json:"cache_billing_enabled"`
	CacheHitRatioBP        *int64  `json:"cache_hit_ratio_bp,omitempty"`
	CacheWriteRatioBP      *int64  `json:"cache_write_ratio_bp,omitempty"`
	CacheHitPriceCredits   *string `json:"cache_hit_price_credits_per_k,omitempty"`
	CacheWritePriceCredits *string `json:"cache_write_price_credits_per_k,omitempty"`
}

type catalogItem struct {
	ID              int64          `json:"id"`
	ModelName       string         `json:"model_name"`
	DisplayName     string         `json:"display_name"`
	ModelType       string         `json:"model_type"`
	ContextLength   int64          `json:"context_length"`
	Protocol        string         `json:"protocol"`
	Aliases         []string       `json:"aliases"`
	Capabilities    capabilityView `json:"capabilities"`
	RecommendWeight int64          `json:"recommend_weight"`
	PublishedAt     *time.Time     `json:"published_at"`
	Pricing         *pricingView   `json:"pricing"`
}

type catalogResponse struct {
	Items    []catalogItem `json:"items"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Total    int64         `json:"total"`
}

type modelRow struct {
	ID              int64
	ModelName       string
	DisplayName     string
	ModelType       string
	ContextLength   int64
	CapabilityMask  int64
	RecommendWeight int64
	PublishedAt     sql.NullTime
	Protocol        string
}

// List implements GET /api/console/models (F8-1..F8-4).
func (s *Service) List(w http.ResponseWriter, r *http.Request) {
	page, apiErr := mw.ParsePage(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	query := r.URL.Query()
	modelType := strings.ToUpper(strings.TrimSpace(query.Get("model_type")))
	if modelType != "" && modelType != "TEXT" && modelType != "IMAGE" && modelType != "EMBEDDING" {
		mw.WriteError(w, r, mw.InvalidQueryField("model_type"))
		return
	}
	protocol := strings.ToUpper(strings.TrimSpace(query.Get("protocol")))
	if len(protocol) > 32 {
		mw.WriteError(w, r, mw.InvalidQueryField("protocol"))
		return
	}
	search := strings.TrimSpace(query.Get("q"))
	if len([]rune(search)) > 128 {
		mw.WriteError(w, r, mw.InvalidQueryField("q"))
		return
	}

	where := []string{
		"m.deleted_at IS NULL",
		"m.listing_state = 'LISTED'",
		"m.region = 'DOMESTIC'",
		"p.enabled = 1",
	}
	args := make([]any, 0, 4)
	if modelType != "" {
		where = append(where, "m.model_type = ?")
		args = append(args, modelType)
	}
	if protocol != "" {
		where = append(where, "p.code = ?")
		args = append(args, protocol)
	}
	if search != "" {
		pattern := likePattern(search)
		where = append(where, `(m.model_name LIKE ? ESCAPE '!' OR EXISTS (
			SELECT 1 FROM t_model_alias a
			 WHERE a.model_id = m.id AND a.enabled = 1 AND a.alias_name LIKE ? ESCAPE '!'))`)
		args = append(args, pattern, pattern)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int64
	if err := s.DB.QueryRowContext(r.Context(), `
		SELECT COUNT(*) FROM t_model m JOIN t_protocol p ON p.id = m.protocol_id
		 WHERE `+whereSQL, args...).Scan(&total); err != nil {
		s.logError(r.Context(), "model_catalog_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	// 默认按推荐权重排（运营手工调的顺序）；列表页允许按名称/上下文窗口重排。
	// 白名单而不是拼 `page.Sort`：那是用户输入，直接进 ORDER BY 是注入面。
	orderBy := "m.recommend_weight DESC, m.published_at DESC"
	switch page.Sort {
	case "model_name", "context_length":
		column := "m.model_name"
		if page.Sort == "context_length" {
			column = "m.context_length"
		}
		direction := "DESC"
		if strings.EqualFold(page.Order, "asc") {
			direction = "ASC"
		}
		// id 作为决胜列，否则同值的行在翻页之间会换位置。
		orderBy = column + " " + direction + ", m.id " + direction
	}
	pageArgs := append(append([]any{}, args...), page.Size, page.OffsetInt64())
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT m.id, m.model_name, m.display_name, m.model_type, m.context_length,
		       m.capability_mask, m.recommend_weight, m.published_at, p.code
		  FROM t_model m JOIN t_protocol p ON p.id = m.protocol_id
		 WHERE `+whereSQL+`
		 ORDER BY `+orderBy+`
		 LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		s.logError(r.Context(), "model_catalog_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer rows.Close()

	models := make([]modelRow, 0, page.Size)
	ids := make([]int64, 0, page.Size)
	for rows.Next() {
		var (
			m         modelRow
			published sql.NullTime
		)
		if err := rows.Scan(&m.ID, &m.ModelName, &m.DisplayName, &m.ModelType,
			&m.ContextLength, &m.CapabilityMask, &m.RecommendWeight, &published, &m.Protocol); err != nil {
			s.logError(r.Context(), "model_catalog_scan_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		m.PublishedAt = published
		models = append(models, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		s.logError(r.Context(), "model_catalog_rows_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	prices, err := s.loadPrices(r.Context(), ids)
	if err != nil {
		s.logError(r.Context(), "model_catalog_price_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	caps, err := s.loadCapabilities(r.Context(), ids)
	if err != nil {
		s.logError(r.Context(), "model_catalog_caps_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	aliases, err := s.loadAliases(r.Context(), ids)
	if err != nil {
		s.logError(r.Context(), "model_catalog_alias_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	cacheEnabled := s.cacheBillingEnabled(r.Context())

	items := make([]catalogItem, 0, len(models))
	for _, m := range models {
		item := catalogItem{
			ID:              m.ID,
			ModelName:       m.ModelName,
			DisplayName:     m.DisplayName,
			ModelType:       m.ModelType,
			ContextLength:   m.ContextLength,
			Protocol:        m.Protocol,
			Aliases:         aliases[m.ID],
			Capabilities:    buildCapabilities(m.CapabilityMask, caps[m.ID]),
			RecommendWeight: m.RecommendWeight,
			PublishedAt:     nullableTime(m.PublishedAt),
		}
		if item.Aliases == nil {
			item.Aliases = []string{}
		}
		if price, ok := prices[m.ID]; ok {
			view := buildPricing(price, cacheEnabled)
			item.Pricing = &view
		} else {
			// LISTED is supposed to have an active price (TDS §1.4). Returning
			// null lets the console flag it instead of showing a fake zero.
			s.logError(r.Context(), "model_catalog_price_missing: model_id="+strconv.FormatInt(m.ID, 10))
		}
		items = append(items, item)
	}

	_ = mw.WriteJSON(w, http.StatusOK, catalogResponse{
		Items: items, Page: page.Number, PageSize: page.Size, Total: total,
	})
}

type priceRow struct {
	InPriceMicro      int64
	OutPriceMicro     int64
	PerCallMicro      int64
	CacheHitRatioBP   sql.NullInt64
	CacheWriteRatioBP sql.NullInt64
}

// loadPrices picks the same effective row boot.RefreshPrices uses: the highest
// version among enabled MODEL-scope rules inside their effective window.
func (s *Service) loadPrices(ctx context.Context, ids []int64) (map[int64]priceRow, error) {
	out := make(map[int64]priceRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	now := s.Now.Now().UTC()
	args := make([]any, 0, len(ids)+4)
	args = append(args, now, now, now, now)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT p.scope_id, p.in_price_micro_per_k, p.out_price_micro_per_k,
		       p.per_call_price_micro, p.cache_hit_ratio_bp, p.cache_write_ratio_bp
		  FROM t_price_rule p
		 WHERE p.scope_level = 'MODEL' AND p.enabled = 1
		   AND p.effective_from <= ? AND (p.effective_to IS NULL OR p.effective_to > ?)
		   AND p.id = (
		       SELECT p2.id FROM t_price_rule p2
		        WHERE p2.scope_level = 'MODEL' AND p2.scope_id = p.scope_id AND p2.enabled = 1
		          AND p2.effective_from <= ? AND (p2.effective_to IS NULL OR p2.effective_to > ?)
		        ORDER BY p2.version DESC, p2.id DESC LIMIT 1)
		   AND p.scope_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id int64
			p  priceRow
		)
		if err := rows.Scan(&id, &p.InPriceMicro, &p.OutPriceMicro, &p.PerCallMicro,
			&p.CacheHitRatioBP, &p.CacheWriteRatioBP); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

// loadCapabilities returns supported flags from t_model_capability. A missing
// row is not "unsupported": B-53 distinguishes "—" (unconfigured) from
// "不支持" (configured false).
func (s *Service) loadCapabilities(ctx context.Context, ids []int64) (map[int64]map[string]bool, error) {
	out := make(map[int64]map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT model_id, cap_code, supported FROM t_model_capability
		 WHERE model_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id      int64
			code    string
			support bool
		)
		if err := rows.Scan(&id, &code, &support); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = make(map[string]bool, 4)
		}
		out[id][code] = support
	}
	return out, rows.Err()
}

func (s *Service) loadAliases(ctx context.Context, ids []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT model_id, alias_name FROM t_model_alias
		 WHERE enabled = 1 AND model_id IN (`+placeholders(len(ids))+`)
		 ORDER BY alias_name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id    int64
			alias string
		)
		if err := rows.Scan(&id, &alias); err != nil {
			return nil, err
		}
		out[id] = append(out[id], alias)
	}
	return out, rows.Err()
}

// cacheBillingEnabled fails closed: an unreadable or missing configuration
// hides the cache price rather than showing a price the platform will not bill.
func (s *Service) cacheBillingEnabled(ctx context.Context) bool {
	var raw []byte
	err := s.DB.QueryRowContext(ctx,
		`SELECT cfg_value FROM t_config_item WHERE cfg_key = ? AND enabled = 1 LIMIT 1`,
		string(config.KeyPricingCacheBillingEnabled)).Scan(&raw)
	if err != nil {
		return false
	}
	value, err := config.Parse(config.KeyPricingCacheBillingEnabled, raw)
	if err != nil {
		return false
	}
	enabled, ok := value.Bool()
	return ok && enabled
}

func buildCapabilities(mask int64, configured map[string]bool) capabilityView {
	return capabilityView{
		Stream:   capValue("STREAM", 0, mask, configured),
		ToolCall: capValue("TOOL_CALL", 1, mask, configured),
		Vision:   capValue("VISION", 2, mask, configured),
		JSONMode: capValue("JSON_MODE", 3, mask, configured),
	}
}

func capValue(code string, bit uint, mask int64, configured map[string]bool) *bool {
	if v, ok := configured[code]; ok {
		return &v
	}
	if mask&(1<<bit) != 0 {
		v := true
		return &v
	}
	return nil
}

func buildPricing(p priceRow, cacheEnabled bool) pricingView {
	v := pricingView{
		InPriceMicroPerK:    p.InPriceMicro,
		OutPriceMicroPerK:   p.OutPriceMicro,
		PerCallPriceMicro:   p.PerCallMicro,
		InPriceCreditsPerK:  units.Credits(p.InPriceMicro),
		OutPriceCreditsPerK: units.Credits(p.OutPriceMicro),
		PerCallCredits:      units.Credits(p.PerCallMicro),
		InPriceCNYPerK:      units.CNY(p.InPriceMicro),
		OutPriceCNYPerK:     units.CNY(p.OutPriceMicro),
		PerCallCNY:          units.CNY(p.PerCallMicro),
		CacheBillingEnabled: cacheEnabled,
	}
	// F8-3: when cache billing is off, no cache price may appear — not even a
	// zero or a ratio that would imply a discount.
	if !cacheEnabled {
		return v
	}
	if p.CacheHitRatioBP.Valid {
		ratio := p.CacheHitRatioBP.Int64
		v.CacheHitRatioBP = &ratio
		if p.InPriceMicro > 0 {
			micro := (p.InPriceMicro*ratio + 5000) / 10000
			credits := units.Credits(micro)
			v.CacheHitPriceCredits = &credits
		}
	}
	if p.CacheWriteRatioBP.Valid {
		ratio := p.CacheWriteRatioBP.Int64
		v.CacheWriteRatioBP = &ratio
		if p.InPriceMicro > 0 {
			micro := (p.InPriceMicro*ratio + 5000) / 10000
			credits := units.Credits(micro)
			v.CacheWritePriceCredits = &credits
		}
	}
	return v
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// likePattern escapes LIKE metacharacters with '!' (not backslash) so the
// query stays valid under NO_BACKSLASH_ESCAPES.
func likePattern(q string) string {
	replacer := strings.NewReplacer(
		"!", "!!",
		"%", "!%",
		"_", "!_",
	)
	return "%" + replacer.Replace(q) + "%"
}

func nullableTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

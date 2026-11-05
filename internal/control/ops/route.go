package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
)

// Route management (T45).
//
// t_route is the only place the gateway reads "which channel may serve this
// model". Before this segment the console could create a model with a
// primary_channel_id and list it, but nothing wrote t_route, so every call
// ended in 503 no_available_upstream. These four endpoints make the write
// surface explicit and keep t_route.region in lockstep with t_channel.region
// (the gateway filters routes by region, so a client-supplied region would
// silently hide the route).
//
// Every write republishes cfg:snap:ROUTING after commit; reads never publish.

const (
	routeWeightMax           int64 = 10_000
	routePriorityMax         int64 = 32_767
	routeMaxTokensCeilingMax int64 = 4_294_967_295
	routeUpstreamNameMax           = 128
)

type routeView struct {
	ID                int64      `json:"id"`
	ModelID           int64      `json:"model_id"`
	ModelName         string     `json:"model_name"`
	ChannelID         int64      `json:"channel_id"`
	ChannelName       string     `json:"channel_name"`
	ProtocolID        int64      `json:"protocol_id"`
	Protocol          string     `json:"protocol"`
	UpstreamModelName string     `json:"upstream_model_name"`
	Region            string     `json:"region"`
	Weight            int64      `json:"weight"`
	Priority          int64      `json:"priority"`
	StreamSupported   bool       `json:"stream_supported"`
	MaxTokensCeiling  int64      `json:"max_tokens_ceiling"`
	Enabled           bool       `json:"enabled"`
	ProbeAt           *time.Time `json:"probe_at,omitempty"`
	ProbeResult       *string    `json:"probe_result,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type routeListResponse struct {
	Items    []routeView `json:"items"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
	Total    int64       `json:"total"`
}

type routeDetailResponse struct {
	Route routeView `json:"route"`
}

// routeCreateRequest deliberately has no region field. The value always comes
// from t_channel; an operator who could override it would create a route the
// gateway never selects.
type routeCreateRequest struct {
	ModelID           *int64  `json:"model_id"`
	ChannelID         *int64  `json:"channel_id"`
	ProtocolID        *int64  `json:"protocol_id"`
	UpstreamModelName *string `json:"upstream_model_name"`
	Weight            *int64  `json:"weight"`
	Priority          *int64  `json:"priority"`
	StreamSupported   *bool   `json:"stream_supported"`
	MaxTokensCeiling  *int64  `json:"max_tokens_ceiling"`
	Enabled           *bool   `json:"enabled"`
}

type routePatchRequest struct {
	UpstreamModelName *string `json:"upstream_model_name"`
	Weight            *int64  `json:"weight"`
	Priority          *int64  `json:"priority"`
	StreamSupported   *bool   `json:"stream_supported"`
	MaxTokensCeiling  *int64  `json:"max_tokens_ceiling"`
	Enabled           *bool   `json:"enabled"`
}

const routeSelectColumns = `
	r.id, r.model_id, m.model_name, r.channel_id, c.name, r.protocol_id, p.code,
	r.upstream_model_name, r.region, r.weight, r.priority, r.stream_supported,
	r.max_tokens_ceiling, r.enabled, r.probe_at, r.probe_result,
	r.created_at, r.updated_at`

const routeFrom = `
	FROM t_route r
	LEFT JOIN t_model m ON m.id = r.model_id
	LEFT JOIN t_channel c ON c.id = r.channel_id
	LEFT JOIN t_protocol p ON p.id = r.protocol_id`

// ListRoutes implements GET /api/admin/routes.
func (s *Service) ListRoutes(w http.ResponseWriter, r *http.Request) {
	page, apiErr := mw.ParsePage(r, "created_at", "updated_at", "id", "priority", "weight")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	q := r.URL.Query()
	modelID, apiErr := queryPositiveInt(q.Get("model_id"), "model_id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	channelID, apiErr := queryPositiveInt(q.Get("channel_id"), "channel_id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	region := strings.ToUpper(strings.TrimSpace(q.Get("region")))
	if region != "" && region != regionDomestic && region != regionOverseas {
		mw.WriteError(w, r, mw.InvalidQueryField("region"))
		return
	}
	var (
		enabledSet bool
		enabled    bool
	)
	switch raw := strings.TrimSpace(q.Get("enabled")); raw {
	case "":
	case "0":
		enabledSet, enabled = true, false
	case "1":
		enabledSet, enabled = true, true
	default:
		mw.WriteError(w, r, mw.InvalidQueryField("enabled"))
		return
	}

	where := []string{"1 = 1"}
	args := make([]any, 0, 4)
	if modelID > 0 {
		where = append(where, "r.model_id = ?")
		args = append(args, modelID)
	}
	if channelID > 0 {
		where = append(where, "r.channel_id = ?")
		args = append(args, channelID)
	}
	if region != "" {
		where = append(where, "r.region = ?")
		args = append(args, region)
	}
	if enabledSet {
		where = append(where, "r.enabled = ?")
		args = append(args, enabled)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int64
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM t_route r WHERE `+whereSQL, args...).Scan(&total); err != nil {
		s.logError(r.Context(), "ops_route_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	queryArgs := append(append([]any{}, args...), page.Size, page.OffsetInt64())
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT `+routeSelectColumns+routeFrom+`
		 WHERE `+whereSQL+`
		 ORDER BY `+routeOrder(page)+` LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		s.logError(r.Context(), "ops_route_list_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer rows.Close()

	items := make([]routeView, 0, page.Size)
	for rows.Next() {
		item, err := scanRoute(rows)
		if err != nil {
			s.logError(r.Context(), "ops_route_scan_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		s.logError(r.Context(), "ops_route_rows_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, routeListResponse{
		Items: items, Page: page.Number, PageSize: page.Size, Total: total,
	})
}

// CreateRoute implements POST /api/admin/routes.
//
// Unlike RegisterUpstreamModels this is not an idempotent import: the caller
// is picking one binding explicitly, so a duplicate unique key is a conflict
// the operator must see (409), not a silent no-op.
func (s *Service) CreateRoute(w http.ResponseWriter, r *http.Request) {
	var req routeCreateRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if req.ModelID == nil || *req.ModelID <= 0 {
		mw.WriteError(w, r, mw.InvalidField("model_id"))
		return
	}
	if req.ChannelID == nil || *req.ChannelID <= 0 {
		mw.WriteError(w, r, mw.InvalidField("channel_id"))
		return
	}
	upstreamName, apiErr := validateUpstreamModelName(req.UpstreamModelName)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	weight := int64(100)
	if req.Weight != nil {
		weight = *req.Weight
	}
	priority := int64(100)
	if req.Priority != nil {
		priority = *req.Priority
	}
	ceiling := int64(0)
	if req.MaxTokensCeiling != nil {
		ceiling = *req.MaxTokensCeiling
	}
	if apiErr := validateRouteNumbers(weight, priority, ceiling); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	streamSupported := true
	if req.StreamSupported != nil {
		streamSupported = *req.StreamSupported
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_route_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	ok, err := modelExists(ctx, tx, *req.ModelID)
	if err != nil {
		s.logError(ctx, "ops_route_model_read_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if !ok {
		mw.WriteError(w, r, mw.InvalidField("model_id"))
		return
	}
	region, channelProtocol, apiErr := loadChannelRegionProtocol(ctx, tx, *req.ChannelID, "channel_id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	protocolID := channelProtocol
	if req.ProtocolID != nil {
		if *req.ProtocolID <= 0 || !protocolExists(ctx, tx, *req.ProtocolID) {
			mw.WriteError(w, r, mw.InvalidField("protocol_id"))
			return
		}
		protocolID = *req.ProtocolID
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO t_route
			(model_id, channel_id, protocol_id, upstream_model_name, region,
			 weight, priority, stream_supported, max_tokens_ceiling, enabled,
			 created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		*req.ModelID, *req.ChannelID, protocolID, upstreamName, region,
		weight, priority, streamSupported, ceiling, enabled, now, now)
	if err != nil {
		if isDuplicateKey(err) {
			mw.WriteError(w, r, routeDuplicate())
			return
		}
		s.logError(ctx, "ops_route_insert_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		s.logError(ctx, "ops_route_id_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	view, apiErr := loadRouteTx(ctx, tx, id, false)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if err := s.insertAudit(ctx, tx, r, "MODEL", "CREATE", "t_route", id, nil,
		routeAuditView(view), ""); err != nil {
		s.logError(ctx, "ops_route_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_route_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	if apiErr := s.publish(ctx); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusCreated, routeDetailResponse{Route: view})
}

// UpdateRoute implements PATCH /api/admin/routes/{id}.
func (s *Service) UpdateRoute(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req routePatchRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if req.UpstreamModelName == nil && req.Weight == nil && req.Priority == nil &&
		req.StreamSupported == nil && req.MaxTokensCeiling == nil && req.Enabled == nil {
		mw.WriteError(w, r, mw.InvalidField("body"))
		return
	}

	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_route_patch_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	before, apiErr := loadRouteTx(ctx, tx, id, true)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	sets := make([]string, 0, 7)
	args := make([]any, 0, 8)
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if req.UpstreamModelName != nil {
		name, apiErr := validateUpstreamModelName(req.UpstreamModelName)
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		add("upstream_model_name", name)
	}
	if req.Weight != nil {
		if *req.Weight < 0 || *req.Weight > routeWeightMax {
			mw.WriteError(w, r, mw.InvalidField("weight"))
			return
		}
		add("weight", *req.Weight)
	}
	if req.Priority != nil {
		if *req.Priority < 0 || *req.Priority > routePriorityMax {
			mw.WriteError(w, r, mw.InvalidField("priority"))
			return
		}
		add("priority", *req.Priority)
	}
	if req.MaxTokensCeiling != nil {
		if *req.MaxTokensCeiling < 0 || *req.MaxTokensCeiling > routeMaxTokensCeilingMax {
			mw.WriteError(w, r, mw.InvalidField("max_tokens_ceiling"))
			return
		}
		add("max_tokens_ceiling", *req.MaxTokensCeiling)
	}
	if req.StreamSupported != nil {
		add("stream_supported", *req.StreamSupported)
	}
	if req.Enabled != nil {
		add("enabled", *req.Enabled)
	}
	add("updated_at", now)
	if _, err := tx.ExecContext(ctx,
		`UPDATE t_route SET `+strings.Join(sets, ", ")+` WHERE id = ?`,
		append(args, id)...); err != nil {
		if isDuplicateKey(err) {
			mw.WriteError(w, r, routeDuplicate())
			return
		}
		s.logError(ctx, "ops_route_update_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	after, apiErr := loadRouteTx(ctx, tx, id, false)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if err := s.insertAudit(ctx, tx, r, "MODEL", "UPDATE", "t_route", id,
		routeAuditView(before), routeAuditView(after), ""); err != nil {
		s.logError(ctx, "ops_route_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_route_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	if apiErr := s.publish(ctx); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, routeDetailResponse{Route: after})
}

// DeleteRoute implements DELETE /api/admin/routes/{id}.
//
// A LISTED model whose last enabled route is deleted would start answering
// 503 immediately, so that specific removal is refused. Deleting a disabled
// route, or any route of a CATALOG_ONLY / DELISTED model, stays allowed.
func (s *Service) DeleteRoute(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	ctx := r.Context()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_route_delete_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	before, apiErr := loadRouteTx(ctx, tx, id, true)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if before.Enabled {
		var listing string
		err := tx.QueryRowContext(ctx, `
			SELECT listing_state FROM t_model
			 WHERE id = ? AND deleted_at IS NULL`, before.ModelID).Scan(&listing)
		switch {
		case err == nil && listing == listingListed:
			var remaining int64
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM t_route
				 WHERE model_id = ? AND enabled = 1 AND id <> ?`,
				before.ModelID, id).Scan(&remaining); err != nil {
				s.logError(ctx, "ops_route_delete_count_failed: "+err.Error())
				mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
				return
			}
			if remaining == 0 {
				mw.WriteError(w, r, lastRouteRefusal(before.ModelID))
				return
			}
		case errors.Is(err, sql.ErrNoRows):
			// Model row is gone (or soft-deleted); there is no listing state to
			// protect, so the route may be cleaned up.
		case err != nil:
			s.logError(ctx, "ops_route_delete_model_read_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM t_route WHERE id = ?`, id); err != nil {
		s.logError(ctx, "ops_route_delete_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := s.insertAudit(ctx, tx, r, "MODEL", "DELETE", "t_route", id,
		routeAuditView(before), nil, ""); err != nil {
		s.logError(ctx, "ops_route_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_route_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	if apiErr := s.publish(ctx); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// ---------------------------------------------------------------------------
// validation and storage helpers
// ---------------------------------------------------------------------------

func validateUpstreamModelName(raw *string) (string, *apierr.APIError) {
	if raw == nil {
		return "", mw.InvalidField("upstream_model_name")
	}
	name := strings.TrimSpace(*raw)
	if name == "" || len(name) > routeUpstreamNameMax {
		return "", mw.InvalidField("upstream_model_name")
	}
	return name, nil
}

func validateRouteNumbers(weight, priority, ceiling int64) *apierr.APIError {
	if weight < 0 || weight > routeWeightMax {
		return mw.InvalidField("weight")
	}
	if priority < 0 || priority > routePriorityMax {
		return mw.InvalidField("priority")
	}
	if ceiling < 0 || ceiling > routeMaxTokensCeilingMax {
		return mw.InvalidField("max_tokens_ceiling")
	}
	return nil
}

// queryPositiveInt treats an absent filter as 0 ("no filter") and refuses a
// malformed or non-positive one, so a typo cannot silently return every route.
func queryPositiveInt(raw, field string) (int64, *apierr.APIError) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, mw.InvalidQueryField(field)
	}
	return v, nil
}

// loadChannelRegionProtocol returns the values a route must copy from its
// channel. field names the request key in the 400 so the console can point at
// the exact member.
func loadChannelRegionProtocol(ctx context.Context, q modelQueryer, channelID int64, field string) (string, int64, *apierr.APIError) {
	var (
		region   string
		protocol int64
	)
	err := q.QueryRowContext(ctx, `
		SELECT region, protocol_id FROM t_channel
		 WHERE id = ? AND deleted_at IS NULL`, channelID).Scan(&region, &protocol)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, mw.InvalidField(field)
	}
	if err != nil {
		return "", 0, apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	return region, protocol, nil
}

func loadRouteTx(ctx context.Context, tx *sql.Tx, id int64, forUpdate bool) (routeView, *apierr.APIError) {
	suffix := ""
	if forUpdate {
		suffix = " FOR UPDATE"
	}
	view, err := scanRoute(tx.QueryRowContext(ctx,
		`SELECT `+routeSelectColumns+routeFrom+` WHERE r.id = ?`+suffix, id))
	if errors.Is(err, sql.ErrNoRows) {
		return routeView{}, apierr.New(apierr.CodeResourceNotFound)
	}
	if err != nil {
		return routeView{}, apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	return view, nil
}

func scanRoute(row rowScanner) (routeView, error) {
	var (
		v                      routeView
		modelName, channelName sql.NullString
		protocol               sql.NullString
		probeAt                sql.NullTime
		probeResult            sql.NullString
		createdAt, updatedAt   time.Time
	)
	if err := row.Scan(
		&v.ID, &v.ModelID, &modelName, &v.ChannelID, &channelName,
		&v.ProtocolID, &protocol, &v.UpstreamModelName, &v.Region,
		&v.Weight, &v.Priority, &v.StreamSupported, &v.MaxTokensCeiling,
		&v.Enabled, &probeAt, &probeResult, &createdAt, &updatedAt,
	); err != nil {
		return routeView{}, err
	}
	v.ModelName = modelName.String
	v.ChannelName = channelName.String
	v.Protocol = protocol.String
	v.ProbeAt = nullableTime(probeAt)
	if probeResult.Valid {
		result := probeResult.String
		v.ProbeResult = &result
	}
	v.CreatedAt = createdAt.UTC()
	v.UpdatedAt = updatedAt.UTC()
	return v, nil
}

func routeOrder(page mw.Page) string {
	col := "r.created_at"
	switch page.Sort {
	case "updated_at":
		col = "r.updated_at"
	case "id":
		col = "r.id"
	case "priority":
		col = "r.priority"
	case "weight":
		col = "r.weight"
	}
	dir := orderDirection(page.Order)
	return col + " " + dir + ", r.id " + dir
}

func routeDuplicate() *apierr.APIError {
	return apierr.Newf(apierr.CodeResourceStateConflict,
		"同一模型在该渠道下已存在相同协议与上游模型名的路由")
}

func lastRouteRefusal(modelID int64) *apierr.APIError {
	return apierr.Newf(apierr.CodeResourceStateConflict,
		fmt.Sprintf("模型（id=%d）当前为 LISTED，删除这条路由后将没有可用的启用路由", modelID)).
		WithHint("先把模型改为 CATALOG_ONLY 或先创建替代路由，再删除")
}

func routeAuditView(v routeView) map[string]any {
	return map[string]any{
		"model_id":            v.ModelID,
		"channel_id":          v.ChannelID,
		"protocol_id":         v.ProtocolID,
		"upstream_model_name": v.UpstreamModelName,
		"region":              v.Region,
		"weight":              v.Weight,
		"priority":            v.Priority,
		"stream_supported":    v.StreamSupported,
		"max_tokens_ceiling":  v.MaxTokensCeiling,
		"enabled":             v.Enabled,
	}
}

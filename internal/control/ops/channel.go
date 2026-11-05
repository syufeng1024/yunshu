package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
)

// Channel scope modes (TDS t_channel).
const (
	scopeAll      = "ALL"
	scopeSelected = "SELECTED"

	regionDomestic = "DOMESTIC"
	regionOverseas = "OVERSEAS"
)

// channelView is the only channel representation that crosses the HTTP
// boundary. There is deliberately no UpstreamKey field and no envelope field:
// the database column is write-only from the API's point of view.
type channelView struct {
	ID                 int64             `json:"id"`
	Name               string            `json:"name"`
	ProtocolID         int64             `json:"protocol_id"`
	Protocol           string            `json:"protocol"`
	Region             string            `json:"region"`
	BaseURL            string            `json:"base_url"`
	UpstreamKeyMask    string            `json:"upstream_key_mask"`
	KeyRotationAt      *time.Time        `json:"key_rotation_at,omitempty"`
	GroupScopeMode     string            `json:"group_scope_mode"`
	DedicatedScopeMode string            `json:"dedicated_scope_mode"`
	FallbackAllowed    bool              `json:"fallback_allowed"`
	DedicatedUserIDs   *[]int64          `json:"dedicated_user_ids,omitempty"`
	Weight             int64             `json:"weight"`
	Priority           int64             `json:"priority"`
	TimeoutConnectMS   int64             `json:"timeout_connect_ms"`
	TimeoutFirstByteMS int64             `json:"timeout_first_byte_ms"`
	TimeoutTotalMS     int64             `json:"timeout_total_ms"`
	RetryMax           int64             `json:"retry_max"`
	StreamSupported    bool              `json:"stream_supported"`
	StreamUsageMode    string            `json:"stream_usage_mode"`
	UpstreamModelNames map[string]string `json:"upstream_model_name_map,omitempty"`
	HealthState        string            `json:"health_state"`
	// Last-15-minute upstream attempt success rate. nil when there were no
	// attempts; never 10000 for "no data" (hard constraint 2).
	SuccessRateBP *int64    `json:"success_rate_bp"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type channelListResponse struct {
	Items    []channelView `json:"items"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Total    int64         `json:"total"`
}

type channelDetailResponse struct {
	Channel channelView `json:"channel"`
}

type channelCreateRequest struct {
	Name                 *string            `json:"name"`
	ProtocolID           *int64             `json:"protocol_id"`
	Region               *string            `json:"region"`
	BaseURL              *string            `json:"base_url"`
	UpstreamKey          *string            `json:"upstream_key"`
	GroupScopeMode       *string            `json:"group_scope_mode"`
	DedicatedScopeMode   *string            `json:"dedicated_scope_mode"`
	DedicatedUserIDs     *[]int64           `json:"dedicated_user_ids"`
	FallbackAllowed      *bool              `json:"fallback_allowed"`
	Weight               *int64             `json:"weight"`
	Priority             *int64             `json:"priority"`
	TimeoutConnectMS     *int64             `json:"timeout_connect_ms"`
	TimeoutFirstByteMS   *int64             `json:"timeout_first_byte_ms"`
	TimeoutTotalMS       *int64             `json:"timeout_total_ms"`
	RetryMax             *int64             `json:"retry_max"`
	StreamSupported      *bool              `json:"stream_supported"`
	StreamUsageMode      *string            `json:"stream_usage_mode"`
	UpstreamModelNameMap *map[string]string `json:"upstream_model_name_map"`
	Enabled              *bool              `json:"enabled"`
}

type channelPatchRequest struct {
	Name                 *string            `json:"name"`
	ProtocolID           *int64             `json:"protocol_id"`
	Region               *string            `json:"region"`
	BaseURL              *string            `json:"base_url"`
	UpstreamKey          *string            `json:"upstream_key"`
	GroupScopeMode       *string            `json:"group_scope_mode"`
	DedicatedScopeMode   *string            `json:"dedicated_scope_mode"`
	DedicatedUserIDs     *[]int64           `json:"dedicated_user_ids"`
	FallbackAllowed      *bool              `json:"fallback_allowed"`
	Weight               *int64             `json:"weight"`
	Priority             *int64             `json:"priority"`
	TimeoutConnectMS     *int64             `json:"timeout_connect_ms"`
	TimeoutFirstByteMS   *int64             `json:"timeout_first_byte_ms"`
	TimeoutTotalMS       *int64             `json:"timeout_total_ms"`
	RetryMax             *int64             `json:"retry_max"`
	StreamSupported      *bool              `json:"stream_supported"`
	StreamUsageMode      *string            `json:"stream_usage_mode"`
	UpstreamModelNameMap *map[string]string `json:"upstream_model_name_map"`
	Enabled              *bool              `json:"enabled"`
}

const channelSelectColumns = `
	c.id, c.name, c.protocol_id, p.code, c.region, c.base_url,
	c.upstream_key_mask, c.key_rotation_at, c.group_scope_mode,
	c.dedicated_scope_mode, c.fallback_allowed, c.weight, c.priority,
	c.timeout_connect_ms, c.timeout_first_byte_ms, c.timeout_total_ms,
	c.retry_max, c.stream_supported, c.stream_usage_mode,
	c.upstream_model_name_map, c.health_state, c.enabled, c.created_at, c.updated_at`

// ListChannels implements GET /api/admin/channels.
func (s *Service) ListChannels(w http.ResponseWriter, r *http.Request) {
	page, apiErr := mw.ParsePage(r, "created_at", "updated_at", "name", "weight", "priority")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("q"))
	if len([]rune(name)) > 64 {
		mw.WriteError(w, r, mw.InvalidQueryField("q"))
		return
	}
	region := strings.ToUpper(strings.TrimSpace(q.Get("region")))
	if region != "" && region != regionDomestic && region != regionOverseas {
		mw.WriteError(w, r, mw.InvalidQueryField("region"))
		return
	}
	enabled := strings.TrimSpace(q.Get("enabled"))
	if enabled != "" && enabled != "0" && enabled != "1" {
		mw.WriteError(w, r, mw.InvalidQueryField("enabled"))
		return
	}
	groupScope := strings.ToUpper(strings.TrimSpace(q.Get("group_scope_mode")))
	if groupScope != "" && groupScope != scopeAll && groupScope != scopeSelected {
		mw.WriteError(w, r, mw.InvalidQueryField("group_scope_mode"))
		return
	}

	where := []string{"c.deleted_at IS NULL"}
	args := make([]any, 0, 4)
	if name != "" {
		where = append(where, "c.name LIKE ? ESCAPE '!'")
		args = append(args, likePattern(name))
	}
	if region != "" {
		where = append(where, "c.region = ?")
		args = append(args, region)
	}
	if enabled != "" {
		where = append(where, "c.enabled = ?")
		args = append(args, enabled)
	}
	if groupScope != "" {
		where = append(where, "c.group_scope_mode = ?")
		args = append(args, groupScope)
	}
	whereSQL := strings.Join(where, " AND ")
	orderBy := channelOrder(page)

	var total int64
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM t_channel c WHERE `+whereSQL, args...).Scan(&total); err != nil {
		s.logError(r.Context(), "ops_channel_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	queryArgs := append(append([]any{}, args...), page.Size, page.OffsetInt64())
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT `+channelSelectColumns+`
		  FROM t_channel c JOIN t_protocol p ON p.id = c.protocol_id
		 WHERE `+whereSQL+`
		 ORDER BY `+orderBy+` LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		s.logError(r.Context(), "ops_channel_list_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer rows.Close()

	items := make([]channelView, 0, page.Size)
	for rows.Next() {
		item, err := scanChannel(rows)
		if err != nil {
			s.logError(r.Context(), "ops_channel_scan_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		s.logError(r.Context(), "ops_channel_rows_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := s.attachChannelRates(r.Context(), s.DB, items); err != nil {
		s.logError(r.Context(), "ops_channel_rate_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, channelListResponse{
		Items: items, Page: page.Number, PageSize: page.Size, Total: total,
	})
}

// GetChannel implements GET /api/admin/channels/{id}. A soft-deleted row is a
// friendly 404, never a 500.
func (s *Service) GetChannel(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	view, apiErr := s.loadChannel(r.Context(), s.DB, id, false)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	views := []channelView{view}
	if err := s.attachChannelRates(r.Context(), s.DB, views); err != nil {
		s.logError(r.Context(), "ops_channel_rate_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, channelDetailResponse{Channel: views[0]})
}

// CreateChannel implements POST /api/admin/channels.
func (s *Service) CreateChannel(w http.ResponseWriter, r *http.Request) {
	var req channelCreateRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	in, envelope, mask, apiErr := s.validateCreateChannel(r, &req)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_channel_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if apiErr := channelNameTaken(ctx, tx, in.name, 0); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if !protocolExists(ctx, tx, in.protocolID) {
		mw.WriteError(w, r, mw.InvalidField("protocol_id"))
		return
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO t_channel
			(name, protocol_id, region, base_url, upstream_key_enc, upstream_key_mask,
			 group_scope_mode, dedicated_scope_mode, fallback_allowed, weight, priority,
			 timeout_connect_ms, timeout_first_byte_ms, timeout_total_ms, retry_max,
			 stream_supported, stream_usage_mode, upstream_model_name_map, enabled,
			 created_by, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.name, in.protocolID, in.region, in.baseURL, []byte(envelope), mask,
		in.groupScopeMode, in.dedicatedScopeMode, in.fallbackAllowed, in.weight, in.priority,
		in.timeoutConnectMS, in.timeoutFirstByteMS, in.timeoutTotalMS, in.retryMax,
		in.streamSupported, in.streamUsageMode, in.modelNameMap, in.enabled,
		mustActorID(r), now, now)
	if err != nil {
		s.logError(ctx, "ops_channel_insert_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		s.logError(ctx, "ops_channel_id_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if apiErr := replaceDedicatedUsers(ctx, tx, id, in.dedicatedUserIDs, mustActorID(r), now); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	view, apiErr := s.loadChannelTx(ctx, tx, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if err := s.insertAudit(ctx, tx, r, "CHANNEL", "CREATE", "t_channel", id, nil, view, ""); err != nil {
		s.logError(ctx, "ops_channel_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_channel_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	views := []channelView{view}
	if err := s.attachChannelRates(ctx, s.DB, views); err != nil {
		s.logError(ctx, "ops_channel_rate_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusCreated, channelDetailResponse{Channel: views[0]})
}

// UpdateChannel implements PATCH /api/admin/channels/{id}.
func (s *Service) UpdateChannel(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req channelPatchRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if !hasChannelPatch(&req) {
		mw.WriteError(w, r, mw.InvalidField("body"))
		return
	}

	ctx := r.Context()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_channel_patch_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	before, apiErr := s.loadChannelTxForUpdate(ctx, tx, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	next, envelope, mask, routingChanged, apiErr := s.validatePatchChannel(r, tx, &req, before)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	now := s.now()
	sets := make([]string, 0, 20)
	args := make([]any, 0, 22)
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if req.Name != nil {
		add("name", next.name)
	}
	if req.ProtocolID != nil {
		add("protocol_id", next.protocolID)
	}
	if req.Region != nil {
		add("region", next.region)
	}
	if req.BaseURL != nil {
		add("base_url", next.baseURL)
	}
	if req.UpstreamKey != nil {
		add("upstream_key_enc", []byte(envelope))
		add("upstream_key_mask", mask)
		add("key_rotation_at", now)
	}
	if req.GroupScopeMode != nil {
		add("group_scope_mode", next.groupScopeMode)
	}
	if req.DedicatedScopeMode != nil {
		add("dedicated_scope_mode", next.dedicatedScopeMode)
	}
	if req.FallbackAllowed != nil {
		add("fallback_allowed", next.fallbackAllowed)
	}
	if req.Weight != nil {
		add("weight", next.weight)
	}
	if req.Priority != nil {
		add("priority", next.priority)
	}
	if req.TimeoutConnectMS != nil {
		add("timeout_connect_ms", next.timeoutConnectMS)
	}
	if req.TimeoutFirstByteMS != nil {
		add("timeout_first_byte_ms", next.timeoutFirstByteMS)
	}
	if req.TimeoutTotalMS != nil {
		add("timeout_total_ms", next.timeoutTotalMS)
	}
	if req.RetryMax != nil {
		add("retry_max", next.retryMax)
	}
	if req.StreamSupported != nil {
		add("stream_supported", next.streamSupported)
	}
	if req.StreamUsageMode != nil {
		add("stream_usage_mode", next.streamUsageMode)
	}
	if req.UpstreamModelNameMap != nil {
		add("upstream_model_name_map", next.modelNameMap)
	}
	if req.Enabled != nil {
		add("enabled", next.enabled)
	}
	if len(sets) > 0 {
		add("updated_at", now)
		add("updated_by", mustActorID(r))
		if _, err := tx.ExecContext(ctx,
			`UPDATE t_channel SET `+strings.Join(sets, ", ")+` WHERE id = ? AND deleted_at IS NULL`,
			append(args, id)...); err != nil {
			s.logError(ctx, "ops_channel_update_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}
	// Replacing the list is also how switching to ALL clears it: the rows are
	// inert under ALL but leaving them would make the next SELECTED switch
	// silently resurrect an old allowlist.
	if req.DedicatedUserIDs != nil || next.dedicatedScopeMode != before.DedicatedScopeMode {
		if apiErr := replaceDedicatedUsers(ctx, tx, id, next.dedicatedUserIDs, mustActorID(r), now); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}

	after, apiErr := s.loadChannelTx(ctx, tx, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if err := s.insertAudit(ctx, tx, r, "CHANNEL", "UPDATE", "t_channel", id,
		channelAuditView(before), channelAuditView(after), ""); err != nil {
		s.logError(ctx, "ops_channel_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_channel_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	if routingChanged {
		if apiErr := s.publish(ctx); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}
	views := []channelView{after}
	if err := s.attachChannelRates(ctx, s.DB, views); err != nil {
		s.logError(ctx, "ops_channel_rate_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, channelDetailResponse{Channel: views[0]})
}

// DisableChannel implements POST /api/admin/channels/{id}/disable.
func (s *Service) DisableChannel(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ctx := r.Context()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_channel_disable_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	before, apiErr := s.loadChannelTxForUpdate(ctx, tx, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	changed := before.Enabled
	if changed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE t_channel SET enabled = 0, updated_at = ?, updated_by = ?
			  WHERE id = ? AND deleted_at IS NULL`,
			s.now(), mustActorID(r), id); err != nil {
			s.logError(ctx, "ops_channel_disable_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		after, apiErr := s.loadChannelTx(ctx, tx, id)
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		if err := s.insertAudit(ctx, tx, r, "CHANNEL", "DISABLE", "t_channel", id,
			channelAuditView(before), channelAuditView(after), ""); err != nil {
			s.logError(ctx, "ops_channel_audit_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_channel_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	if changed {
		if apiErr := s.publish(ctx); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}
	view, apiErr := s.loadChannel(ctx, s.DB, id, false)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	views := []channelView{view}
	if err := s.attachChannelRates(ctx, s.DB, views); err != nil {
		s.logError(ctx, "ops_channel_rate_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, channelDetailResponse{Channel: views[0]})
}

// DeleteChannel implements DELETE /api/admin/channels/{id}. A channel still
// referenced by t_route answers 409 channel_in_use; the code's hint already
// names the two ways out.
func (s *Service) DeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ctx := r.Context()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_channel_delete_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	before, apiErr := s.loadChannelTxForUpdate(ctx, tx, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var refs int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_route WHERE channel_id = ?`, id).Scan(&refs); err != nil {
		s.logError(ctx, "ops_channel_refs_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if refs > 0 {
		mw.WriteError(w, r, apierr.New(apierr.CodeChannelInUse))
		return
	}

	now := s.now()
	if _, err := tx.ExecContext(ctx, `DELETE FROM t_channel_dedicated_user WHERE channel_id = ?`, id); err != nil {
		s.logError(ctx, "ops_channel_dedicated_delete_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE t_channel SET deleted_at = ?, updated_at = ?, updated_by = ?
		  WHERE id = ? AND deleted_at IS NULL`,
		now, now, mustActorID(r), id); err != nil {
		s.logError(ctx, "ops_channel_soft_delete_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := s.insertAudit(ctx, tx, r, "CHANNEL", "DELETE", "t_channel", id,
		channelAuditView(before), map[string]any{"deleted_at": now}, ""); err != nil {
		s.logError(ctx, "ops_channel_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_channel_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	if before.Enabled {
		if apiErr := s.publish(ctx); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}
	_ = mw.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

type channelInput struct {
	name               string
	protocolID         int64
	region             string
	baseURL            string
	groupScopeMode     string
	dedicatedScopeMode string
	dedicatedUserIDs   []int64
	fallbackAllowed    bool
	weight             int64
	priority           int64
	timeoutConnectMS   int64
	timeoutFirstByteMS int64
	timeoutTotalMS     int64
	retryMax           int64
	streamSupported    bool
	streamUsageMode    string
	modelNameMap       []byte // JSON bytes; nil means SQL NULL
	enabled            bool
}

func (s *Service) validateCreateChannel(r *http.Request, req *channelCreateRequest) (channelInput, string, string, *apierr.APIError) {
	var in channelInput
	if req.Name == nil {
		return in, "", "", mw.InvalidField("name")
	}
	name := strings.TrimSpace(*req.Name)
	if name == "" || len([]rune(name)) > 64 {
		return in, "", "", mw.InvalidField("name")
	}
	if req.ProtocolID == nil || *req.ProtocolID <= 0 {
		return in, "", "", mw.InvalidField("protocol_id")
	}
	region, apiErr := validateRegion(req.Region, "region", "渠道")
	if apiErr != nil {
		return in, "", "", apiErr
	}
	if req.BaseURL == nil || !validBaseURL(strings.TrimSpace(*req.BaseURL)) {
		return in, "", "", mw.InvalidField("base_url")
	}
	if req.UpstreamKey == nil || strings.TrimSpace(*req.UpstreamKey) == "" {
		return in, "", "", mw.InvalidField("upstream_key")
	}
	key := strings.TrimSpace(*req.UpstreamKey)
	if len(key) > 512 {
		return in, "", "", mw.InvalidField("upstream_key")
	}
	if s.Box == nil {
		return in, "", "", apierr.New(apierr.CodeConfigUnavailable)
	}
	envelope, err := s.Box.SealString(key)
	if err != nil {
		s.logError(r.Context(), "ops_channel_seal_failed: "+err.Error())
		return in, "", "", apierr.New(apierr.CodeConfigUnavailable)
	}
	mask := maskKey(key)

	in.name = name
	in.protocolID = *req.ProtocolID
	in.region = region
	in.baseURL = strings.TrimSpace(*req.BaseURL)

	var err2 *apierr.APIError
	if in.groupScopeMode, err2 = scopeMode(req.GroupScopeMode, scopeAll, "group_scope_mode"); err2 != nil {
		return in, "", "", err2
	}
	if in.dedicatedScopeMode, err2 = scopeMode(req.DedicatedScopeMode, scopeAll, "dedicated_scope_mode"); err2 != nil {
		return in, "", "", err2
	}
	if req.DedicatedUserIDs != nil {
		in.dedicatedUserIDs, err2 = normalizeUserIDs(*req.DedicatedUserIDs)
		if err2 != nil {
			return in, "", "", err2
		}
	}
	if in.dedicatedScopeMode != scopeSelected && len(in.dedicatedUserIDs) > 0 {
		return in, "", "", mw.InvalidField("dedicated_user_ids")
	}
	if req.FallbackAllowed != nil {
		in.fallbackAllowed = *req.FallbackAllowed
	}
	in.weight = 100
	if req.Weight != nil {
		if *req.Weight < 0 || *req.Weight > 1_000_000 {
			return in, "", "", mw.InvalidField("weight")
		}
		in.weight = *req.Weight
	}
	in.priority = 100
	if req.Priority != nil {
		if *req.Priority < -32768 || *req.Priority > 32767 {
			return in, "", "", mw.InvalidField("priority")
		}
		in.priority = *req.Priority
	}
	in.timeoutConnectMS = 3000
	if req.TimeoutConnectMS != nil {
		if *req.TimeoutConnectMS <= 0 || *req.TimeoutConnectMS > 600000 {
			return in, "", "", mw.InvalidField("timeout_connect_ms")
		}
		in.timeoutConnectMS = *req.TimeoutConnectMS
	}
	in.timeoutFirstByteMS = 60000
	if req.TimeoutFirstByteMS != nil {
		if *req.TimeoutFirstByteMS <= 0 || *req.TimeoutFirstByteMS > 600000 {
			return in, "", "", mw.InvalidField("timeout_first_byte_ms")
		}
		in.timeoutFirstByteMS = *req.TimeoutFirstByteMS
	}
	in.timeoutTotalMS = 120000
	if req.TimeoutTotalMS != nil {
		if *req.TimeoutTotalMS <= 0 || *req.TimeoutTotalMS > 3600000 {
			return in, "", "", mw.InvalidField("timeout_total_ms")
		}
		in.timeoutTotalMS = *req.TimeoutTotalMS
	}
	in.retryMax = 2
	if req.RetryMax != nil {
		if *req.RetryMax < 0 || *req.RetryMax > 2 {
			return in, "", "", mw.InvalidField("retry_max")
		}
		in.retryMax = *req.RetryMax
	}
	in.streamSupported = true
	if req.StreamSupported != nil {
		in.streamSupported = *req.StreamSupported
	}
	in.streamUsageMode = "UNKNOWN"
	if req.StreamUsageMode != nil {
		mode, e := streamUsageMode(*req.StreamUsageMode)
		if e != nil {
			return in, "", "", e
		}
		in.streamUsageMode = mode
	}
	if req.UpstreamModelNameMap != nil {
		if apiErr := s.validateModelNameMap(r.Context(), s.DB, *req.UpstreamModelNameMap); apiErr != nil {
			return in, "", "", apiErr
		}
		// database/sql cannot bind a map to a JSON column; bind the encoded
		// bytes (or NULL when empty).
		if len(*req.UpstreamModelNameMap) > 0 {
			raw, err := jsonx.Marshal(*req.UpstreamModelNameMap)
			if err != nil {
				return in, "", "", mw.InvalidField("upstream_model_name_map")
			}
			in.modelNameMap = raw
		}
	}
	in.enabled = true
	if req.Enabled != nil {
		in.enabled = *req.Enabled
	}
	return in, envelope, mask, nil
}

// validatePatchChannel merges the patch onto the current row and validates the
// result. routingChanged reports whether the routing snapshot must be
// republished after commit.
func (s *Service) validatePatchChannel(r *http.Request, tx *sql.Tx, req *channelPatchRequest, cur channelView) (channelInput, string, string, bool, *apierr.APIError) {
	in := channelInput{
		name:               cur.Name,
		protocolID:         cur.ProtocolID,
		region:             cur.Region,
		baseURL:            cur.BaseURL,
		groupScopeMode:     cur.GroupScopeMode,
		dedicatedScopeMode: cur.DedicatedScopeMode,
		fallbackAllowed:    cur.FallbackAllowed,
		weight:             cur.Weight,
		priority:           cur.Priority,
		timeoutConnectMS:   cur.TimeoutConnectMS,
		timeoutFirstByteMS: cur.TimeoutFirstByteMS,
		timeoutTotalMS:     cur.TimeoutTotalMS,
		retryMax:           cur.RetryMax,
		streamSupported:    cur.StreamSupported,
		streamUsageMode:    cur.StreamUsageMode,
		enabled:            cur.Enabled,
	}
	if cur.DedicatedUserIDs != nil {
		in.dedicatedUserIDs = append([]int64(nil), (*cur.DedicatedUserIDs)...)
	}
	if len(cur.UpstreamModelNames) > 0 {
		raw, err := jsonx.Marshal(cur.UpstreamModelNames)
		if err != nil {
			return in, "", "", false, apierr.New(apierr.CodeInternalError).WithCause(err)
		}
		in.modelNameMap = raw
	}

	routingChanged := false
	var envelope, mask string
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len([]rune(name)) > 64 {
			return in, "", "", false, mw.InvalidField("name")
		}
		if apiErr := channelNameTaken(r.Context(), tx, name, cur.ID); apiErr != nil {
			return in, "", "", false, apiErr
		}
		in.name = name
	}
	if req.ProtocolID != nil {
		if *req.ProtocolID <= 0 || !protocolExists(r.Context(), tx, *req.ProtocolID) {
			return in, "", "", false, mw.InvalidField("protocol_id")
		}
		in.protocolID = *req.ProtocolID
	}
	if req.Region != nil {
		region, apiErr := validateRegion(req.Region, "region", "渠道")
		if apiErr != nil {
			return in, "", "", false, apiErr
		}
		in.region = region
	}
	if req.BaseURL != nil {
		u := strings.TrimSpace(*req.BaseURL)
		if !validBaseURL(u) {
			return in, "", "", false, mw.InvalidField("base_url")
		}
		in.baseURL = u
	}
	if req.UpstreamKey != nil {
		key := strings.TrimSpace(*req.UpstreamKey)
		if key == "" || len(key) > 512 {
			return in, "", "", false, mw.InvalidField("upstream_key")
		}
		if s.Box == nil {
			return in, "", "", false, apierr.New(apierr.CodeConfigUnavailable)
		}
		sealed, err := s.Box.SealString(key)
		if err != nil {
			s.logError(r.Context(), "ops_channel_seal_failed: "+err.Error())
			return in, "", "", false, apierr.New(apierr.CodeConfigUnavailable)
		}
		envelope = sealed
		mask = maskKey(key)
	}
	if req.GroupScopeMode != nil {
		v, apiErr := scopeMode(req.GroupScopeMode, cur.GroupScopeMode, "group_scope_mode")
		if apiErr != nil {
			return in, "", "", false, apiErr
		}
		routingChanged = routingChanged || v != cur.GroupScopeMode
		in.groupScopeMode = v
	}
	if req.DedicatedScopeMode != nil {
		v, apiErr := scopeMode(req.DedicatedScopeMode, cur.DedicatedScopeMode, "dedicated_scope_mode")
		if apiErr != nil {
			return in, "", "", false, apiErr
		}
		routingChanged = routingChanged || v != cur.DedicatedScopeMode
		in.dedicatedScopeMode = v
	}
	if req.DedicatedUserIDs != nil {
		ids, apiErr := normalizeUserIDs(*req.DedicatedUserIDs)
		if apiErr != nil {
			return in, "", "", false, apiErr
		}
		if in.dedicatedScopeMode != scopeSelected && len(ids) > 0 {
			return in, "", "", false, mw.InvalidField("dedicated_user_ids")
		}
		in.dedicatedUserIDs = ids
	} else if in.dedicatedScopeMode != scopeSelected {
		// Switching to ALL drops the old allowlist instead of keeping a list
		// that would silently come back on the next SELECTED switch.
		in.dedicatedUserIDs = nil
	}
	if req.FallbackAllowed != nil {
		routingChanged = routingChanged || *req.FallbackAllowed != cur.FallbackAllowed
		in.fallbackAllowed = *req.FallbackAllowed
	}
	if req.Weight != nil {
		if *req.Weight < 0 || *req.Weight > 1_000_000 {
			return in, "", "", false, mw.InvalidField("weight")
		}
		routingChanged = routingChanged || *req.Weight != cur.Weight
		in.weight = *req.Weight
	}
	if req.Priority != nil {
		if *req.Priority < -32768 || *req.Priority > 32767 {
			return in, "", "", false, mw.InvalidField("priority")
		}
		routingChanged = routingChanged || *req.Priority != cur.Priority
		in.priority = *req.Priority
	}
	if req.TimeoutConnectMS != nil {
		if *req.TimeoutConnectMS <= 0 || *req.TimeoutConnectMS > 600000 {
			return in, "", "", false, mw.InvalidField("timeout_connect_ms")
		}
		in.timeoutConnectMS = *req.TimeoutConnectMS
	}
	if req.TimeoutFirstByteMS != nil {
		if *req.TimeoutFirstByteMS <= 0 || *req.TimeoutFirstByteMS > 600000 {
			return in, "", "", false, mw.InvalidField("timeout_first_byte_ms")
		}
		in.timeoutFirstByteMS = *req.TimeoutFirstByteMS
	}
	if req.TimeoutTotalMS != nil {
		if *req.TimeoutTotalMS <= 0 || *req.TimeoutTotalMS > 3600000 {
			return in, "", "", false, mw.InvalidField("timeout_total_ms")
		}
		in.timeoutTotalMS = *req.TimeoutTotalMS
	}
	if req.RetryMax != nil {
		if *req.RetryMax < 0 || *req.RetryMax > 2 {
			return in, "", "", false, mw.InvalidField("retry_max")
		}
		in.retryMax = *req.RetryMax
	}
	if req.StreamSupported != nil {
		in.streamSupported = *req.StreamSupported
	}
	if req.StreamUsageMode != nil {
		mode, apiErr := streamUsageMode(*req.StreamUsageMode)
		if apiErr != nil {
			return in, "", "", false, apiErr
		}
		in.streamUsageMode = mode
	}
	if req.UpstreamModelNameMap != nil {
		if apiErr := s.validateModelNameMap(r.Context(), tx, *req.UpstreamModelNameMap); apiErr != nil {
			return in, "", "", false, apiErr
		}
		if len(*req.UpstreamModelNameMap) > 0 {
			raw, err := jsonx.Marshal(*req.UpstreamModelNameMap)
			if err != nil {
				return in, "", "", false, mw.InvalidField("upstream_model_name_map")
			}
			in.modelNameMap = raw
		} else {
			in.modelNameMap = nil
		}
	}
	if req.Enabled != nil {
		routingChanged = routingChanged || *req.Enabled != cur.Enabled
		in.enabled = *req.Enabled
	}
	return in, envelope, mask, routingChanged, nil
}

func validateRegion(raw *string, field, noun string) (string, *apierr.APIError) {
	if raw == nil {
		return "", mw.InvalidField(field)
	}
	region := strings.ToUpper(strings.TrimSpace(*raw))
	switch region {
	case regionDomestic:
		return regionDomestic, nil
	case regionOverseas:
		// Hard constraint 1: V1.0 is domestic-only. The message states the
		// reason, not just the field, because "region" alone would look like a
		// typo rather than a product limit.
		return "", apierr.Newf(apierr.CodeInvalidRequestBody,
			"V1.0 仅支持境内"+noun+"，region 不能为 OVERSEAS")
	default:
		return "", mw.InvalidField(field)
	}
}

func scopeMode(raw *string, def, field string) (string, *apierr.APIError) {
	if raw == nil {
		return def, nil
	}
	v := strings.ToUpper(strings.TrimSpace(*raw))
	if v != scopeAll && v != scopeSelected {
		return "", mw.InvalidField(field)
	}
	return v, nil
}

func streamUsageMode(raw string) (string, *apierr.APIError) {
	v := strings.ToUpper(strings.TrimSpace(raw))
	switch v {
	case "NATIVE", "STREAM_OPTIONS", "ESTIMATE", "UNKNOWN":
		return v, nil
	default:
		return "", mw.InvalidField("stream_usage_mode")
	}
}

func normalizeUserIDs(ids []int64) ([]int64, *apierr.APIError) {
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, mw.InvalidField("dedicated_user_ids")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > 1000 {
		return nil, mw.InvalidField("dedicated_user_ids")
	}
	return out, nil
}

// modelNameQueryer is satisfied by both *sql.DB and *sql.Tx, so the same
// validation runs inside the patch transaction and before the create one.
type modelNameQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// validateModelNameMap checks shape and, more importantly, that every key is a
// platform model that actually exists.
//
// Why existence matters: the map is keyed by the platform model name and
// consulted at send time (dispatch.buildBody). A typo like
// "qwen-maxx=qwen-max" is a perfectly well-formed entry that never matches, so
// the channel keeps sending the route default and nothing anywhere reports a
// problem. Rejecting the save turns that silent no-op into a 400 that names the
// key.
func (s *Service) validateModelNameMap(ctx context.Context, q modelNameQueryer, m map[string]string) *apierr.APIError {
	if len(m) > 200 {
		return mw.InvalidField("upstream_model_name_map")
	}
	for k, v := range m {
		if strings.TrimSpace(k) == "" || len([]rune(k)) > 128 || len([]rune(v)) > 128 {
			return mw.InvalidField("upstream_model_name_map")
		}
	}
	if len(m) == 0 {
		return nil
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, strings.TrimSpace(k))
	}
	sort.Strings(keys)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	rows, err := q.QueryContext(ctx, `
		SELECT model_name FROM t_model
		WHERE deleted_at IS NULL AND model_name IN (`+placeholders+`)`, args...)
	if err != nil {
		s.logError(ctx, "ops_channel_model_map_check_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	defer rows.Close()
	known := make(map[string]struct{}, len(keys))
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr == nil {
			known[name] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		s.logError(ctx, "ops_channel_model_map_iterate_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}

	var unknown []string
	for _, k := range keys {
		if _, ok := known[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return apierr.Newf(apierr.CodeInvalidRequestBody,
			"模型名映射的左侧必须是已登记的平台模型名，这些不存在："+strings.Join(unknown, "、")+
				"（别名请改用其规范模型名；新模型先到「模型管理」登记）")
	}
	return nil
}

// validBaseURL enforces https, with plain http allowed only for a loopback or
// private address (TDS t_channel: "https 强制；http 仅私网地址放行").
func validBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		return privateHost(u.Hostname())
	default:
		return false
	}
}

func privateHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// maskKey renders the display form. The last four bytes are the only part of
// the key that may leave the process.
func maskKey(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	tail := key[len(key)-4:]
	if strings.HasPrefix(key, "sk-") {
		return "sk-****" + tail
	}
	return "****" + tail
}

func hasChannelPatch(req *channelPatchRequest) bool {
	return req.Name != nil || req.ProtocolID != nil || req.Region != nil ||
		req.BaseURL != nil || req.UpstreamKey != nil || req.GroupScopeMode != nil ||
		req.DedicatedScopeMode != nil || req.DedicatedUserIDs != nil ||
		req.FallbackAllowed != nil || req.Weight != nil || req.Priority != nil ||
		req.TimeoutConnectMS != nil || req.TimeoutFirstByteMS != nil ||
		req.TimeoutTotalMS != nil || req.RetryMax != nil ||
		req.StreamSupported != nil || req.StreamUsageMode != nil ||
		req.UpstreamModelNameMap != nil || req.Enabled != nil
}

// channelAuditView is the JSON written to before/after. The mask replaces the
// key; the envelope never appears.
func channelAuditView(v channelView) map[string]any {
	out := map[string]any{
		"name":                    v.Name,
		"protocol_id":             v.ProtocolID,
		"region":                  v.Region,
		"base_url":                v.BaseURL,
		"upstream_key_mask":       v.UpstreamKeyMask,
		"group_scope_mode":        v.GroupScopeMode,
		"dedicated_scope_mode":    v.DedicatedScopeMode,
		"fallback_allowed":        v.FallbackAllowed,
		"weight":                  v.Weight,
		"priority":                v.Priority,
		"timeout_connect_ms":      v.TimeoutConnectMS,
		"timeout_first_byte_ms":   v.TimeoutFirstByteMS,
		"timeout_total_ms":        v.TimeoutTotalMS,
		"retry_max":               v.RetryMax,
		"stream_supported":        v.StreamSupported,
		"stream_usage_mode":       v.StreamUsageMode,
		"upstream_model_name_map": v.UpstreamModelNames,
		"enabled":                 v.Enabled,
	}
	if v.DedicatedUserIDs != nil {
		out["dedicated_user_ids"] = *v.DedicatedUserIDs
	}
	return out
}

// ---------------------------------------------------------------------------
// storage helpers
// ---------------------------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanChannel(row rowScanner) (channelView, error) {
	var (
		v           channelView
		keyRotation sql.NullTime
		nameMap     []byte
		createdAt   time.Time
		updatedAt   time.Time
	)
	if err := row.Scan(
		&v.ID, &v.Name, &v.ProtocolID, &v.Protocol, &v.Region, &v.BaseURL,
		&v.UpstreamKeyMask, &keyRotation, &v.GroupScopeMode,
		&v.DedicatedScopeMode, &v.FallbackAllowed, &v.Weight, &v.Priority,
		&v.TimeoutConnectMS, &v.TimeoutFirstByteMS, &v.TimeoutTotalMS,
		&v.RetryMax, &v.StreamSupported, &v.StreamUsageMode,
		&nameMap, &v.HealthState, &v.Enabled, &createdAt, &updatedAt,
	); err != nil {
		return channelView{}, err
	}
	v.KeyRotationAt = nullableTime(keyRotation)
	v.CreatedAt = createdAt.UTC()
	v.UpdatedAt = updatedAt.UTC()
	if len(nameMap) > 0 {
		var m map[string]string
		if err := jsonx.Unmarshal(nameMap, &m); err == nil {
			v.UpstreamModelNames = m
		}
	}
	return v, nil
}

func (s *Service) loadChannel(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id int64, includeDeleted bool) (channelView, *apierr.APIError) {
	where := "c.id = ? AND c.deleted_at IS NULL"
	if includeDeleted {
		where = "c.id = ?"
	}
	row := q.QueryRowContext(ctx, `
		SELECT `+channelSelectColumns+`
		  FROM t_channel c JOIN t_protocol p ON p.id = c.protocol_id
		 WHERE `+where, id)
	view, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return channelView{}, apierr.New(apierr.CodeChannelNotFound)
	}
	if err != nil {
		s.logError(ctx, "ops_channel_load_failed: "+err.Error())
		return channelView{}, apierr.New(apierr.CodeInternalError)
	}
	ids, err := dedicatedUserIDs(ctx, s.DB, id)
	if err != nil {
		s.logError(ctx, "ops_channel_dedicated_load_failed: "+err.Error())
		return channelView{}, apierr.New(apierr.CodeInternalError)
	}
	view.DedicatedUserIDs = &ids
	return view, nil
}

func (s *Service) loadChannelTx(ctx context.Context, tx *sql.Tx, id int64) (channelView, *apierr.APIError) {
	row := tx.QueryRowContext(ctx, `
		SELECT `+channelSelectColumns+`
		  FROM t_channel c JOIN t_protocol p ON p.id = c.protocol_id
		 WHERE c.id = ? AND c.deleted_at IS NULL`, id)
	view, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return channelView{}, apierr.New(apierr.CodeChannelNotFound)
	}
	if err != nil {
		s.logError(ctx, "ops_channel_load_failed: "+err.Error())
		return channelView{}, apierr.New(apierr.CodeInternalError)
	}
	ids, err := dedicatedUserIDs(ctx, tx, id)
	if err != nil {
		s.logError(ctx, "ops_channel_dedicated_load_failed: "+err.Error())
		return channelView{}, apierr.New(apierr.CodeInternalError)
	}
	view.DedicatedUserIDs = &ids
	return view, nil
}

func (s *Service) loadChannelTxForUpdate(ctx context.Context, tx *sql.Tx, id int64) (channelView, *apierr.APIError) {
	row := tx.QueryRowContext(ctx, `
		SELECT `+channelSelectColumns+`
		  FROM t_channel c JOIN t_protocol p ON p.id = c.protocol_id
		 WHERE c.id = ? AND c.deleted_at IS NULL FOR UPDATE`, id)
	view, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return channelView{}, apierr.New(apierr.CodeChannelNotFound)
	}
	if err != nil {
		s.logError(ctx, "ops_channel_load_failed: "+err.Error())
		return channelView{}, apierr.New(apierr.CodeInternalError)
	}
	ids, err := dedicatedUserIDs(ctx, tx, id)
	if err != nil {
		s.logError(ctx, "ops_channel_dedicated_load_failed: "+err.Error())
		return channelView{}, apierr.New(apierr.CodeInternalError)
	}
	view.DedicatedUserIDs = &ids
	return view, nil
}

func dedicatedUserIDs(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, channelID int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT user_id FROM t_channel_dedicated_user WHERE channel_id = ? ORDER BY user_id`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]int64, 0, 8)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func replaceDedicatedUsers(ctx context.Context, tx *sql.Tx, channelID int64, ids []int64, operatorID int64, now time.Time) *apierr.APIError {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM t_channel_dedicated_user WHERE channel_id = ?`, channelID); err != nil {
		return apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	if len(ids) == 0 {
		return nil
	}
	// One multi-row INSERT keeps the write atomic and bounded; ids were
	// deduplicated by normalizeUserIDs.
	values := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids)*4)
	for _, uid := range ids {
		values = append(values, "(?,?,?,?)")
		args = append(args, channelID, uid, operatorID, now)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO t_channel_dedicated_user (channel_id, user_id, created_by, created_at) VALUES `+
			strings.Join(values, ","), args...)
	if err != nil {
		return apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	return nil
}

func channelNameTaken(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, name string, exceptID int64) *apierr.APIError {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT id FROM t_channel WHERE name = ? LIMIT 1`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	if id == exceptID {
		return nil
	}
	return apierr.Newf(apierr.CodeInvalidRequestBody,
		fmt.Sprintf("渠道名 %s 已被占用（id=%d）", name, id))
}

// protocolExists is checked before insert so a bad id is a body error rather
// than a raw foreign-key-style database error.
func protocolExists(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id int64) bool {
	var one int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM t_protocol WHERE id = ? AND enabled = 1`, id).Scan(&one)
	return err == nil
}

func channelOrder(page mw.Page) string {
	col := "c.created_at"
	switch page.Sort {
	case "updated_at":
		col = "c.updated_at"
	case "name":
		col = "c.name"
	case "weight":
		col = "c.weight"
	case "priority":
		col = "c.priority"
	}
	return col + " " + orderDirection(page.Order) + ", c.id " + orderDirection(page.Order)
}

func orderDirection(order string) string {
	if order == "asc" {
		return "asc"
	}
	return "desc"
}

// likePattern escapes LIKE metacharacters with '!' so the query stays valid
// under NO_BACKSLASH_ESCAPES (the same convention as the console lists).
func likePattern(q string) string {
	replacer := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return "%" + replacer.Replace(q) + "%"
}

func mustActorID(r *http.Request) int64 {
	uid, _, _ := actor(r)
	return uid
}

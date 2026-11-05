package key

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
)

type updateRequest struct {
	KeyName        *string          `json:"key_name"`
	ModelIDs       *[]int64         `json:"model_ids"`
	ModelScopeMode *string          `json:"model_scope_mode"`
	ExpireAt       jsonx.RawMessage `json:"expire_at"`
	IPAllowlist    *[]string        `json:"ip_allowlist"`
	Status         *string          `json:"status"`
	Confirm        *bool            `json:"confirm"`
	GroupCode      jsonx.RawMessage `json:"group_code"`
}

type updateResponse struct {
	Changed         bool       `json:"changed"`
	KeyName         string     `json:"key_name"`
	Status          string     `json:"status"`
	ModelScopeMode  string     `json:"model_scope_mode"`
	ExpireAt        *time.Time `json:"expire_at"`
	IPWhitelistMode string     `json:"ip_whitelist_mode"`
	IPAllowlist     []string   `json:"ip_allowlist"`
}

// Update implements PATCH /api/console/keys/{id} (F6-4, F7-3, F7-4).
//
// Pointer fields are the contract: nil means "not supplied", an empty slice or
// empty string means "supplied as empty". That distinction is the only way an
// empty IP list can clear a whitelist without an omitted field clearing it by
// accident (T12 hard constraint 9).
func (s *Service) Update(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	id, apiErr := pathKeyID(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req updateRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if len(req.GroupCode) > 0 {
		mw.WriteError(w, r, mw.InvalidField("group_code"))
		return
	}
	if req.KeyName == nil && req.ModelIDs == nil && req.ModelScopeMode == nil &&
		len(req.ExpireAt) == 0 && req.IPAllowlist == nil && req.Status == nil {
		mw.WriteError(w, r, mw.InvalidField("body"))
		return
	}

	name := ""
	hasName := req.KeyName != nil
	if hasName {
		name = strings.TrimSpace(*req.KeyName)
		if name == "" || len([]rune(name)) > 64 {
			mw.WriteError(w, r, mw.InvalidField("key_name"))
			return
		}
	}
	if req.ModelIDs != nil && len(*req.ModelIDs) == 0 {
		// An empty array is not "clear the whitelist": a SELECTED key with no
		// models cannot call anything. Reject instead of creating a dead key.
		mw.WriteError(w, r, mw.InvalidField("model_ids"))
		return
	}
	var scopeMode string
	if req.ModelScopeMode != nil {
		scopeMode = strings.ToUpper(strings.TrimSpace(*req.ModelScopeMode))
		if scopeMode != "SELECTED" && scopeMode != "ALL" {
			mw.WriteError(w, r, mw.InvalidField("model_scope_mode"))
			return
		}
		if scopeMode == "ALL" && req.ModelIDs != nil {
			mw.WriteError(w, r, mw.InvalidField("model_ids"))
			return
		}
	}
	var newExpire sql.NullTime
	hasExpire := len(req.ExpireAt) > 0
	if hasExpire {
		expire, apiErr := parseOptionalExpireAt(req.ExpireAt, s.now())
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		// null 表示清空有效期（改为永不过期）；零值 sql.NullTime 写入即 NULL。
		if expire != nil {
			newExpire = sql.NullTime{Time: *expire, Valid: true}
		}
	}
	var newRules []ipRule
	hasIP := req.IPAllowlist != nil
	if hasIP {
		newRules, apiErr = normalizeIPRules(*req.IPAllowlist)
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}

	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "key_update_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	rec, apiErr := s.loadOwnedKey(ctx, tx, sess.UID, id, true)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	nextStatus, statusChanged, apiErr := statusTransition(rec.Status, req.Status, req.Confirm)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	if scopeMode == "" {
		scopeMode = rec.ModelScopeMode
	}
	if scopeMode == "ALL" && req.ModelIDs != nil {
		// Under ALL the model table has no effect. Accepting the write would
		// persist rows the client believes are active.
		mw.WriteError(w, r, mw.InvalidField("model_ids"))
		return
	}
	existingModels, err := s.keyModelIDs(ctx, tx, id)
	if err != nil {
		s.logError(ctx, "key_update_model_read_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if scopeMode == "SELECTED" && req.ModelIDs == nil && len(existingModels) == 0 {
		mw.WriteError(w, r, mw.InvalidField("model_ids"))
		return
	}
	var modelIDs []int64
	if req.ModelIDs != nil {
		modelIDs, apiErr = s.resolveModelScope(ctx, "SELECTED", *req.ModelIDs)
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	} else {
		modelIDs = existingModels
	}
	modelsChanged := scopeMode != rec.ModelScopeMode ||
		(req.ModelIDs != nil && !sameIDs(existingModels, modelIDs))

	existingRules, err := s.loadIPRules(ctx, tx, id)
	if err != nil {
		s.logError(ctx, "key_update_ip_read_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if !hasIP {
		newRules = existingRules
	}
	ipModeValue := ipMode(newRules)
	ipChanged := hasIP && !sameRules(existingRules, newRules)

	nameChanged := hasName && name != rec.Name
	if !hasName {
		name = rec.Name
	}
	expireChanged := hasExpire && (!rec.ExpireAt.Valid || !rec.ExpireAt.Time.Equal(newExpire.Time))
	expireValue := rec.ExpireAt
	if hasExpire {
		expireValue = newExpire
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE t_api_key
		   SET key_name = ?, model_scope_mode = ?, ip_whitelist_mode = ?,
		       status = ?, expire_at = ?, updated_at = ?
		 WHERE id = ? AND user_id = ? AND deleted_at IS NULL`,
		name, scopeMode, ipModeValue, nextStatus, nullableSQLTime(expireValue), now, id, sess.UID); err != nil {
		s.logError(ctx, "key_update_row_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if req.ModelIDs != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM t_api_key_model WHERE api_key_id = ?`, id); err != nil {
			s.logError(ctx, "key_update_model_delete_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		for _, modelID := range modelIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO t_api_key_model (api_key_id, model_id, created_at) VALUES (?,?,?)`,
				id, modelID, now); err != nil {
				s.logError(ctx, "key_update_model_insert_failed: "+err.Error())
				mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
				return
			}
		}
	}
	// The version currently visible to P06. It is the fallback IPM if the
	// snapshot cannot be republished after the commit.
	ipmBefore := s.maxIPRuleID(ctx, id)
	if ipChanged {
		if err := s.replaceIPRules(ctx, tx, id, newRules, now); err != nil {
			s.logError(ctx, "key_update_ip_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}

	ip := auditIP(r)
	auditErr := func(action string, before, after any) error {
		return insertKeyAudit(ctx, tx, id, action, before, after, sess.UID, ip, now)
	}
	if nameChanged {
		if err := auditErr("RENAME", map[string]any{"key_name": rec.Name}, map[string]any{"key_name": name}); err != nil {
			s.logError(ctx, "key_update_audit_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}
	if modelsChanged {
		if err := auditErr("MODEL_SCOPE_CHANGE",
			map[string]any{"model_scope_mode": rec.ModelScopeMode, "model_ids": existingModels},
			map[string]any{"model_scope_mode": scopeMode, "model_ids": modelIDs}); err != nil {
			s.logError(ctx, "key_update_audit_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}
	if ipChanged {
		if err := auditErr("IP_RULE_CHANGE",
			map[string]any{"ip_whitelist_mode": rec.IPWhitelistMode, "ip_allowlist": ruleCIDRs(existingRules)},
			map[string]any{"ip_whitelist_mode": ipModeValue, "ip_allowlist": ruleCIDRs(newRules)},
		); err != nil {
			s.logError(ctx, "key_update_audit_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}
	if expireChanged {
		if err := auditErr("EXPIRE_CHANGE",
			map[string]any{"expire_at": nullableTime(rec.ExpireAt)},
			map[string]any{"expire_at": nullableTime(newExpire)}); err != nil {
			s.logError(ctx, "key_update_audit_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}
	if statusChanged {
		action := "ENABLE"
		if nextStatus == "DISABLED" {
			action = "DISABLE"
		}
		if err := auditErr(action,
			map[string]any{"status": rec.Status},
			map[string]any{"status": nextStatus}); err != nil {
			s.logError(ctx, "key_update_audit_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}

	if err := tx.Commit(); err != nil {
		s.logError(ctx, "key_update_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	rec.Name = name
	rec.Status = nextStatus
	rec.ModelScopeMode = scopeMode
	rec.IPWhitelistMode = ipModeValue
	rec.ExpireAt = expireValue

	// P06 reads the whitelist from the routing snapshot, P07 reads the model
	// grant from the same one. Republish when either changes — gating on ipChanged
	// alone left a widened/narrowed model scope invisible for up to four minutes.
	// On failure keep the old ipm so the gateway stays consistent with the
	// snapshot it already has.
	if ipChanged || modelsChanged {
		if err := s.publishIPRules(ctx); err != nil {
			s.logError(ctx, "key_update_route_publish_failed: "+err.Error())
			if perr := s.pushK13WithIPM(ctx, rec, ipmBefore); perr != nil {
				s.logError(ctx, "key_update_k13_fallback_failed: "+perr.Error())
			}
			mw.WriteError(w, r, apierr.New(apierr.CodeConfigUnavailable).WithCause(err))
			return
		}
	}
	if err := s.pushK13(ctx, rec); err != nil {
		s.logError(ctx, "key_update_k13_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable).WithCause(err))
		return
	}

	_ = mw.WriteJSON(w, http.StatusOK, updateResponse{
		Changed:         nameChanged || modelsChanged || ipChanged || expireChanged || statusChanged,
		KeyName:         name,
		Status:          nextStatus,
		ModelScopeMode:  scopeMode,
		ExpireAt:        nullableTime(expireValue),
		IPWhitelistMode: ipModeValue,
		IPAllowlist:     ruleCIDRs(newRules),
	})
}

func (s *Service) keyModelIDs(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, keyID int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT model_id FROM t_api_key_model WHERE api_key_id = ? ORDER BY model_id`, keyID)
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

func sameRules(a, b []ipRule) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, rule := range a {
		seen[rule.CIDR] = true
	}
	for _, rule := range b {
		if !seen[rule.CIDR] {
			return false
		}
	}
	return true
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[int64]bool, len(a))
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			return false
		}
	}
	return true
}

func nullableSQLTime(t sql.NullTime) any {
	if !t.Valid {
		return nil
	}
	return t.Time.UTC()
}

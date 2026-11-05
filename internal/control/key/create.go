package key

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/apikey"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/redisx"
)

// Batch bounds from PRD §5.2 / F6-5 (1~10). The T12 card says 1~20; PRD is the
// source of truth and the receipt records the discrepancy.
const (
	minBatch = 1
	maxBatch = 10
)

type createRequest struct {
	Name           string           `json:"name"`
	ModelIDs       []int64          `json:"model_ids"`
	ModelScopeMode *string          `json:"model_scope_mode"`
	ExpireAt       jsonx.RawMessage `json:"expire_at"`
	IPAllowlist    []string         `json:"ip_allowlist"`
	Count          *int             `json:"count"`
	// GroupCode is declared only so its presence can be rejected. A field that
	// is absent in the struct cannot be distinguished from one the client sent
	// and the server silently dropped.
	GroupCode jsonx.RawMessage `json:"group_code"`
}

type createdKey struct {
	ID             int64      `json:"id"`
	KeyName        string     `json:"key_name"`
	Key            string     `json:"key"` // plaintext: the only occurrence
	KeyPrefix      string     `json:"key_prefix"`
	KeyLast4       string     `json:"key_last4"`
	KeyMasked      string     `json:"key_masked"`
	ModelScopeMode string     `json:"model_scope_mode"`
	ModelIDs       []int64    `json:"model_ids"`
	GroupCode      string     `json:"group_code"`
	ExpireAt       *time.Time `json:"expire_at"`
	IPAllowlist    []string   `json:"ip_allowlist"`
}

type createResponse struct {
	Items   []createdKey `json:"items"`
	Count   int          `json:"count"`
	Warning string       `json:"warning"`
}

// Create implements POST /api/console/keys (F6-2, F6-3, F6-5, F6-7).
func (s *Service) Create(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	var req createRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if len(req.GroupCode) > 0 {
		// F7-4 / T12 hard constraint 3: presence, not value, is the offence.
		mw.WriteError(w, r, mw.InvalidField("group_code"))
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 64 {
		mw.WriteError(w, r, mw.InvalidField("name"))
		return
	}
	count := 1
	if req.Count != nil {
		count = *req.Count
	}
	if count < minBatch || count > maxBatch {
		mw.WriteError(w, r, mw.InvalidField("count"))
		return
	}

	scopeMode := "SELECTED"
	if req.ModelScopeMode != nil {
		scopeMode = strings.ToUpper(strings.TrimSpace(*req.ModelScopeMode))
	}
	if scopeMode != "SELECTED" && scopeMode != "ALL" {
		mw.WriteError(w, r, mw.InvalidField("model_scope_mode"))
		return
	}
	modelIDs, apiErr := s.resolveModelScope(r.Context(), scopeMode, req.ModelIDs)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	expireAt, apiErr := parseOptionalExpireAt(req.ExpireAt, s.now())
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	rules, apiErr := normalizeIPRules(req.IPAllowlist)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	items, apiErr := s.createBatch(r, sess.UID, name, count, scopeMode, modelIDs, expireAt, rules)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	_ = mw.WriteJSON(w, http.StatusCreated, createResponse{
		Items:   items,
		Count:   len(items),
		Warning: "完整密钥仅本次返回，关闭后无法再次查看",
	})
}

// resolveModelScope validates the model set. ALL is the only mode that may
// carry an empty list; SELECTED must name at least one existing model (B-35).
func (s *Service) resolveModelScope(ctx context.Context, mode string, raw []int64) ([]int64, *apierr.APIError) {
	if mode == "ALL" {
		if len(raw) > 0 {
			return nil, mw.InvalidField("model_ids")
		}
		return []int64{}, nil
	}
	if len(raw) == 0 || len(raw) > 100 {
		return nil, mw.InvalidField("model_ids")
	}
	seen := make(map[int64]bool, len(raw))
	out := make([]int64, 0, len(raw))
	for _, id := range raw {
		if id <= 0 {
			return nil, mw.InvalidField("model_ids")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	args := make([]any, len(out))
	for i, id := range out {
		args[i] = id
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id FROM t_model WHERE deleted_at IS NULL AND id IN (`+placeholders(len(out))+`)`, args...)
	if err != nil {
		s.logError(ctx, "key_model_scope_lookup_failed: "+err.Error())
		return nil, apierr.New(apierr.CodeInternalError)
	}
	defer rows.Close()
	found := 0
	for rows.Next() {
		found++
	}
	if err := rows.Err(); err != nil || found != len(out) {
		return nil, mw.InvalidField("model_ids")
	}
	return out, nil
}

// Whether a key expires is carried to the gateway as K13's EXP field, where 0
// means no expiry (internal/redisx/authcache.go), so nil maps cleanly onto the
// existing auth path.
func parseOptionalExpireAt(raw jsonx.RawMessage, now time.Time) (*time.Time, *apierr.APIError) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var text string
	if err := jsonx.Unmarshal(raw, &text); err != nil {
		return nil, mw.InvalidField("expire_at")
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(text))
	if err != nil || !t.After(now) {
		return nil, mw.InvalidField("expire_at")
	}
	utc := t.UTC()
	return &utc, nil
}

type pendingKey struct {
	id      int64
	hash16  string
	maxIPID int64
	item    createdKey
}

// createBatch writes every row in one transaction and publishes K13 before
// commit. Publishing first means a Redis failure rolls the whole batch back,
// so a caller who never received the plaintext cannot burn free-tier slots.
// If the commit then fails, the already-written K13 entries are deleted.
func (s *Service) createBatch(r *http.Request, uid int64, name string, count int, scopeMode string, modelIDs []int64, expireAt *time.Time, rules []ipRule) ([]createdKey, *apierr.APIError) {
	ctx := r.Context()
	now := s.now()
	if s.KeyAuth == nil {
		s.logError(ctx, "key_create_k13_writer_missing")
		return nil, apierr.New(apierr.CodeAuthUnavailable)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "key_create_begin_failed: "+err.Error())
		return nil, apierr.New(apierr.CodeInternalError)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// Serialise creation per account so the free-tier count cannot be read by
	// two concurrent requests and exceeded by both.
	var lockedID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM t_user WHERE id = ? AND deleted_at IS NULL FOR UPDATE`, uid).Scan(&lockedID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, mw.SessionMissing()
		}
		s.logError(ctx, "key_create_user_lock_failed: "+err.Error())
		return nil, apierr.New(apierr.CodeInternalError)
	}
	if apiErr := s.checkFreeLimit(ctx, tx, uid, int64(count), now); apiErr != nil {
		return nil, apiErr
	}
	ratio, err := groupRatio(ctx, tx, "standard")
	if err != nil {
		s.logError(ctx, "key_create_group_lookup_failed: "+err.Error())
		return nil, apierr.New(apierr.CodeInternalError)
	}

	pending := make([]pendingKey, 0, count)
	for i := 0; i < count; i++ {
		plain, hash, err := apikey.Generate()
		if err != nil {
			return nil, apierr.New(apierr.CodeInternalError).WithCause(err)
		}
		hash16, err := apikey.HashPrefix16(hash)
		if err != nil {
			return nil, apierr.New(apierr.CodeInternalError).WithCause(err)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO t_api_key
				(user_id, key_name, key_prefix, key_hash, key_hash_version, key_last4, key_enc,
				 status, model_scope_mode, ip_whitelist_mode, group_code, expire_at, created_at, updated_at)
			VALUES (?,?,?,?,1,?,?,'ENABLED',?,?,?,?,?,?)`,
			uid, name, apikey.DisplayPrefix(plain), hash, apikey.Last4(plain),
			s.sealSecret(ctx, plain),
			scopeMode, ipMode(rules), "standard", expireAt, now, now)
		if err != nil {
			s.logError(ctx, "key_create_insert_failed: "+err.Error())
			return nil, apierr.New(apierr.CodeInternalError)
		}
		kid, err := res.LastInsertId()
		if err != nil {
			return nil, apierr.New(apierr.CodeInternalError)
		}
		for _, modelID := range modelIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO t_api_key_model (api_key_id, model_id, created_at) VALUES (?,?,?)`,
				kid, modelID, now); err != nil {
				s.logError(ctx, "key_create_model_insert_failed: "+err.Error())
				return nil, apierr.New(apierr.CodeInternalError)
			}
		}
		var maxIPID int64
		for _, rule := range rules {
			ruleRes, err := tx.ExecContext(ctx, `
				INSERT INTO t_api_key_ip_rule (api_key_id, cidr, ip_family, note, created_at)
				VALUES (?,?,?,?,?)`, kid, rule.CIDR, rule.Family, nil, now)
			if err != nil {
				s.logError(ctx, "key_create_ip_insert_failed: "+err.Error())
				return nil, apierr.New(apierr.CodeInternalError)
			}
			if id, err := ruleRes.LastInsertId(); err == nil && id > maxIPID {
				maxIPID = id
			}
		}

		item := createdKey{
			ID:             kid,
			KeyName:        name,
			Key:            plain,
			KeyPrefix:      apikey.DisplayPrefix(plain),
			KeyLast4:       apikey.Last4(plain),
			KeyMasked:      mask(apikey.DisplayPrefix(plain), apikey.Last4(plain)),
			ModelScopeMode: scopeMode,
			ModelIDs:       modelIDs,
			GroupCode:      "standard",
			ExpireAt:       expireAt,
			IPAllowlist:    ruleCIDRs(rules),
		}
		after, err := jsonx.Marshal(map[string]any{
			"key_name":          name,
			"key_prefix":        item.KeyPrefix,
			"key_last4":         item.KeyLast4,
			"status":            "ENABLED",
			"model_scope_mode":  scopeMode,
			"model_ids":         modelIDs,
			"group_code":        "standard",
			"expire_at":         expireAt,
			"ip_whitelist_mode": ipMode(rules),
			"ip_allowlist":      item.IPAllowlist,
		})
		if err != nil {
			return nil, apierr.New(apierr.CodeInternalError).WithCause(err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO t_api_key_audit
				(api_key_id, action, before_json, after_json, operator_id, operator_ip, created_at)
			VALUES (?, 'CREATE', NULL, ?, ?, ?, ?)`,
			kid, string(after), uid, audit.IPBytes(r), now); err != nil {
			s.logError(ctx, "key_create_audit_failed: "+err.Error())
			return nil, apierr.New(apierr.CodeInternalError)
		}
		pending = append(pending, pendingKey{
			id: kid, hash16: hash16, maxIPID: maxIPID, item: item,
		})
	}

	hasIPRules := len(rules) > 0

	// Without IP rules the snapshot needs no entry: P06 treats a missing rule
	// as ip_whitelist_mode=OFF. K13 can therefore be written before commit, so
	// a Redis failure rolls the whole batch back and the caller never receives
	// a plaintext for a key that does not exist.
	if !hasIPRules {
		if err := s.upsertBatchK13(ctx, pending, uid, name, expireAt, ratio.Int64); err != nil {
			s.dropK13(ctx, pending)
			return nil, apierr.New(apierr.CodeAuthUnavailable).WithCause(err)
		}
	}

	if err := tx.Commit(); err != nil {
		s.logError(ctx, "key_create_commit_failed: "+err.Error())
		if !hasIPRules {
			s.dropK13(ctx, pending)
		}
		return nil, apierr.New(apierr.CodeInternalError)
	}

	// A key with a whitelist must not reach K13 before cfg:snap:ROUTING carries
	// its rules: P06 finds no snapshot entry and would allow every source IP.
	// Publish first, then write K13. A failure soft-deletes the batch so the
	// caller's free-tier slots are not consumed by keys it never saw.
	if hasIPRules {
		if err := s.publishIPRules(ctx); err != nil {
			s.logError(ctx, "key_create_route_publish_failed: "+err.Error())
			s.compensateCreate(ctx, pending)
			return nil, apierr.New(apierr.CodeConfigUnavailable).WithCause(err)
		}
		if err := s.upsertBatchK13(ctx, pending, uid, name, expireAt, ratio.Int64); err != nil {
			s.logError(ctx, "key_create_k13_failed: "+err.Error())
			s.compensateCreate(ctx, pending)
			return nil, apierr.New(apierr.CodeAuthUnavailable).WithCause(err)
		}
	}

	items := make([]createdKey, 0, len(pending))
	for _, p := range pending {
		items = append(items, p.item)
	}
	return items, nil
}

func (s *Service) upsertBatchK13(ctx context.Context, pending []pendingKey, uid int64, name string, expireAt *time.Time, ratioBP int64) error {
	for _, p := range pending {
		if err := s.KeyAuth.Upsert(ctx, p.hash16, redisx.KeyAuthRecord{
			UID: uid, KID: p.id, ST: "ENABLED", IPM: p.maxIPID,
			EXP: optionalExpireMillis(expireAt), NT: "", KN: name,
			GRP: "standard", GP: ratioBP,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) dropK13(ctx context.Context, pending []pendingKey) {
	for _, p := range pending {
		if err := s.KeyAuth.Delete(ctx, p.hash16); err != nil {
			s.logWarn(ctx, "key_create_k13_compensation_failed: "+err.Error())
		}
	}
}

// compensateCreate rolls back a committed batch whose K13/snapshot publication
// failed. The rows stay (usage history may already reference them in theory)
// but are soft-deleted with the same sentinel the delete endpoint uses.
func (s *Service) compensateCreate(ctx context.Context, pending []pendingKey) {
	now := s.now()
	for _, p := range pending {
		if _, err := s.DB.ExecContext(ctx, `
			UPDATE t_api_key
			   SET deleted_at = ?, key_hash = CONCAT('#del#', id, '#', ?),
			       key_name = CONCAT(LEFT(key_name, 58), ' (已删除)'), updated_at = ?
			 WHERE id = ? AND deleted_at IS NULL`,
			now, now.UnixMilli(), now, p.id); err != nil {
			s.logWarn(ctx, "key_create_compensation_failed: "+err.Error())
		}
		if err := s.KeyAuth.Delete(ctx, p.hash16); err != nil {
			s.logWarn(ctx, "key_create_k13_compensation_failed: "+err.Error())
		}
	}
}

func ruleCIDRs(rules []ipRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.CIDR)
	}
	return out
}

// decodeJSON is the shared bounded body reader for console writes.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *apierr.APIError {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	payload, err := io.ReadAll(r.Body)
	if err != nil || len(strings.TrimSpace(string(payload))) == 0 {
		return mw.InvalidField("body")
	}
	if err := jsonx.Unmarshal(payload, dst); err != nil {
		return mw.InvalidField("body")
	}
	return nil
}

// optionalExpireMillis converts a nullable expiry into K13's EXP field.
// 0 means "no expiry" there, so a key without a deadline keeps working
// through the existing check in gw/p05_auth.go (`rec.EXP > 0 && ...`).
func optionalExpireMillis(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UTC().UnixMilli()
}

package key

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/apikey"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/redisx"
)

type rotateRequest struct {
	ExpireAt jsonx.RawMessage `json:"expire_at"`
	// Presence is rejected for the same reason as create/update.
	GroupCode jsonx.RawMessage `json:"group_code"`
}

type rotateResponse struct {
	ID          int64      `json:"id"`
	RotatedFrom int64      `json:"rotated_from_id"`
	KeyName     string     `json:"key_name"`
	Key         string     `json:"key"` // plaintext: the only occurrence
	KeyPrefix   string     `json:"key_prefix"`
	KeyLast4    string     `json:"key_last4"`
	KeyMasked   string     `json:"key_masked"`
	GroupCode   string     `json:"group_code"`
	ExpireAt    *time.Time `json:"expire_at"`
	Warning     string     `json:"warning"`
	// Revocation is immediate. The card allowed "immediate or 5-minute grace";
	// immediate is the safer choice because a leaked key is the usual reason to
	// rotate, and a grace window keeps the leak usable.
	Revocation string `json:"revocation"`
}

// Rotate implements POST /api/console/keys/{id}/rotate (B-40).
//
// The old key is disabled and its revoked_at stamped in the same transaction
// that inserts the replacement, so there is no window in which both keys pass
// authentication. The replacement inherits model scope, IP rules, group and
// expiry from the old row.
func (s *Service) Rotate(w http.ResponseWriter, r *http.Request) {
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
	var req rotateRequest
	if apiErr := decodeOptionalJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if len(req.GroupCode) > 0 {
		mw.WriteError(w, r, mw.InvalidField("group_code"))
		return
	}

	ctx := r.Context()
	now := s.now()
	if s.KeyAuth == nil {
		s.logError(ctx, "key_rotate_k13_writer_missing")
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable))
		return
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "key_rotate_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	old, apiErr := s.loadOwnedKey(ctx, tx, sess.UID, id, true)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	expire := old.ExpireAt
	if len(req.ExpireAt) > 0 {
		parsed, apiErr := parseOptionalExpireAt(req.ExpireAt, now)
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		// 显式给 null 表示改成永不过期，与创建时的口径一致。
		if parsed == nil {
			expire = sql.NullTime{}
		} else {
			expire = sql.NullTime{Time: *parsed, Valid: true}
		}
	} else if old.ExpireAt.Valid && !old.ExpireAt.Time.After(now) {
		// Copying an already-expired date would produce a replacement that is
		// dead on arrival. Ask for a new expiry instead of inventing one.
		mw.WriteError(w, r, mw.InvalidField("expire_at"))
		return
	}

	oldActive := old.Status == "ENABLED" && (!old.ExpireAt.Valid || old.ExpireAt.Time.After(now))
	if !oldActive {
		// Rotating a disabled/expired key creates a new active key, so the
		// free-tier count must still hold.
		if apiErr := s.checkFreeLimit(ctx, tx, sess.UID, 1, now); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}

	modelIDs, err := s.keyModelIDs(ctx, tx, old.ID)
	if err != nil {
		s.logError(ctx, "key_rotate_models_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	rules, err := s.loadIPRules(ctx, tx, old.ID)
	if err != nil {
		s.logError(ctx, "key_rotate_ip_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	plain, hash, err := apikey.Generate()
	if err != nil {
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError).WithCause(err))
		return
	}
	hash16, err := apikey.HashPrefix16(hash)
	if err != nil {
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError).WithCause(err))
		return
	}
	expireArg := nullableSQLTime(expire)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO t_api_key
			(user_id, key_name, key_prefix, key_hash, key_hash_version, key_last4, key_enc,
			 status, model_scope_mode, ip_whitelist_mode, group_code, expire_at,
			 rotated_from_id, created_at, updated_at)
		VALUES (?,?,?,?,1,?,?, 'ENABLED',?,?,?,?,?,?,?)`,
		sess.UID, old.Name, apikey.DisplayPrefix(plain), hash, apikey.Last4(plain),
		s.sealSecret(ctx, plain),
		old.ModelScopeMode, old.IPWhitelistMode, old.GroupCode, expireArg, old.ID, now, now)
	if err != nil {
		s.logError(ctx, "key_rotate_insert_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	newID, err := res.LastInsertId()
	if err != nil {
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	for _, modelID := range modelIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO t_api_key_model (api_key_id, model_id, created_at) VALUES (?,?,?)`,
			newID, modelID, now); err != nil {
			s.logError(ctx, "key_rotate_model_insert_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}
	var maxIPID int64
	for _, rule := range rules {
		ruleRes, err := tx.ExecContext(ctx, `
			INSERT INTO t_api_key_ip_rule (api_key_id, cidr, ip_family, note, created_at)
			VALUES (?,?,?,?,?)`, newID, rule.CIDR, rule.Family, nil, now)
		if err != nil {
			s.logError(ctx, "key_rotate_ip_insert_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		if rid, err := ruleRes.LastInsertId(); err == nil && rid > maxIPID {
			maxIPID = rid
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE t_api_key
		   SET status = 'DISABLED', revoked_at = ?, updated_at = ?
		 WHERE id = ? AND user_id = ? AND deleted_at IS NULL`,
		now, now, old.ID, sess.UID); err != nil {
		s.logError(ctx, "key_rotate_revoke_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	before := map[string]any{
		"status":     old.Status,
		"key_prefix": old.Prefix,
		"key_last4":  old.Last4,
		"expire_at":  nullableTime(old.ExpireAt),
	}
	after := map[string]any{
		"status":         "DISABLED",
		"revoked_at":     now,
		"new_key_id":     newID,
		"new_key_prefix": apikey.DisplayPrefix(plain),
		"new_key_last4":  apikey.Last4(plain),
		"expire_at":      nullableTime(expire),
	}
	if err := insertKeyAudit(ctx, tx, old.ID, "ROTATE", before, after, sess.UID, auditIP(r), now); err != nil {
		s.logError(ctx, "key_rotate_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	ratio, err := groupRatio(ctx, tx, old.GroupCode)
	if err != nil {
		s.logError(ctx, "key_rotate_group_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	// Publish the replacement before commit; roll back on failure so a caller
	// that never saw the plaintext cannot leave a live key behind.
	if err := s.KeyAuth.Upsert(ctx, hash16, redisx.KeyAuthRecord{
		UID: sess.UID, KID: newID, ST: "ENABLED", IPM: maxIPID,
		EXP: expireMillis(expire), NT: "", KN: old.Name,
		GRP: old.GroupCode, GP: ratio.Int64,
	}); err != nil {
		s.logError(ctx, "key_rotate_k13_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable).WithCause(err))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "key_rotate_commit_failed: "+err.Error())
		if derr := s.KeyAuth.Delete(ctx, hash16); derr != nil {
			s.logWarn(ctx, "key_rotate_k13_compensation_failed: "+derr.Error())
		}
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	// The replacement needs a snapshot entry for the same reason a new key does:
	// P07 resolves its model grant from the snapshot, and an absent grant is a
	// denial. Gating on len(rules) left every whitelist-free rotation unusable
	// for up to four minutes (same defect as create, found 2026-11-05).
	{
		if err := s.publishIPRules(ctx); err != nil {
			s.logError(ctx, "key_rotate_route_publish_failed: "+err.Error())
			s.compensateRotate(ctx, newID, hash16)
			old.Status = "DISABLED"
			if perr := s.pushK13(ctx, old); perr != nil {
				s.logError(ctx, "key_rotate_old_k13_failed: "+perr.Error())
			}
			mw.WriteError(w, r, apierr.New(apierr.CodeConfigUnavailable).WithCause(err))
			return
		}
	}

	// The old key must stop authenticating. Its row is already DISABLED; push
	// that state so the gateway rejects it before the next cache refresh.
	old.Status = "DISABLED"
	if err := s.pushK13(ctx, old); err != nil {
		s.logError(ctx, "key_rotate_old_k13_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable).WithCause(err))
		return
	}

	_ = mw.WriteJSON(w, http.StatusCreated, rotateResponse{
		ID:          newID,
		RotatedFrom: old.ID,
		KeyName:     old.Name,
		Key:         plain,
		KeyPrefix:   apikey.DisplayPrefix(plain),
		KeyLast4:    apikey.Last4(plain),
		KeyMasked:   mask(apikey.DisplayPrefix(plain), apikey.Last4(plain)),
		GroupCode:   visibleGroupOrEmpty(old.GroupCode),
		ExpireAt:    nullableTime(expire),
		Warning:     "完整密钥仅本次返回；旧密钥已立即失效",
		Revocation:  "immediate",
	})
}

func visibleGroupOrEmpty(code string) string {
	if code == "internal" {
		return ""
	}
	return code
}

// compensateRotate removes a replacement that never became usable because the
// routing snapshot could not be published. The old key stays DISABLED; the
// caller gets a 503 and can retry, which is safer than leaving an unrestricted
// key behind.
func (s *Service) compensateRotate(ctx context.Context, id int64, hash16 string) {
	now := s.now()
	if _, err := s.DB.ExecContext(ctx, `
		UPDATE t_api_key
		   SET deleted_at = ?, key_hash = CONCAT('#del#', id, '#', ?),
		       key_name = CONCAT(LEFT(key_name, 58), ' (已删除)'), updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		now, now.UnixMilli(), now, id); err != nil {
		s.logWarn(ctx, "key_rotate_compensation_failed: "+err.Error())
	}
	if err := s.KeyAuth.Delete(ctx, hash16); err != nil {
		s.logWarn(ctx, "key_rotate_k13_compensation_failed: "+err.Error())
	}
}

// decodeOptionalJSON accepts an empty body: rotate has no required fields, and
// requiring "{}" would be a needless client trap.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) *apierr.APIError {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		return mw.InvalidField("body")
	}
	if strings.TrimSpace(string(payload)) == "" {
		return nil
	}
	if err := jsonx.Unmarshal(payload, dst); err != nil {
		return mw.InvalidField("body")
	}
	return nil
}

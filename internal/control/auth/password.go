package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
)

// passwordChangeRequest is POST /api/v1/auth/password. old_password is
// compared against the stored bcrypt hash; new_password goes through the same
// rule set as registration (8..72 bytes, no whitespace-only value).
type passwordChangeRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// ChangePassword implements POST /api/v1/auth/password.
//
// The update and its audit row commit in one transaction, then every K20 for
// the uid is deleted through K30. The order is deliberate: a failure between
// the two leaves the new password active and the old sessions alive (the
// caller gets 503), while the reverse order would sign everyone out and then
// possibly fail to change the password at all.
func (s *Service) ChangePassword(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok || sess == nil {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	var req passwordChangeRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if strings.TrimSpace(req.OldPassword) == "" {
		mw.WriteError(w, r, mw.InvalidField("old_password"))
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		mw.WriteError(w, r, mw.InvalidField("new_password"))
		return
	}

	ctx := r.Context()
	var hash, algo string
	err := s.DB.QueryRowContext(ctx, `
		SELECT password_hash, password_algo
		  FROM t_user
		 WHERE id = ? AND deleted_at IS NULL`, sess.UID,
	).Scan(&hash, &algo)
	if errors.Is(err, sql.ErrNoRows) {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	if err != nil {
		s.errorLog(ctx, "password_lookup_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if algo != "bcrypt" {
		// Same fail-closed choice as login: an argon2id row is not a bcrypt
		// mismatch and must not lock the account with a misleading error.
		s.errorLog(ctx, "password_unsupported_algo")
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)); err != nil {
		// A wrong old password changes nothing: no transaction, no revoke, so
		// the existing session survives (hard requirement for this endpoint).
		s.auditPassword(ctx, r, sess.UID, sess.Role, "REJECTED_BY_GUARD")
		mw.WriteError(w, r, apierr.New(apierr.CodeInvalidCredentials))
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcryptCost)
	if err != nil {
		s.errorLog(ctx, "password_hash_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	now := s.now().Now().UTC()
	after, err := jsonx.Marshal(map[string]string{
		"password_updated_at": now.Format(time.RFC3339),
		"sessions_revoked":    "all",
	})
	if err != nil {
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError).WithCause(err))
		return
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.errorLog(ctx, "password_begin_tx_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	res, err := tx.ExecContext(ctx, `
		UPDATE t_user
		   SET password_hash = ?, password_updated_at = ?, password_is_initial = 0, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		string(newHash), now, now, sess.UID)
	if err != nil {
		s.errorLog(ctx, "password_update_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		s.errorLog(ctx, "password_update_no_rows")
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	if err := s.Audit.Insert(ctx, tx, audit.Entry{
		OperatorID:   sess.UID,
		OperatorRole: sess.Role,
		OperatorIP:   audit.IPBytes(r),
		Module:       "USER",
		Action:       "UPDATE",
		TargetType:   "t_user",
		TargetID:     strconv.FormatInt(sess.UID, 10),
		After:        after,
		Result:       "OK",
	}); err != nil {
		s.errorLog(ctx, "password_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.errorLog(ctx, "password_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	// Every device: K30 is the uid→sid index, so this is one SMEMBERS plus one
	// DEL rather than a keyspace scan.
	if err := s.Sessions.RevokeAll(ctx, sess.UID); err != nil {
		// The password is already new; returning 204 would leave the old
		// sessions usable. 503 tells the client the revoke did not happen.
		s.errorLog(ctx, "password_revoke_all_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable))
		return
	}
	s.Sessions.ClearCookie(w)
	s.Sessions.ClearCSRFCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// auditPassword records a rejected attempt. It is best-effort: a failed audit
// write must not turn a wrong-password response into a 500 (and, crucially,
// must not revoke the session).
func (s *Service) auditPassword(ctx context.Context, r *http.Request, uid int64, role, result string) {
	if s.Audit == nil {
		return
	}
	if err := s.Audit.Insert(ctx, s.DB, audit.Entry{
		OperatorID:   uid,
		OperatorRole: role,
		OperatorIP:   audit.IPBytes(r),
		Module:       "USER",
		Action:       "UPDATE",
		TargetType:   "t_user",
		TargetID:     strconv.FormatInt(uid, 10),
		Result:       result,
	}); err != nil {
		s.warn(ctx, "password_audit_failed: "+err.Error())
	}
}

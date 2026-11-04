package auth

import (
	"context"
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

// resetPasswordRequest is POST /api/v1/auth/password/reset.
//
// One request carries both the proof (code) and the new secret. A two-step
// "verify the code, get a ticket" protocol was considered and rejected: it adds
// a ticket namespace with its own expiry for no security gain, and the fixed
// validation order below already makes a weak password cost nothing.
type resetPasswordRequest struct {
	Identifier  string `json:"identifier"`
	Code        string `json:"code"`
	NewPassword string `json:"new_password"`
}

// ResetPassword implements POST /api/v1/auth/password/reset — anonymous
// self-service password recovery.
//
// The validation order is a hard requirement and is not interchangeable:
//
//		decode → normalize → mode → password rule → code present
//		  → consume code → look the account up → UPDATE + audit → revoke
//
//	  - The password rule runs BEFORE the code is consumed, so a weak password
//	    does not burn a code the user then has to wait 60 seconds to replace.
//	  - The account lookup runs AFTER the code is consumed, so account existence
//	    cannot influence any observable behaviour of the first six steps.
//
// Nothing here is read from the client except the three body fields: uid, role
// and status come from the row the identifier resolves to, never from the
// request.
func (s *Service) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ident, err := NormalizeIdentifier(req.Identifier)
	if err != nil {
		mw.WriteError(w, r, mw.InvalidField("identifier"))
		return
	}
	// A username is not an address: there is no channel to deliver a code to.
	// Refusing is not an enumeration signal — it is a property of the input.
	if err := RequireEmailOrPhone(ident); err != nil {
		mw.WriteError(w, r, mw.InvalidField("identifier").
			WithHint("用户名不能用于找回密码，请使用邮箱或手机号"))
		return
	}
	// Fail closed when the deployment has no verification at all. Without a code
	// there is no proof of identity, so allowing this would mean "knowing an
	// e-mail address is enough to reset that account".
	if s.verificationMode() == VerificationOff {
		mw.WriteError(w, r, apierr.New(apierr.CodeInvalidRequestBody).
			WithHint("本部署未启用验证码，无法自助重置，请联系管理员"))
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		mw.WriteError(w, r, mw.InvalidField("new_password"))
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		mw.WriteError(w, r, mw.InvalidField("code"))
		return
	}

	ctx := r.Context()
	// Consumed before the lookup and before the update: a wrong code must change
	// nothing, and a right one must not survive a second use.
	if apiErr := s.consumeVerificationCode(ctx, ident, PurposeReset, code); apiErr != nil {
		// No audit row: the audit trail records what an authorised operator did,
		// and an anonymous failed guess would only be noise. The WARN keeps the
		// pattern visible for a support engineer.
		s.warn(ctx, "password_reset_code_rejected: "+ident.Kind)
		mw.WriteError(w, r, apiErr)
		return
	}

	row, findErr := s.findResetAccount(ctx, ident)
	if findErr != nil {
		// In theory unreachable: the send-code path already dropped the code for
		// an unusable account. Answering the same invalid_credentials keeps the
		// window between the two calls from becoming an oracle (404/409 here
		// would be one).
		s.warn(ctx, "password_reset_account_unavailable: "+ident.Kind)
		mw.WriteError(w, r, verificationInvalid())
		return
	}

	newHash, hashErr := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcryptCost)
	if hashErr != nil {
		s.errorLog(ctx, "password_reset_hash_failed: "+hashErr.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	now := s.now().Now().UTC()
	// after_json is a facts-only snapshot: no password, no hash, no code, and no
	// plaintext address. `via` is what lets an operator tell a self-service reset
	// apart from a change-password call three months later.
	after, marshalErr := jsonx.Marshal(map[string]string{
		"password_updated_at": now.Format(time.RFC3339),
		"sessions_revoked":    "all",
		"via":                 "reset:" + channelName(ident.Kind),
	})
	if marshalErr != nil {
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError).WithCause(marshalErr))
		return
	}

	if apiErr := s.writeReset(ctx, r, row, string(newHash), now, after); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	// Revoke after the commit, not before. A failure here leaves the new password
	// active and the old sessions alive, so the answer is 503 — claiming 204 would
	// tell the client the old sessions are gone when they are not.
	if err := s.Sessions.RevokeAll(ctx, row.uid); err != nil {
		s.errorLog(ctx, "password_reset_revoke_all_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable))
		return
	}
	// The caller may be signed in on this device (the "signed in but forgot the
	// old password" story), so the current cookies are cleared as well. No new
	// session is issued: signing in again is the only way to prove on the spot
	// that the new password works.
	s.Sessions.ClearCookie(w)
	s.Sessions.ClearCSRFCookie(w)
	// Without this, someone who exhausted the login budget before remembering
	// their password would reset successfully and still be locked out for the
	// rest of the window.
	s.clearAccountFailures(ctx, ident)
	w.WriteHeader(http.StatusNoContent)
}

// writeReset updates the credential and writes its audit row in one
// transaction: a password that changed with no trace, or an audit row for a
// password that did not change, are both worse than a failed request.
func (s *Service) writeReset(ctx context.Context, r *http.Request, row userRow,
	passwordHash string, now time.Time, after []byte) *apierr.APIError {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.errorLog(ctx, "password_reset_begin_tx_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	res, err := tx.ExecContext(ctx, `
		UPDATE t_user
		   SET password_hash = ?, password_updated_at = ?, password_is_initial = 0, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		passwordHash, now, now, row.uid)
	if err != nil {
		s.errorLog(ctx, "password_reset_update_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil || n == 0 {
		// The row vanished between the lookup and the update (cancelled
		// concurrently). Same answer as "no account": the code is already spent.
		s.warn(ctx, "password_reset_update_no_rows")
		return verificationInvalid()
	}
	// password_is_initial = 0: the user chose this password themselves, so the
	// "still using the initial password" banner would be a factual error. Only
	// the bootstrap and a DBA hand-edit may set it back to 1.
	if err := s.Audit.Insert(ctx, tx, audit.Entry{
		OperatorID:   row.uid,
		OperatorRole: row.role,
		OperatorIP:   audit.IPBytes(r),
		Module:       "USER",
		Action:       "UPDATE",
		TargetType:   "t_user",
		TargetID:     strconv.FormatInt(row.uid, 10),
		After:        after,
		Result:       "OK",
	}); err != nil {
		s.errorLog(ctx, "password_reset_audit_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	if err := tx.Commit(); err != nil {
		s.errorLog(ctx, "password_reset_commit_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	return nil
}

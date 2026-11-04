package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
)

// Binding a login mailbox is deliberately two endpoints and NOT a field on
// PATCH /auth/profile:
//
//	POST /api/v1/auth/email/code  {email}        -> send a purpose=bind code
//	POST /api/v1/auth/email       {email, code}  -> verify, then write t_user.email
//
// profile.nickname is a required field that nobody verifies. Mixing "an
// unverified label" and "a verified login identifier" into one request makes the
// failing case undefinable: if the nickname was stored and the mailbox turned
// out to be taken, what is the result of the request?

// emailBindCodeRequest is POST /api/v1/auth/email/code.
type emailBindCodeRequest struct {
	Email string `json:"email"`
}

// emailBindRequest is POST /api/v1/auth/email.
type emailBindRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// Hint texts. They are constants so the docs, the tests and the handler cannot
// drift apart.
const (
	emailAlreadyBoundHint = "当前账号已绑定登录邮箱；更换邮箱请提交工单"
	emailModeOffHint      = "本部署未启用验证码，无法验证邮箱"
	emailTakenHint        = "该邮箱已被使用"
)

// bindPurposeQuotaLimit is how many binding codes one signed-in account may
// request per day (vc:quota:bind:uid:{uid}). Per-address limits cannot bound
// this: a script would get a fresh cooldown and a fresh address budget for every
// mailbox it names.
const bindPurposeQuotaLimit = 5

// SendEmailBindCode implements POST /api/v1/auth/email/code.
//
// Signed in + CSRF + account.write: this is "change my own profile", not the
// operator-wide user.write.
func (s *Service) SendEmailBindCode(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok || sess == nil {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	var req emailBindCodeRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ident, apiErr := parseEmailField(req.Email)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ctx := r.Context()

	// Already bound: this increment only implements "bind when empty". Changing a
	// bound mailbox is an account-takeover channel that needs a notice to the old
	// address and a cooling-off window, i.e. its own security review.
	if current, apiErr := s.currentEmail(ctx, sess.UID); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	} else if current != "" {
		mw.WriteError(w, r, mw.InvalidField("email").WithHint(emailAlreadyBoundHint))
		return
	}
	if s.verificationMode() == VerificationOff {
		// Fail closed: without a code there is no proof the mailbox belongs to the
		// caller, and writing an unverified address would hand that address the
		// password-reset channel.
		mw.WriteError(w, r, apierr.New(apierr.CodeInvalidRequestBody).WithHint(emailModeOffHint))
		return
	}
	// Occupied by someone else. 409 with no message sent: the caller is signed in,
	// and POST /verification-code{purpose:register} already discloses "this
	// address is registered" anonymously, so this is not a new leak surface.
	if _, findErr := s.findUser(ctx, ident); findErr == nil {
		mw.WriteError(w, r, apierr.New(apierr.CodeAccountExists).WithHint(emailTakenHint))
		return
	} else if !isNotFound(findErr) {
		s.errorLog(ctx, "email_bind_lookup_failed: "+findErr.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	code, issueErr := s.issueVerificationCode(ctx, ident, PurposeBind, clientIP(r))
	if issueErr != nil {
		// The cooldown, the address budget and the per-IP ceiling are all decided
		// in there, and every one of them is a refusal to send — so the account
		// budget below must not have been charged for this request yet. Charging it
		// first was a real defect (F1): five clicks on one address where only the
		// first message went out exhausted the whole day's allowance.
		mw.WriteError(w, r, issueErr)
		return
	}
	// Per-account daily budget, charged ONLY for a code that was actually issued.
	// The per-address limits cannot bound "one signed-in account walking a list of
	// mailboxes": each new address brings a fresh cooldown and a fresh address
	// quota, so this dimension is the one that keeps a signed-in session from
	// becoming a free sender.
	//
	// Ordering note for whoever reads this next — do not move it back above
	// issueVerificationCode: that function's internal order is
	// `per-IP hourly → cooldown SETNX → per-address daily → generate & store`,
	// so anything charged before it is also charged for requests that were
	// rejected by a cheaper limit and never sent a message.
	//
	// Residual, accepted: when this dimension is the one that refuses (the sixth
	// code of the day), the cooldown key from the call above stays set for 60s.
	// The caller is already out of budget for the whole day, so that key costs
	// them nothing; it expires on its own.
	if apiErr := s.consumeUIDQuota(ctx, PurposeBind, sess.UID, bindPurposeQuotaLimit); apiErr != nil {
		// Drop the code issued a moment ago: nobody will ever receive it, and
		// leaving it in Redis would mean a code addressed to a mailbox exists
		// without a corresponding delivery.
		s.dropVerificationCode(ctx, ident, PurposeBind)
		mw.WriteError(w, r, apiErr)
		return
	}
	if deliverErr := s.deliverCode(ctx, ident, PurposeBind, code); deliverErr != nil {
		mw.WriteError(w, r, deliverErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, verificationResponse{
		Channel:     channelName(ident.Kind),
		ExpiresIn:   int(verificationCodeTTL / time.Second),
		ResendAfter: int(verificationResendAfter / time.Second),
	})
}

// BindEmail implements POST /api/v1/auth/email.
//
// success returns 200 {user} with the new address. The session is NOT revoked:
// binding changes neither role nor status nor the credential, and the session is
// bound to the uid, so the next Session.Load re-reads t_user and /auth/me
// naturally reports the new address.
func (s *Service) BindEmail(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok || sess == nil {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	var req emailBindRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ident, apiErr := parseEmailField(req.Email)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ctx := r.Context()

	if current, apiErr := s.currentEmail(ctx, sess.UID); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	} else if current != "" {
		mw.WriteError(w, r, mw.InvalidField("email").WithHint(emailAlreadyBoundHint))
		return
	}
	if s.verificationMode() == VerificationOff {
		mw.WriteError(w, r, apierr.New(apierr.CodeInvalidRequestBody).WithHint(emailModeOffHint))
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		mw.WriteError(w, r, mw.InvalidField("code"))
		return
	}
	// Consumed before the write: a wrong code must not leave a half-done state,
	// and a right one must not be replayable.
	if consumeErr := s.consumeVerificationCode(ctx, ident, PurposeBind, code); consumeErr != nil {
		mw.WriteError(w, r, consumeErr)
		return
	}

	now := s.now().Now().UTC()
	after, marshalErr := jsonx.Marshal(map[string]any{
		"email_changed": true,
		// Masked: the audit trail records that an address was bound, not which
		// address (a***@x.com is enough to recognise the account in a ticket).
		"email": maskIdentifier(KindEmail, ident.Value),
	})
	if marshalErr != nil {
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError).WithCause(marshalErr))
		return
	}
	if apiErr := s.writeBoundEmail(ctx, r, sess, ident.Value, now, after); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	usr, apiErr := s.currentUserView(ctx, sess.UID)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, map[string]any{"user": usr})
}

// writeBoundEmail performs the conditional UPDATE and its audit row in one
// transaction.
//
// The WHERE clause carries the "only when empty" rule as well as the application
// check, so a concurrent request cannot overwrite an address that another
// request just bound: RowsAffected = 0 is the answer for that race, mapped to the
// same 409 as a duplicate key.
func (s *Service) writeBoundEmail(ctx context.Context, r *http.Request, sess *mw.Session,
	email string, now time.Time, after []byte) *apierr.APIError {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.errorLog(ctx, "email_bind_begin_tx_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	res, err := tx.ExecContext(ctx, `
		UPDATE t_user
		   SET email = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL AND (email IS NULL OR email = '')`,
		email, now, sess.UID)
	if err != nil {
		if IsDuplicate(err) {
			// uk_user_email. 409, not 500: the body was fine, the address is taken.
			return apierr.New(apierr.CodeAccountExists).WithHint(emailTakenHint)
		}
		s.errorLog(ctx, "email_bind_update_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil || n == 0 {
		// Lost the race: somebody bound an address (ours or another) between the
		// check and the update.
		s.warn(ctx, "email_bind_no_rows")
		return apierr.New(apierr.CodeAccountExists).WithHint(emailTakenHint)
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
		s.errorLog(ctx, "email_bind_audit_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	if err := tx.Commit(); err != nil {
		s.errorLog(ctx, "email_bind_commit_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	return nil
}

// parseEmailField normalizes one mailbox from a request body.
//
// It reuses NormalizeIdentifier and then insists on the email kind: an input that
// classifies as a phone number or a username is not a mailbox, and answering
// "the field is malformed" is more useful than a lookup that can never match.
func parseEmailField(raw string) (Identifier, *apierr.APIError) {
	ident, err := NormalizeIdentifier(raw)
	if err != nil || ident.Kind != KindEmail {
		return Identifier{}, mw.InvalidField("email")
	}
	return ident, nil
}

// currentEmail reads COALESCE(t_user.email, ”). An empty result means "not
// bound" and is the only state in which this increment allows a binding.
func (s *Service) currentEmail(ctx context.Context, uid int64) (string, *apierr.APIError) {
	var email string
	err := s.DB.QueryRowContext(ctx,
		`SELECT COALESCE(email, '') FROM t_user WHERE id = ? AND deleted_at IS NULL`,
		uid).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", mw.SessionMissing()
	}
	if err != nil {
		s.errorLog(ctx, "email_bind_current_lookup_failed: "+err.Error())
		return "", apierr.New(apierr.CodeInternalError)
	}
	return strings.TrimSpace(email), nil
}

// currentUserView rebuilds the console's user shape after a write, so the
// response carries the same fields /auth/me would return.
func (s *Service) currentUserView(ctx context.Context, uid int64) (userView, *apierr.APIError) {
	var email, phone, username, nickname, role, status string
	var passwordIsInitial bool
	err := s.DB.QueryRowContext(ctx, `
		SELECT COALESCE(email, ''), COALESCE(phone, ''), COALESCE(username, ''),
		       COALESCE(nickname, ''), role, status, password_is_initial
		  FROM t_user
		 WHERE id = ? AND deleted_at IS NULL`, uid).
		Scan(&email, &phone, &username, &nickname, &role, &status, &passwordIsInitial)
	if errors.Is(err, sql.ErrNoRows) {
		return userView{}, mw.SessionMissing()
	}
	if err != nil {
		s.errorLog(ctx, "email_bind_readback_failed: "+err.Error())
		return userView{}, apierr.New(apierr.CodeInternalError)
	}
	return view(uid, email, phone, username, nickname, role, status, passwordIsInitial), nil
}

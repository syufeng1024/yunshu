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
	"yunshu.dev/core/internal/platform/phoneclaim"
)

const (
	// cancelDisableSource distinguishes a self-service cancellation from an
	// operator ban (OPS_MANUAL) and an automatic risk ban (RISK_AUTO). It is a
	// disable_source value, not a new t_user.status enum value: the account
	// still lands in DISABLED, which every existing guard already understands.
	cancelDisableSource = "SELF_CANCEL"
	cancelDisableReason = "用户主动注销"
)

// Confirmation modes. The value is echoed to the console (userView.login_kind)
// so the confirmation field can label itself with the identifier the server
// will actually compare, instead of guessing from an empty e-mail string.
const (
	confirmModeEmail       = "email"
	confirmModePhoneSuffix = "phone_suffix"
	confirmModeUsername    = "username"
	confirmModeNone        = "none"
)

// cancelRequest is the body of POST /api/v1/auth/cancel.
//
// The body is required: cancellation now releases the phone number (the instant
// it is committed, anyone may register it) and is not reversible, so a single
// click with no typed value is not enough of a speed bump. An empty body is
// refused by decodeJSON (400 invalid_request_body) before anything is read.
type cancelRequest struct {
	Confirm string `json:"confirm"`
}

// outstandingTicketNos is the narrow interface implemented by
// ticket.OutstandingTicketsError. errors.As targets the interface, so the auth
// package never needs a compile-time dependency on the ticket package.
type outstandingTicketNos interface {
	OutstandingTicketNos() []string
}

// Cancel implements POST /api/v1/auth/cancel.
//
// Order: the ticket pre-flight runs first and changes nothing when it refuses;
// then, inside one transaction, the account row is locked, the typed
// confirmation is compared, status=DISABLED is written, the phone number is
// recorded in t_phone_claim and released, and the audit row is written; then
// every K20/K30 session key is deleted. A revoke failure answers 503 instead of
// the success status because the old sessions outliving the account is the
// exact condition this endpoint must rule out.
//
// CANCELLATION RELEASES phone AND phone_hmac (INC-phone-claim). This is the
// reverse of the rule this handler used to carry, and the reversal is the point
// of the increment:
//
//   - t_user.phone is set to NULL and t_user.phone_hmac to NULL in the same
//     transaction that disables the account. The number therefore becomes
//     registrable again immediately — a user who cancelled by mistake is no
//     longer locked out by their own handset (PRD U1/U3).
//   - Losing the hmac would also lose the two facts the number carries, so they
//     are moved to t_phone_claim first: how many times this number has been
//     released (cancel_count), and when it last received a trial
//     (trial_granted_at). The ledger is addressed by the hmac and is the only
//     memory that survives the release.
//   - What the release does NOT do is weaken registrability: uniqueness still
//     lives on t_user.uk_user_phone_hmac and is still decided atomically by the
//     INSERT in registration. The ledger is never consulted for "is this number
//     taken" (that would be check-then-act).
//   - Only SELF_CANCEL releases. Operator bans (OPS_MANUAL) and automatic risk
//     bans (RISK_AUTO) keep the number occupied: releasing them would let a
//     banned account re-register with the same handset.
//
// The single place that decides what happens to the phone columns is
// cancelPhonePII below.
func (s *Service) Cancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok || sess == nil {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	ctx := r.Context()

	if s.CancellableCheck != nil {
		if err := s.CancellableCheck(ctx, sess.UID); err != nil {
			var blocked outstandingTicketNos
			if errors.As(err, &blocked) {
				nos := blocked.OutstandingTicketNos()
				mw.WriteError(w, r, apierr.Newf(apierr.CodeResourceStateConflict,
					"存在未结工单，无法注销：%s", strings.Join(nos, "、")).
					WithHint("请先关闭工单或等待工单处理完成后再注销"))
				return
			}
			s.errorLog(ctx, "cancel_preflight_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}

	now := s.now().Now().UTC()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.errorLog(ctx, "cancel_begin_tx_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// First ring of the lock order: t_user, then t_phone_claim (design §6.1).
	// The read is also what the confirmation guard compares against, so it
	// happens before any write — a wrong confirmation must leave no trace.
	ident, apiErr := lockCancelIdentity(ctx, tx, sess.UID)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	// The confirmation's shape — and therefore its hint — comes from the row this
	// transaction just locked, not from the session snapshot: the snapshot can be
	// older than the account (a profile edit, an address bound after login), and
	// a hint derived from stale data would ask for a value the check rejects.
	// Reading it here is free: the lock is taken either way, before any write.
	hintMode := ident.primaryMode()

	var req cancelRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		// One exit for every malformed or missing body. decodeJSON answers
		// `field=body` for an empty body, and the client must not be able to tell
		// "you sent nothing" from "you sent the wrong value" — both are the same
		// 400, differing only in the hint (which follows the account shape, never
		// the mistake). A missing row never reaches this point: lockCancelIdentity
		// answers 401 session_expired instead of dressing it up as a bad field.
		mw.WriteError(w, r, confirmRefusal(hintMode))
		return
	}
	if ident.Status == "DISABLED" {
		// Already cancelled. Answer the state conflict the old handler answered,
		// and do it before the confirmation guard: once the number has been
		// released the row may hold no identifier at all, so a second attempt
		// could never produce a matching confirmation and would be reported as
		// "wrong field" — the misleading answer.
		mw.WriteError(w, r, apierr.Newf(apierr.CodeResourceStateConflict,
			"账号已注销或已禁用，不能重复注销"))
		return
	}
	if apiErr := checkCancelConfirm(ident, req.Confirm, hintMode); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE t_user
		   SET status = 'DISABLED', disable_source = ?, disable_reason = ?,
		       disabled_at = ?, disabled_by = NULL, cancel_apply_at = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL AND status <> 'DISABLED'`,
		cancelDisableSource, cancelDisableReason, now, now, now, sess.UID)
	if err != nil {
		s.errorLog(ctx, "cancel_update_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		// A session can only reach this handler while the account is usable, so
		// zero rows means a concurrent request already disabled it.
		mw.WriteError(w, r, apierr.Newf(apierr.CodeResourceStateConflict,
			"账号已注销或已禁用，不能重复注销"))
		return
	}

	release, err := cancelPhonePII(ctx, tx, ident, now)
	if err != nil {
		s.errorLog(ctx, "cancel_phone_pii_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if release.SkippedNoHMAC {
		// The account is cancelled; only the number release was skipped, and it
		// is recoverable with an operator command. WARN, not ERROR: nothing is
		// broken for this request, and an ERROR here would page someone at 3am
		// for a row that a documented one-liner fixes.
		s.warn(ctx, "cancel_phone_release_skipped reason=phone_without_hmac uid="+
			strconv.FormatInt(sess.UID, 10))
	}

	if s.Audit != nil {
		before, after, err := audit.MarshalBeforeAfter(
			map[string]any{"status": sess.Status},
			map[string]any{
				"status":             "DISABLED",
				"disable_source":     cancelDisableSource,
				"cancel_apply_at":    now,
				"phone_released":     release.Released,
				"claim_cancel_count": release.ClaimCancelCount,
			})
		if err != nil {
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError).WithCause(err))
			return
		}
		if err := s.Audit.Insert(ctx, tx, audit.Entry{
			OperatorID:   sess.UID,
			OperatorRole: sess.Role,
			OperatorIP:   audit.IPBytes(r),
			Module:       "USER",
			Action:       "CANCEL",
			TargetType:   "t_user",
			TargetID:     strconv.FormatInt(sess.UID, 10),
			Before:       before,
			After:        after,
			Reason:       cancelDisableReason,
			Result:       "OK",
		}); err != nil {
			s.errorLog(ctx, "cancel_audit_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}

	if err := tx.Commit(); err != nil {
		s.errorLog(ctx, "cancel_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	if release.Released {
		// uid only: the number and its hmac are both PII-adjacent, and this
		// line is the one an operator greps for when a release is questioned.
		s.warn(ctx, "phone_claim_released uid="+strconv.FormatInt(sess.UID, 10))
	}

	// K30 is the only uid→sid index; RevokeAll deletes every K20 it names and
	// the set itself in one Lua script, so no sid survives a partial failure.
	if err := s.Sessions.RevokeAll(ctx, sess.UID); err != nil {
		s.errorLog(ctx, "cancel_revoke_all_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable))
		return
	}
	s.Sessions.ClearCookie(w)
	s.Sessions.ClearCSRFCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// cancelIdentity is the locked account row as the cancellation path sees it.
// Only the four columns that decide the confirmation mode and the release are
// read; nothing else is needed to make either decision.
type cancelIdentity struct {
	UID            int64
	Status         string
	Email          string
	Phone          string
	PhoneHMAC      string
	Username       string
	TrialClaimedAt sql.NullTime
}

// loginKindFor names the identifier the cancellation confirmation is compared
// against, and is the single source for both sides: the server guard below and
// the console's field label (userView.login_kind).
func loginKindFor(email, phone, username string) string {
	switch {
	case strings.TrimSpace(email) != "":
		return confirmModeEmail
	case strings.TrimSpace(phone) != "":
		return confirmModePhoneSuffix
	case strings.TrimSpace(username) != "":
		return confirmModeUsername
	default:
		return confirmModeNone
	}
}

// primaryMode names the identifier the console should ask the user to type.
// Priority matches the login order: an e-mail is the account name, a phone is
// the fallback, a username is the operator/built-in-admin case.
func (id cancelIdentity) primaryMode() string {
	return loginKindFor(id.Email, id.Phone, id.Username)
}

// lockCancelIdentity reads and locks the account row, and returns the columns
// the rest of the handler decides on.
func lockCancelIdentity(ctx context.Context, tx *sql.Tx, uid int64) (cancelIdentity, *apierr.APIError) {
	ident := cancelIdentity{UID: uid}
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(status, ''), COALESCE(email, ''), COALESCE(phone, ''),
		       COALESCE(phone_hmac, ''), COALESCE(username, ''), trial_claimed_at
		  FROM t_user WHERE id = ? AND deleted_at IS NULL FOR UPDATE`, uid).
		Scan(&ident.Status, &ident.Email, &ident.Phone, &ident.PhoneHMAC,
			&ident.Username, &ident.TrialClaimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cancelIdentity{}, mw.SessionMissing()
	}
	if err != nil {
		return cancelIdentity{}, apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	return ident, nil
}

// checkCancelConfirm is the server-side half of the confirmation dialog.
//
// The console used to compare the typed value against the account's e-mail; for
// a phone-registered account that column is NULL, so the comparison ran against
// an empty string and an empty input passed. The dialog was decorative exactly
// where the consequence is worst. The rule is now:
//
//	e-mail present  -> the full e-mail (lower-cased, trimmed: the login
//	                   normalisation, so nothing new has to be remembered)
//	otherwise phone -> the last four digits of the number
//	otherwise name  -> the username (lower-cased, trimmed)
//
// Only the account's own primary identifier is compared, and the three tiers are
// mutually exclusive by construction, so there is no "ambiguous match" state to
// report. Every failure — empty value, mismatched value — answers the same
// 400 invalid_request_body naming the `confirm` field; the hint differs by
// account shape, never by which mistake was made. A distinct code for "wrong
// value" would also give an unauthenticated probe a way to ask "is this account
// identifier real", which is the one thing this endpoint must not answer.
func checkCancelConfirm(id cancelIdentity, confirm, hintMode string) *apierr.APIError {
	refusal := confirmRefusal(hintMode)
	typed := strings.TrimSpace(confirm)
	if typed == "" {
		return refusal
	}
	switch id.primaryMode() {
	case confirmModeEmail:
		if normalizeConfirmText(typed) == normalizeConfirmText(id.Email) {
			return nil
		}
	case confirmModePhoneSuffix:
		// The typed value only counts when it is exactly four digits; anything
		// else cannot be the suffix and falls through to the same refusal.
		if digits := confirmDigits(typed); digits != "" && digits == phoneSuffix(id.Phone) {
			return nil
		}
	case confirmModeUsername:
		if normalizeConfirmText(typed) == normalizeConfirmText(id.Username) {
			return nil
		}
	}
	return refusal
}

// confirmRefusal is the single 400 every confirmation failure produces. Using
// mw.InvalidField keeps the field name in `message` ("请求体参数有误：confirm"),
// which is how a client learns which member to fix; the hint is the only part
// that varies, and it varies with the account shape.
func confirmRefusal(hintMode string) *apierr.APIError {
	return mw.InvalidField("confirm").WithHint(confirmHintForMode(hintMode))
}

// confirmHintForMode is the one place the three prompts live, so the console and
// the API answer describe the same field.
func confirmHintForMode(mode string) string {
	switch mode {
	case confirmModeEmail:
		return "请输入完整的登录邮箱以确认注销"
	case confirmModePhoneSuffix:
		return "请输入手机号后 4 位以确认注销"
	case confirmModeUsername:
		return "请输入登录用户名以确认注销"
	default:
		return "请输入登录标识以确认注销"
	}
}

// normalizeConfirmText is the login normalisation (trim + lower-case). Reusing
// it means a user who can log in with a capitalised address can also cancel
// with it, instead of being told their own address is wrong.
func normalizeConfirmText(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// confirmDigits keeps only the digits of the typed value. It lets the console
// accept "1234" and "****1234" alike, and refuses anything that is not exactly
// four digits by returning a value no suffix can equal.
func confirmDigits(v string) string {
	var b strings.Builder
	for _, r := range v {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if len(digits) != 4 {
		return ""
	}
	return digits
}

// phoneSuffix returns the last four digits of a stored number, or "" when the
// number is short enough that a suffix comparison would be meaningless.
func phoneSuffix(phone string) string {
	var digits strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	all := digits.String()
	if len(all) < 4 {
		return ""
	}
	return all[len(all)-4:]
}

// cancelRelease is what cancelPhonePII did, for the audit row and the log line.
type cancelRelease struct {
	// Released is true only when both phone and phone_hmac were cleared.
	Released bool
	// SkippedNoHMAC is true in the third state: the row carries a plaintext
	// number but no hash, so neither column was touched.
	SkippedNoHMAC bool
	// ClaimCancelCount is cancel_count after the ledger update (0 when no
	// ledger row was touched, i.e. an e-mail account or the third state).
	ClaimCancelCount int
}

// cancelPhonePII is the only place that decides what cancellation does to
// t_user.phone, t_user.phone_hmac and t_phone_claim.
//
// Three states, and only three (design §5.2):
//
//	phone empty, hmac empty     -> e-mail / username account: nothing to do
//	phone set,   hmac set       -> record the release, then clear both columns
//	phone set,   hmac empty     -> touch neither column, WARN, and leave the row
//	                               to `backfill-phone-claim release --uid`
//
// The third state is not laziness. The hmac can only be recomputed from the
// plaintext phone, so clearing the plaintext would make this number's window
// memory permanently unreconstructable — that is a free trial, not a cleanup.
// The number is not occupied either way (NULL does not participate in
// uk_user_phone_hmac), so registrability is unaffected; the operator completes
// the release after the hmac is repaired (docs/ops-runbook.md §10.3).
//
// Lock order: the caller already holds the t_user row; this function takes the
// t_phone_claim row. Nothing here locks t_user again except the final UPDATE on
// the row it already owns.
func cancelPhonePII(ctx context.Context, tx *sql.Tx, id cancelIdentity, now time.Time) (cancelRelease, error) {
	if id.PhoneHMAC == "" {
		if id.Phone != "" {
			return cancelRelease{SkippedNoHMAC: true}, nil
		}
		return cancelRelease{}, nil
	}

	claim, err := phoneclaim.RecordCancelTx(ctx, tx, id.PhoneHMAC, id.UID, now,
		id.TrialClaimedAt, phoneclaim.TrialGrantUserID(id.TrialClaimedAt, id.UID))
	if err != nil {
		return cancelRelease{}, err
	}
	// The release is the last write on the t_user row in this transaction, and
	// it is guarded by the hmac we read under the lock: a concurrent change of
	// the number (which the lock already rules out) must not be silently
	// overwritten.
	res, err := tx.ExecContext(ctx, `
		UPDATE t_user SET phone = NULL, phone_hmac = NULL, updated_at = ?
		 WHERE id = ? AND phone_hmac = ?`, now, id.UID, id.PhoneHMAC)
	if err != nil {
		return cancelRelease{}, err
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil {
		return cancelRelease{}, rowsErr
	} else if n != 1 {
		return cancelRelease{}, errPhoneReleaseFailed
	}
	return cancelRelease{Released: true, ClaimCancelCount: int(claim.CancelCount)}, nil
}

var (
	// errPhoneReleaseFailed means the guarded UPDATE matched no row while the
	// row was locked for this transaction — a drift, never a request problem.
	errPhoneReleaseFailed = errors.New("cancel: phone release matched no row")
)

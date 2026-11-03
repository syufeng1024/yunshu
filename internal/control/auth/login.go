package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	obslog "yunshu.dev/core/internal/obs/log"
)

type loginRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
	Code       string `json:"code"`
}

type loginResponse struct {
	User userView `json:"user"`
	// CSRFToken mirrors the readable `csrf` cookie. A SPA that stores the
	// response before reading document.cookie can use either copy; both are
	// bound to this session.
	CSRFToken string `json:"csrf_token"`
}

// Login implements POST /api/v1/auth/login.
//
// Two mutually exclusive bodies are accepted:
//
//	{identifier, password}  password login (unchanged)
//	{identifier, code}      verification-code login, for accounts that can read
//	                        the address they registered with
//
// Sending both is rejected rather than picking one: a client that sends a code
// and a password is confused about which factor it is proving, and silently
// ignoring the code would look like the code was accepted.
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	code := strings.TrimSpace(req.Code)
	password := strings.TrimSpace(req.Password)
	if code != "" && password != "" {
		mw.WriteError(w, r, mw.InvalidField("code").
			WithHint("验证码登录与密码登录不能同时提交"))
		return
	}
	ident, err := NormalizeIdentifier(req.Identifier)
	if err != nil || (password == "" && code == "") {
		s.auditLogin(r.Context(), r, 0, "ANONYMOUS", "-", "REJECTED_BY_GUARD")
		mw.WriteError(w, r, apierr.New(apierr.CodeInvalidCredentials))
		return
	}
	if code != "" {
		s.loginWithCode(w, r, ident, code)
		return
	}

	// The lock is checked before the user lookup and before bcrypt. A correct
	// password during the window still gets 429; otherwise an attacker who
	// guesses right on attempt six would make the lock meaningless. Malformed
	// input above is not counted: it never reached a credential comparison.
	ip := clientIP(r)
	lock, lockErr := s.loginLockState(r.Context(), ident, ip)
	if lockErr != nil {
		s.errorLog(r.Context(), "login_lock_check_failed: "+lockErr.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable))
		return
	}
	if lock.locked() {
		s.auditLogin(r.Context(), r, 0, "ANONYMOUS", "-", "REJECTED_BY_GUARD")
		mw.WriteError(w, r, s.lockError(lock))
		return
	}

	row, findErr := s.findUser(r.Context(), ident)
	if findErr != nil {
		if errors.Is(findErr, sql.ErrNoRows) {
			// Equalize the timing with a real bcrypt comparison so the response
			// time does not reveal whether the account exists.
			_ = bcrypt.CompareHashAndPassword(dummyPasswordHash(), []byte(req.Password))
			s.recordLoginFailure(r.Context(), ident, ip)
			s.auditLogin(r.Context(), r, 0, "ANONYMOUS", "-", "REJECTED_BY_GUARD")
			mw.WriteError(w, r, apierr.New(apierr.CodeInvalidCredentials))
			return
		}
		s.errorLog(r.Context(), "login_lookup_failed: "+findErr.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable))
		return
	}

	if row.algo != "bcrypt" {
		// argon2id rows are a planned upgrade, not implemented in segment 1.
		// Failing closed beats treating an argon2id hash as a bcrypt mismatch,
		// which would lock the user out with a misleading "invalid key".
		s.errorLog(r.Context(), "login_unsupported_password_algo")
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(row.hash), []byte(req.Password)); err != nil {
		s.recordLoginFailure(r.Context(), ident, ip)
		s.auditLogin(r.Context(), r, row.uid, row.role, strconv.FormatInt(row.uid, 10), "REJECTED_BY_GUARD")
		mw.WriteError(w, r, apierr.New(apierr.CodeInvalidCredentials))
		return
	}

	s.finishLogin(w, r, row, ident)
}

// loginWithCode proves identity with a mailbox code instead of a password.
//
// The lock is checked first, exactly like the password path: without it a code
// (six digits, five attempts) would be a cheaper brute-force target than a
// password, and the attempt counter alone would let an attacker rotate
// sessions. A failure here counts toward the same account/IP budget, so a
// stolen mailbox does not reset the password-guessing counters either.
func (s *Service) loginWithCode(w http.ResponseWriter, r *http.Request, ident Identifier, code string) {
	ip := clientIP(r)
	lock, lockErr := s.loginLockState(r.Context(), ident, ip)
	if lockErr != nil {
		s.errorLog(r.Context(), "login_lock_check_failed: "+lockErr.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable))
		return
	}
	if lock.locked() {
		s.auditLogin(r.Context(), r, 0, "ANONYMOUS", "-", "REJECTED_BY_GUARD")
		mw.WriteError(w, r, s.lockError(lock))
		return
	}

	row, findErr := s.findUser(r.Context(), ident)
	if findErr != nil {
		if errors.Is(findErr, sql.ErrNoRows) {
			// Do not touch the code store: there is no code for an address that
			// was never registered, and answering differently here would turn
			// the login endpoint into an account oracle.
			s.recordLoginFailure(r.Context(), ident, ip)
			s.auditLogin(r.Context(), r, 0, "ANONYMOUS", "-", "REJECTED_BY_GUARD")
			mw.WriteError(w, r, apierr.New(apierr.CodeInvalidCredentials))
			return
		}
		s.errorLog(r.Context(), "login_lookup_failed: "+findErr.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable))
		return
	}

	if apiErr := s.consumeVerificationCode(r.Context(), ident, PurposeLogin, code); apiErr != nil {
		s.recordLoginFailure(r.Context(), ident, ip)
		s.auditLogin(r.Context(), r, row.uid, row.role, strconv.FormatInt(row.uid, 10), "REJECTED_BY_GUARD")
		mw.WriteError(w, r, apiErr)
		return
	}
	s.clearAccountFailures(r.Context(), ident)
	s.finishLogin(w, r, row, ident)
}

// finishLogin applies the account-status gate and issues the session. Shared by
// the password and code paths so a status rule cannot apply to only one of
// them — a disabled account must not be reachable by switching login method.
func (s *Service) finishLogin(w http.ResponseWriter, r *http.Request, row userRow, ident Identifier) {
	switch row.status {
	case "NORMAL":
	case "DISABLED":
		s.auditLogin(r.Context(), r, row.uid, row.role, strconv.FormatInt(row.uid, 10), "REJECTED_BY_GUARD")
		mw.WriteError(w, r, apierr.New(apierr.CodeAccountDisabled))
		return
	case "FROZEN_OVERDRAFT":
		s.auditLogin(r.Context(), r, row.uid, row.role, strconv.FormatInt(row.uid, 10), "REJECTED_BY_GUARD")
		mw.WriteError(w, r, apierr.New(apierr.CodeAccountFrozenDueToOverdraft))
		return
	default:
		s.auditLogin(r.Context(), r, row.uid, row.role, strconv.FormatInt(row.uid, 10), "REJECTED_BY_GUARD")
		mw.WriteError(w, r, apierr.New(apierr.CodeAccountDisabled))
		return
	}

	sess, cookie, err := s.Sessions.Issue(r.Context(), row.uid)
	if err != nil {
		s.errorLog(r.Context(), "login_session_issue_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable))
		return
	}
	s.clearAccountFailures(r.Context(), ident)
	s.Sessions.SetCookie(w, cookie)
	csrfToken := s.Sessions.SetCSRFCookie(w, sess.SID)
	s.auditLogin(r.Context(), r, row.uid, row.role, strconv.FormatInt(row.uid, 10), "OK")
	_ = mw.WriteJSON(w, http.StatusOK, loginResponse{
		User:      view(row.uid, row.email, row.phone, row.nickname, row.role, row.status),
		CSRFToken: csrfToken,
	})
}

// meResponse is CurrentUser plus the CSRF token. The token is additive: the
// existing CurrentUser consumer (web/src/api/types.ts) ignores unknown fields,
// and the readable cookie remains the primary source.
type meResponse struct {
	userView
	CSRFToken string `json:"csrf_token"`
}

// Me implements GET /api/v1/auth/me.
func (s *Service) Me(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	// Re-issue the readable cookie on every whoami, and repeat the token in the
	// body and X-CSRF-Token header so a SPA that hydrated from an older tab can
	// recover without a hard reload.
	token := s.Sessions.SetCSRFCookie(w, sess.SID)
	w.Header().Set(mw.CSRFHeaderName, token)
	_ = mw.WriteJSON(w, http.StatusOK, meResponse{
		userView:  view(sess.UID, sess.Email, sess.Phone, sess.Nickname, sess.Role, sess.Status),
		CSRFToken: token,
	})
}

// Logout implements POST /api/v1/auth/logout. Deleting K20 makes the old sid
// unusable immediately; the browser cookie is cleared as a convenience.
//
// A failed delete answers 503, not 204, and deliberately leaves the cookies in
// place: the session is still alive server-side, so clearing them would remove
// the only credential the client could retry with. Answering 204 here is what
// made a Redis stall look like a successful logout (G34).
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if ok {
		if err := s.Sessions.Revoke(r.Context(), sess.SID); err != nil {
			if s.Logger != nil {
				s.Logger.Warn(r.Context(), "logout_revoke_failed: "+err.Error(),
					obslog.Fields{Stage: "auth.logout"})
			}
			mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable).
				WithHint("退出登录未完成，请重试"))
			return
		}
	}
	s.Sessions.ClearCookie(w)
	s.Sessions.ClearCSRFCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

type userRow struct {
	uid      int64
	hash     string
	algo     string
	status   string
	role     string
	email    string
	phone    string
	nickname string
}

func (s *Service) findUser(ctx context.Context, ident Identifier) (userRow, error) {
	column := "email"
	if ident.Kind == KindPhone {
		column = "phone"
	}
	q := `SELECT id, password_hash, password_algo, status, role,
		COALESCE(email, ''), COALESCE(phone, ''), nickname
		FROM t_user
		WHERE deleted_at IS NULL AND ` + column + ` = ?`
	var row userRow
	err := s.DB.QueryRowContext(ctx, q, ident.Value).Scan(
		&row.uid, &row.hash, &row.algo, &row.status, &row.role,
		&row.email, &row.phone, &row.nickname,
	)
	if err != nil {
		return userRow{}, err
	}
	return row, nil
}

// auditLogin is best-effort: an audit outage must not stop a legitimate login,
// but it is logged so the gap is visible. State-changing endpoints
// (registration, key creation) write their audit row in the same transaction.
func (s *Service) auditLogin(ctx context.Context, r *http.Request, uid int64, role, target, result string) {
	if s.Audit == nil {
		return
	}
	if err := s.Audit.Insert(ctx, s.DB, audit.Entry{
		OperatorID:   uid,
		OperatorRole: role,
		OperatorIP:   audit.IPBytes(r),
		Module:       "USER",
		Action:       "LOGIN",
		TargetType:   "t_user",
		TargetID:     target,
		Result:       result,
	}); err != nil {
		s.warn(ctx, "login_audit_failed: "+err.Error())
	}
}

var (
	dummyOnce sync.Once
	dummyHash []byte
)

// dummyPasswordHash is a cost-12 hash used only to equalize the not-found path.
func dummyPasswordHash() []byte {
	dummyOnce.Do(func() {
		dummyHash, _ = bcrypt.GenerateFromPassword([]byte("yunshu-dummy-password"), bcryptCost)
	})
	return dummyHash
}

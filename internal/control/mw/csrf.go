package mw

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"yunshu.dev/core/internal/apierr"
)

// CSRF double-submit contract.
//
// The sid cookie is HttpOnly, so a cross-site page cannot read it — but it can
// still make the browser send it. SameSite=Lax blocks most cross-site POSTs and
// is a second line of defence, not a substitute: a same-site XSS or a browser
// that ignores the attribute still wins without a token.
//
// The token is HMAC(secret, "csrf|" + sid). It is:
//   - bound to the session, so it dies with K20 and cannot be replayed after
//     logout or a password-change revoke-all;
//   - readable by the console (the `csrf` cookie is deliberately not
//     HttpOnly), so the SPA can echo it in X-CSRF-Token;
//   - never accepted from a client that merely repeats a value it chose,
//     because the server recomputes the expected token from the sid.
const (
	// CSRFCookieName is the non-HttpOnly double-submit cookie.
	CSRFCookieName = "csrf"
	// CSRFHeaderName is the request header that must mirror the cookie.
	CSRFHeaderName = "X-CSRF-Token"
)

// CSRFToken derives the token for one session. The sid is already a 256-bit
// random value; HMAC keeps the token from leaking it and stops a client from
// choosing a token that matches a session it does not own.
func (m *SessionManager) CSRFToken(sid string) string {
	mac := hmac.New(sha256.New, m.Secret)
	mac.Write([]byte("csrf|"))
	mac.Write([]byte(sid))
	return hex.EncodeToString(mac.Sum(nil))
}

// SetCSRFCookie writes the readable cookie and returns the token so a JSON
// login/register response can carry the same value.
func (m *SessionManager) SetCSRFCookie(w http.ResponseWriter, sid string) string {
	token := m.CSRFToken(sid)
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     m.path(),
		HttpOnly: false,
		Secure:   m.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(m.absoluteTTL().Seconds()),
	})
	return token
}

// ClearCSRFCookie expires the readable cookie on logout.
func (m *SessionManager) ClearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    "",
		Path:     m.path(),
		HttpOnly: false,
		Secure:   m.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// CSRF checks the double-submit pair for session-authenticated mutations. It
// must run after the session middleware: the expected token is derived from
// the loaded sid, not from the cookie being checked.
//
// Safe methods and paths outside /api/ are not checked. Login/register and the
// payment callback are exempt: they have no console session (the callback is
// authenticated by its HMAC over the body), so a token derived from a sid
// either does not exist or would be a meaningless extra requirement on a
// public endpoint.
func (m *SessionManager) CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !csrfProtected(r) {
			next.ServeHTTP(w, r)
			return
		}
		sess, ok := SessionFrom(r.Context())
		if !ok || sess == nil {
			WriteError(w, r, SessionMissing())
			return
		}
		header := strings.TrimSpace(r.Header.Get(CSRFHeaderName))
		cookie, err := r.Cookie(CSRFCookieName)
		if err != nil || strings.TrimSpace(cookie.Value) == "" || header == "" {
			WriteError(w, r, csrfDenied())
			return
		}
		expected := m.CSRFToken(sess.SID)
		// Both comparisons are constant-time. Checking the pair against the
		// derived value (not just header == cookie) is what stops a caller from
		// inventing a matching pair the server never issued.
		if !hmac.Equal([]byte(header), []byte(cookie.Value)) ||
			!hmac.Equal([]byte(cookie.Value), []byte(expected)) {
			WriteError(w, r, csrfDenied())
			return
		}
		next.ServeHTTP(w, r)
	})
}

func csrfProtected(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	return !csrfExemptPath(r.URL.Path)
}

func csrfExemptPath(path string) bool {
	switch path {
	case "/api/v1/auth/login", "/api/v1/auth/register", "/api/v1/auth/verification-code":
		return true
	}
	return strings.HasPrefix(path, "/api/v1/pay/callback/")
}

// csrfDenied uses permission_denied (403), not invalid_request_body (400): the
// body is not what is wrong and editing it cannot fix the request. The missing
// proof is an authorization fact, and the client action is to refresh the page
// and send the current token.
func csrfDenied() *apierr.APIError {
	e := PermissionDenied()
	e.Message = "请求来源校验失败：缺少或错误的 CSRF token"
	e = e.WithHint("刷新页面重新获取 csrf cookie，并在写请求的 X-CSRF-Token 头中回传")
	return e
}

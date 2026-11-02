package control

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/control/ctltest"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

var testDB *sql.DB

func TestMain(m *testing.M) {
	db, err := ctltest.SetupDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "control tests: %v\n", err)
		os.Exit(1)
	}
	testDB = db
	code := redistest.Setup(m)
	_ = db.Close()
	os.Exit(code)
}

// issueSession returns the signed sid cookie value and the CSRF token bound to
// that session. Every request this package sends through the real mux must
// carry both; tests that inject mw.WithSession directly bypass the middleware
// chain and are unaffected.
func issueSession(t *testing.T, sessMgr *mw.SessionManager, uid int64) (cookieValue, csrfToken string) {
	t.Helper()
	sess, cookieValue, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	return cookieValue, sessMgr.CSRFToken(sess.SID)
}

// newAuthedRequest builds a request that satisfies Session + CSRF. The CSRF
// cookie is non-HttpOnly by design; the header carries the same value.
func newAuthedRequest(method, path string, body io.Reader, cookieValue, csrfToken string) *http.Request {
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookieValue})
	req.AddCookie(&http.Cookie{Name: mw.CSRFCookieName, Value: csrfToken})
	req.Header.Set(mw.CSRFHeaderName, csrfToken)
	return req
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), wantStatus)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != wantCode {
		t.Fatalf("code=%q, want %q", body.Error.Code, wantCode)
	}
}

// adminRoutes is the T15 admin surface. The test sends a PERSONAL session to
// every one of them; the middleware must answer 403 before any handler runs.
var adminRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/admin/whoami"},
	// T11 second segment: audit.read is OPS/FINANCE only; PERSONAL must be 403
	// before the query runs.
	{http.MethodGet, "/api/admin/audit-logs"},
	{http.MethodGet, "/api/admin/channels"},
	{http.MethodGet, "/api/admin/channels/1"},
	{http.MethodPost, "/api/admin/channels"},
	{http.MethodPatch, "/api/admin/channels/1"},
	{http.MethodPost, "/api/admin/channels/1/disable"},
	{http.MethodDelete, "/api/admin/channels/1"},
	{http.MethodGet, "/api/admin/models"},
	{http.MethodGet, "/api/admin/models/1"},
	{http.MethodPost, "/api/admin/models"},
	{http.MethodPatch, "/api/admin/models/1"},
	{http.MethodPost, "/api/admin/models/1/listing-state"},
	{http.MethodPost, "/api/admin/models/1/recommend-weight"},
	{http.MethodGet, "/api/admin/models/1/aliases"},
	{http.MethodPost, "/api/admin/models/1/aliases"},
	{http.MethodDelete, "/api/admin/models/1/aliases/qwen-max"},
	{http.MethodGet, "/api/admin/prices"},
	{http.MethodPost, "/api/admin/prices"},
	{http.MethodPatch, "/api/admin/prices/1"},
	{http.MethodGet, "/api/admin/groups"},
	{http.MethodPatch, "/api/admin/groups/1"},
	{http.MethodGet, "/api/admin/announcements"},
	{http.MethodPost, "/api/admin/announcements"},
	{http.MethodPatch, "/api/admin/announcements/1"},
	{http.MethodPost, "/api/admin/announcements/1/publish"},
	{http.MethodGet, "/api/admin/risk/rules"},
	{http.MethodPost, "/api/admin/risk/rules"},
	{http.MethodPatch, "/api/admin/risk/rules/1"},
	{http.MethodDelete, "/api/admin/risk/rules/1"},
	{http.MethodGet, "/api/admin/risk/hits"},
	{http.MethodPost, "/api/admin/risk/users/1/score"},
	{http.MethodGet, "/api/admin/users"},
	{http.MethodGet, "/api/admin/users/1"},
	{http.MethodPost, "/api/admin/users/1/disable"},
	{http.MethodPost, "/api/admin/users/1/enable"},
	{http.MethodGet, "/api/admin/config"},
	{http.MethodPut, "/api/admin/config"},
	// T16 ticket centre: the operator queue is ticket.reply, which PERSONAL
	// never holds.
	{http.MethodGet, "/api/admin/tickets"},
	{http.MethodPost, "/api/admin/tickets/1/reply"},
	{http.MethodPost, "/api/admin/tickets/1/state"},
	// T16 review centre: fund.approve is FINANCE/SUPER_ADMIN only.
	{http.MethodGet, "/api/admin/reviews"},
	{http.MethodPost, "/api/admin/reviews/1/approve"},
	{http.MethodPost, "/api/admin/reviews/1/reject"},
	// Operator fund surface: the pending-order queue, the manual payment
	// confirmation and redeem-code issuance. fund.read / fund.approve are
	// FINANCE and SUPER_ADMIN only.
	{http.MethodGet, "/api/admin/orders"},
	{http.MethodPost, "/api/admin/orders/1/confirm-payment"},
	{http.MethodPost, "/api/admin/orders/1/close"},
	{http.MethodGet, "/api/admin/redeem-codes"},
	{http.MethodPost, "/api/admin/redeem-codes/batch"},
}

func TestPersonalAccountGets403OnEveryAdminRoute(t *testing.T) {
	rdb := redisx.New(redisx.Options{Addr: redistest.Addr(), DB: redistest.DB(), PoolSize: 5})
	t.Cleanup(func() { _ = rdb.Close() })
	sessMgr, err := mw.NewSessionManager(testDB, rdb, "0123456789abcdef")
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	srv, err := New(Deps{DB: testDB, Redis: rdb, Sessions: sessMgr, Now: clock.Real{}})
	if err != nil {
		t.Fatalf("control server: %v", err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)

	uid := newRoleUser(t, "PERSONAL")
	cookieValue, csrfToken := issueSession(t, sessMgr, uid)

	for _, route := range adminRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := newAuthedRequest(route.method, route.path, bytes.NewReader(nil), cookieValue, csrfToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assertErrorCode(t, rec, http.StatusForbidden, "permission_denied")
		})
	}
}

// Positive control: OPS holds model.read, so the channel list is 200; a
// PERSONAL account is 403 on the same route. Without this pair, the deny test
// above could pass because the whole route table is broken.
func TestOpsCanReadChannels(t *testing.T) {
	rdb := redisx.New(redisx.Options{Addr: redistest.Addr(), DB: redistest.DB(), PoolSize: 5})
	t.Cleanup(func() { _ = rdb.Close() })
	sessMgr, err := mw.NewSessionManager(testDB, rdb, "0123456789abcdef")
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	srv, err := New(Deps{DB: testDB, Redis: rdb, Sessions: sessMgr, Now: clock.Real{}})
	if err != nil {
		t.Fatalf("control server: %v", err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)

	uid := newRoleUser(t, "OPS")
	cookieValue, csrfToken := issueSession(t, sessMgr, uid)
	req := newAuthedRequest(http.MethodGet, "/api/admin/channels", nil, cookieValue, csrfToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("OPS channel list status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	// audit.read is the same role's permission on the audit reader.
	req = newAuthedRequest(http.MethodGet, "/api/admin/audit-logs?module=USER", nil, cookieValue, csrfToken)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("OPS audit list status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	t.Logf("OPS GET /api/admin/audit-logs?module=USER -> %d", rec.Code)
}

// The ticket consoles are split: OPS answers through ticket.reply but must not
// open the account-scoped console routes. Request-log reads are account-scoped
// for the same reason: log.read is PERSONAL/SUPER_ADMIN only. Ids in the path
// are irrelevant because Require rejects before the handler runs.
func TestOpsCannotReachAccountConsole(t *testing.T) {
	rdb := redisx.New(redisx.Options{Addr: redistest.Addr(), DB: redistest.DB(), PoolSize: 5})
	t.Cleanup(func() { _ = rdb.Close() })
	sessMgr, err := mw.NewSessionManager(testDB, rdb, "0123456789abcdef")
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	srv, err := New(Deps{DB: testDB, Redis: rdb, Sessions: sessMgr, Now: clock.Real{}})
	if err != nil {
		t.Fatalf("control server: %v", err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)

	uid := newRoleUser(t, "OPS")
	cookieValue, csrfToken := issueSession(t, sessMgr, uid)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/console/tickets"},
		{http.MethodGet, "/api/console/tickets"},
		{http.MethodGet, "/api/console/tickets/1"},
		{http.MethodPost, "/api/console/tickets/1/close"},
		{http.MethodGet, "/api/console/request-ids"},
		{http.MethodGet, "/api/console/logs"},
		{http.MethodGet, "/api/console/logs/1"},
		{http.MethodGet, "/api/console/logs/1/body"},
		{http.MethodGet, "/api/console/usage"},
		{http.MethodGet, "/api/console/bills"},
		{http.MethodGet, "/api/console/usage/metrics"},
		{http.MethodPost, "/api/console/usage/export"},
		{http.MethodGet, "/api/console/exports/not-a-token"},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := newAuthedRequest(route.method, route.path, bytes.NewReader(nil), cookieValue, csrfToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assertErrorCode(t, rec, http.StatusForbidden, "permission_denied")
		})
	}
}

// Positive control: the same route table answers a PERSONAL session on the
// console tickets and logs lists (the handlers then scope by uid). Without
// this pair the 403 test above could pass because the whole surface is broken.
func TestPersonalCanReachAccountConsole(t *testing.T) {
	rdb := redisx.New(redisx.Options{Addr: redistest.Addr(), DB: redistest.DB(), PoolSize: 5})
	t.Cleanup(func() { _ = rdb.Close() })
	sessMgr, err := mw.NewSessionManager(testDB, rdb, "0123456789abcdef")
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	srv, err := New(Deps{DB: testDB, Redis: rdb, Sessions: sessMgr, Now: clock.Real{}})
	if err != nil {
		t.Fatalf("control server: %v", err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)

	uid := newRoleUser(t, "PERSONAL")
	cookieValue, csrfToken := issueSession(t, sessMgr, uid)
	for _, path := range []string{
		"/api/console/tickets", "/api/console/logs",
		"/api/console/usage", "/api/console/bills", "/api/console/usage/metrics",
	} {
		req := newAuthedRequest(http.MethodGet, path, nil, cookieValue, csrfToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PERSONAL %s status=%d body=%s, want 200", path, rec.Code, rec.Body.String())
		}
	}
}

// accountOperatorRoutes are the two operator read surfaces that leaked before
// the read permissions were split by resource scope: as long as FINANCE held
// admin.read and PERSONAL held user.read, the mux answered the whole account
// directory (including each row's disable reason) and every upstream channel
// (base_url, key mask). The list routes answer 200 for an authorised role; the
// detail routes answer 200 or 404 depending on whether the row exists, so the
// tests below assert "not 403" for those.
var accountOperatorRoutes = []struct {
	method string
	path   string
	isList bool
}{
	{http.MethodGet, "/api/admin/users", true},
	{http.MethodGet, "/api/admin/users/1", false},
	{http.MethodGet, "/api/admin/channels", true},
	{http.MethodGet, "/api/admin/channels/1", false},
}

// FINANCE keeps the operator overview and the model cost sheet, but the account
// directory and the upstream channels are not part of the money surface. The
// overview assertion is the positive control: it proves the session, cookie and
// CSRF pair are fine, so the 403s below are the permission layer speaking.
func TestFinanceCannotReadAccountsOrChannels(t *testing.T) {
	mux, sessMgr := newRouteMux(t)
	uid := newRoleUser(t, "FINANCE")
	cookieValue, csrfToken := issueSession(t, sessMgr, uid)

	for _, route := range accountOperatorRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := newAuthedRequest(route.method, route.path, nil, cookieValue, csrfToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assertErrorCode(t, rec, http.StatusForbidden, "permission_denied")
		})
	}

	req := newAuthedRequest(http.MethodGet, "/api/admin/overview", nil, cookieValue, csrfToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("FINANCE GET /api/admin/overview status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	t.Logf("FINANCE: users/channels -> 403, overview -> %d", rec.Code)
}

// OPS and SUPER_ADMIN are the two roles that own these reads, so the same
// routes must answer them. Without this pair a typo that gave the routes a
// permission nobody holds would make the deny tests above pass vacuously.
func TestOpsAndSuperAdminCanReadAccountsAndChannels(t *testing.T) {
	for _, role := range []string{"OPS", "SUPER_ADMIN"} {
		t.Run(role, func(t *testing.T) {
			mux, sessMgr := newRouteMux(t)
			uid := newRoleUser(t, role)
			cookieValue, csrfToken := issueSession(t, sessMgr, uid)

			for _, route := range accountOperatorRoutes {
				req := newAuthedRequest(route.method, route.path, nil, cookieValue, csrfToken)
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if route.isList {
					if rec.Code != http.StatusOK {
						t.Fatalf("%s GET %s status=%d body=%s, want 200",
							role, route.path, rec.Code, rec.Body.String())
					}
					continue
				}
				// A missing row is 404 (the handler ran); a permission
				// failure would be 403 before the handler.
				if rec.Code == http.StatusForbidden {
					t.Fatalf("%s GET %s status=403 body=%s, want the handler to run",
						role, route.path, rec.Body.String())
				}
			}
			t.Logf("%s: users/channels lists -> 200, details -> handler ran", role)
		})
	}
}

// The account console overview is account.read: PERSONAL (its owner) and
// SUPER_ADMIN (support diagnostics) must reach it, FINANCE must not. FINANCE is
// the operator role that does not even have a console route, and OPS is the
// same; keeping them out means the account scope never silently widens back to
// a shared name.
func TestAccountOverviewPermissionSplit(t *testing.T) {
	cases := []struct {
		role string
		want int
		code string // expected error code when want != 200
	}{
		{"PERSONAL", http.StatusOK, ""},
		{"SUPER_ADMIN", http.StatusOK, ""},
		{"FINANCE", http.StatusForbidden, "permission_denied"},
		{"OPS", http.StatusForbidden, "permission_denied"},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			mux, sessMgr := newRouteMux(t)
			uid := newRoleUser(t, tc.role)
			cookieValue, csrfToken := issueSession(t, sessMgr, uid)

			req := newAuthedRequest(http.MethodGet, "/api/console/overview", nil, cookieValue, csrfToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if tc.code != "" {
				assertErrorCode(t, rec, tc.want, tc.code)
				return
			}
			if rec.Code != tc.want {
				t.Fatalf("%s GET /api/console/overview status=%d body=%s, want %d",
					tc.role, rec.Code, rec.Body.String(), tc.want)
			}
			t.Logf("%s GET /api/console/overview -> %d", tc.role, rec.Code)
		})
	}
}

// No cookie at all: the split must not have turned an authentication check into
// an authorisation check. Require runs after Session, so a missing session is
// still 401 session_expired rather than 403.
func TestAccountAndChannelReadRequireSession(t *testing.T) {
	mux, _ := newRouteMux(t)
	for _, path := range []string{
		"/api/admin/users", "/api/admin/users/1",
		"/api/admin/channels", "/api/admin/channels/1",
		"/api/console/overview",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			assertErrorCode(t, rec, http.StatusUnauthorized, "session_expired")
		})
	}
}

// CSRF double submit: a valid session alone is not enough for a mutation.
// There is no "skip CSRF in tests" switch — the same middleware runs here.
func TestSessionMutationRequiresCSRF(t *testing.T) {
	rdb := redisx.New(redisx.Options{Addr: redistest.Addr(), DB: redistest.DB(), PoolSize: 5})
	t.Cleanup(func() { _ = rdb.Close() })
	sessMgr, err := mw.NewSessionManager(testDB, rdb, "0123456789abcdef")
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	srv, err := New(Deps{DB: testDB, Redis: rdb, Sessions: sessMgr, Now: clock.Real{}})
	if err != nil {
		t.Fatalf("control server: %v", err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)

	uid := newRoleUser(t, "PERSONAL")
	cookieValue, csrfToken := issueSession(t, sessMgr, uid)

	// Safe method: no token required.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookieValue})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me without CSRF status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var me struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode /me: %v", err)
	}
	if me.CSRFToken == "" || me.CSRFToken != csrfToken {
		t.Fatalf("GET /me csrf_token=%q, want the session token", me.CSRFToken)
	}
	if got := rec.Header().Get(mw.CSRFHeaderName); got != csrfToken {
		t.Fatalf("GET /me X-CSRF-Token=%q, want the session token", got)
	}

	// Reverse case: valid session, no CSRF header or cookie.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookieValue})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assertErrorCode(t, rec, http.StatusForbidden, "permission_denied")
	t.Logf("valid sid, no CSRF header/cookie -> %d permission_denied", rec.Code)

	// Matching header/cookie pair that the server never issued: the token is
	// checked against HMAC(secret, sid), not only against itself.
	req = newAuthedRequest(http.MethodPost, "/api/v1/auth/logout", nil, cookieValue, "deadbeef")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assertErrorCode(t, rec, http.StatusForbidden, "permission_denied")
	t.Logf("valid sid, wrong CSRF token -> %d permission_denied", rec.Code)

	// A token issued for another session is also rejected: the expected value
	// is HMAC(secret, sid), not merely "header equals cookie".
	_, otherToken := issueSession(t, sessMgr, uid)
	req = newAuthedRequest(http.MethodPost, "/api/v1/auth/logout", nil, cookieValue, otherToken)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assertErrorCode(t, rec, http.StatusForbidden, "permission_denied")
	t.Logf("token from another session -> %d permission_denied", rec.Code)

	// Public login is exempt by construction (it is not inside s.authed): the
	// request reaches the handler and fails on the empty body, not on CSRF.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assertErrorCode(t, rec, http.StatusBadRequest, "invalid_request_body")

	// Correct pair succeeds.
	req = newAuthedRequest(http.MethodPost, "/api/v1/auth/logout", nil, cookieValue, csrfToken)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout with CSRF status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}

	// Replaying the old token after logout: K20 is gone, so Load answers 401
	// before the CSRF comparison runs. The token is dead with the session.
	req = newAuthedRequest(http.MethodPost, "/api/v1/auth/logout", nil, cookieValue, csrfToken)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assertErrorCode(t, rec, http.StatusUnauthorized, "session_expired")
	t.Logf("old CSRF token after logout -> %d session_expired", rec.Code)
}

// The website reads these five routes without a session or CSRF token. The
// positive case is the point: the deny tests above would still pass if the
// whole mux were broken, and a site route accidentally wired through s.authed
// would answer 401 here.
func TestPublicSiteRoutesNeedNoSession(t *testing.T) {
	rdb := redisx.New(redisx.Options{Addr: redistest.Addr(), DB: redistest.DB(), PoolSize: 5})
	t.Cleanup(func() { _ = rdb.Close() })
	sessMgr, err := mw.NewSessionManager(testDB, rdb, "0123456789abcdef")
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	srv, err := New(Deps{DB: testDB, Redis: rdb, Sessions: sessMgr, Now: clock.Real{}})
	if err != nil {
		t.Fatalf("control server: %v", err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)

	for _, path := range []string{
		"/api/public/plans",
		"/api/public/models",
		"/api/public/announcements",
		"/api/public/endpoints",
		"/api/public/integration-links",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil) // no cookies at all
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
				t.Fatalf("Cache-Control=%q, want public, max-age=60", got)
			}
		})
	}
}

var authzUserSeq int64

func newRoleUser(t *testing.T, role string) int64 {
	t.Helper()
	now := time.Now()
	authzUserSeq++
	// t_user.user_no is char(12): A + yyyymmdd + 3 digits.
	userNo := fmt.Sprintf("A%s%03d", now.Format("20060102"),
		(now.UnixNano()/1_000_000+authzUserSeq)%1000)
	email := fmt.Sprintf("t15-authz-%d@test.local", now.UnixNano())
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, email, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at)
		VALUES (?,?,'not-a-real-hash','bcrypt',?,'t15-authz',?,'NORMAL',?,?)`,
		userNo, email, now, role, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "uk_user_no") {
			return newRoleUser(t, role)
		}
		t.Fatalf("insert user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

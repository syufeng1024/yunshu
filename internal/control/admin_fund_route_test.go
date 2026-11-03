package control

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/platform/clock"
)

// newRouteMux builds the real control mux so these tests exercise the
// permission middleware, not the handlers in isolation.
func newRouteMux(t *testing.T) (*http.ServeMux, *mw.SessionManager) {
	t.Helper()
	rdb := newTestRedis(t)
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
	return mux, sessMgr
}

// The operator fund surface is money: OPS holds neither fund.read nor
// fund.approve, so recording a payment or issuing credit must be refused
// before the handler runs.
func TestOpsCannotReachOperatorFundRoutes(t *testing.T) {
	mux, sessMgr := newRouteMux(t)
	uid := newRoleUser(t, "OPS")
	cookieValue, csrfToken := issueSession(t, sessMgr, uid)

	routes := []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodGet, "/api/admin/orders", nil},
		{http.MethodPost, "/api/admin/orders/1/confirm-payment", []byte(`{"bank_txn_no":"20261030001"}`)},
		{http.MethodPost, "/api/admin/orders/1/close", []byte(`{"reason":"客户未转账"}`)},
		{http.MethodGet, "/api/admin/redeem-codes", nil},
		{http.MethodPost, "/api/admin/redeem-codes/batch", []byte(`{"batch_name":"x","face_value_fen":1000,"quantity":1,"valid_days":30}`)},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := newAuthedRequest(route.method, route.path, bytes.NewReader(route.body), cookieValue, csrfToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assertErrorCode(t, rec, http.StatusForbidden, "permission_denied")
		})
	}
}

// Positive control: FINANCE reaches all four routes. The confirm and generate
// calls are refused for their own reasons (no such order / no body), which is
// exactly the proof that the permission layer let them through.
func TestFinanceCanReachOperatorFundRoutes(t *testing.T) {
	mux, sessMgr := newRouteMux(t)
	uid := newRoleUser(t, "FINANCE")
	cookieValue, csrfToken := issueSession(t, sessMgr, uid)

	// Reads answer 200 with an empty or full page.
	for _, path := range []string{"/api/admin/orders", "/api/admin/redeem-codes"} {
		req := newAuthedRequest(http.MethodGet, path, nil, cookieValue, csrfToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("FINANCE GET %s status=%d body=%s, want 200", path, rec.Code, rec.Body.String())
		}
	}

	// A missing order is 404, which can only be reached past the permission
	// check.
	req := newAuthedRequest(http.MethodPost, "/api/admin/orders/99999999/confirm-payment",
		bytes.NewReader([]byte(`{"bank_txn_no":"20261030001"}`)), cookieValue, csrfToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assertErrorCode(t, rec, http.StatusNotFound, "resource_not_found")

	// An empty body is a 400 from the handler, again past the permission gate.
	req = newAuthedRequest(http.MethodPost, "/api/admin/redeem-codes/batch",
		bytes.NewReader(nil), cookieValue, csrfToken)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assertErrorCode(t, rec, http.StatusBadRequest, "invalid_request_body")

	// The close path is gated the same way: a missing order is 404, not 403.
	req = newAuthedRequest(http.MethodPost, "/api/admin/orders/99999999/close",
		bytes.NewReader([]byte(`{"reason":"客户未转账"}`)), cookieValue, csrfToken)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assertErrorCode(t, rec, http.StatusNotFound, "resource_not_found")
}

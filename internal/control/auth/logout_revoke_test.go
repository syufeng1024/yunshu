package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/redisx"
)

// A logout whose DEL was not acknowledged must not answer 204. The old code
// shared one 50ms budget between the uid GET and the pipeline DEL, discarded
// pipe.Exec's error and always answered 204, so a Redis stall produced "the
// client thinks it logged out, the session key is still alive" (G34).
//
// The bad client points at a closed port: every command fails immediately, so
// this exercises the failure branch rather than the slow one.
func TestLogoutRevokeFailureIs503AndKeepsCookies(t *testing.T) {
	bad := redisx.New(redisx.Options{
		Addr:           "127.0.0.1:1",
		CommandTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = bad.Close() })

	sessions, err := mw.NewSessionManager(testDB, bad, "test-session-secret-0123456789")
	if err != nil {
		t.Fatalf("new session manager: %v", err)
	}

	// Direct seam: Revoke must report the lost delete instead of swallowing it.
	if err := sessions.Revoke(context.Background(), "sid-revoke-failure"); err == nil {
		t.Fatalf("Revoke with an unreachable Redis returned nil, want an error")
	}

	svc := &Service{Sessions: sessions}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req = req.WithContext(mw.WithSession(req.Context(), &mw.Session{
		SID: "sid-revoke-failure", UID: 1, Role: "PERSONAL", Status: "NORMAL",
	}))
	rec := httptest.NewRecorder()
	svc.Logout(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("logout status=%d body=%s, want 503 (the session was not deleted)",
			rec.Code, rec.Body.String())
	}
	// The cookies are the client's only retry credential while the session is
	// alive server-side; clearing them would strand the user.
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("logout cleared cookies (%q) even though the session is alive", got)
	}
}

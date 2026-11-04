package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/jsonx"
)

// This file covers username identifiers end to end: the normalization branch
// that produces KindUsername, the column switch in findUser, and the four
// call sites that must refuse a username because they need a deliverable
// address.

var usernameSeq int64

// uniqueUsername returns a short valid username, e.g. "u1732000000123456789".
// Usernames are lowercase ASCII only (^[a-z][a-z0-9._-]{2,31}$), so the
// nanosecond clock plus a counter is the cheapest source of a unique value.
func uniqueUsername() string {
	n := atomic.AddInt64(&usernameSeq, 1)
	return fmt.Sprintf("u%d%03d", time.Now().UnixNano(), n%1000)
}

// newAuthUsernameUser inserts a NORMAL PERSONAL account whose only identifier
// is a username — the shape the startup bootstrap creates for the built-in
// SUPER_ADMIN.
func newAuthUsernameUser(t *testing.T, username, role string) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcryptCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UTC()
	userNo := fmt.Sprintf("U%011d", now.UnixNano()%100000000000)
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, username, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at)
		VALUES (?,?,?,'bcrypt',?,'t11-username',?,'NORMAL',?,?)`,
		userNo, username, string(hash), now, role, now, now)
	if err != nil {
		t.Fatalf("insert username user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx,
			`DELETE FROM t_admin_audit_log WHERE operator_id = ? OR target_id = ?`, uid, fmt.Sprint(uid))
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_credit_account WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

// TestLoginByUsername asserts the full 归一化 → 列查询 → bcrypt → session chain
// for a username account, including the uppercase form: the row is stored
// lowercase, so "ADMIN"-style input only works if NormalizeIdentifier lowers it
// before the WHERE clause.
func TestLoginByUsername(t *testing.T) {
	svc, _, _ := newAuthService(t)
	username := uniqueUsername()
	uid := newAuthUsernameUser(t, username, RoleSuperAdmin)

	for _, raw := range []string{username, strings.ToUpper(username), " " + username + " "} {
		ip := uniqueIP()
		rec := doLogin(t, svc, raw, testPassword, ip)
		if rec.Code != http.StatusOK {
			t.Fatalf("login as %q: status=%d body=%s, want 200", raw, rec.Code, rec.Body.String())
		}
		var body struct {
			User struct {
				ID          string `json:"id"`
				Email       string `json:"email"`
				DisplayName string `json:"display_name"`
				Role        string `json:"role"`
				Status      string `json:"status"`
			} `json:"user"`
			CSRFToken string `json:"csrf_token"`
		}
		if err := jsonx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode login body: %v", err)
		}
		if body.User.Role != RoleSuperAdmin {
			t.Fatalf("%q: role = %q, want %q", raw, body.User.Role, RoleSuperAdmin)
		}
		if body.User.ID != fmt.Sprint(uid) {
			t.Fatalf("%q: id = %q, want %d", raw, body.User.ID, uid)
		}
		// A username account has no email; the P0 contract returns an empty
		// string rather than failing the scan on a NULL column.
		if body.User.Email != "" {
			t.Fatalf("%q: email = %q, want empty", raw, body.User.Email)
		}
		if body.CSRFToken == "" {
			t.Fatalf("%q: csrf_token is empty", raw)
		}
	}
}

// TestLoginByUsernameWrongPasswordAndUnknown pins the two failure answers: both
// are 401 invalid_credentials, and neither leaks "account_exists" or a
// not-found code that would turn the login form into an account oracle.
func TestLoginByUsernameWrongPasswordAndUnknown(t *testing.T) {
	svc, _, _ := newAuthService(t)
	username := uniqueUsername()
	newAuthUsernameUser(t, username, RoleSuperAdmin)

	cases := []struct {
		name       string
		identifier string
		password   string
	}{
		{"wrong-password", username, "WrongPassword9"},
		{"unknown-username", uniqueUsername() + "zz", testPassword},
		{"not-a-valid-username", "_nope", testPassword},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doLogin(t, svc, tc.identifier, tc.password, uniqueIP())
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s, want 401", rec.Code, rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.Error.Code != apierr.CodeInvalidCredentials {
				t.Fatalf("code=%q, want %q", body.Error.Code, apierr.CodeInvalidCredentials)
			}
		})
	}
}

// TestLoginByUsernameLockApplies proves the account lock was not exempted for
// the new kind: the lock key is derived from the normalized value, so it must
// count failures exactly like an email.
func TestLoginByUsernameLockApplies(t *testing.T) {
	svc, _, _ := newAuthService(t)
	username := uniqueUsername()
	newAuthUsernameUser(t, username, RoleSuperAdmin)
	ip := uniqueIP()

	for i := 0; i < loginAccountFailLimit; i++ {
		rec := doLogin(t, svc, username, "WrongPassword9", ip)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status=%d body=%s, want 401", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := doLogin(t, svc, username, testPassword, ip)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked login status=%d body=%s, want 429", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != apierr.CodeLoginLocked {
		t.Fatalf("code=%q, want %q", body.Error.Code, apierr.CodeLoginLocked)
	}
}

// TestRegisterRejectsUsername: registration proves mailbox ownership, and a
// username has no mailbox. 400 invalid_request_body naming the field, and
// nothing is written.
func TestRegisterRejectsUsername(t *testing.T) {
	svc, _, _ := newAuthService(t)
	payload, err := jsonx.Marshal(map[string]string{
		"identifier": "newuser",
		"password":   testPassword,
	})
	if err != nil {
		t.Fatalf("marshal register body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(string(payload)))
	req.RemoteAddr = uniqueIP() + ":12345"
	rec := httptest.NewRecorder()
	svc.Register(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != apierr.CodeInvalidRequestBody {
		t.Fatalf("code=%q, want %q", body.Error.Code, apierr.CodeInvalidRequestBody)
	}
	if !strings.Contains(body.Error.Message, "identifier") {
		t.Fatalf("message=%q, want it to name the identifier field", body.Error.Message)
	}

	var n int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE username = ?`, "newuser").Scan(&n); err != nil {
		t.Fatalf("count username rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("a rejected registration created %d t_user rows", n)
	}
}

// TestSendVerificationCodeRejectsUsername: there is no address to deliver to,
// so the answer is a malformed body, not "verification is not enabled".
func TestSendVerificationCodeRejectsUsername(t *testing.T) {
	svc, _, _ := newAuthService(t)
	payload, err := jsonx.Marshal(map[string]string{
		"identifier": "opszhang",
		"purpose":    "login",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/verification-code", strings.NewReader(string(payload)))
	req.RemoteAddr = uniqueIP() + ":12345"
	rec := httptest.NewRecorder()
	svc.SendVerificationCode(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != apierr.CodeInvalidRequestBody {
		t.Fatalf("code=%q, want %q", body.Error.Code, apierr.CodeInvalidRequestBody)
	}
	if !strings.Contains(body.Error.Message, "identifier") {
		t.Fatalf("message=%q, want it to name the identifier field", body.Error.Message)
	}
}

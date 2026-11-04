package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/control/ticket"
	"yunshu.dev/core/internal/redisx"
)

func newCancelService(t *testing.T) (*Service, *mw.SessionManager, *redisx.Client) {
	t.Helper()
	svc, mgr, rdb := newAuthService(t)
	tickets := ticket.New(testDB, svc.Audit, svc.Now, nil)
	svc.CancellableCheck = tickets.CheckAccountCancellable
	return svc, mgr, rdb
}

func seedTicket(t *testing.T, uid int64, state string) string {
	t.Helper()
	now := time.Now().UTC()
	ticketNo := fmt.Sprintf("TK%s%04d", now.Format("20060102150405"), now.UnixNano()%10000)
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_ticket
			(ticket_no, user_id, ticket_type, title, description, state,
			 first_response_due_at, created_at, updated_at)
		VALUES (?,?, 'OTHER', '注销拦截', 'leave this ticket open', ?, ?, ?, ?)`,
		ticketNo, uid, state, now.Add(4*time.Hour), now, now)
	if err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("ticket id: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_ticket_reply WHERE ticket_id = ?`, id)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_ticket WHERE id = ?`, id)
	})
	return ticketNo
}

func runCancel(svc *Service, sess *mw.Session, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/cancel", nil)
	req.RemoteAddr = "198.51.100.77:12345"
	req = req.WithContext(mw.WithSession(req.Context(), sess))
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	svc.Cancel(rec, req)
	return rec
}

// A13 first half: an open (RESPONDED or OPEN) ticket blocks cancellation and
// the refusal lists every blocking ticket number.
func TestAuthCancelBlockedByOpenTicket(t *testing.T) {
	svc, mgr, _ := newCancelService(t)
	uid := newAuthUser(t, uniqueEmail())
	ticketNo := seedTicket(t, uid, ticket.StateResponded)

	sess, _, err := mgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	rec := runCancel(svc, sess, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel with open ticket status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != apierr.CodeResourceStateConflict {
		t.Fatalf("code=%q, want %s", body.Error.Code, apierr.CodeResourceStateConflict)
	}
	if !strings.Contains(body.Error.Message, ticketNo) {
		t.Fatalf("message=%q does not list ticket %s", body.Error.Message, ticketNo)
	}

	var status string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT status FROM t_user WHERE id = ?`, uid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "NORMAL" {
		t.Fatalf("status=%s after blocked cancellation, want NORMAL", status)
	}
	t.Logf("A13 blocked: 409 %s; message=%q; account still %s",
		body.Error.Code, body.Error.Message, status)
}

// A13 second half: a successful cancellation disables the account, deletes
// every K20/K30 key and makes the old sid fail immediately.
func TestAuthCancelRevokesSession(t *testing.T) {
	svc, mgr, rdb := newCancelService(t)
	uid := newAuthUser(t, uniqueEmail())

	sess, cookie, err := mgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	// The sid is valid before cancellation.
	probe := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	probe.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookie})
	if loaded, apiErr := mgr.Load(context.Background(), probe); apiErr != nil || loaded.UID != uid {
		t.Fatalf("precondition: load uid=%d err=%v", loaded.UID, apiErr)
	}

	rec := runCancel(svc, sess, cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}

	var status, source string
	var cancelAt time.Time
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT status, disable_source, cancel_apply_at
		  FROM t_user WHERE id = ?`, uid).Scan(&status, &source, &cancelAt); err != nil {
		t.Fatal(err)
	}
	if status != "DISABLED" || source != cancelDisableSource || cancelAt.IsZero() {
		t.Fatalf("user after cancel: status=%s source=%s cancel_at=%v", status, source, cancelAt)
	}

	ctx := context.Background()
	if n, err := rdb.Raw().Exists(ctx, redisx.AdminSessionKey(sess.SID)).Result(); err != nil || n != 0 {
		t.Fatalf("K20 still exists=%d err=%v", n, err)
	}
	if n, err := rdb.Raw().Exists(ctx, redisx.AdminUserSessionSetKey(uid)).Result(); err != nil || n != 0 {
		t.Fatalf("K30 still exists=%d err=%v", n, err)
	}

	// The old cookie can no longer load a session.
	after := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	after.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookie})
	if _, apiErr := mgr.Load(ctx, after); apiErr == nil {
		t.Fatal("old sid still loads after cancellation")
	} else if apiErr.Code != apierr.CodeSessionExpired {
		t.Fatalf("old sid code=%q, want %s", apiErr.Code, apierr.CodeSessionExpired)
	}

	var audits int64
	if err := testDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM t_admin_audit_log
		 WHERE module = 'USER' AND action = 'CANCEL' AND target_type = 't_user'
		   AND target_id = ? AND result = 'OK'`, fmt.Sprint(uid)).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("CANCEL audit rows=%d, want 1", audits)
	}
	t.Logf("A13 revoke: status=%s source=%s; K20/K30 deleted; old sid -> %s; audit=%d",
		status, source, apierr.CodeSessionExpired, audits)
}

// T11 (red line): cancellation changes the account's status and nothing about
// its phone columns.
//
// phone_hmac staying is the whole design of migration 0011: it is the stable
// de-duplication key the trial check compares, and it is what keeps the number
// claimed by uk_user_phone_hmac (product decision D1: a cancelled account keeps
// its number until an operator releases it on purpose). A future F-12 PII job
// may null `phone`; touching `phone_hmac` would be a defect, and the assertion
// below is what would catch it.
func TestCancelKeepsPhoneAndHMAC(t *testing.T) {
	svc, mgr, _ := newCancelService(t)
	phone := uniqueRegisterPhone(t)
	mac, err := PhoneHMAC(phoneFixtureKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	uid := insertPhoneUser(t, phone, mac)
	cleanupPhone(t, phone)

	sess, _, err := mgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	if rec := runCancel(svc, sess, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}

	var gotPhone, gotHMAC, status string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone, ''), COALESCE(phone_hmac, ''), status FROM t_user WHERE id = ?`,
		uid).Scan(&gotPhone, &gotHMAC, &status); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if status != "DISABLED" {
		t.Fatalf("status = %q, want DISABLED", status)
	}
	if gotPhone != phone {
		t.Fatalf("phone after cancel = %q, want %q (cancellation must not clear it)", gotPhone, phone)
	}
	if gotHMAC != mac {
		t.Fatalf("phone_hmac after cancel = %q, want %q (the de-duplication key must survive)", gotHMAC, mac)
	}

	// The number is still occupied: a second account cannot take it.
	svc2, _, _ := newAuthService(t)
	svc2.PhoneHMACKey = phoneFixtureKey
	svc2.VerificationMode = VerificationOff
	dup := callRegister(t, svc2, map[string]string{
		"identifier": phone, "password": "Str0ng-Passw0rd!",
	})
	if dup.Code != http.StatusConflict {
		t.Fatalf("re-registering a cancelled number status=%d body=%s, want 409 (D1: 注销即占号)",
			dup.Code, dup.Body.String())
	}
}

// T12: after a PII clearing pass sets phone = NULL, the hash is still there and
// still blocks the number. This is the state the trial check must survive, and
// the reason `phone` and `phone_hmac` have different lifetimes.
func TestPhoneHMACSurvivesPhoneClearing(t *testing.T) {
	phone := uniqueRegisterPhone(t)
	mac, err := PhoneHMAC(phoneFixtureKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	uid := insertPhoneUser(t, phone, mac)
	cleanupPhone(t, phone)

	// The F-12 shape: only phone is cleared.
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone = NULL WHERE id = ?`, uid); err != nil {
		t.Fatalf("clear phone: %v", err)
	}
	var gotHMAC string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone_hmac, '') FROM t_user WHERE id = ?`, uid).Scan(&gotHMAC); err != nil {
		t.Fatalf("read hmac: %v", err)
	}
	if gotHMAC != mac {
		t.Fatalf("phone_hmac = %q after clearing phone, want %q", gotHMAC, mac)
	}
	// The unique key is still occupied, so a new registration of that number is
	// still refused — that is the intended, documented consequence of D1.
	svc, _, _ := newAuthService(t)
	svc.PhoneHMACKey = phoneFixtureKey
	svc.VerificationMode = VerificationOff
	dup := callRegister(t, svc, map[string]string{
		"identifier": phone, "password": "Str0ng-Passw0rd!",
	})
	if dup.Code != http.StatusConflict {
		t.Fatalf("registering a PII-cleared number status=%d body=%s, want 409",
			dup.Code, dup.Body.String())
	}
}

// The convergence point exists and does nothing yet: it must not clear either
// phone column, and its whole purpose is to be the one place a future change
// edits. Asserting the no-op keeps a well-meaning F-12 change from silently
// clearing the de-duplication key.
func TestCancelPhonePIIIsANoOpToday(t *testing.T) {
	phone := uniqueRegisterPhone(t)
	mac, err := PhoneHMAC(phoneFixtureKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	uid := insertPhoneUser(t, phone, mac)
	cleanupPhone(t, phone)

	tx, err := testDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only assertion
	if err := cancelPhonePII(context.Background(), tx, uid, nowForTest()); err != nil {
		t.Fatalf("cancelPhonePII: %v", err)
	}
	var gotPhone, gotHMAC string
	if err := tx.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone, ''), COALESCE(phone_hmac, '') FROM t_user WHERE id = ?`,
		uid).Scan(&gotPhone, &gotHMAC); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if gotPhone != phone || gotHMAC != mac {
		t.Fatalf("cancelPhonePII changed the row: phone=%q hmac=%q, want %q/%q",
			gotPhone, gotHMAC, phone, mac)
	}
}

func nowForTest() time.Time { return time.Now().UTC() }

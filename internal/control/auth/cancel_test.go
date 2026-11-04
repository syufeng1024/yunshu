package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/control/ticket"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/phoneclaim"
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

// runCancelBody posts /api/v1/auth/cancel. payload nil means "no body at all",
// which is a different case from {"confirm": ""} and must stay reachable so the
// 400 invalid_request_body branch is asserted, not assumed.
func runCancelBody(t *testing.T, svc *Service, sess *mw.Session, cookie string,
	payload any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if payload != nil {
		raw, err := jsonx.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal cancel body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	var req *http.Request
	if reader == nil {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/cancel", nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/cancel", reader)
	}
	req.RemoteAddr = "198.51.100.77:12345"
	req = req.WithContext(mw.WithSession(req.Context(), sess))
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	svc.Cancel(rec, req)
	return rec
}

// cancelConfirmFor is what the console would ask this account to type: the
// e-mail, else the phone's last four digits, else the username. Reading it from
// the database instead of the fixture keeps the existing tests honest — they
// pass the same value a real user would.
func cancelConfirmFor(t *testing.T, uid int64) string {
	t.Helper()
	var email, phone, username string
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT COALESCE(email, ''), COALESCE(phone, ''), COALESCE(username, '')
		  FROM t_user WHERE id = ?`, uid).Scan(&email, &phone, &username); err != nil {
		t.Fatalf("read identifiers: %v", err)
	}
	if email != "" {
		return email
	}
	if phone != "" {
		return phoneSuffix(phone)
	}
	return username
}

func runCancel(t *testing.T, svc *Service, sess *mw.Session, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	return runCancelBody(t, svc, sess, cookie, map[string]string{"confirm": cancelConfirmFor(t, sess.UID)})
}

// cleanupClaim removes the ledger row a phone fixture created, so a later run
// starts from "this number was never seen".
func cleanupClaim(t *testing.T, hmac string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(),
			`DELETE FROM t_phone_claim WHERE phone_hmac = ?`, hmac)
	})
}

func claimFor(t *testing.T, hmac string) phoneclaim.Claim {
	t.Helper()
	claim, err := phoneclaim.Get(context.Background(), testDB, hmac)
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}
	return claim
}

// T12 (INC-phone-hmac, still true): after a PII pass that sets phone = NULL, the
// hash is still there and still blocks the number. Cancellation no longer
// produces this state (it nulls both columns), but a manual edit or a future
// PII job can, and the unique key's behaviour must not drift with it.
func TestPhoneHMACSurvivesPhoneClearing(t *testing.T) {
	phone := uniqueRegisterPhone(t)
	mac, err := PhoneHMAC(phoneFixtureKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	uid := insertPhoneUser(t, phone, mac)
	cleanupPhone(t, phone)
	cleanupClaim(t, mac)

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
	// still refused.
	svc, _, _ := newAuthService(t)
	svc.PhoneHMACKey = phoneFixtureKey
	svc.VerificationMode = VerificationOff
	dup := callRegister(t, svc, map[string]string{
		"identifier": phone, "password": "Str0ng-Passw0rd!",
	})
	if dup.Code != http.StatusConflict {
		t.Fatalf("registering a number whose hmac is still held status=%d body=%s, want 409",
			dup.Code, dup.Body.String())
	}
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
	rec := runCancel(t, svc, sess, "")
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

	rec := runCancel(t, svc, sess, cookie)
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

// AC-1 / AC-2 / AC-15 (red line): cancellation releases the number and records
// the release in the ledger; a second account can then claim the same number.
//
// This replaces the pre-increment assertion "cancellation keeps phone and
// phone_hmac". The semantics were reversed on purpose (INC-phone-claim D1–D3):
// the old rule locked a user out of their own handset forever.
func TestCancelReleasesPhoneAndHMAC(t *testing.T) {
	svc, mgr, _ := newCancelService(t)
	phone := uniqueRegisterPhone(t)
	mac, err := PhoneHMAC(phoneFixtureKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	uid := insertPhoneUser(t, phone, mac)
	cleanupPhone(t, phone)
	cleanupClaim(t, mac)

	// A trial granted three days ago: the cancellation must carry that fact into
	// the ledger, because the account row is about to lose the number.
	grantedAt := time.Now().UTC().AddDate(0, 0, -3).Truncate(time.Millisecond)
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET trial_claimed_at = ? WHERE id = ?`, grantedAt, uid); err != nil {
		t.Fatalf("seed trial: %v", err)
	}

	sess, _, err := mgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	if rec := runCancel(t, svc, sess, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}

	var gotPhone, gotHMAC, status sql.NullString
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT phone, phone_hmac, status FROM t_user WHERE id = ?`,
		uid).Scan(&gotPhone, &gotHMAC, &status); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if status.String != "DISABLED" {
		t.Fatalf("status = %q, want DISABLED", status.String)
	}
	if gotPhone.Valid || gotHMAC.Valid {
		t.Fatalf("phone=%v hmac=%v after cancel, want both NULL (the number must be released)",
			gotPhone, gotHMAC)
	}

	claim := claimFor(t, mac)
	if claim.CancelCount != 1 {
		t.Fatalf("cancel_count = %d, want 1", claim.CancelCount)
	}
	if !claim.TrialGrantedAt.Valid || !claim.TrialGrantedAt.Time.Equal(grantedAt) {
		t.Fatalf("claim.trial_granted_at = %+v, want %s (self-healed from the account row)",
			claim.TrialGrantedAt, grantedAt)
	}
	if !claim.LastTrialUserID.Valid || claim.LastTrialUserID.Int64 != uid {
		t.Fatalf("claim.last_trial_user_id = %+v, want %d", claim.LastTrialUserID, uid)
	}
	if !claim.LastCancelAt.Valid || claim.LastCancelAt.Time.IsZero() {
		t.Fatalf("claim.last_cancel_at = %+v, want the cancellation instant", claim.LastCancelAt)
	}

	// AC-15: the audit row says what happened, without naming the number.
	var afterJSON string
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT CAST(after_json AS CHAR) FROM t_admin_audit_log
		 WHERE module = 'USER' AND action = 'CANCEL' AND target_id = ?
		 ORDER BY id DESC LIMIT 1`, fmt.Sprint(uid)).Scan(&afterJSON); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var after map[string]any
	if err := json.Unmarshal([]byte(afterJSON), &after); err != nil {
		t.Fatalf("decode after_json %q: %v", afterJSON, err)
	}
	if released, ok := after["phone_released"].(bool); !ok || !released {
		t.Fatalf("after_json.phone_released = %v, want true", after["phone_released"])
	}
	if count, ok := after["claim_cancel_count"].(float64); !ok || int(count) != 1 {
		t.Fatalf("after_json.claim_cancel_count = %v, want 1", after["claim_cancel_count"])
	}
	if strings.Contains(afterJSON, phone) || strings.Contains(afterJSON, mac) {
		t.Fatalf("audit after_json leaks the number or its hmac: %s", afterJSON)
	}

	// AC-2: the same number registers again immediately, as a new account.
	svc2, _, _ := newAuthService(t)
	svc2.PhoneHMACKey = phoneFixtureKey
	svc2.VerificationMode = VerificationOff
	rec := callRegister(t, svc2, map[string]string{
		"identifier": phone, "password": "Str0ng-Passw0rd!",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-registering a released number status=%d body=%s, want 201",
			rec.Code, rec.Body.String())
	}
	var newUID int64
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT id FROM t_user WHERE phone_hmac = ? AND deleted_at IS NULL`, mac).Scan(&newUID); err != nil {
		t.Fatalf("read new holder: %v", err)
	}
	if newUID == uid {
		t.Fatalf("re-registration reused uid %d; cancellation must not resurrect the old row", uid)
	}

	// B6's coupling: login by plaintext phone must find exactly one account.
	ident, err := NormalizeIdentifier(phone)
	if err != nil {
		t.Fatalf("normalize phone: %v", err)
	}
	row, err := svc2.findUser(context.Background(), ident)
	if err != nil {
		t.Fatalf("findUser by phone after re-registration: %v", err)
	}
	if row.uid != newUID {
		t.Fatalf("findUser returned uid %d, want the new account %d", row.uid, newUID)
	}
	var holders int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE phone = ?`, phone).Scan(&holders); err != nil {
		t.Fatalf("count holders: %v", err)
	}
	if holders != 1 {
		t.Fatalf("rows holding the number = %d, want 1 (the cancelled row must not keep it)", holders)
	}
	t.Logf("release: uid=%d disabled; claim.cancel_count=1; trial_granted_at=%s; new uid=%d",
		uid, grantedAt, newUID)
}

// AC-10: an e-mail account never touches the ledger. Its cancellation must not
// create a claim row (it has no number to record).
func TestCancelEmailAccountLeavesLedgerEmpty(t *testing.T) {
	svc, mgr, _ := newCancelService(t)
	uid := newAuthUser(t, uniqueEmail())
	sess, _, err := mgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	if rec := runCancel(t, svc, sess, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}
	var rows int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_phone_claim WHERE first_user_id = ? OR last_claim_user_id = ?`,
		uid, uid).Scan(&rows); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if rows != 0 {
		t.Fatalf("ledger rows for an e-mail account = %d, want 0", rows)
	}
}

// The third state: a plaintext number with no hash is left completely alone.
// Clearing the plaintext would make the hash (and therefore the 90-day window)
// impossible to reconstruct, which is a free trial rather than a cleanup.
func TestCancelPhonePIISkipsRowWithoutHMAC(t *testing.T) {
	phone := uniqueRegisterPhone(t)
	uid := insertPhoneUser(t, phone, "")
	cleanupPhone(t, phone)
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone_hmac = NULL WHERE id = ?`, uid); err != nil {
		t.Fatalf("clear hmac: %v", err)
	}

	tx, err := testDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // assertions only
	ident := cancelIdentity{UID: uid, Phone: phone}
	release, err := cancelPhonePII(context.Background(), tx, ident, nowForTest())
	if err != nil {
		t.Fatalf("cancelPhonePII: %v", err)
	}
	if release.Released || !release.SkippedNoHMAC {
		t.Fatalf("release = %+v, want Released=false SkippedNoHMAC=true", release)
	}
	var gotPhone sql.NullString
	if err := tx.QueryRowContext(context.Background(),
		`SELECT phone FROM t_user WHERE id = ?`, uid).Scan(&gotPhone); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if !gotPhone.Valid || gotPhone.String != phone {
		t.Fatalf("phone = %v, want %q untouched", gotPhone, phone)
	}
}

// TestCancelPhonePIIReleasesNumber: the release itself, called directly.
func TestCancelPhonePIIReleasesNumber(t *testing.T) {
	phone := uniqueRegisterPhone(t)
	mac, err := PhoneHMAC(phoneFixtureKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	uid := insertPhoneUser(t, phone, mac)
	cleanupPhone(t, phone)
	cleanupClaim(t, mac)

	// cancelPhonePII must be called with the row already locked, exactly as
	// Cancel does; the lock is what makes the guarded UPDATE safe.
	ctx := context.Background()
	tx, err := testDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	ident, apiErr := lockCancelIdentity(ctx, tx, uid)
	if apiErr != nil {
		_ = tx.Rollback()
		t.Fatalf("lock identity: %v", apiErr)
	}
	release, err := cancelPhonePII(ctx, tx, ident, nowForTest())
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("cancelPhonePII: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !release.Released || release.ClaimCancelCount != 1 {
		t.Fatalf("release = %+v, want Released=true CancelCount=1", release)
	}
	var gotPhone, gotHMAC sql.NullString
	if err := testDB.QueryRowContext(ctx,
		`SELECT phone, phone_hmac FROM t_user WHERE id = ?`, uid).Scan(&gotPhone, &gotHMAC); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if gotPhone.Valid || gotHMAC.Valid {
		t.Fatalf("phone=%v hmac=%v, want both NULL", gotPhone, gotHMAC)
	}
}

// The confirmation rule, as a table. No database, no clock: the three modes and
// the ambiguity case are decided entirely by the locked row's columns.
func TestCancelConfirmMatching(t *testing.T) {
	cases := []struct {
		name    string
		ident   cancelIdentity
		confirm string
		want    bool
	}{
		{"email exact", cancelIdentity{Email: "a@b.com"}, "a@b.com", true},
		{"email case and spaces", cancelIdentity{Email: "A@B.com"}, "  a@b.COM ", true},
		{"email wrong", cancelIdentity{Email: "a@b.com"}, "c@b.com", false},
		{"email empty", cancelIdentity{Email: "a@b.com"}, "   ", false},
		{"phone suffix", cancelIdentity{Phone: "8613800138000"}, "8000", true},
		{"phone suffix with decoration", cancelIdentity{Phone: "8613800138000"}, "**8000", true},
		{"phone suffix too short", cancelIdentity{Phone: "8613800138000"}, "000", false},
		{"phone suffix wrong", cancelIdentity{Phone: "8613800138000"}, "1234", false},
		{
			// The full number is not the confirmation value for this mode: the
			// prompt asks for four digits, and accepting anything else is how the
			// prompt and the check drifted apart the first time.
			name:    "phone full number is not the suffix",
			ident:   cancelIdentity{Phone: "8613800138000"},
			confirm: "8613800138000",
			want:    false,
		},
		{"username", cancelIdentity{Username: "Admin01"}, "admin01", true},
		{"username wrong", cancelIdentity{Username: "admin01"}, "admin02", false},
		{
			// A username that happens to be the number's last four digits. Only
			// the account's PRIMARY identifier is compared, so this is not an
			// ambiguous case: the phone is primary, the suffix matches, and the
			// endpoint has no "two candidates" branch to report.
			name:    "username equal to the phone suffix is not ambiguous",
			ident:   cancelIdentity{Phone: "8613800138000", Username: "8000"},
			confirm: "8000",
			want:    true,
		},
		{
			// E-mail outranks the phone: with an address on the row, the number's
			// last four digits are the wrong answer.
			name:    "email outranks the phone",
			ident:   cancelIdentity{Email: "a@b.com", Phone: "8613800138000"},
			confirm: "8000",
			want:    false,
		},
		{"nothing on the row", cancelIdentity{}, "anything", false},
	}
	for _, tc := range cases {
		err := checkCancelConfirm(tc.ident, tc.confirm, tc.ident.primaryMode())
		if ok := err == nil; ok != tc.want {
			t.Fatalf("%s: checkCancelConfirm(%q) ok=%v, want %v (err=%v)",
				tc.name, tc.confirm, ok, tc.want, err)
		}
	}
}

// The endpoint-level confirmation behaviour.
//
// G1 (red line): the three ways to get the confirmation wrong — no body at all,
// an empty value, a wrong value — must be ONE response. The console cannot tell
// them apart, and neither may the API: a distinguishable "empty vs wrong" pair
// turns the endpoint into an oracle for "is this the account's identifier".
// Only the hint varies, and it varies with the account shape, never with the
// mistake. `field=confirm` appears inside `message` (errorObject has no separate
// field member), so the assertion is on the message text.
func TestCancelConfirmGuard(t *testing.T) {
	const wantMessage = "请求体参数有误：confirm"

	t.Run("G1: three failures share one 400", func(t *testing.T) {
		svc, mgr, _ := newCancelService(t)
		uid := newAuthUser(t, uniqueEmail())
		sess, _, err := mgr.Issue(context.Background(), uid)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}

		inputs := []struct {
			name    string
			payload any
		}{
			{"no body", nil},
			{"empty confirm", map[string]string{"confirm": "   "}},
			{"wrong confirm", map[string]string{"confirm": "someone-else@test.local"}},
		}
		var first errorBody
		for i, in := range inputs {
			rec := runCancelBody(t, svc, sess, "", in.payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status=%d body=%s, want 400", in.name, rec.Code, rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.Error.Code != apierr.CodeInvalidRequestBody {
				t.Fatalf("%s: code=%q, want %s", in.name, body.Error.Code, apierr.CodeInvalidRequestBody)
			}
			if body.Error.Message != wantMessage {
				t.Fatalf("%s: message=%q, want %q", in.name, body.Error.Message, wantMessage)
			}
			// The empty-body branch of decodeJSON names `body`; mapping it to the
			// confirm field is what makes the three answers identical. Asserting
			// on the message suffix is the only way to see that: there is no
			// separate `field` member in the serialized error object.
			if strings.HasSuffix(body.Error.Message, "：body") {
				t.Fatalf("%s: message=%q still names the whole body", in.name, body.Error.Message)
			}
			if !strings.Contains(body.Error.Hint, "邮箱") {
				t.Fatalf("%s: hint=%q, want the e-mail prompt for an e-mail account", in.name, body.Error.Hint)
			}
			if i == 0 {
				first = body
				continue
			}
			if body.Error.Message != first.Error.Message || body.Error.Code != first.Error.Code ||
				body.Error.Type != first.Error.Type || body.Error.Hint != first.Error.Hint {
				t.Fatalf("%s: response differs from the first failure\n got %+v\nwant %+v",
					in.name, body.Error, first.Error)
			}
		}
		// Nothing was written: a wrong confirmation must leave no trace.
		assertStatus(t, uid, "NORMAL")
	})

	t.Run("hint follows the account shape, not the mistake", func(t *testing.T) {
		svc, mgr, _ := newCancelService(t)
		phone := uniqueRegisterPhone(t)
		mac, err := PhoneHMAC(phoneFixtureKey, phone)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		uid := insertPhoneUser(t, phone, mac)
		cleanupPhone(t, phone)
		cleanupClaim(t, mac)

		sess, _, err := mgr.Issue(context.Background(), uid)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		for _, payload := range []any{nil, map[string]string{"confirm": ""}, map[string]string{"confirm": "0000"}} {
			rec := runCancelBody(t, svc, sess, "", payload)
			body := decodeError(t, rec)
			if rec.Code != http.StatusBadRequest || body.Error.Code != apierr.CodeInvalidRequestBody {
				t.Fatalf("payload %v: %d/%s, want 400/%s", payload, rec.Code, body.Error.Code,
					apierr.CodeInvalidRequestBody)
			}
			if body.Error.Message != wantMessage {
				t.Fatalf("payload %v: message=%q, want %q", payload, body.Error.Message, wantMessage)
			}
			if !strings.Contains(body.Error.Hint, "后 4 位") {
				t.Fatalf("payload %v: hint=%q, want the last-four prompt", payload, body.Error.Hint)
			}
		}
	})

	t.Run("phone account confirms with last four digits", func(t *testing.T) {
		svc, mgr, _ := newCancelService(t)
		phone := uniqueRegisterPhone(t)
		mac, err := PhoneHMAC(phoneFixtureKey, phone)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		uid := insertPhoneUser(t, phone, mac)
		cleanupPhone(t, phone)
		cleanupClaim(t, mac)

		sess, _, err := mgr.Issue(context.Background(), uid)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		// The full number is NOT the confirmation value for this mode: the field
		// asks for four digits, and accepting anything else would let the prompt
		// and the check drift apart again.
		bad := runCancelBody(t, svc, sess, "", map[string]string{"confirm": phone})
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("full number status=%d body=%s, want 400", bad.Code, bad.Body.String())
		}
		rec := runCancelBody(t, svc, sess, "", map[string]string{"confirm": phoneSuffix(phone)})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("last-four status=%d body=%s, want 204", rec.Code, rec.Body.String())
		}
	})

	t.Run("username account confirms with the username", func(t *testing.T) {
		svc, mgr, _ := newCancelService(t)
		username := fmt.Sprintf("op%010d", time.Now().UnixNano()%10000000000)
		uid := newAuthUser(t, "")
		if _, err := testDB.ExecContext(context.Background(),
			`UPDATE t_user SET email = NULL, username = ? WHERE id = ?`, username, uid); err != nil {
			t.Fatalf("set username: %v", err)
		}
		sess, _, err := mgr.Issue(context.Background(), uid)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		// The session snapshot was issued from the row above, so the hint is the
		// username prompt; a wrong value still answers the same 400 message.
		bad := runCancelBody(t, svc, sess, "", map[string]string{"confirm": "wrong-name"})
		badBody := decodeError(t, bad)
		if bad.Code != http.StatusBadRequest || badBody.Error.Message != wantMessage {
			t.Fatalf("wrong username: %d/%q, want 400/%q", bad.Code, badBody.Error.Message, wantMessage)
		}
		if !strings.Contains(badBody.Error.Hint, "用户名") {
			t.Fatalf("hint=%q, want the username prompt", badBody.Error.Hint)
		}
		rec := runCancelBody(t, svc, sess, "", map[string]string{
			"confirm": strings.ToUpper(username),
		})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status=%d body=%s, want 204", rec.Code, rec.Body.String())
		}
	})
}

// A repeated cancellation is a state conflict and must not inflate the ledger:
// cancel_count belongs to the number, not to the request.
func TestCancelTwiceIsConflictAndKeepsCount(t *testing.T) {
	svc, mgr, _ := newCancelService(t)
	phone := uniqueRegisterPhone(t)
	mac, err := PhoneHMAC(phoneFixtureKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	uid := insertPhoneUser(t, phone, mac)
	cleanupPhone(t, phone)
	cleanupClaim(t, mac)

	sess, _, err := mgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if rec := runCancel(t, svc, sess, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("first cancel status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}
	// The number is gone from the row, so the second attempt must be refused by
	// the status guard (409), not by the confirmation guard (400).
	second := runCancelBody(t, svc, sess, "", map[string]string{"confirm": phoneSuffix(phone)})
	if second.Code != http.StatusConflict {
		t.Fatalf("second cancel status=%d body=%s, want 409", second.Code, second.Body.String())
	}
	if claim := claimFor(t, mac); claim.CancelCount != 1 {
		t.Fatalf("cancel_count = %d after a refused second cancel, want 1", claim.CancelCount)
	}
}

func assertStatus(t *testing.T, uid int64, want string) {
	t.Helper()
	var status string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT status FROM t_user WHERE id = ?`, uid).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != want {
		t.Fatalf("status = %q, want %q", status, want)
	}
}

func nowForTest() time.Time { return time.Now().UTC() }

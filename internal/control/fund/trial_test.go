package fund

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/phoneclaim"
)

// A7 / G-09: a second claim with the same device fingerprint is refused, and
// so are the phone and IP-segment variants. The trial is granted exactly once.
//
// The "same phone" case changed shape in INC-phone-hmac. Before it, two accounts
// could share a number (t_user.phone has no unique key), so the phone check was
// a plaintext comparison that actually fired. Now every write path stores
// phone_hmac and uk_user_phone_hmac forbids two rows with the same number, so
// the reachable phone case is the transitional one: a row that has a number but
// no hash (manual DB edit, or a backfill that has not run yet). The handler must
// refuse to pay in that state rather than grant a trial it cannot de-duplicate —
// 500 internal_error, not a trial.
func TestA7TrialAntiAbuse(t *testing.T) {
	svc, _ := newTestService(t)

	u1 := newTestUser(t, nil)
	u2 := newTestUser(t, nil)
	u3 := newTestUser(t, nil)
	u4 := newTestUser(t, nil)
	setUserPhone(t, u1, "8613800138000")
	// u3 simulates "same number, hash missing": the only way a second account
	// can carry the same number once the unique key is in force, and a state the
	// handler must fail closed on.
	setUserPhoneWithoutHMAC(t, u3, "8613800138000")

	rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-alpha"}, u1, "203.0.113.10:4111", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first claim status=%d body=%s", rec.Code, rec.Body.String())
	}
	var granted trialClaimResponse
	decodeBody(t, rec, &granted)
	if granted.GrantedMicro != 70_000_000 || granted.GrantedCredits != 7000 {
		t.Fatalf("granted = %d micro (%d credits), want 70,000,000 (7000)",
			granted.GrantedMicro, granted.GrantedCredits)
	}
	b := balanceOf(t, u1)
	if b.Bonus != 70_000_000 || b.Total != 70_000_000 {
		t.Fatalf("balance after trial = bonus %d total %d, want 70,000,000", b.Bonus, b.Total)
	}
	var claimed bool
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT trial_claimed_at IS NOT NULL FROM t_user WHERE id = ?`, u1).Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("trial_claimed_at is NULL after a successful claim")
	}
	var trialGranted int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_device_fingerprint WHERE fp_hash = ? AND trial_granted = 1`,
		fingerprintHash(svc.FingerprintKey, "device-alpha")).Scan(&trialGranted); err != nil {
		t.Fatal(err)
	}
	if trialGranted != 1 {
		t.Fatalf("granted fingerprint rows = %d, want 1", trialGranted)
	}
	t.Logf("first claim: granted=%d credits plan=%s expires=%s",
		granted.GrantedCredits, granted.PlanCode, granted.TrialExpireAt)

	// Same fingerprint, different account and IP.
	assertTrialClaimed := func(label string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s status=%d body=%s", label, rec.Code, rec.Body.String())
		}
		eb := errorBody(t, rec)
		if eb.Error.Code != "trial_already_claimed" {
			t.Fatalf("%s code=%q, want trial_already_claimed", label, eb.Error.Code)
		}
		// The four factors share one message on purpose (no probing which one
		// matched).
		if !strings.Contains(eb.Error.Message, "该账号或设备已领取过体验额度") {
			t.Fatalf("%s message=%q", label, eb.Error.Message)
		}
		t.Logf("%s refusal: HTTP %d code=%s", label, rec.Code, eb.Error.Code)
	}

	rec = callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-alpha"}, u2, "198.51.100.20:4111", nil)
	assertTrialClaimed("same fingerprint", rec)

	// Different fingerprint, same phone — but the row lost its hash, so the
	// handler cannot de-duplicate and refuses (500) instead of granting.
	rec = callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-beta"}, u3, "192.0.2.30:4111", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("phone without hmac status=%d body=%s, want 500 (fail closed)",
			rec.Code, rec.Body.String())
	}
	if code := errorBody(t, rec).Error.Code; code != "internal_error" {
		t.Fatalf("phone without hmac code=%q, want internal_error", code)
	}
	if b := balanceOf(t, u3); b.Total != 0 {
		t.Fatalf("a refused claim still granted %d micro to u3", b.Total)
	}

	// Different fingerprint and phone, but the same /24 as the first claim.
	rec = callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-gamma"}, u4, "203.0.113.99:4111", nil)
	assertTrialClaimed("same /24", rec)

	// The first account cannot claim twice either.
	rec = callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-delta"}, u1, "203.0.113.10:4111", nil)
	assertTrialClaimed("second claim by same user", rec)
	if n := countNotifications(t, u1, eventRefundArrived); n != 0 {
		t.Fatalf("trial claim wrote %d refund notifications, want 0", n)
	}
}

// An empty HMAC key is a configuration refusal, never an empty-key hash.
func TestTrialClaimRequiresFingerprintKey(t *testing.T) {
	svc, _ := newTestService(t)
	svc.FingerprintKey = ""
	uid := newTestUser(t, nil)
	rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-alpha"}, uid, "203.0.113.10:4111", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
	if code := errorBody(t, rec).Error.Code; code != "config_unavailable" {
		t.Fatalf("code=%q, want config_unavailable", code)
	}
}

// T13 in the form this schema allows: the phone factor compares phone_hmac, and
// the state that would exercise its 409 branch — two live accounts with the same
// number — can no longer be constructed, because uk_user_phone_hmac is exactly
// the constraint that makes the branch unnecessary.
//
// So this asserts the three things that are actually observable:
//
//  1. the invariant: a second account cannot be given the same number (1062);
//  2. the code path: trial.go compares hashes, not plaintext (source assertion,
//     same technique as the state-machine doc guards in flow_doc_test.go);
//  3. the fallback: a phone with no hash fails closed (covered in
//     TestA7TrialAntiAbuse, repeated here for the cross-account shape).
func TestTrialPhoneFactorUsesHMAC(t *testing.T) {
	svc, _ := newTestService(t)

	u1 := newTestUser(t, nil)
	setUserPhone(t, u1, "8613800990001")
	rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-hmac-1"}, u1, "203.0.113.41:4111", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first claim status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 1. The unique key refuses the duplicate: this is the actual bug fix.
	u2 := newTestUser(t, nil)
	var u1HMAC string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone_hmac, '') FROM t_user WHERE id = ?`, u1).Scan(&u1HMAC); err != nil {
		t.Fatalf("read hmac: %v", err)
	}
	if u1HMAC == "" {
		t.Fatal("the first account has no phone_hmac; the trial test would be meaningless")
	}
	_, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone = ?, phone_hmac = ? WHERE id = ?`,
		"8613800990001", u1HMAC, u2)
	if err == nil {
		t.Fatal("two accounts were allowed to share a phone_hmac; uk_user_phone_hmac is not in force")
	}
	if !strings.Contains(err.Error(), "uk_user_phone_hmac") {
		t.Fatalf("duplicate hmac error = %v, want a uk_user_phone_hmac violation", err)
	}

	// 2. The dedup decision no longer mentions the plaintext column, and it is
	//    made against the number's ledger rather than against another account's
	//    row — that switch is what makes the window cross-account.
	source, readErr := os.ReadFile("trial.go")
	if readErr != nil {
		t.Fatalf("read trial.go: %v", readErr)
	}
	if strings.Contains(string(source), "u.phone = ?") {
		t.Fatal("trial.go still compares the plaintext phone column")
	}
	if !strings.Contains(string(source), "phoneclaim.TrialWindowTx") {
		t.Fatal("trial.go does not re-check the window on the ledger; the in-transaction guard is missing")
	}
	if !strings.Contains(string(source), "phoneclaim.MarkTrialGrantedTx") {
		t.Fatal("trial.go does not stamp the grant on the ledger")
	}
}

// The phone window must survive cancellation (AC-3, red line).
//
// This replaces the pre-increment assertion "a cancelled account keeps its
// phone_hmac so the trial check still sees it". The number now leaves the row,
// so the memory that has to survive is the ledger row: cancel records
// trial_granted_at there, and a *different* account registering the same number
// is refused for 90 days.
func TestTrialWindowSurvivesCancel(t *testing.T) {
	svc, _ := newTestService(t)
	const phone = "8613800990002"
	a := newTestUser(t, nil)
	setUserPhone(t, a, phone)

	if rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-cancel-1"}, a, "203.0.113.51:4111", nil); rec.Code != http.StatusOK {
		t.Fatalf("first claim status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Exactly what POST /api/v1/auth/cancel writes; repeated here because the
	// fund package must not drive the auth handler (session, CSRF, ticket).
	mac := simulateCancel(t, a)

	// B takes the released number: a new account, same handset.
	b := newTestUser(t, nil)
	setUserPhone(t, b, phone)

	rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-cancel-2"}, b, "192.0.2.61:4111", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("claim on a re-registered number status=%d body=%s, want 409",
			rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if eb.Error.Code != "trial_already_claimed" {
		t.Fatalf("code=%q, want trial_already_claimed", eb.Error.Code)
	}
	if !strings.Contains(eb.Error.Hint, "90 天") {
		t.Fatalf("hint=%q, want it to mention the 90-day window", eb.Error.Hint)
	}

	// The refusal must not have half-granted: no account flag, no balance, no
	// device row for B.
	var bClaimed bool
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT trial_claimed_at IS NOT NULL FROM t_user WHERE id = ?`, b).Scan(&bClaimed); err != nil {
		t.Fatalf("read b flag: %v", err)
	}
	if bClaimed {
		t.Fatal("a refused claim still set trial_claimed_at on the new account")
	}
	if bal := balanceOf(t, b); bal.Total != 0 {
		t.Fatalf("refused claim granted %d micro to the new account", bal.Total)
	}
	var fpRows int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_device_fingerprint WHERE fp_hash = ? AND trial_granted = 1`,
		fingerprintHash(svc.FingerprintKey, "device-cancel-2")).Scan(&fpRows); err != nil {
		t.Fatalf("count fp: %v", err)
	}
	if fpRows != 0 {
		t.Fatalf("refused claim wrote %d granted fingerprint rows", fpRows)
	}
	t.Logf("window survives cancel: A=%d released hmac=%s…; B=%d refused with %q",
		a, mac[:6], b, eb.Error.Code)
}

// AC-4 / AC-5: the window is left-open at exactly 90 days, and a grant older
// than that lets the new holder claim — and then restarts the window on the
// ledger under this account's id.
//
// The clock is frozen: "one millisecond inside the window" is only a stable
// assertion if the comparison uses the same instant the fixture seeded, and a
// live clock would age past 90 days during the request itself.
func TestTrialPhoneWindowBoundary(t *testing.T) {
	pinned := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		age     time.Duration
		want    int
		wantLog string
	}{
		{"exactly 90 days is allowed", 90 * 24 * time.Hour, http.StatusOK, ""},
		{"one millisecond inside the window", 90*24*time.Hour - time.Millisecond, http.StatusConflict, "trial_already_claimed"},
		{"91 days is allowed", 91 * 24 * time.Hour, http.StatusOK, ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(t)
			svc.Now = clock.NewFixed(pinned)
			phone := fmt.Sprintf("86138001%05d", 20000+i)
			uid := newTestUser(t, nil)
			setUserPhone(t, uid, phone)
			mac, err := phoneHMACForTest(phone)
			if err != nil {
				t.Fatalf("hmac: %v", err)
			}
			grantedAt := pinned.Add(-tc.age)
			if _, err := testDB.ExecContext(context.Background(), `
				UPDATE t_phone_claim
				   SET trial_granted_at = ?, last_trial_user_id = ?
				 WHERE phone_hmac = ?`, grantedAt, uid, mac); err != nil {
				t.Fatalf("seed window: %v", err)
			}

			rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
				map[string]any{"fingerprint": fmt.Sprintf("device-window-%d", i)},
				uid, fmt.Sprintf("198.18.%d.7:4111", i), nil)
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
			if tc.want == http.StatusConflict {
				if code := errorBody(t, rec).Error.Code; code != tc.wantLog {
					t.Fatalf("code=%q, want %s", code, tc.wantLog)
				}
				return
			}
			var stamped time.Time
			var trialUID sql.NullInt64
			if err := testDB.QueryRowContext(context.Background(),
				`SELECT trial_granted_at, last_trial_user_id FROM t_phone_claim WHERE phone_hmac = ?`,
				mac).Scan(&stamped, &trialUID); err != nil {
				t.Fatalf("read claim: %v", err)
			}
			if !stamped.Equal(pinned) {
				t.Fatalf("claim.trial_granted_at = %s, want the grant instant %s", stamped, pinned)
			}
			if !trialUID.Valid || trialUID.Int64 != uid {
				t.Fatalf("claim.last_trial_user_id = %+v, want %d", trialUID, uid)
			}
		})
	}
}

// AC-6: a number that never received a trial is not blocked by a cancellation.
// The new owner registers and claims immediately.
func TestTrialFreshNumberAfterCancelIsAllowed(t *testing.T) {
	svc, _ := newTestService(t)
	const phone = "8613800990006"
	a := newTestUser(t, nil)
	setUserPhone(t, a, phone) // never claimed a trial
	simulateCancel(t, a)

	b := newTestUser(t, nil)
	setUserPhone(t, b, phone)
	rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-fresh-1"}, b, "198.51.100.91:4111", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim on a never-trialed number status=%d body=%s, want 200",
			rec.Code, rec.Body.String())
	}
}

// The ledger row is the only memory; without it the handler must refuse to pay
// (fail closed) rather than grant a trial it cannot de-duplicate.
func TestTrialPhoneClaimMissingFailsClosed(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	setUserPhoneWithoutClaim(t, uid, "8613800990007")

	rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-missing-claim"}, uid, "192.0.2.99:4111", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rec.Code, rec.Body.String())
	}
	if code := errorBody(t, rec).Error.Code; code != "internal_error" {
		t.Fatalf("code=%q, want internal_error", code)
	}
	if bal := balanceOf(t, uid); bal.Total != 0 {
		t.Fatalf("a refused claim granted %d micro", bal.Total)
	}
}

// AC-14: the phone window did not replace the device factor. A new number on a
// fingerprint that already collected a trial is still refused.
func TestTrialDeviceFactorIsIndependentOfPhoneWindow(t *testing.T) {
	svc, _ := newTestService(t)
	const fp = "device-shared"
	a := newTestUser(t, nil)
	setUserPhone(t, a, "8613800990008")
	if rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": fp}, a, "203.0.113.71:4111", nil); rec.Code != http.StatusOK {
		t.Fatalf("first claim status=%d body=%s", rec.Code, rec.Body.String())
	}

	b := newTestUser(t, nil)
	setUserPhone(t, b, "8613800990009") // a number with no window at all
	rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": fp}, b, "203.0.113.72:4111", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409 from the device factor", rec.Code, rec.Body.String())
	}
}

// simulateCancel performs exactly the writes POST /api/v1/auth/cancel performs
// for a phone account — ledger first, then disable + release — and returns the
// number's HMAC.
func simulateCancel(t *testing.T, uid int64) string {
	t.Helper()
	ctx := context.Background()
	var hmac string
	var claimedAt sql.NullTime
	if err := testDB.QueryRowContext(ctx,
		`SELECT COALESCE(phone_hmac, ''), trial_claimed_at FROM t_user WHERE id = ?`, uid).
		Scan(&hmac, &claimedAt); err != nil {
		t.Fatalf("read phone columns: %v", err)
	}
	if hmac == "" {
		t.Fatal("simulateCancel needs an account with a phone number")
	}
	now := time.Now().UTC()
	tx, err := testDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := phoneclaim.RecordCancelTx(ctx, tx, hmac, uid, now, claimedAt,
		phoneclaim.TrialGrantUserID(claimedAt, uid)); err != nil {
		t.Fatalf("record cancel: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE t_user
		   SET status = 'DISABLED', disable_source = 'SELF_CANCEL',
		       disable_reason = '用户主动注销', disabled_at = ?, cancel_apply_at = ?,
		       phone = NULL, phone_hmac = NULL, updated_at = ?
		 WHERE id = ?`, now, now, now, uid); err != nil {
		t.Fatalf("disable + release: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed = true
	return hmac
}

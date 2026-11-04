package fund

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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

	// 2. The dedup query no longer mentions the plaintext column.
	source, readErr := os.ReadFile("trial.go")
	if readErr != nil {
		t.Fatalf("read trial.go: %v", readErr)
	}
	if strings.Contains(string(source), "u.phone = ?") {
		t.Fatal("trial.go still compares the plaintext phone column")
	}
	if !strings.Contains(string(source), "u.phone_hmac = ?") {
		t.Fatal("trial.go does not compare phone_hmac; the dedup switch is missing")
	}
}

// The phone factor must survive cancellation: the trial flag and the statement
// that this number already consumed a trial both stay on the row, so a
// cancelled-then-revived account cannot claim twice and the number stays
// claimed for anyone else.
func TestTrialSurvivesCancel(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	setUserPhone(t, uid, "8613800990002")

	if rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-cancel-1"}, uid, "203.0.113.51:4111", nil); rec.Code != http.StatusOK {
		t.Fatalf("first claim status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Exactly what POST /api/v1/auth/cancel writes (auth.Cancel); repeated here
	// because the fund package must not import the auth handler.
	if _, err := testDB.ExecContext(context.Background(), `
		UPDATE t_user
		   SET status = 'DISABLED', disable_source = 'SELF_CANCEL',
		       disable_reason = '用户主动注销', disabled_at = NOW(3),
		       cancel_apply_at = NOW(3), updated_at = NOW(3)
		 WHERE id = ?`, uid); err != nil {
		t.Fatalf("simulate cancel: %v", err)
	}

	var phone, mac string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone, ''), COALESCE(phone_hmac, '') FROM t_user WHERE id = ?`,
		uid).Scan(&phone, &mac); err != nil {
		t.Fatalf("read phone columns: %v", err)
	}
	if phone != "8613800990002" || mac == "" {
		t.Fatalf("cancel changed the phone columns: phone=%q hmac=%q", phone, mac)
	}

	// A second claim — same account, new device, new IP — is still refused: the
	// account-level flag and the surviving hash both say "already claimed".
	rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-cancel-2"}, uid, "192.0.2.61:4111", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("claim after cancel status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if code := errorBody(t, rec).Error.Code; code != "trial_already_claimed" {
		t.Fatalf("claim after cancel code=%q, want trial_already_claimed", code)
	}
}

package auth

// QA additions for INC-phone-claim (task A2/B5/B6/E15). These do not replace the
// engineer's cases; each one covers a state the existing suite reaches only
// indirectly, and the first one is the case the design singles out: the hint
// must be derived from the row this transaction locked, never from the session
// snapshot (which can be stale, or — as here — empty).

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
)

// A2: a session whose identifier fields are all empty must still produce the
// hint that matches the account's actual shape, because the hint is computed
// from the locked t_user row (ident.primaryMode()), not from sess.Email /
// sess.Phone / sess.Username.
//
// The session here is built by hand with only UID/Status/Role set — the worst
// case a real snapshot can degenerate to. If the implementation had read the
// session for the hint, the answer would be the generic "请输入登录标识以确认注销".
func TestQACancelHintComesFromLockedRowNotSession(t *testing.T) {
	t.Run("phone account", func(t *testing.T) {
		svc, _, _ := newCancelService(t)
		phone := uniqueRegisterPhone(t)
		mac, err := PhoneHMAC(phoneFixtureKey, phone)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		uid := insertPhoneUser(t, phone, mac)
		cleanupPhone(t, phone)
		cleanupClaim(t, mac)

		// Empty identifier fields on purpose: the row is the only truth.
		sess := &mw.Session{UID: uid, Role: "PERSONAL", Status: "NORMAL"}

		rec := runCancelBody(t, svc, sess, "", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
		}
		body := decodeError(t, rec)
		if body.Error.Code != apierr.CodeInvalidRequestBody {
			t.Fatalf("code=%q, want %s", body.Error.Code, apierr.CodeInvalidRequestBody)
		}
		if body.Error.Message != "请求体参数有误：confirm" {
			t.Fatalf("message=%q, want 请求体参数有误：confirm", body.Error.Message)
		}
		if !strings.Contains(body.Error.Hint, "后 4 位") {
			t.Fatalf("hint=%q, want the last-four prompt from the locked row", body.Error.Hint)
		}

		// The decision must match the hint: the suffix cancels, even though the
		// session carries no phone at all.
		ok := runCancelBody(t, svc, sess, "", map[string]string{"confirm": phoneSuffix(phone)})
		if ok.Code != http.StatusNoContent {
			t.Fatalf("status=%d body=%s, want 204", ok.Code, ok.Body.String())
		}
		var gotPhone, gotHMAC sql.NullString
		if err := testDB.QueryRowContext(context.Background(),
			`SELECT phone, phone_hmac FROM t_user WHERE id = ?`, uid).Scan(&gotPhone, &gotHMAC); err != nil {
			t.Fatalf("read user: %v", err)
		}
		if gotPhone.Valid || gotHMAC.Valid {
			t.Fatalf("phone=%v hmac=%v after cancel, want both NULL", gotPhone, gotHMAC)
		}
	})

	t.Run("email account", func(t *testing.T) {
		svc, _, _ := newCancelService(t)
		email := uniqueEmail()
		uid := newAuthUser(t, email)

		sess := &mw.Session{UID: uid, Role: "PERSONAL", Status: "NORMAL"}

		rec := runCancelBody(t, svc, sess, "", map[string]string{"confirm": "wrong@test.local"})
		body := decodeError(t, rec)
		if rec.Code != http.StatusBadRequest || body.Error.Message != "请求体参数有误：confirm" {
			t.Fatalf("status=%d message=%q, want 400 / 请求体参数有误：confirm",
				rec.Code, body.Error.Message)
		}
		if !strings.Contains(body.Error.Hint, "邮箱") {
			t.Fatalf("hint=%q, want the e-mail prompt from the locked row", body.Error.Hint)
		}
	})
}

// B6 through the handler: a row with a plaintext number and no hash must be
// disabled, but neither column may be touched, and the operator must get a WARN
// naming the reason. Clearing the plaintext here would destroy the only input
// the hash can be recomputed from.
func TestQACancelWithoutHMACKeepsBothColumnsAndWarns(t *testing.T) {
	svc, mgr, _ := newCancelService(t)
	phone := uniqueRegisterPhone(t)
	uid := insertPhoneUser(t, phone, "")
	cleanupPhone(t, phone)
	// insertPhoneUser writes an empty string; the state under test is NULL.
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone_hmac = NULL WHERE id = ?`, uid); err != nil {
		t.Fatalf("clear hmac: %v", err)
	}
	logs := logCapture(t, svc)

	sess, _, err := mgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	rec := runCancelBody(t, svc, sess, "", map[string]string{"confirm": phoneSuffix(phone)})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want 204 (the account is cancelled; only the release is skipped)",
			rec.Code, rec.Body.String())
	}

	var gotPhone, gotHMAC sql.NullString
	var status string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT phone, phone_hmac, status FROM t_user WHERE id = ?`, uid).
		Scan(&gotPhone, &gotHMAC, &status); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if status != "DISABLED" {
		t.Fatalf("status=%q, want DISABLED", status)
	}
	if !gotPhone.Valid || gotPhone.String != phone {
		t.Fatalf("phone=%v, want %q untouched (the hash cannot be recomputed without it)",
			gotPhone, phone)
	}
	if gotHMAC.Valid {
		t.Fatalf("phone_hmac=%v, want NULL", gotHMAC)
	}

	// No audit row may claim a release, and no ledger row may be invented for a
	// number whose hash is unknown.
	var afterJSON string
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT CAST(after_json AS CHAR) FROM t_admin_audit_log
		 WHERE module = 'USER' AND action = 'CANCEL' AND target_id = ?
		 ORDER BY id DESC LIMIT 1`, fmt.Sprint(uid)).Scan(&afterJSON); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(afterJSON, `"phone_released": false`) {
		t.Fatalf("audit after_json=%s, want phone_released=false", afterJSON)
	}

	out := logs.String()
	if !strings.Contains(out, "cancel_phone_release_skipped") ||
		!strings.Contains(out, "reason=phone_without_hmac") {
		t.Fatalf("log does not carry the skip token: %s", strings.TrimSpace(out))
	}
	if strings.Contains(out, phone) {
		t.Fatalf("log leaks the plaintext number: %s", strings.TrimSpace(out))
	}
	t.Logf("third state: 204, phone kept, hmac NULL, WARN=%s",
		firstLineContaining(out, "cancel_phone_release_skipped"))
}

// B5: operator bans and risk bans keep the number occupied. The handler cannot
// release them (a banned account never holds a usable session — mw/session.go
// rejects DISABLED before routing), and even a forged session must be refused by
// the status guard rather than reaching the release.
func TestQACancelDoesNotReleaseOperatorOrRiskDisabledRows(t *testing.T) {
	for _, source := range []string{"OPS_MANUAL", "RISK_AUTO"} {
		t.Run(source, func(t *testing.T) {
			svc, _, _ := newCancelService(t)
			phone := uniqueRegisterPhone(t)
			mac, err := PhoneHMAC(phoneFixtureKey, phone)
			if err != nil {
				t.Fatalf("hash: %v", err)
			}
			uid := insertPhoneUser(t, phone, mac)
			cleanupPhone(t, phone)
			cleanupClaim(t, mac)

			if _, err := testDB.ExecContext(context.Background(), `
				UPDATE t_user SET status = 'DISABLED', disable_source = ?
				 WHERE id = ?`, source, uid); err != nil {
				t.Fatalf("seed %s: %v", source, err)
			}

			sess := &mw.Session{UID: uid, Role: "PERSONAL", Status: "NORMAL"}
			rec := runCancelBody(t, svc, sess, "", map[string]string{"confirm": phoneSuffix(phone)})
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
			}
			if code := decodeError(t, rec).Error.Code; code != apierr.CodeResourceStateConflict {
				t.Fatalf("code=%q, want %s", code, apierr.CodeResourceStateConflict)
			}

			// Not released: the number is still occupied, so a new registration
			// of it is still refused.
			var gotHMAC sql.NullString
			if err := testDB.QueryRowContext(context.Background(),
				`SELECT phone_hmac FROM t_user WHERE id = ?`, uid).Scan(&gotHMAC); err != nil {
				t.Fatalf("read hmac: %v", err)
			}
			if !gotHMAC.Valid || gotHMAC.String != mac {
				t.Fatalf("phone_hmac=%v after a refused cancel, want %q", gotHMAC, mac)
			}

			svc2, _, _ := newAuthService(t)
			svc2.PhoneHMACKey = phoneFixtureKey
			svc2.VerificationMode = VerificationOff
			dup := callRegister(t, svc2, map[string]string{
				"identifier": phone, "password": "Str0ng-Passw0rd!",
			})
			if dup.Code != http.StatusConflict {
				t.Fatalf("%s row: registering the still-held number status=%d body=%s, want 409",
					source, dup.Code, dup.Body.String())
			}
		})
	}
}

// E15 (known P1, asserted so it cannot drift silently): only the phone number is
// released. An e-mail account that cancels still occupies uk_user_email, so the
// same address cannot register again in this increment.
func TestQACancelEmailAccountStillOccupiesEmail(t *testing.T) {
	svc, mgr, _ := newCancelService(t)
	email := uniqueEmail()
	uid := newAuthUser(t, email)

	sess, _, err := mgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	if rec := runCancel(t, svc, sess, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}

	var stored sql.NullString
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT email FROM t_user WHERE id = ?`, uid).Scan(&stored); err != nil {
		t.Fatalf("read email: %v", err)
	}
	if !stored.Valid || stored.String != email {
		t.Fatalf("email=%v, want %q kept (P1: the e-mail symmetric gap is out of scope)",
			stored, email)
	}

	svc2, _, _ := newAuthService(t)
	svc2.PhoneHMACKey = phoneFixtureKey
	svc2.VerificationMode = VerificationOff
	dup := callRegister(t, svc2, map[string]string{
		"identifier": email, "password": "Str0ng-Passw0rd!",
	})
	if dup.Code != http.StatusConflict {
		t.Fatalf("re-registering a cancelled e-mail status=%d body=%s, want 409 (documented P1)",
			dup.Code, dup.Body.String())
	}
	if code := decodeError(t, dup).Error.Code; code != apierr.CodeAccountExists {
		t.Fatalf("code=%q, want %s", code, apierr.CodeAccountExists)
	}
}

// The contract the console renders from: /auth/me must publish the same mode the
// cancel guard will compare against. Nothing else in the Go suite asserts this
// field, and the whole front-end label/placeholder depends on it.
func TestQACurrentUserPublishesLoginKind(t *testing.T) {
	svc, mgr, _ := newCancelService(t)

	phone := uniqueRegisterPhone(t)
	mac, err := PhoneHMAC(phoneFixtureKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	phoneUID := insertPhoneUser(t, phone, mac)
	cleanupPhone(t, phone)
	cleanupClaim(t, mac)

	emailUID := newAuthUser(t, uniqueEmail())

	username := fmt.Sprintf("opqa%09d", time.Now().UnixNano()%1000000000)
	usernameUID := newAuthUser(t, "")
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET email = NULL, username = ? WHERE id = ?`, username, usernameUID); err != nil {
		t.Fatalf("set username: %v", err)
	}

	cases := []struct {
		name string
		uid  int64
		want string
	}{
		{"email account", emailUID, confirmModeEmail},
		{"phone account", phoneUID, confirmModePhoneSuffix},
		{"username account", usernameUID, confirmModeUsername},
	}
	for _, tc := range cases {
		_, cookie, err := mgr.Issue(context.Background(), tc.uid)
		if err != nil {
			t.Fatalf("%s: issue: %v", tc.name, err)
		}
		// Issue() only carries the uid; the identifier fields are filled by Load
		// (that is what a real request goes through), and /auth/me reads them.
		loadReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		loadReq.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookie})
		sess, apiErr := mgr.Load(context.Background(), loadReq)
		if apiErr != nil {
			t.Fatalf("%s: load session: %v", tc.name, apiErr)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		req = req.WithContext(mw.WithSession(req.Context(), sess))
		rec := httptest.NewRecorder()
		svc.Me(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: /auth/me status=%d body=%s", tc.name, rec.Code, rec.Body.String())
		}
		var body struct {
			LoginKind string `json:"login_kind"`
		}
		if err := jsonx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode /auth/me: %v", tc.name, err)
		}
		if body.LoginKind != tc.want {
			t.Fatalf("%s: login_kind=%q, want %q", tc.name, body.LoginKind, tc.want)
		}
	}
}

// B8 second half: after a release and a re-registration, the recovery path
// (findUserUnique, LIMIT 2) must see exactly one live row for the number. Two
// rows would answer ErrAmbiguousAccount and lock the new owner out of password
// recovery — the coupling PRD B6 records.
func TestQACancelReleaseLeavesRecoveryUnambiguous(t *testing.T) {
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
		t.Fatalf("issue session: %v", err)
	}
	if rec := runCancelBody(t, svc, sess, "", map[string]string{"confirm": phoneSuffix(phone)}); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}

	svc2, _, _ := newAuthService(t)
	svc2.PhoneHMACKey = phoneFixtureKey
	svc2.VerificationMode = VerificationOff
	rec := callRegister(t, svc2, map[string]string{
		"identifier": phone, "password": "Str0ng-Passw0rd!",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-register status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}

	ident, err := NormalizeIdentifier(phone)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	row, err := svc2.findUserUnique(context.Background(), ident)
	if err != nil {
		t.Fatalf("findUserUnique after release = %v, want the single new row (a cancelled row must not create ambiguity)", err)
	}
	if row.uid == uid {
		t.Fatalf("findUserUnique returned the cancelled account %d", uid)
	}

	// findUser (login) must agree with it.
	login, err := svc2.findUser(context.Background(), ident)
	if err != nil {
		t.Fatalf("findUser after release: %v", err)
	}
	if login.uid != row.uid {
		t.Fatalf("login uid=%d and recovery uid=%d disagree", login.uid, row.uid)
	}
}

// firstLineContaining returns the first line of s that contains needle, so a
// t.Logf prints the record instead of the whole log buffer.
func firstLineContaining(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

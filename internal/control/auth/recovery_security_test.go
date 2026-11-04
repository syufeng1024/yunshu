package auth

// Independent security regressions for the self-service password recovery
// increment (written by QA, kept as a permanent suite).
//
// These tests are written from the acceptance table in
// docs/tasks/INC-password-recovery-design.md §10 and the security rules in §7/§11,
// and they deliberately re-derive every expectation from that document instead of
// from the handlers under test. They are additive: no production file is touched.
//
// Coverage focus (the items the shipped suite does not already pin):
//   - cross-account and cross-purpose replay of a reset code
//   - the five-attempt burn through the HTTP endpoint (not the internal seam)
//   - password_is_initial -> 0 on the reset path
//   - the log red line in smtp/provider mode (the code must not appear)
//   - bind email: session survives, /auth/me carries the new address, the new
//     address + old password can log in
//   - the handset channel end to end with the real internal/notify/sms client in
//     log mode (code read back out of the log line it printed)

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/notify/sms"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
)

// qaSendCodeLogRE pulls the code out of the log-mode line the way the operator
// does. It is anchored on the wording of the message, so it also fails if the
// body stops carrying the code a human could retype.
var qaSendCodeLogRE = regexp.MustCompile(`验证码 (\d{6})`)

// TestQA_ResetSendCodeZeroDistinction re-derives the §3.1 table: a usable
// account, an unknown address, a soft-deleted account, a DISABLED account and an
// ambiguous handset number must produce byte-identical 200 bodies, and only the
// usable cases may actually send a message.
func TestQA_ResetSendCodeZeroDistinction(t *testing.T) {
	svc, mailer, smsCap, _, _ := recoveryService(t)

	usableEmail := uniqueVerifyEmail(t, "qa-zd-usable")
	newRecoveryUser(t, usableEmail, "", "NORMAL")
	base := callResetSendCode(t, svc, usableEmail)
	if base.Code != http.StatusOK || mailer.sends != 1 {
		t.Fatalf("usable: status=%d sends=%d body=%s", base.Code, mailer.sends, base.Body.String())
	}
	emailBody := base.Body.String()

	unknown := uniqueVerifyEmail(t, "qa-zd-unknown")
	soft := uniqueVerifyEmail(t, "qa-zd-soft")
	softDelete(t, newRecoveryUser(t, soft, "", "NORMAL"))
	disabled := uniqueVerifyEmail(t, "qa-zd-disabled")
	newRecoveryUser(t, disabled, "", "DISABLED")

	for _, email := range []string{unknown, soft, disabled} {
		rec := callResetSendCode(t, svc, email)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s, want 200", email, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != emailBody {
			t.Fatalf("%s: body differs\n got: %s\nwant: %s", email, rec.Body.String(), emailBody)
		}
	}
	if mailer.sends != 1 {
		t.Fatalf("an unusable mailbox triggered a send: sends=%d", mailer.sends)
	}

	// Handset: one live row sends, two rows sharing the same number must not.
	phone := uniquePhone()
	newRecoveryUser(t, "", phone, "NORMAL")
	phoneOK := callResetSendCode(t, svc, phone)
	if phoneOK.Code != http.StatusOK || smsCap.sends != 1 {
		t.Fatalf("handset: status=%d sends=%d body=%s", phoneOK.Code, smsCap.sends, phoneOK.Body.String())
	}
	ambiguous := uniquePhone()
	newRecoveryUser(t, "", ambiguous, "NORMAL")
	newRecoveryUser(t, "", ambiguous, "NORMAL")
	ambiguousRec := callResetSendCode(t, svc, ambiguous)
	if ambiguousRec.Code != http.StatusOK {
		t.Fatalf("ambiguous handset: status=%d body=%s, want 200", ambiguousRec.Code, ambiguousRec.Body.String())
	}
	if ambiguousRec.Body.String() != phoneOK.Body.String() {
		t.Fatalf("ambiguous handset body differs\n got: %s\nwant: %s",
			ambiguousRec.Body.String(), phoneOK.Body.String())
	}
	if smsCap.sends != 1 {
		t.Fatalf("an ambiguous handset triggered a text: sends=%d", smsCap.sends)
	}
}

// TestQA_ResetCodeNotReplayableAcrossAccountsOrPurposes covers design §12 R3 from
// the angle the shipped suite does not: a valid reset code issued to account B
// must not reset account A, and the failure must be the same body as a plain
// wrong code (no account oracle). Failed replays must also not burn A's code.
func TestQA_ResetCodeNotReplayableAcrossAccountsOrPurposes(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)

	victim := uniqueVerifyEmail(t, "qa-rp-victim")
	newRecoveryUser(t, victim, "", "NORMAL")
	attacker := uniqueVerifyEmail(t, "qa-rp-attacker")
	newRecoveryUser(t, attacker, "", "NORMAL")

	// A real code for the victim, so "code exists" is not the reason the replay
	// fails, and a real code for the attacker.
	if rec := callResetSendCode(t, svc, victim); rec.Code != http.StatusOK {
		t.Fatalf("victim send status=%d body=%s", rec.Code, rec.Body.String())
	}
	victimCode := mailer.lastCode(t)
	if rec := callResetSendCode(t, svc, attacker); rec.Code != http.StatusOK {
		t.Fatalf("attacker send status=%d body=%s", rec.Code, rec.Body.String())
	}
	attackerCode := mailer.lastCode(t)
	if victimCode == attackerCode {
		t.Fatalf("the two accounts drew the same code %q; rerun", victimCode)
	}

	// 1) Account B's reset code against account A's identifier.
	crossAccount := callResetPassword(t, svc, map[string]string{
		"identifier": victim, "code": attackerCode, "new_password": testNewPassword,
	})
	if crossAccount.Code != http.StatusUnauthorized {
		t.Fatalf("another account's code reset the victim: status=%d body=%s",
			crossAccount.Code, crossAccount.Body.String())
	}
	if body := decodeError(t, crossAccount); body.Error.Code != "invalid_credentials" {
		t.Fatalf("code=%q, want invalid_credentials", body.Error.Code)
	}

	// 2) A plain wrong code must answer with exactly the same error object: no
	//    oracle. request_id is per-request by design, so the comparison is the
	//    envelope minus that field.
	plainWrong := callResetPassword(t, svc, map[string]string{
		"identifier": victim, "code": "000000", "new_password": testNewPassword,
	})
	if plainWrong.Code != crossAccount.Code {
		t.Fatalf("cross-account status=%d, wrong-code status=%d", crossAccount.Code, plainWrong.Code)
	}
	got, want := decodeError(t, crossAccount), decodeError(t, plainWrong)
	if got != want {
		t.Fatalf("cross-account failure is distinguishable from a wrong code\n got: %+v\nwant: %+v", got, want)
	}

	// 3) A register-purpose code must not reset either.
	registerTarget := uniqueVerifyEmail(t, "qa-rp-register")
	if rec := callSendCode(t, svc, registerTarget, PurposeRegister); rec.Code != http.StatusOK {
		t.Fatalf("register send status=%d body=%s", rec.Code, rec.Body.String())
	}
	registerCode := mailer.lastCode(t)
	if registerCode == victimCode || registerCode == attackerCode {
		t.Fatalf("code collision across purposes; rerun")
	}
	// The register code belongs to a different address, so use its own identifier:
	// this is "register code replayed into reset for the same address".
	newRecoveryUser(t, registerTarget, "", "NORMAL")
	registerReplay := callResetPassword(t, svc, map[string]string{
		"identifier": registerTarget, "code": registerCode, "new_password": testNewPassword,
	})
	if registerReplay.Code != http.StatusUnauthorized {
		t.Fatalf("a register code reset a password: status=%d body=%s",
			registerReplay.Code, registerReplay.Body.String())
	}

	// 4) Nothing changed: the victim's code still works, and both old passwords
	//    still authenticate.
	if rec := callResetPassword(t, svc, map[string]string{
		"identifier": victim, "code": victimCode, "new_password": testNewPassword,
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("the victim's own code was burned by the replays: status=%d body=%s",
			rec.Code, rec.Body.String())
	}
	if rec := doLogin(t, svc, victim, testNewPassword, uniqueIP()); rec.Code != http.StatusOK {
		t.Fatalf("victim new password login status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doLogin(t, svc, attacker, testPassword, uniqueIP()); rec.Code != http.StatusOK {
		t.Fatalf("attacker password changed: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestQA_ResetBurnsCodeAfterFiveWrongAttempts drives the §7.2 attempt cap through
// the public endpoint: after five wrong submissions the sixth — with the correct
// code — must still fail, and the password must be unchanged.
func TestQA_ResetBurnsCodeAfterFiveWrongAttempts(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	email := uniqueVerifyEmail(t, "qa-attempts")
	newRecoveryUser(t, email, "", "NORMAL")

	if rec := callResetSendCode(t, svc, email); rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := mailer.lastCode(t)
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := 0; i < verificationMaxAttempts; i++ {
		rec := callResetPassword(t, svc, map[string]string{
			"identifier": email, "code": wrong, "new_password": testNewPassword,
		})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d body=%s, want 401", i+1, rec.Code, rec.Body.String())
		}
	}
	after := callResetPassword(t, svc, map[string]string{
		"identifier": email, "code": code, "new_password": testNewPassword,
	})
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("the correct code still worked after %d wrong attempts: status=%d body=%s",
			verificationMaxAttempts, after.Code, after.Body.String())
	}
	if rec := doLogin(t, svc, email, testPassword, uniqueIP()); rec.Code != http.StatusOK {
		t.Fatalf("original password no longer works: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doLogin(t, svc, email, testNewPassword, uniqueIP()); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the password was changed despite the failed reset: status=%d", rec.Code)
	}
}

// TestQA_ResetClearsPasswordIsInitial pins §3.2 / §11-8: a self-service reset
// means the user chose the password, so the "still using the initial password"
// banner must turn off.
func TestQA_ResetClearsPasswordIsInitial(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	email := uniqueVerifyEmail(t, "qa-initial")
	uid := newRecoveryUser(t, email, "", "NORMAL")
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET password_is_initial = 1 WHERE id = ?`, uid); err != nil {
		t.Fatalf("seed password_is_initial: %v", err)
	}

	if rec := callResetSendCode(t, svc, email); rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := callResetPassword(t, svc, map[string]string{
		"identifier": email, "code": mailer.lastCode(t), "new_password": testNewPassword,
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("reset status=%d body=%s", rec.Code, rec.Body.String())
	}

	var initial bool
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT password_is_initial FROM t_user WHERE id = ?`, uid).Scan(&initial); err != nil {
		t.Fatalf("read password_is_initial: %v", err)
	}
	if initial {
		t.Fatalf("password_is_initial survived the self-service reset")
	}
	rec := doLogin(t, svc, email, testNewPassword, uniqueIP())
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"password_warning":false`) {
		t.Fatalf("login still reports the initial-password warning: %s", rec.Body.String())
	}
}

// TestQA_LogRedLineSmtpAndProvider is §7.5 / §11-5: the code may appear in a log
// only in log mode. In smtp and provider mode neither the code nor the plaintext
// address may reach the log line.
func TestQA_LogRedLineSmtpAndProvider(t *testing.T) {
	var buf bytes.Buffer
	logger := obslog.NewWithWriter("controld", "test", &buf, clock.Real{})

	svc, _, _ := newAuthService(t)
	svc.Logger = logger
	mailer := &captureMailer{}
	smsCap := &captureSMS{}
	svc.VerificationMode = VerificationSMTP
	svc.Mailer = mailer
	svc.SMSMode = SMSProvider
	svc.SMSSender = smsCap

	// smtp mode, mailbox channel.
	email := uniqueVerifyEmail(t, "qa-redline")
	newRecoveryUser(t, email, "", "NORMAL")
	if rec := callResetSendCode(t, svc, email); rec.Code != http.StatusOK {
		t.Fatalf("mailbox send status=%d body=%s", rec.Code, rec.Body.String())
	}
	mailCode := mailer.lastCode(t)
	logs := buf.String()
	if strings.Contains(logs, mailCode) {
		t.Fatalf("smtp mode logged the verification code %q: %s", mailCode, logs)
	}
	if strings.Contains(logs, email) {
		t.Fatalf("smtp mode logged the plaintext address %q: %s", email, logs)
	}

	// provider mode, handset channel.
	buf.Reset()
	phone := uniquePhone()
	newRecoveryUser(t, "", phone, "NORMAL")
	if rec := callResetSendCode(t, svc, phone); rec.Code != http.StatusOK {
		t.Fatalf("handset send status=%d body=%s", rec.Code, rec.Body.String())
	}
	if smsCap.sends != 1 {
		t.Fatalf("provider mode did not hand the message to the sender: sends=%d", smsCap.sends)
	}
	smsCode := smsCap.lastCode(t)
	logs = buf.String()
	if strings.Contains(logs, smsCode) {
		t.Fatalf("provider mode logged the verification code %q: %s", smsCode, logs)
	}
	if strings.Contains(logs, phone) {
		t.Fatalf("provider mode logged the plaintext handset %q: %s", phone, logs)
	}
}

// TestQA_BindEmailKeepsSessionAndEnablesLogin covers §3.4 / PRD P0-3⑤: binding
// does not revoke the session, /auth/me reports the new address afterwards, and
// the new address plus the old password logs in.
func TestQA_BindEmailKeepsSessionAndEnablesLogin(t *testing.T) {
	svc, mailer, _, sessMgr, _ := recoveryService(t)
	uid := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	sess, cookie, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	target := uniqueVerifyEmail(t, "qa-bind")

	if rec := callBindCode(t, svc, sess, target); rec.Code != http.StatusOK {
		t.Fatalf("bind code status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := callBindEmail(t, svc, sess, target, mailer.lastCode(t)); rec.Code != http.StatusOK {
		t.Fatalf("bind status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The session is still valid and now resolves the new address out of t_user.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookie})
	loaded, apiErr := sessMgr.Load(context.Background(), req)
	if apiErr != nil || loaded == nil {
		t.Fatalf("binding revoked the session: %v", apiErr)
	}
	if loaded.Email != target {
		t.Fatalf("session still resolves email=%q, want %q", loaded.Email, target)
	}

	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	meReq = meReq.WithContext(mw.WithSession(meReq.Context(), loaded))
	meRec := httptest.NewRecorder()
	svc.Me(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("/auth/me status=%d body=%s, want 200", meRec.Code, meRec.Body.String())
	}
	if !strings.Contains(meRec.Body.String(), target) {
		t.Fatalf("/auth/me does not report the new address: %s", meRec.Body.String())
	}

	// The new address is now a login identifier with the unchanged password.
	if rec := doLogin(t, svc, target, testPassword, uniqueIP()); rec.Code != http.StatusOK {
		t.Fatalf("login with the bound address status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestQA_BindQuotaNotChargedByCooldown is the F1 regression, written from the QA
// side: the per-account daily budget must count codes that were actually issued,
// not attempts the cheaper limits refused. Before the fix, five clicks on one
// address (four of them cooldown 429s, one message sent) exhausted the whole day.
func TestQA_BindQuotaNotChargedByCooldown(t *testing.T) {
	svc, mailer, _, _, rdb := recoveryService(t)
	uid := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	sess := bindSession(uid)
	target := uniqueVerifyEmail(t, "qa-bindquota")
	quotaKey := redisx.VerifyCodeUIDQuotaKey(PurposeBind, uid)

	readQuota := func() int {
		t.Helper()
		n, err := rdb.Raw().Get(context.Background(), quotaKey).Int()
		if err != nil {
			t.Fatalf("read %s: %v", quotaKey, err)
		}
		return n
	}

	if rec := callBindCodeFrom(t, svc, sess, target, uniqueTestIP()); rec.Code != http.StatusOK {
		t.Fatalf("first send status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readQuota(); got != 1 {
		t.Fatalf("uid quota after one issued code=%d, want 1", got)
	}

	// Three more clicks on the same address: every one is refused by the 60s
	// cooldown, and none of them may spend account budget.
	for i := 0; i < 3; i++ {
		rec := callBindCodeFrom(t, svc, sess, target, uniqueTestIP())
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("repeat %d status=%d body=%s, want 429 from the cooldown", i+1, rec.Code, rec.Body.String())
		}
	}
	if got := readQuota(); got != 1 {
		t.Fatalf("uid quota=%d after three cooldown refusals, want 1 (F1 regression)", got)
	}
	if mailer.sends != 1 {
		t.Fatalf("sends=%d, want 1: only the first request ever delivered", mailer.sends)
	}

	// A brand-new address is therefore still available.
	if rec := callBindCodeFrom(t, svc, sess, uniqueVerifyEmail(t, "qa-bindquota2"), uniqueTestIP()); rec.Code != http.StatusOK {
		t.Fatalf("fresh address after cooldown refusals status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if got := readQuota(); got != 2 {
		t.Fatalf("uid quota=%d after the second issued code, want 2", got)
	}
	if mailer.sends != 2 {
		t.Fatalf("sends=%d, want 2", mailer.sends)
	}

	// Phase 2: at the day's ceiling the refusal must not leave an undeliverable
	// code behind — nobody received it, so it must not exist in Redis either.
	for i := 0; i < bindPurposeQuotaLimit-2; i++ {
		rec := callBindCodeFrom(t, svc, sess, uniqueVerifyEmail(t, "qa-bindquota-fill"), uniqueTestIP())
		if rec.Code != http.StatusOK {
			t.Fatalf("fill %d status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if got := readQuota(); got != bindPurposeQuotaLimit {
		t.Fatalf("uid quota=%d, want the ceiling %d", got, bindPurposeQuotaLimit)
	}
	overflow := uniqueVerifyEmail(t, "qa-bindquota-over")
	if rec := callBindCodeFrom(t, svc, sess, overflow, uniqueTestIP()); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request at the ceiling status=%d body=%s, want 429", rec.Code, rec.Body.String())
	}
	overflowIdent, err := NormalizeIdentifier(overflow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	codeKey := redisx.VerifyCodeKey(PurposeBind, identifierHash(overflowIdent.Value))
	if n, err := rdb.Raw().Exists(context.Background(), codeKey).Result(); err != nil || n != 0 {
		t.Fatalf("a code issued moments before the refusal survived (exists=%d err=%v)", n, err)
	}
	if mailer.sends != bindPurposeQuotaLimit {
		t.Fatalf("sends=%d, want %d", mailer.sends, bindPurposeQuotaLimit)
	}
}

// TestQA_SMSLogModeEndToEnd is checklist item C16: with AUTH_SMS_MODE=log and the
// real sms.Client, a handset user asks for a code, reads it out of the log line,
// resets the password and signs in with it.
func TestQA_SMSLogModeEndToEnd(t *testing.T) {
	var buf bytes.Buffer
	logger := obslog.NewWithWriter("controld", "test", &buf, clock.Real{})

	svc, _, _ := newAuthService(t)
	svc.Logger = logger
	svc.VerificationMode = VerificationSMTP
	svc.Mailer = &captureMailer{}
	svc.SMSMode = SMSLog
	svc.SMSSender = &sms.Client{Mode: sms.ModeLog, Logger: logger}

	phone := uniquePhone()
	uid := newRecoveryUser(t, "", phone, "NORMAL")

	sent := callResetSendCode(t, svc, phone)
	if sent.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", sent.Code, sent.Body.String())
	}
	if !strings.Contains(sent.Body.String(), `"channel":"sms"`) {
		t.Fatalf("handset response does not report channel=sms: %s", sent.Body.String())
	}
	logs := buf.String()
	if strings.Contains(logs, phone) {
		t.Fatalf("log mode printed the plaintext handset: %s", logs)
	}
	match := qaSendCodeLogRE.FindStringSubmatch(logs)
	if match == nil {
		t.Fatalf("no retypeable code in the log-mode line: %s", logs)
	}
	code := match[1]

	reset := callResetPassword(t, svc, map[string]string{
		"identifier": phone, "code": code, "new_password": testNewPassword,
	})
	if reset.Code != http.StatusNoContent {
		t.Fatalf("reset status=%d body=%s, want 204", reset.Code, reset.Body.String())
	}
	if rec := doLogin(t, svc, phone, testNewPassword, uniqueIP()); rec.Code != http.StatusOK {
		t.Fatalf("login with the new password status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doLogin(t, svc, phone, testPassword, uniqueIP()); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the old password still works: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// §7.5: the audit row records the channel the proof arrived on, and it
	// carries no password, no code and no plaintext number.
	var after string
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT after_json FROM t_admin_audit_log
		 WHERE operator_id = ? AND module='USER' AND action='UPDATE' AND result='OK'
		 ORDER BY id DESC LIMIT 1`, uid).Scan(&after); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	for _, want := range []string{`"sessions_revoked": "all"`, `"via": "reset:sms"`} {
		if !strings.Contains(after, want) {
			t.Fatalf("after_json %s does not contain %s", after, want)
		}
	}
	for _, forbidden := range []string{phone, code, testNewPassword, "$2a$", "$2b$"} {
		if strings.Contains(after, forbidden) {
			t.Fatalf("after_json leaks %q: %s", forbidden, after)
		}
	}
}

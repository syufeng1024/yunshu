package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"yunshu.dev/core/internal/jsonx"
)

// captureMailer records the last message instead of sending it. The code is
// extracted from the body the way a user would read it, so the test also pins
// that the body actually contains a usable code.
type captureMailer struct {
	to, subject, body string
	sends             int
	err               error
}

func (m *captureMailer) Send(_ context.Context, to, subject, body string) error {
	m.sends++
	m.to, m.subject, m.body = to, subject, body
	return m.err
}

var sixDigits = regexp.MustCompile(`\b(\d{6})\b`)

func (m *captureMailer) lastCode(t *testing.T) string {
	t.Helper()
	if m.body == "" {
		t.Fatalf("no message was sent")
	}
	got := sixDigits.FindStringSubmatch(m.body)
	if got == nil {
		t.Fatalf("message body has no six-digit code: %q", m.body)
	}
	return got[1]
}

func verifierService(t *testing.T) (*Service, *captureMailer) {
	t.Helper()
	svc, _, _ := newAuthService(t)
	mailer := &captureMailer{}
	svc.VerificationMode = VerificationSMTP
	svc.Mailer = mailer
	return svc, mailer
}

func callSendCode(t *testing.T, svc *Service, identifier, purpose string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(map[string]string{"identifier": identifier, "purpose": purpose})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/verification-code", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	svc.SendVerificationCode(rec, req)
	return rec
}

func callRegister(t *testing.T, svc *Service, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	svc.Register(rec, req)
	return rec
}

func callLogin(t *testing.T, svc *Service, body map[string]string, ip string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(payload)))
	if ip != "" {
		req.RemoteAddr = ip + ":12345"
	}
	rec := httptest.NewRecorder()
	svc.Login(rec, req)
	return rec
}

// uniqueEmail scopes an address to this test and this run: the cooldown and the
// daily quota live in Redis, so a shared address would make the second run of
// the same test see 429 instead of the behaviour under test.
func uniqueVerifyEmail(t *testing.T, prefix string) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random: %v", err)
	}
	return prefix + "-" + hex.EncodeToString(b[:]) + "@test.local"
}

// TestSendCodeEmailsUsableCode is the happy path end to end: a code is mailed,
// the body carries it, and it registers an account exactly once.
func TestSendCodeEmailsUsableCode(t *testing.T) {
	svc, mailer := verifierService(t)
	email := uniqueVerifyEmail(t, "vc-ok")

	if rec := callSendCode(t, svc, email, PurposeRegister); rec.Code != http.StatusOK {
		t.Fatalf("send code status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if mailer.to != email {
		t.Fatalf("mail sent to %q, want %q", mailer.to, email)
	}
	code := mailer.lastCode(t)

	rec := callRegister(t, svc, map[string]string{
		"identifier": email, "password": "Str0ng-Passw0rd!", "code": code,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register with mailed code status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	// The same code cannot be replayed to create a second account.
	again := callRegister(t, svc, map[string]string{
		"identifier": uniqueVerifyEmail(t, "vc-ok2"), "password": "Str0ng-Passw0rd!", "code": code,
	})
	if again.Code == http.StatusOK {
		t.Fatalf("a consumed code registered a second account")
	}
}

// TestSendCodeCooldownStopsSecondMessage pins the 60s resend window. Without
// it one caller can drain the daily quota (and the SMTP reputation) in a loop.
func TestSendCodeCooldownStopsSecondMessage(t *testing.T) {
	svc, mailer := verifierService(t)
	email := uniqueVerifyEmail(t, "vc-cool")

	if rec := callSendCode(t, svc, email, PurposeRegister); rec.Code != http.StatusOK {
		t.Fatalf("first send status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := callSendCode(t, svc, email, PurposeRegister)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second send status=%d body=%s, want 429", rec.Code, rec.Body.String())
	}
	if mailer.sends != 1 {
		t.Fatalf("mailer saw %d sends, want 1 (the second must not be sent)", mailer.sends)
	}
}

// TestWrongCodeFiveTimesBurnsTheCode proves the attempt cap. It calls the
// service seam directly: going through Login would trip the account lockout at
// five failures and answer login_locked instead of exercising the code counter.
func TestWrongCodeFiveTimesBurnsTheCode(t *testing.T) {
	svc, mailer := verifierService(t)
	email := uniqueVerifyEmail(t, "vc-attempts")
	ident, err := NormalizeIdentifier(email)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec := callSendCode(t, svc, email, PurposeLogin); rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := mailer.lastCode(t)

	for i := 0; i < verificationMaxAttempts; i++ {
		if apiErr := svc.consumeVerificationCode(context.Background(), ident, PurposeLogin, "000000"); apiErr == nil {
			t.Fatalf("attempt %d: wrong code accepted", i+1)
		}
	}
	// The correct code must now be dead: a cap that only rejects wrong guesses
	// but still honours the right one is not a cap.
	if apiErr := svc.consumeVerificationCode(context.Background(), ident, PurposeLogin, code); apiErr == nil {
		t.Fatalf("correct code accepted after %d wrong attempts", verificationMaxAttempts)
	}
}

// TestCodeIsSingleUse: the second consume with the same code must fail.
func TestCodeIsSingleUse(t *testing.T) {
	svc, mailer := verifierService(t)
	email := uniqueVerifyEmail(t, "vc-once")
	ident, err := NormalizeIdentifier(email)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec := callSendCode(t, svc, email, PurposeLogin); rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := mailer.lastCode(t)

	if apiErr := svc.consumeVerificationCode(context.Background(), ident, PurposeLogin, code); apiErr != nil {
		t.Fatalf("first consume: %v", apiErr)
	}
	if apiErr := svc.consumeVerificationCode(context.Background(), ident, PurposeLogin, code); apiErr == nil {
		t.Fatalf("second consume accepted the same code")
	}
}

// TestCodePurposeIsolation: a login code must not register an account.
func TestCodePurposeIsolation(t *testing.T) {
	svc, mailer := verifierService(t)
	email := uniqueVerifyEmail(t, "vc-purpose")
	ident, err := NormalizeIdentifier(email)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec := callSendCode(t, svc, email, PurposeLogin); rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := mailer.lastCode(t)
	if apiErr := svc.consumeVerificationCode(context.Background(), ident, PurposeRegister, code); apiErr == nil {
		t.Fatalf("a login code was accepted for register")
	}
}

// TestVerificationLoginIssuesSession is the other half of the feature: login
// with a code and no password.
func TestVerificationLoginIssuesSession(t *testing.T) {
	svc, mailer := verifierService(t)
	email := uniqueVerifyEmail(t, "vc-login")
	newAuthUser(t, email)

	if rec := callSendCode(t, svc, email, PurposeLogin); rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := mailer.lastCode(t)

	rec := callLogin(t, svc, map[string]string{"identifier": email, "code": code}, "203.0.113.40")
	if rec.Code != http.StatusOK {
		t.Fatalf("code login status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Set-Cookie") == "" {
		t.Fatalf("code login issued no session cookie")
	}
}

// TestLoginRejectsPasswordAndCodeTogether: silently ignoring one factor would
// make it look like both were verified.
func TestLoginRejectsPasswordAndCodeTogether(t *testing.T) {
	svc, _ := verifierService(t)
	rec := callLogin(t, svc, map[string]string{
		"identifier": "both@test.local", "password": "x", "code": "123456",
	}, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

// TestSMSChannelIsDeliberatelyUnavailable: the SMS path is implemented as a
// refusal. Billing is per message, so an accidental enable would cost money;
// the endpoint must say so instead of pretending to send.
func TestSMSChannelIsDeliberatelyUnavailable(t *testing.T) {
	svc, mailer := verifierService(t)
	rec := callSendCode(t, svc, "13800138000", PurposeRegister)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sms status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if !strings.Contains(body.Error.Hint, "短信验证码暂未开放") {
		t.Fatalf("hint=%q does not explain that SMS is not available", body.Error.Hint)
	}
	if mailer.sends != 0 {
		t.Fatalf("sms request sent %d e-mails", mailer.sends)
	}
}

// TestRegisterRequiresCode: the account name IS the mailbox, so an unverified
// address is not an acceptable account.
func TestRegisterRequiresCode(t *testing.T) {
	svc, _ := verifierService(t)
	rec := callRegister(t, svc, map[string]string{
		"identifier": uniqueVerifyEmail(t, "vc-nocode"), "password": "Str0ng-Passw0rd!",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != "invalid_request_body" {
		t.Fatalf("code=%q, want invalid_request_body", body.Error.Code)
	}
}

// TestMailFailureDropsCode: a code that was stored but not delivered must not
// survive, or the user's retry would be blocked by the cooldown while the
// undelivered code sits in Redis.
func TestMailFailureDropsCode(t *testing.T) {
	svc, mailer := verifierService(t)
	mailer.err = errors.New("smtp down")
	email := uniqueVerifyEmail(t, "vc-mailfail")
	ident, err := NormalizeIdentifier(email)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	rec := callSendCode(t, svc, email, PurposeLogin)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
	if apiErr := svc.consumeVerificationCode(context.Background(), ident, PurposeLogin, "123456"); apiErr == nil {
		t.Fatalf("a code survived a failed delivery")
	}
}

// TestVerificationOffModeChangesRegisterBehaviour pins the "off" contract:
// registration works without a code, and a code that was sent anyway is
// rejected rather than ignored. The browser suite relies on exactly this (it
// creates accounts over the API and has no mailbox), so a change here breaks
// e2e/security before it breaks a unit test.
func TestVerificationOffModeChangesRegisterBehaviour(t *testing.T) {
	svc, _ := verifierService(t)
	svc.VerificationMode = VerificationOff

	rec := callRegister(t, svc, map[string]string{
		"identifier": uniqueVerifyEmail(t, "vc-off"), "password": "Str0ng-Passw0rd!",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register without a code in off mode status=%d body=%s, want 201",
			rec.Code, rec.Body.String())
	}

	withCode := callRegister(t, svc, map[string]string{
		"identifier": uniqueVerifyEmail(t, "vc-off2"), "password": "Str0ng-Passw0rd!", "code": "123456",
	})
	if withCode.Code != http.StatusBadRequest {
		t.Fatalf("a code sent in off mode status=%d body=%s, want 400",
			withCode.Code, withCode.Body.String())
	}
}

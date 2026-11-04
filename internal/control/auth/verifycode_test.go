package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
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

// ipSeq keeps every callSendCode on its own source address: the per-IP hourly
// ceiling is keyed by address, and reusing one address across unrelated tests
// would make them fail for a reason that has nothing to do with what they test.
var ipSeq atomic.Int64

func uniqueTestIP() string {
	n := ipSeq.Add(1)
	return fmt.Sprintf("10.%d.%d.%d", byte(n>>16), byte(n>>8), byte(n))
}

func callSendCode(t *testing.T, svc *Service, identifier, purpose string) *httptest.ResponseRecorder {
	t.Helper()
	return callSendCodeFrom(t, svc, identifier, purpose, uniqueTestIP())
}

// callSendCodeFrom pins the caller address. RemoteAddr is loopback on purpose:
// audit.IPBytes only trusts X-Real-IP when the peer is a private/loopback
// address (that is the nginx case), so the header is ignored otherwise.
func callSendCodeFrom(t *testing.T, svc *Service, identifier, purpose, ip string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(map[string]string{"identifier": identifier, "purpose": purpose})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/verification-code", strings.NewReader(string(payload)))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("X-Real-IP", ip)
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

// TestWrongCodeMessageDoesNotMentionPassword pins the wording fix (G36).
//
// invalid_credentials defaults to "账号或密码不正确". In a code-only register or
// login there is no password at all, so that sentence sends the user looking
// for the wrong problem — which is exactly what the bug report described.
func TestWrongCodeMessageDoesNotMentionPassword(t *testing.T) {
	svc, _ := verifierService(t)
	rec := callRegister(t, svc, map[string]string{
		"identifier": uniqueVerifyEmail(t, "vc-msg"),
		"password":   "Str0ng-Passw0rd!",
		"code":       "000000",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if strings.Contains(body.Error.Message, "密码") {
		t.Fatalf("message=%q still talks about a password", body.Error.Message)
	}
	if !strings.Contains(body.Error.Message, "验证码") {
		t.Fatalf("message=%q does not name the verification code", body.Error.Message)
	}
	// The machine-readable code must not change: clients branch on it.
	if body.Error.Code != "invalid_credentials" {
		t.Fatalf("code=%q, want invalid_credentials", body.Error.Code)
	}
}

// TestVerificationCodeHasPerIPCeiling 验证「换收件人」不能绕过限流。
//
// 按邮箱的两条限制（60 秒冷却、每日 20 条）对每一个新地址都是全新的，所以一个
// 遍历地址列表的脚本可以无限发信——消耗的是我们的发信额度与发件域名信誉。
// 这条按来源地址的每小时上限是唯一会先触发的一道。
func TestVerificationCodeHasPerIPCeiling(t *testing.T) {
	svc, mailer := verifierService(t)
	const ip = "203.0.113.9"

	for i := 0; i < verificationIPHourlyLimit; i++ {
		rec := callSendCodeFrom(t, svc, uniqueVerifyEmail(t, fmt.Sprintf("vc-ip-%d", i)), PurposeRegister, ip)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status=%d body=%s, want 200", i+1, rec.Code, rec.Body.String())
		}
	}
	if mailer.sends != verificationIPHourlyLimit {
		t.Fatalf("sends=%d, want %d", mailer.sends, verificationIPHourlyLimit)
	}

	over := callSendCodeFrom(t, svc, uniqueVerifyEmail(t, "vc-ip-over"), PurposeRegister, ip)
	if over.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit status=%d body=%s, want 429", over.Code, over.Body.String())
	}
	if mailer.sends != verificationIPHourlyLimit {
		t.Fatalf("an over-limit request still sent mail: sends=%d", mailer.sends)
	}

	// 同一时刻换个来源地址必须仍然可用，否则 Office/NAT 后面的正常用户会被
	// 邻居的失败次数连坐。
	other := callSendCodeFrom(t, svc, uniqueVerifyEmail(t, "vc-ip-other"), PurposeRegister, "203.0.113.10")
	if other.Code != http.StatusOK {
		t.Fatalf("other ip status=%d body=%s, want 200", other.Code, other.Body.String())
	}
}

// TestPurposeMatrix 是本轮最该写的测试：4×4 的用途矩阵，只有同用途的码能用，
// 跨用途一律 invalid_credentials。
//
// 具体攻击链（设计 §12 R3）：若 bind 与 reset 共用键，攻击者可以给自己的账号绑
// 目标邮箱 → 拿到该地址的 bind 码 → 绑定成功后账号已有该邮箱 → 再用未消费的同一个
// 码去 reset，直通式账号接管。purpose 进 key 之后这条链在第一步就断了。
func TestPurposeMatrix(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	purposes := []string{PurposeLogin, PurposeRegister, PurposeReset}

	for _, sent := range purposes {
		t.Run("issued for "+sent, func(t *testing.T) {
			// 每个用途用它自己的地址条件：register 需要「未注册」，其余需要
			// 「已注册且可用」，否则发码阶段就不会真的写下这个码。
			var identifier string
			if sent == PurposeRegister {
				identifier = uniqueVerifyEmail(t, "mx-register")
			} else {
				identifier = uniqueVerifyEmail(t, "mx-"+sent)
				newRecoveryUser(t, identifier, "", "NORMAL")
			}
			if rec := callSendCode(t, svc, identifier, sent); rec.Code != http.StatusOK {
				t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
			}
			code := mailer.lastCode(t)
			ident, err := NormalizeIdentifier(identifier)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}

			// 先打错用途（它们不会成功，也不会消费这个码），再打对的那个，
			// 否则「同用途成功」会把码烧掉、后面的断言就没有意义了。
			for _, wrong := range purposes {
				if wrong == sent {
					continue
				}
				apiErr := svc.consumeVerificationCode(context.Background(), ident, wrong, code)
				if apiErr == nil {
					t.Fatalf("a %s code was accepted for %s", sent, wrong)
				}
				if apiErr.Code != "invalid_credentials" {
					t.Fatalf("%s -> %s answered code=%q, want invalid_credentials", sent, wrong, apiErr.Code)
				}
			}
			if apiErr := svc.consumeVerificationCode(context.Background(), ident, sent, code); apiErr != nil {
				t.Fatalf("the %s code was rejected by its own purpose: %v", sent, apiErr)
			}
		})
	}
}

package auth

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/redisx"
)

// captureSMS stands in for internal/notify/sms: it records instead of sending,
// so the assertions can count messages and read the code out of the body exactly
// the way the vendor driver does.
type captureSMS struct {
	to, body string
	sends    int
	err      error
}

func (m *captureSMS) Send(_ context.Context, to, body string) error {
	m.sends++
	m.to, m.body = to, body
	return m.err
}

func (m *captureSMS) lastCode(t *testing.T) string {
	t.Helper()
	if m.body == "" {
		t.Fatalf("no text message was sent")
	}
	code, err := extractTestCode(m.body)
	if err != nil {
		t.Fatalf("message body has no six-digit code: %q", m.body)
	}
	return code
}

func extractTestCode(body string) (string, error) {
	for i := 0; i+6 <= len(body); i++ {
		digits := body[i : i+6]
		numeric := true
		for j := 0; j < 6; j++ {
			if digits[j] < '0' || digits[j] > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			return digits, nil
		}
	}
	return "", fmt.Errorf("no code")
}

// recoveryService is the wiring the recovery endpoints need: a mailbox channel,
// an open handset channel and a capture sender for each.
func recoveryService(t *testing.T) (*Service, *captureMailer, *captureSMS, *mw.SessionManager, *redisx.Client) {
	t.Helper()
	svc, sessMgr, rdb := newAuthService(t)
	mailer := &captureMailer{}
	sms := &captureSMS{}
	svc.VerificationMode = VerificationSMTP
	svc.Mailer = mailer
	svc.SMSMode = SMSLog
	svc.SMSSender = sms
	return svc, mailer, sms, sessMgr, rdb
}

// recoveryUserSeq keeps user_no unique across runs (char(12), uk_user_no).
var recoveryUserSeq atomic.Int64

// newRecoveryUser inserts one account. Exactly one identifier column is set, so
// a mailbox account has phone IS NULL and a handset account has email IS NULL.
func newRecoveryUser(t *testing.T, email, phone, status string) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcryptCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UTC()
	userNo := fmt.Sprintf("R%011d", (now.UnixNano()+recoveryUserSeq.Add(1))%100000000000)
	// mysql_test rows must bind NULL, not "", so "unbound" and "empty string"
	// cannot be confused by the binding tests.
	var emailVal, phoneVal any
	if email != "" {
		emailVal = email
	}
	if phone != "" {
		phoneVal = phone
	}
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, email, phone, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at)
		VALUES (?,?,?,?,'bcrypt',?,'recovery','PERSONAL',?,?,?)`,
		userNo, emailVal, phoneVal, string(hash), now, status, now, now)
	if err != nil {
		t.Fatalf("insert recovery user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_admin_audit_log WHERE operator_id = ? OR target_id = ?`,
			uid, fmt.Sprint(uid))
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

// softDelete reproduces what POST /auth/cancel leaves behind: deleted_at set and
// the mailbox released by rewriting it (see the 06 design §4.1).
func softDelete(t *testing.T, uid int64) {
	t.Helper()
	now := time.Now().UTC()
	_, err := testDB.ExecContext(context.Background(), `
		UPDATE t_user
		   SET deleted_at = ?,
		       email = CASE WHEN email IS NULL THEN NULL ELSE CONCAT('#del#', id, '#', ?) END,
		       status = 'DISABLED'
		 WHERE id = ?`, now, now.UnixMilli(), uid)
	if err != nil {
		t.Fatalf("soft delete: %v", err)
	}
}

var recoveryPhoneSeq atomic.Int64

// uniquePhone returns an unused, syntactically valid handset number.
func uniquePhone() string {
	return fmt.Sprintf("199%08d", recoveryPhoneSeq.Add(1)%100000000)
}

func callResetSendCode(t *testing.T, svc *Service, identifier string) *httptest.ResponseRecorder {
	t.Helper()
	return callSendCode(t, svc, identifier, PurposeReset)
}

func callResetPassword(t *testing.T, svc *Service, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/reset", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	svc.ResetPassword(rec, req)
	return rec
}

// sessionAlive reports whether the cookie value still resolves to a session.
func sessionAlive(t *testing.T, sessMgr *mw.SessionManager, cookieValue string) bool {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookieValue})
	sess, apiErr := sessMgr.Load(context.Background(), req)
	return apiErr == nil && sess != nil
}

// TestResetSendCodeIndistinguishable 是本增量最重要的一条：`purpose=reset` 的
// 发码接口对「账号可用」与「账号不可用」必须给出逐字节相同的 200，且只有可用的那一侧
// 真的发信。任何区分（404/409/不同 hint/不同 body）都会把找回接口变成账号枚举面。
//
// 五组不可用形态：不存在、已软删、status=DISABLED、手机号歧义（两行同号）。
// 「手机号歧义」这一组也是 C1 的直接后果：t_user.phone 没有唯一键。
func TestResetSendCodeIndistinguishable(t *testing.T) {
	svc, mailer, sms, _, _ := recoveryService(t)

	// 1) 可用账号：发信，并把它作为字节基准。
	usable := uniqueVerifyEmail(t, "rc-usable")
	newRecoveryUser(t, usable, "", "NORMAL")
	base := callResetSendCode(t, svc, usable)
	if base.Code != http.StatusOK {
		t.Fatalf("usable account status=%d body=%s", base.Code, base.Body.String())
	}
	if mailer.sends != 1 {
		t.Fatalf("usable account sent %d mails, want 1", mailer.sends)
	}
	wantBody := base.Body.Bytes()

	cases := []struct {
		name    string
		prepare func() string
	}{
		{
			name:    "unknown address",
			prepare: func() string { return uniqueVerifyEmail(t, "rc-missing") },
		},
		{
			name: "soft-deleted account",
			prepare: func() string {
				email := uniqueVerifyEmail(t, "rc-soft")
				softDelete(t, newRecoveryUser(t, email, "", "NORMAL"))
				return email
			},
		},
		{
			name: "disabled account",
			prepare: func() string {
				email := uniqueVerifyEmail(t, "rc-disabled")
				newRecoveryUser(t, email, "", "DISABLED")
				return email
			},
		},
		{
			name: "frozen account",
			prepare: func() string {
				email := uniqueVerifyEmail(t, "rc-frozen")
				newRecoveryUser(t, email, "", "FROZEN_OVERDRAFT")
				return email
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			email := tc.prepare()
			rec := callResetSendCode(t, svc, email)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
			}
			if !bytes.Equal(wantBody, rec.Body.Bytes()) {
				t.Fatalf("body differs from the usable case\n got: %s\nwant: %s",
					rec.Body.String(), string(wantBody))
			}
			if mailer.sends != 1 {
				t.Fatalf("an unusable account caused a send: sends=%d", mailer.sends)
			}
		})
	}

	// 5) 手机号：歧义（两行同号）与唯一一行必须给出同样的 200，只有唯一那行发短信。
	single := uniquePhone()
	newRecoveryUser(t, "", single, "NORMAL")
	phoneRec := callResetSendCode(t, svc, single)
	if phoneRec.Code != http.StatusOK {
		t.Fatalf("single handset status=%d body=%s", phoneRec.Code, phoneRec.Body.String())
	}
	if sms.sends != 1 {
		t.Fatalf("single handset sent %d texts, want 1", sms.sends)
	}
	if !strings.Contains(phoneRec.Body.String(), `"channel":"sms"`) {
		t.Fatalf("handset response does not report channel=sms: %s", phoneRec.Body.String())
	}

	ambiguous := uniquePhone()
	newRecoveryUser(t, "", ambiguous, "NORMAL")
	newRecoveryUser(t, "", ambiguous, "NORMAL")
	ambRec := callResetSendCode(t, svc, ambiguous)
	if ambRec.Code != http.StatusOK {
		t.Fatalf("ambiguous handset status=%d body=%s, want 200", ambRec.Code, ambRec.Body.String())
	}
	if !bytes.Equal(phoneRec.Body.Bytes(), ambRec.Body.Bytes()) {
		t.Fatalf("ambiguous body differs\n got: %s\nwant: %s",
			ambRec.Body.String(), phoneRec.Body.String())
	}
	if sms.sends != 1 {
		t.Fatalf("an ambiguous handset caused a text: sends=%d", sms.sends)
	}
}

// TestResetWeakPasswordDoesNotBurnCode 是 §3.2 的顺序约束：password 规则必须早于
// 验码。反过来的表现是「用户打错一个短口令，验证码被烧掉，还要等 60 秒再要一次」。
func TestResetWeakPasswordDoesNotBurnCode(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	email := uniqueVerifyEmail(t, "rc-weak")
	newRecoveryUser(t, email, "", "NORMAL")

	if rec := callResetSendCode(t, svc, email); rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := mailer.lastCode(t)

	weak := callResetPassword(t, svc, map[string]string{
		"identifier": email, "code": code, "new_password": "123",
	})
	if weak.Code != http.StatusBadRequest {
		t.Fatalf("weak password status=%d body=%s, want 400", weak.Code, weak.Body.String())
	}
	if body := decodeError(t, weak); body.Error.Code != "invalid_request_body" {
		t.Fatalf("code=%q, want invalid_request_body", body.Error.Code)
	}

	// 同一个码仍然有效：弱口令那一步既没写库也没动 Redis。
	ok := callResetPassword(t, svc, map[string]string{
		"identifier": email, "code": code, "new_password": testNewPassword,
	})
	if ok.Code != http.StatusNoContent {
		t.Fatalf("reset with the still-valid code status=%d body=%s, want 204", ok.Code, ok.Body.String())
	}
	rec := doLogin(t, svc, email, testNewPassword, uniqueIP())
	if rec.Code != http.StatusOK {
		t.Fatalf("login with the new password status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestResetRevokesAllSessions: 重置前在另一台设备登录的会话，重置后必须立刻失效。
func TestResetRevokesAllSessions(t *testing.T) {
	svc, mailer, _, sessMgr, _ := recoveryService(t)
	email := uniqueVerifyEmail(t, "rc-revoke")
	uid := newRecoveryUser(t, email, "", "NORMAL")

	_, cookie1, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session 1: %v", err)
	}
	_, cookie2, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session 2: %v", err)
	}
	if !sessionAlive(t, sessMgr, cookie1) || !sessionAlive(t, sessMgr, cookie2) {
		t.Fatalf("a fresh session did not load")
	}

	if rec := callResetSendCode(t, svc, email); rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	reset := callResetPassword(t, svc, map[string]string{
		"identifier": email, "code": mailer.lastCode(t), "new_password": testNewPassword,
	})
	if reset.Code != http.StatusNoContent {
		t.Fatalf("reset status=%d body=%s, want 204", reset.Code, reset.Body.String())
	}
	// 204 不能签发会话：只允许清 cookie（Max-Age=0 的空值），不允许写入任何
	// 非空值 —— 自动登录会吃掉「必须再登一次才能证明新口令可用」这一环。
	for _, cookie := range reset.Result().Cookies() {
		if cookie.Value != "" && cookie.MaxAge >= 0 {
			t.Fatalf("reset issued cookie %s=%q", cookie.Name, cookie.Value)
		}
	}
	if sessionAlive(t, sessMgr, cookie1) {
		t.Fatalf("session 1 survived the reset")
	}
	if sessionAlive(t, sessMgr, cookie2) {
		t.Fatalf("session 2 survived the reset")
	}
}

// TestResetAuditRow: 审计与 DB 同事务，after_json 只放非敏感事实。
func TestResetAuditRow(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	email := uniqueVerifyEmail(t, "rc-audit")
	uid := newRecoveryUser(t, email, "", "NORMAL")

	if rec := callResetSendCode(t, svc, email); rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := mailer.lastCode(t)
	if rec := callResetPassword(t, svc, map[string]string{
		"identifier": email, "code": code, "new_password": testNewPassword,
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("reset status=%d body=%s", rec.Code, rec.Body.String())
	}

	var after string
	err := testDB.QueryRowContext(context.Background(), `
		SELECT after_json FROM t_admin_audit_log
		 WHERE operator_id = ? AND module = 'USER' AND action = 'UPDATE' AND result = 'OK'
		 ORDER BY id DESC LIMIT 1`, uid).Scan(&after)
	if err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	// jsonx 会在冒号后留一个空格，这里只钉住键与值，不钉序列化器的排版。
	for _, want := range []string{`"sessions_revoked": "all"`, `"via": "reset:email"`, `"password_updated_at"`} {
		if !strings.Contains(after, want) {
			t.Fatalf("after_json %s does not contain %s", after, want)
		}
	}
	for _, forbidden := range []string{testNewPassword, code, email, "password_hash", "$2a$", "$2b$"} {
		if strings.Contains(after, forbidden) {
			t.Fatalf("after_json leaks %q: %s", forbidden, after)
		}
	}
}

// TestResetClearsAccountFailures: 「忘记密码 → 试错 5 次被锁 15 分钟 → 重置成功却
// 登不进去」是一个闭环陷阱。重置成功必须清掉 K28。
func TestResetClearsAccountFailures(t *testing.T) {
	svc, mailer, _, _, rdb := recoveryService(t)
	email := uniqueVerifyEmail(t, "rc-lock")
	newRecoveryUser(t, email, "", "NORMAL")

	if rec := callResetSendCode(t, svc, email); rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := mailer.lastCode(t)

	// 一个固定 IP，避免 IP 维度的 20 次预算把这一步也锁掉。
	const ip = "203.0.113.77"
	for i := 0; i < loginAccountFailLimit; i++ {
		if rec := callLogin(t, svc, map[string]string{"identifier": email, "password": "definitely-wrong"}, ip); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d status=%d body=%s, want 401", i+1, rec.Code, rec.Body.String())
		}
	}
	failKey := redisx.LoginAccountFailKey(email)
	n, err := rdb.Raw().Get(context.Background(), failKey).Int()
	if err != nil || n != loginAccountFailLimit {
		t.Fatalf("K28=%d err=%v, want %d", n, err, loginAccountFailLimit)
	}

	if rec := callResetPassword(t, svc, map[string]string{
		"identifier": email, "code": code, "new_password": testNewPassword,
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("reset status=%d body=%s", rec.Code, rec.Body.String())
	}
	exists, err := rdb.Raw().Exists(context.Background(), failKey).Result()
	if err != nil {
		t.Fatalf("exists K28: %v", err)
	}
	if exists != 0 {
		t.Fatalf("K28 survived the reset; the user would stay locked out")
	}
	if rec := doLogin(t, svc, email, testNewPassword, ip); rec.Code != http.StatusOK {
		t.Fatalf("login after reset status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

}

// TestResetQuotaTen: reset 的地址日配额是 10（裁定 2），且与 login 的计数互不干扰。
func TestResetQuotaTen(t *testing.T) {
	svc, mailer, _, _, rdb := recoveryService(t)
	email := uniqueVerifyEmail(t, "rc-quota")
	newRecoveryUser(t, email, "", "NORMAL")
	ctx := context.Background()
	coolKey := redisx.VerifyCodeCooldownKey(PurposeReset, identifierHash(email))

	for i := 0; i < resetDailyLimit; i++ {
		// 每次换一个来源地址：60s 冷却按地址计，这里要测的是日配额。
		rec := callSendCodeFrom(t, svc, email, PurposeReset, uniqueTestIP())
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status=%d body=%s, want 200", i+1, rec.Code, rec.Body.String())
		}
		if err := rdb.Raw().Del(ctx, coolKey).Err(); err != nil {
			t.Fatalf("clear cooldown: %v", err)
		}
	}
	if mailer.sends != resetDailyLimit {
		t.Fatalf("sends=%d, want %d", mailer.sends, resetDailyLimit)
	}

	over := callSendCodeFrom(t, svc, email, PurposeReset, uniqueTestIP())
	if over.Code != http.StatusTooManyRequests {
		t.Fatalf("11th reset code status=%d body=%s, want 429", over.Code, over.Body.String())
	}
	// 另一个用途的计数是独立的：找回被限流不该让人连「验证码登录」都做不到。
	login := callSendCodeFrom(t, svc, email, PurposeLogin, uniqueTestIP())
	if login.Code != http.StatusOK {
		t.Fatalf("login code after the reset quota status=%d body=%s, want 200",
			login.Code, login.Body.String())
	}
}

// TestResetDisabledModeFailsClosed: AUTH_VERIFICATION_MODE=off 的部署里没有身份
// 证明，重置必须 400 —— 放行等于「知道邮箱即可重置任何人」。
func TestResetDisabledModeFailsClosed(t *testing.T) {
	svc, _, _, _, _ := recoveryService(t)
	svc.VerificationMode = VerificationOff
	email := uniqueVerifyEmail(t, "rc-off")
	newRecoveryUser(t, email, "", "NORMAL")

	rec := callResetPassword(t, svc, map[string]string{
		"identifier": email, "code": "123456", "new_password": testNewPassword,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != "invalid_request_body" {
		t.Fatalf("code=%q, want invalid_request_body", body.Error.Code)
	}
	if !strings.Contains(body.Error.Hint, "未启用验证码") {
		t.Fatalf("hint=%q does not explain the deployment mode", body.Error.Hint)
	}
}

// TestResetRejectsUsernameAndMissingFields covers the field-level refusals that
// must not touch Redis at all.
func TestResetRejectsUsernameAndMissingFields(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)

	username := callResetPassword(t, svc, map[string]string{
		"identifier": "operator.name", "code": "123456", "new_password": testNewPassword,
	})
	if username.Code != http.StatusBadRequest {
		t.Fatalf("username status=%d body=%s, want 400", username.Code, username.Body.String())
	}
	if body := decodeError(t, username); !strings.Contains(body.Error.Hint, "用户名不能用于找回密码") {
		t.Fatalf("hint=%q", body.Error.Hint)
	}

	// 字段校验失败不消耗码：这条路径上什么都没发过，所以 mailer 也不该动。
	emptyCode := callResetPassword(t, svc, map[string]string{
		"identifier": "someone@test.local", "code": "", "new_password": testNewPassword,
	})
	if emptyCode.Code != http.StatusBadRequest {
		t.Fatalf("empty code status=%d body=%s, want 400", emptyCode.Code, emptyCode.Body.String())
	}
	if mailer.sends != 0 {
		t.Fatalf("a field-level refusal sent mail")
	}

	// 无码或错码统一 401，不区分账号是否存在。抽查一个从未注册过的地址：
	// 它必须和「有账号但码错」完全同一个响应。
	unknown := callResetPassword(t, svc, map[string]string{
		"identifier": uniqueVerifyEmail(t, "rc-nocode"), "code": "000000", "new_password": testNewPassword,
	})
	if unknown.Code != http.StatusUnauthorized {
		t.Fatalf("unknown address status=%d body=%s, want 401", unknown.Code, unknown.Body.String())
	}
	if body := decodeError(t, unknown); body.Error.Code != "invalid_credentials" {
		t.Fatalf("code=%q, want invalid_credentials", body.Error.Code)
	}
}

// TestResetPurposeNamespaceIsolation 钉住 R3 那条攻击链的第一环：register/login
// 的码不能被当成 reset 的码用。
func TestResetPurposeNamespaceIsolation(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	email := uniqueVerifyEmail(t, "rc-ns")
	newRecoveryUser(t, email, "", "NORMAL")

	if rec := callSendCode(t, svc, email, PurposeLogin); rec.Code != http.StatusOK {
		t.Fatalf("send login code status=%d body=%s", rec.Code, rec.Body.String())
	}
	loginCode := mailer.lastCode(t)

	rec := callResetPassword(t, svc, map[string]string{
		"identifier": email, "code": loginCode, "new_password": testNewPassword,
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a login code reset the password: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

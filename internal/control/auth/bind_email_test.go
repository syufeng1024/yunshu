package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/redisx"
)

// bindSession builds the session context the bind handlers read. The uid is the
// only field that matters: both handlers ignore everything else in the body, and
// the role is used verbatim in the audit row.
func bindSession(uid int64) *mw.Session {
	return &mw.Session{SID: "sid-bind-test", UID: uid, Role: "PERSONAL", Status: "NORMAL"}
}

func callBindCode(t *testing.T, svc *Service, sess *mw.Session, email string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(map[string]string{"email": email})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/email/code", strings.NewReader(string(payload)))
	req = req.WithContext(mw.WithSession(req.Context(), sess))
	rec := httptest.NewRecorder()
	svc.SendEmailBindCode(rec, req)
	return rec
}

func callBindEmail(t *testing.T, svc *Service, sess *mw.Session, email, code string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(map[string]string{"email": email, "code": code})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/email", strings.NewReader(string(payload)))
	req = req.WithContext(mw.WithSession(req.Context(), sess))
	rec := httptest.NewRecorder()
	svc.BindEmail(rec, req)
	return rec
}

type bindResponse struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

// TestBindCodeRejectsExistingEmail: 本轮只做「email 为空时绑定」。更换已绑定邮箱
// 是账号接管主通道，需要旧邮箱通知 + 冷静期，属于独立的一次安全评审。
func TestBindCodeRejectsExistingEmail(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	email := uniqueVerifyEmail(t, "bind-hasmail")
	uid := newAuthUser(t, email)

	rec := callBindCode(t, svc, bindSession(uid), uniqueVerifyEmail(t, "bind-target"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != "invalid_request_body" {
		t.Fatalf("code=%q, want invalid_request_body", body.Error.Code)
	}
	if !strings.Contains(body.Error.Hint, "已绑定登录邮箱") {
		t.Fatalf("hint=%q does not explain that the mailbox is already bound", body.Error.Hint)
	}
	if mailer.sends != 0 {
		t.Fatalf("a refused bind sent %d mails", mailer.sends)
	}
}

// TestBindCodeConflict: 邮箱属于他人时 409 且不发信。调用者已登录，且
// POST /verification-code{purpose:register} 本来就匿名披露「某邮箱已注册」，
// 所以这里不构成新增的枚举面。
func TestBindCodeConflict(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	uid := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	taken := uniqueVerifyEmail(t, "bind-taken")
	newAuthUser(t, taken)

	rec := callBindCode(t, svc, bindSession(uid), taken)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != "account_exists" {
		t.Fatalf("code=%q, want account_exists", body.Error.Code)
	}
	if !strings.Contains(body.Error.Hint, "该邮箱已被使用") {
		t.Fatalf("hint=%q", body.Error.Hint)
	}
	if mailer.sends != 0 {
		t.Fatalf("a conflicting bind sent %d mails", mailer.sends)
	}
}

// TestBindEmailHappyPath is the whole feature end to end: code → 200 {user} with
// the new address → the row is written → the audit row is masked.
func TestBindEmailHappyPath(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	uid := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	sess := bindSession(uid)
	target := uniqueVerifyEmail(t, "bind-happy")

	if rec := callBindCode(t, svc, sess, target); rec.Code != http.StatusOK {
		t.Fatalf("send code status=%d body=%s", rec.Code, rec.Body.String())
	}
	if mailer.to != target {
		t.Fatalf("code mailed to %q, want %q", mailer.to, target)
	}
	// 绑定正文必须是绑定专用文案，不能复用登录码正文。
	if !strings.Contains(mailer.body, "绑定登录邮箱") {
		t.Fatalf("bind mail body %q is not bind-specific", mailer.body)
	}
	code := mailer.lastCode(t)

	rec := callBindEmail(t, svc, sess, target, code)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var resp bindResponse
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.User.Email != target {
		t.Fatalf("response user.email=%q, want %q", resp.User.Email, target)
	}

	var stored string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(email,'') FROM t_user WHERE id = ?`, uid).Scan(&stored); err != nil {
		t.Fatalf("read email: %v", err)
	}
	if stored != target {
		t.Fatalf("t_user.email=%q, want %q", stored, target)
	}

	var after string
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT after_json FROM t_admin_audit_log
		 WHERE operator_id = ? AND module='USER' AND action='UPDATE' AND result='OK'
		 ORDER BY id DESC LIMIT 1`, uid).Scan(&after); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if !strings.Contains(after, `"email_changed": true`) {
		t.Fatalf("after_json=%s does not record email_changed", after)
	}
	if strings.Contains(after, target) {
		t.Fatalf("after_json contains the plaintext address: %s", after)
	}
	if !strings.Contains(after, maskIdentifier(KindEmail, target)) {
		t.Fatalf("after_json=%s does not carry the masked address", after)
	}
}

// TestBindEmailDuplicate 构造 1062：码发出之后、绑定之前，地址被另一个账号占用。
// 必须是 409 而不是 500 —— 唯一键冲突是状态冲突，不是服务故障。
func TestBindEmailDuplicate(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	uid := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	sess := bindSession(uid)
	target := uniqueVerifyEmail(t, "bind-race")

	if rec := callBindCode(t, svc, sess, target); rec.Code != http.StatusOK {
		t.Fatalf("send code status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := mailer.lastCode(t)
	// 竞争者先占住这个地址。
	newAuthUser(t, target)

	rec := callBindEmail(t, svc, sess, target, code)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec); body.Error.Code != "account_exists" {
		t.Fatalf("code=%q, want account_exists", body.Error.Code)
	}
}

// TestBindDoesNotRevokeSession: 绑定不改变 role / status / password_hash，当前会话
// 绑的是 uid，Session.Load 每次重读 t_user，所以下一次 /auth/me 自然带出新邮箱。
func TestBindDoesNotRevokeSession(t *testing.T) {
	svc, mailer, _, sessMgr, _ := recoveryService(t)
	uid := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	sess, cookie, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	target := uniqueVerifyEmail(t, "bind-session")
	if rec := callBindCode(t, svc, sess, target); rec.Code != http.StatusOK {
		t.Fatalf("send code status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := callBindEmail(t, svc, sess, target, mailer.lastCode(t)); rec.Code != http.StatusOK {
		t.Fatalf("bind status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !sessionAlive(t, sessMgr, cookie) {
		t.Fatalf("binding revoked the current session")
	}
}

// TestBindQuotaPerUID: 一个已登录账号不能变成向任意邮箱投递的免费发信器。地址维度
// 限流挡不住它（每个新地址都是全新的冷却与配额），所以有账号维度的 5 条/天。
func TestBindQuotaPerUID(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	uid := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	sess := bindSession(uid)

	for i := 0; i < bindPurposeQuotaLimit; i++ {
		// 每次换一个目标地址与来源地址：这里要测的只有账号维度。
		rec := callBindCodeFrom(t, svc, sess, uniqueVerifyEmail(t, "bind-quota"), uniqueTestIP())
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status=%d body=%s, want 200", i+1, rec.Code, rec.Body.String())
		}
	}
	if mailer.sends != bindPurposeQuotaLimit {
		t.Fatalf("sends=%d, want %d", mailer.sends, bindPurposeQuotaLimit)
	}
	over := callBindCodeFrom(t, svc, sess, uniqueVerifyEmail(t, "bind-quota-over"), uniqueTestIP())
	if over.Code != http.StatusTooManyRequests {
		t.Fatalf("6th bind code status=%d body=%s, want 429", over.Code, over.Body.String())
	}

	// 换一个账号不受影响：账号维度不能连坐。
	otherUID := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	other := callBindCode(t, svc, bindSession(otherUID), uniqueVerifyEmail(t, "bind-quota-other"))
	if other.Code != http.StatusOK {
		t.Fatalf("another account status=%d body=%s, want 200", other.Code, other.Body.String())
	}
}

// TestBindQuotaNotChargedOnCooldown 钉住 F1：账号日配额只能为「真正发出的码」计费。
//
// 顺序放错时的可观测后果（QA 复现）：同一账号、同一目标邮箱连点 5 次，只有第 1 次
// 真的发出 1 封信，但 vc:quota:bind:uid:<uid> 被 +5 —— 用户想换绑一个新邮箱要等到
// 次日。这里把那条复现直接写成断言：第 2 次是 60s 冷却的 429，预算必须仍是 1，
// 换一个新邮箱必须仍能 200。
func TestBindQuotaNotChargedOnCooldown(t *testing.T) {
	svc, mailer, _, _, rdb := recoveryService(t)
	uid := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	sess := bindSession(uid)
	target := uniqueVerifyEmail(t, "bind-cool")
	ctx := context.Background()
	uidQuotaKey := redisx.VerifyCodeUIDQuotaKey(PurposeBind, uid)

	first := callBindCodeFrom(t, svc, sess, target, uniqueTestIP())
	if first.Code != http.StatusOK {
		t.Fatalf("first send status=%d body=%s", first.Code, first.Body.String())
	}
	if mailer.sends != 1 {
		t.Fatalf("sends=%d, want 1", mailer.sends)
	}

	// 同地址第二次：被 60s 冷却拒掉，且**不**计入账号日配额。
	for i := 0; i < 3; i++ {
		rec := callBindCodeFrom(t, svc, sess, target, uniqueTestIP())
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("repeat %d status=%d body=%s, want 429 (cooldown)", i+2, rec.Code, rec.Body.String())
		}
	}
	if mailer.sends != 1 {
		t.Fatalf("cooldown rejections sent %d mails, want 1", mailer.sends)
	}
	charged, err := rdb.Raw().Get(ctx, uidQuotaKey).Int()
	if err != nil {
		t.Fatalf("read %s: %v", uidQuotaKey, err)
	}
	if charged != 1 {
		t.Fatalf("%s=%d after 1 delivery + 3 cooldown rejections, want 1", uidQuotaKey, charged)
	}

	// 冷却被拒没有吃掉预算：换一个新邮箱应当照常发码。
	fresh := callBindCodeFrom(t, svc, sess, uniqueVerifyEmail(t, "bind-cool-fresh"), uniqueTestIP())
	if fresh.Code != http.StatusOK {
		t.Fatalf("send to a fresh address after cooldown rejections status=%d body=%s, want 200",
			fresh.Code, fresh.Body.String())
	}
	if mailer.sends != 2 {
		t.Fatalf("sends=%d, want 2", mailer.sends)
	}
	if charged, err = rdb.Raw().Get(ctx, uidQuotaKey).Int(); err != nil || charged != 2 {
		t.Fatalf("%s=%d err=%v after the second delivery, want 2", uidQuotaKey, charged, err)
	}
}

// TestBindRequiresCSRF: 两个接口都是「改自己的资料」，必须是会话 + CSRF +
// account.write 三件套。缺 X-CSRF-Token 一律 403，且请求体不会被处理。
func TestBindRequiresCSRF(t *testing.T) {
	svc, mailer, _, sessMgr, _ := recoveryService(t)
	uid := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	sess, _, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	target := uniqueVerifyEmail(t, "bind-csrf")

	payload, err := jsonx.Marshal(map[string]string{"email": target})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	handler := sessMgr.CSRF(http.HandlerFunc(svc.SendEmailBindCode))

	// 只有会话、没有 token 对。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/email/code", strings.NewReader(string(payload)))
	req = req.WithContext(mw.WithSession(req.Context(), sess))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if mailer.sends != 0 {
		t.Fatalf("a request without CSRF reached the handler (sends=%d)", mailer.sends)
	}

	// 带上服务端签发的那一对才放行。
	token := sessMgr.CSRFToken(sess.SID)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/email/code", strings.NewReader(string(payload)))
	req = req.WithContext(mw.WithSession(req.Context(), sess))
	req.Header.Set(mw.CSRFHeaderName, token)
	req.AddCookie(&http.Cookie{Name: mw.CSRFCookieName, Value: token})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid CSRF status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

// TestBindCodeRejectsNonEmailInput: 手机号与用户名不是投递地址，字段级 400。
func TestBindCodeRejectsNonEmailInput(t *testing.T) {
	svc, mailer, _, _, _ := recoveryService(t)
	uid := newRecoveryUser(t, "", uniquePhone(), "NORMAL")
	for _, field := range []string{"13800001234", "operator.name", ""} {
		rec := callBindCode(t, svc, bindSession(uid), field)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("email=%q status=%d body=%s, want 400", field, rec.Code, rec.Body.String())
		}
	}
	if mailer.sends != 0 {
		t.Fatalf("a malformed address sent %d mails", mailer.sends)
	}
}

// TestBindRequiresSession: 没有会话时返回 session_expired，不是 400 —— 缺的是
// 身份，不是请求体。
func TestBindRequiresSession(t *testing.T) {
	svc, _, _, _, _ := recoveryService(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/email", strings.NewReader(`{"email":"a@b.com","code":"123456"}`))
	rec := httptest.NewRecorder()
	svc.BindEmail(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401", rec.Code, rec.Body.String())
	}
}

// callBindCodeFrom pins the caller address so the IP dimension cannot interfere
// with an account-dimension assertion.
func callBindCodeFrom(t *testing.T, svc *Service, sess *mw.Session, email, ip string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(map[string]string{"email": email})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/email/code", strings.NewReader(string(payload)))
	req = req.WithContext(mw.WithSession(req.Context(), sess))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("X-Real-IP", ip)
	rec := httptest.NewRecorder()
	svc.SendEmailBindCode(rec, req)
	return rec
}

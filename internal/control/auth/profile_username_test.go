package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
)

// uniqueUsername keeps test rows out of each other's way: uk_user_username is
// global on the shared test schema, and two packages may run at the same time.
func testUsername(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

// patchProfileRaw sends a profile PATCH with an arbitrary body, so a test can
// express "username absent" (the map simply has no key) versus "clear it"
// (username: "") — the two cases the handler must tell apart.
func patchProfileRaw(t *testing.T, svc *Service, sess *mw.Session, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(body)
	if err != nil {
		t.Fatalf("marshal profile body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/auth/profile", strings.NewReader(string(payload)))
	req = req.WithContext(mw.WithSession(req.Context(), sess))
	rec := httptest.NewRecorder()
	svc.UpdateProfile(rec, req)
	return rec
}

type profileUsernameBody struct {
	User struct {
		Username        string `json:"username"`
		DisplayName     string `json:"display_name"`
		PasswordWarning bool   `json:"password_warning"`
	} `json:"user"`
}

func decodeProfileUsername(t *testing.T, rec *httptest.ResponseRecorder) profileUsernameBody {
	t.Helper()
	var body profileUsernameBody
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode profile body: %v (raw=%s)", err, rec.Body.String())
	}
	return body
}

// 用户名是账号自选的登录标识：设置后能登录、能归一化成小写、能清空。
func TestUpdateProfileSetsAndClearsUsername(t *testing.T) {
	svc, sessMgr, _ := newAuthService(t)
	email := uniqueEmail()
	uid := newAuthUser(t, email)
	sess, _, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	sess.Role = "PERSONAL"

	name := testUsername("demo_")
	// 大小写与首尾空格都在服务端归一化：存进去的是小写 trimmed 值。
	rec := patchProfileRaw(t, svc, sess, map[string]any{
		"nickname": "演示",
		"username": "  " + strings.ToUpper(name) + "  ",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	got := decodeProfileUsername(t, rec).User.Username
	if got != name {
		t.Fatalf("username=%q, want lower-cased %q", got, name)
	}

	var stored string
	if err := svc.DB.QueryRowContext(context.Background(),
		`SELECT username FROM t_user WHERE id = ?`, uid).Scan(&stored); err != nil {
		t.Fatalf("read back username: %v", err)
	}
	if stored != name {
		t.Fatalf("stored username=%q, want %q", stored, name)
	}

	// 设置后登录查询必须能按用户名命中同一个账号。
	row, err := svc.findUser(context.Background(), Identifier{Kind: KindUsername, Value: name})
	if err != nil {
		t.Fatalf("findUser by username: %v", err)
	}
	if row.uid != uid {
		t.Fatalf("findUser uid=%d, want %d", row.uid, uid)
	}

	// 空串是"清空"，不是"不改"。
	rec = patchProfileRaw(t, svc, sess, map[string]any{"nickname": "演示", "username": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if got := decodeProfileUsername(t, rec).User.Username; got != "" {
		t.Fatalf("username after clear=%q, want empty", got)
	}
	var isNull bool
	if err := svc.DB.QueryRowContext(context.Background(),
		`SELECT username IS NULL FROM t_user WHERE id = ?`, uid).Scan(&isNull); err != nil {
		t.Fatalf("check null username: %v", err)
	}
	if !isNull {
		t.Fatalf("username column is not NULL after clear")
	}
}

// 字段缺省时必须保持原值：否则每次保存昵称都会把用户名冲掉。
func TestUpdateProfileLeavesUsernameWhenFieldAbsent(t *testing.T) {
	svc, sessMgr, _ := newAuthService(t)
	uid := newAuthUser(t, uniqueEmail())
	sess, _, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	sess.Role = "PERSONAL"

	name := testUsername("keep_")
	if rec := patchProfileRaw(t, svc, sess, map[string]any{"nickname": "a", "username": name}); rec.Code != http.StatusOK {
		t.Fatalf("seed username status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := patchProfileRaw(t, svc, sess, map[string]any{"nickname": "b"})
	if rec.Code != http.StatusOK {
		t.Fatalf("nickname-only status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if got := decodeProfileUsername(t, rec).User.Username; got != name {
		t.Fatalf("username=%q, want untouched %q", got, name)
	}
}

// 用户名唯一：抢别人的名字必须 409，而不是静默覆盖或 500。
func TestUpdateProfileRejectsTakenUsername(t *testing.T) {
	svc, sessMgr, _ := newAuthService(t)
	first := newAuthUser(t, uniqueEmail())
	second := newAuthUser(t, uniqueEmail())
	name := testUsername("taken_")

	sess, _, err := sessMgr.Issue(context.Background(), first)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	sess.Role = "PERSONAL"
	if rec := patchProfileRaw(t, svc, sess, map[string]any{"nickname": "a", "username": name}); rec.Code != http.StatusOK {
		t.Fatalf("first claim status=%d body=%s", rec.Code, rec.Body.String())
	}

	other, _, err := sessMgr.Issue(context.Background(), second)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	other.Role = "PERSONAL"
	rec := patchProfileRaw(t, svc, other, map[string]any{"nickname": "a", "username": name})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	var count int
	if err := svc.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE username = ?`, name).Scan(&count); err != nil {
		t.Fatalf("count username: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows with username=%q is %d, want 1", name, count)
	}
}

// 没有邮箱也没有手机号的账号（内置超管就是这样）不能清空用户名：清完就再也
// 登不进来，而且是静默的、要等下次重启才发现。
func TestUpdateProfileRefusesClearingLastIdentifier(t *testing.T) {
	svc, sessMgr, _ := newAuthService(t)
	uid := newAuthUser(t, uniqueEmail())
	sess, _, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	sess.Role = "SUPER_ADMIN"

	if rec := patchProfileRaw(t, svc, sess, map[string]any{"nickname": "a", "username": testUsername("only_")}); rec.Code != http.StatusOK {
		t.Fatalf("seed username status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 抹掉邮箱，模拟只有用户名的账号。
	if _, err := svc.DB.ExecContext(context.Background(),
		`UPDATE t_user SET email = NULL WHERE id = ?`, uid); err != nil {
		t.Fatalf("clear email: %v", err)
	}

	rec := patchProfileRaw(t, svc, sess, map[string]any{"nickname": "a", "username": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var username string
	if err := svc.DB.QueryRowContext(context.Background(),
		`SELECT username FROM t_user WHERE id = ?`, uid).Scan(&username); err != nil {
		t.Fatalf("read username: %v", err)
	}
	if username == "" {
		t.Fatalf("username was cleared even though the account has no other identifier")
	}
}

func TestUpdateProfileRejectsInvalidUsername(t *testing.T) {
	svc, sessMgr, _ := newAuthService(t)
	uid := newAuthUser(t, uniqueEmail())
	sess, _, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	sess.Role = "PERSONAL"

	for _, name := range []string{"ab", "1admin", "_admin", "admin name", "admin@example.com", strings.Repeat("a", 33)} {
		rec := patchProfileRaw(t, svc, sess, map[string]any{"nickname": "a", "username": name})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("username %q: status=%d body=%s, want 400", name, rec.Code, rec.Body.String())
		}
	}
}

// 初始口令只驱动提醒：登录照常成功并带回 password_warning=true；改密成功后
// 同一次登录（新会话）必须变成 false，否则提醒永远关不掉。
func TestPasswordWarningFollowsInitialFlag(t *testing.T) {
	svc, sessMgr, _ := newAuthService(t)
	uid := newAuthUser(t, uniqueEmail())
	name := testUsername("init_")
	if _, err := svc.DB.ExecContext(context.Background(),
		`UPDATE t_user SET username = ?, password_is_initial = 1 WHERE id = ?`, name, uid); err != nil {
		t.Fatalf("mark initial: %v", err)
	}

	rec := doLogin(t, svc, name, testPassword, uniqueIP())
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"password_warning":true`) {
		t.Fatalf("login body=%s, want password_warning true", rec.Body.String())
	}

	// 走真实的改密接口：服务端必须在同一事务里把标记清零。
	sess, _, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	sess.Role = "PERSONAL"
	const newPassword = "Str0ng-New-Passw0rd!"
	if pwRec := changePasswordRequest(t, svc, sess, testPassword, newPassword); pwRec.Code != http.StatusNoContent {
		t.Fatalf("change password status=%d body=%s, want 204", pwRec.Code, pwRec.Body.String())
	}

	rec = doLogin(t, svc, name, newPassword, uniqueIP())
	if rec.Code != http.StatusOK {
		t.Fatalf("second login status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"password_warning":false`) {
		t.Fatalf("login body=%s, want password_warning false", rec.Body.String())
	}
}

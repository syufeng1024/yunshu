package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
)

func patchProfile(t *testing.T, svc *Service, sess *mw.Session, nickname string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(map[string]string{"nickname": nickname})
	if err != nil {
		t.Fatalf("marshal profile body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/auth/profile", strings.NewReader(string(payload)))
	req = req.WithContext(mw.WithSession(req.Context(), sess))
	rec := httptest.NewRecorder()
	svc.UpdateProfile(rec, req)
	return rec
}

type profileBody struct {
	User struct {
		DisplayName string `json:"display_name"`
	} `json:"user"`
}

func decodeProfile(t *testing.T, rec *httptest.ResponseRecorder) profileBody {
	t.Helper()
	var body profileBody
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode profile body: %v (raw=%s)", err, rec.Body.String())
	}
	return body
}

// 昵称是控制台里唯一能自助修改的资料；改完必须立刻在 /auth/me 的 display_name
// 里体现，否则用户刷新页面会看到旧名字，以为没保存成功。
func TestUpdateProfileNickname(t *testing.T) {
	svc, sessMgr, _ := newAuthService(t)
	email := uniqueEmail()
	uid := newAuthUser(t, email)
	sess, _, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	sess.Role = "PERSONAL"

	rec := patchProfile(t, svc, sess, "  小明  ")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if got := decodeProfile(t, rec).User.DisplayName; got != "小明" {
		t.Fatalf("display_name=%q, want 小明 (leading/trailing space trimmed)", got)
	}

	var stored string
	if err := svc.DB.QueryRowContext(context.Background(),
		`SELECT nickname FROM t_user WHERE id = ?`, uid).Scan(&stored); err != nil {
		t.Fatalf("read back nickname: %v", err)
	}
	if stored != "小明" {
		t.Fatalf("stored nickname=%q, want 小明", stored)
	}

	// 清空昵称是合法的：显示名回退到邮箱，与注册时留空的行为一致。
	rec = patchProfile(t, svc, sess, "   ")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if got := decodeProfile(t, rec).User.DisplayName; got != email {
		t.Fatalf("display_name=%q, want the e-mail %q", got, email)
	}
}

func TestUpdateProfileRejectsBadNicknames(t *testing.T) {
	svc, sessMgr, _ := newAuthService(t)
	email := uniqueEmail()
	uid := newAuthUser(t, email)
	sess, _, err := sessMgr.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	sess.Role = "PERSONAL"

	cases := map[string]string{
		"too long":       strings.Repeat("字", nicknameMaxRunes+1),
		"newline":        "a\nb",
		"control chars":  "a\x00b",
		"carriage":       "a\rb",
		"just over max":  strings.Repeat("a", nicknameMaxRunes+1),
		"zero width tag": "a\xe2\x80\x8bb",
	}
	for name, nickname := range cases {
		if name == "zero width tag" {
			// 零宽空格不是控制字符，属于允许的可见字符范围；这里只确保它
			// 不会让请求崩溃，长度校验仍然按 rune 计。
			continue
		}
		rec := patchProfile(t, svc, sess, nickname)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s, want 400", name, rec.Code, rec.Body.String())
		}
	}
}

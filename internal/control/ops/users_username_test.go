package ops

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// 内置超管只有 username（email/phone 均为 NULL）。运营后台的用户列表如果不返回
// username，那一行就没有任何登录标识可显示，也无法按它搜索——这正是本测试钉住
// 的两件事：字段出现在响应里，且 `?username=` 能命中同一个账号。
func TestListUsersExposesAndFiltersUsername(t *testing.T) {
	svc := newTestService(t)
	operator := newTestUser(t, "OPS")
	target, email := newLoginUser(t, "Passw0rd!x")

	name := fmt.Sprintf("opsearch%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET username = ? WHERE id = ?`, name, target); err != nil {
		t.Fatalf("set username: %v", err)
	}

	rec := call(t, svc.ListUsers, http.MethodGet, "/api/admin/users?username="+name, nil,
		testCallOptions{uid: operator, role: "OPS"})
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body userListResponse
	decodeBody(t, rec, &body)
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("total=%d items=%d, want exactly the seeded account", body.Total, len(body.Items))
	}
	item := body.Items[0]
	if item.ID != target {
		t.Fatalf("id=%d, want %d", item.ID, target)
	}
	if item.Username != name {
		t.Fatalf("username=%q, want %q", item.Username, name)
	}
	if item.Email != email {
		t.Fatalf("email=%q, want %q", item.Email, email)
	}

	// 详情走的是同一个 userSelectColumns/scanUser 组合：漏改一处就会在详情里
	// 丢掉用户名，所以这里也从详情确认一次。
	rec = call(t, svc.GetUser, http.MethodGet, fmt.Sprintf("/api/admin/users/%d", target), nil,
		testCallOptions{uid: operator, role: "OPS", pathVal: map[string]string{"id": fmt.Sprint(target)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", rec.Code, rec.Body.String())
	}
	var detail userDetailView
	decodeBody(t, rec, &detail)
	if detail.User.Username != name {
		t.Fatalf("detail username=%q, want %q", detail.User.Username, name)
	}
}

// 用户名未设置的账号不能凭空返回一个字段：列表里 email/phone 仍按原样展示，
// username 走 omitempty，前端才能用"有没有值"决定渲染。
func TestListUsersOmitsEmptyUsername(t *testing.T) {
	svc := newTestService(t)
	operator := newTestUser(t, "OPS")
	target, _ := newLoginUser(t, "Passw0rd!x")

	rec := call(t, svc.GetUser, http.MethodGet, fmt.Sprintf("/api/admin/users/%d", target), nil,
		testCallOptions{uid: operator, role: "OPS", pathVal: map[string]string{"id": fmt.Sprint(target)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]map[string]any
	decodeBody(t, rec, &raw)
	if _, exists := raw["user"]["username"]; exists {
		t.Fatalf("username key present for an account without a username: %v", raw["user"]["username"])
	}
}

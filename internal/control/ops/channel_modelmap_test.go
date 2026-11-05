package ops

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// seedPlatformModel inserts a platform model row and removes it afterwards.
func seedPlatformModel(t *testing.T, name string) int64 {
	t.Helper()
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_model
			(model_name, display_name, region, model_type, protocol_id, listing_state,
			 published_at, created_at, updated_at)
		VALUES (?, ?, 'DOMESTIC', 'TEXT', ?, 'LISTED', NOW(3), NOW(3), NOW(3))`,
		name, name, protocolID(t, "OPENAI_COMPAT"))
	if err != nil {
		t.Fatalf("seed model %s: %v", name, err)
	}
	id, _ := res.LastInsertId()
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_model WHERE id = ?`, id)
	})
	return id
}

// 映射的左侧是「平台模型名」，写错一个字母就是一条永不命中的规则：保存成功、
// 调用静默走路由默认名，没有任何地方报错。这条用例就是把这个静默失败变成 400。
func TestChannelCreateRejectsUnknownModelMapKey(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	name := "t15-map-typo-" + uniqueSuffix()

	body := channelBody(t, name, "DOMESTIC")
	body["upstream_model_name_map"] = map[string]string{"no-such-model-xyz": "deepseek-flash"}

	rec := call(t, svc.CreateChannel, http.MethodPost, "/api/admin/channels", body,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if eb.Error.Code != "invalid_request_body" {
		t.Fatalf("code = %q, want invalid_request_body", eb.Error.Code)
	}
	if !strings.Contains(eb.Error.Message, "no-such-model-xyz") {
		t.Fatalf("message = %q, want it to name the offending key", eb.Error.Message)
	}

	var rows int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_channel WHERE name = ?`, name).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("channel rows = %d, want 0 (the typo must block the save)", rows)
	}
}

func TestChannelCreateAcceptsRegisteredModelMapKey(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	modelName := "t15-map-ok-model-" + uniqueSuffix()
	seedPlatformModel(t, modelName)

	body := channelBody(t, "t15-map-ok-"+uniqueSuffix(), "DOMESTIC")
	body["upstream_model_name_map"] = map[string]string{modelName: "deepseek-flash"}

	rec := call(t, svc.CreateChannel, http.MethodPost, "/api/admin/channels", body,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var resp channelDetailResponse
	decodeBody(t, rec, &resp)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_channel_dedicated_user WHERE channel_id = ?`, resp.Channel.ID)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_route WHERE channel_id = ?`, resp.Channel.ID)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_channel WHERE id = ?`, resp.Channel.ID)
	})
	if got := resp.Channel.UpstreamModelNames[modelName]; got != "deepseek-flash" {
		t.Fatalf("saved map = %v, want %s=deepseek-flash", resp.Channel.UpstreamModelNames, modelName)
	}
}

func TestChannelPatchRejectsUnknownModelMapKey(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	ch := createTestChannel(t, svc, uid, channelBody(t, "t15-map-patch-"+uniqueSuffix(), "DOMESTIC"))

	rec := call(t, svc.UpdateChannel, http.MethodPatch,
		"/api/admin/channels/"+strconv.FormatInt(ch.ID, 10),
		map[string]any{"upstream_model_name_map": map[string]string{"typo-model": "x"}},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(ch.ID, 10)}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if !strings.Contains(eb.Error.Message, "typo-model") {
		t.Fatalf("message = %q, want it to name the offending key", eb.Error.Message)
	}

	// 旧值必须原样保留：校验失败不能留下半改状态。
	var raw string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(upstream_model_name_map, '') FROM t_channel WHERE id = ?`, ch.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "" {
		t.Fatalf("map = %q, want untouched empty value", raw)
	}
}

func TestChannelPatchAcceptsEmptyMapAndClearsIt(t *testing.T) {
	// 清空映射是合法操作（回落到路由默认名），不能被存在性校验误伤——空映射
	// 没有 key 要校验，也不该触发一次 IN () 查询。
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	ch := createTestChannel(t, svc, uid, channelBody(t, "t15-map-clear-"+uniqueSuffix(), "DOMESTIC"))

	rec := call(t, svc.UpdateChannel, http.MethodPatch,
		"/api/admin/channels/"+strconv.FormatInt(ch.ID, 10),
		map[string]any{"upstream_model_name_map": map[string]string{}},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(ch.ID, 10)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

// seedModelAlias binds an alias to a model the way the ops API does.
func seedModelAlias(t *testing.T, modelID int64, alias string, enabled bool) {
	t.Helper()
	flag := 0
	if enabled {
		flag = 1
	}
	if _, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_model_alias (alias_name, model_id, source, enabled, created_at)
		VALUES (?, ?, 'OPS', ?, NOW(3))`, alias, modelID, flag); err != nil {
		t.Fatalf("seed alias %s: %v", alias, err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(),
			`DELETE FROM t_model_alias WHERE alias_name = ?`, alias)
	})
}

// 别名也能做映射的键：分发侧是两级查找（先请求名、后规范名），别名键正是
// 「只给这个别名指定专用上游」的落点。
func TestChannelCreateAcceptsAliasMapKey(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	modelName := "t15-alias-model-" + uniqueSuffix()
	modelID := seedPlatformModel(t, modelName)
	alias := "t15-alias-" + uniqueSuffix()
	seedModelAlias(t, modelID, alias, true)

	body := channelBody(t, "t15-alias-chan-"+uniqueSuffix(), "DOMESTIC")
	body["upstream_model_name_map"] = map[string]string{alias: "deepseek-v4-pro"}

	rec := call(t, svc.CreateChannel, http.MethodPost, "/api/admin/channels", body,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var resp channelDetailResponse
	decodeBody(t, rec, &resp)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_channel_dedicated_user WHERE channel_id = ?`, resp.Channel.ID)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_route WHERE channel_id = ?`, resp.Channel.ID)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_channel WHERE id = ?`, resp.Channel.ID)
	})
	if got := resp.Channel.UpstreamModelNames[alias]; got != "deepseek-v4-pro" {
		t.Fatalf("saved map = %v, want %s=deepseek-v4-pro", resp.Channel.UpstreamModelNames, alias)
	}
}

// 停用的别名要被拒：快照只加载 enabled=1 的别名，放行一个路由层不认的名字
// 等于又造了一条静默不命中的规则。
func TestChannelCreateRejectsDisabledAliasMapKey(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	modelName := "t15-disabled-model-" + uniqueSuffix()
	modelID := seedPlatformModel(t, modelName)
	alias := "t15-disabled-alias-" + uniqueSuffix()
	seedModelAlias(t, modelID, alias, false)

	body := channelBody(t, "t15-disabled-chan-"+uniqueSuffix(), "DOMESTIC")
	body["upstream_model_name_map"] = map[string]string{alias: "deepseek-v4-pro"}

	rec := call(t, svc.CreateChannel, http.MethodPost, "/api/admin/channels", body,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if !strings.Contains(eb.Error.Message, alias) {
		t.Fatalf("message = %q, want it to name the disabled alias", eb.Error.Message)
	}
}

// 改了映射必须立刻重发路由快照：网关按快照里的映射决定发什么上游名，若不重发，
// 保存成功到定时器刷新之间有长达 4 分钟的「配置生效了但行为没变」的窗口。
func TestChannelPatchRepublishesWhenOnlyModelMapChanges(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	modelName := "t15-repub-model-" + uniqueSuffix()
	seedPlatformModel(t, modelName)
	ch := createTestChannel(t, svc, uid, channelBody(t, "t15-repub-"+uniqueSuffix(), "DOMESTIC"))

	calls := 0
	svc.PublishRoutes = func(context.Context) error {
		calls++
		return nil
	}
	opts := testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(ch.ID, 10)}}
	path := "/api/admin/channels/" + strconv.FormatInt(ch.ID, 10)

	rec := call(t, svc.UpdateChannel, http.MethodPatch, path,
		map[string]any{"upstream_model_name_map": map[string]string{modelName: "upstream-x"}}, opts)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("publish calls = %d, want 1 (map change must republish)", calls)
	}

	// 值没变就不该重复发布：避免无意义的快照重建。
	rec = call(t, svc.UpdateChannel, http.MethodPatch, path,
		map[string]any{"upstream_model_name_map": map[string]string{modelName: "upstream-x"}}, opts)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("publish calls = %d, want still 1 (identical map)", calls)
	}
}

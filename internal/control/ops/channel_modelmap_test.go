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

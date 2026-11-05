package key

import (
	"context"
	"net/http"
	"strconv"
	"testing"
)

// 这组用例钉住一条容易再次踩空的规则：**快照不只是 IP 白名单的载体**。
//
// P07 从同一个 cfg:snap:ROUTING 里读密钥的模型授权（snap.KeyGrant / KeyModels），
// 缺条目就是拒绝。原实现只在「有 IP 白名单」时重发快照，注释也只写了 P06 的
// 理由，于是没有白名单的新密钥在下次发布前一律 403 model_not_authorized
// （2026-11-05 实测：新建后立刻调用 403，重启控制面重发快照后同一把 key 200）。

func countPublishes(svc *Service) *int {
	calls := 0
	svc.PublishRoutes = func(context.Context) error {
		calls++
		return nil
	}
	return &calls
}

func TestRotatePublishesSnapshotEvenWithoutWhitelist(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)

	body := createBody("pub-rotate-"+futureExpiry(), []int64{modelID}, 1)
	body["ip_allowlist"] = []string{}
	created := call(t, svc.Create, http.MethodPost, "/api/console/keys", body, uid, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	var resp createResponse
	decodeBody(t, created, &resp)
	id := resp.Items[0].ID

	// 只在轮换这一刻开始计数：创建那次已经计过。
	calls := countPublishes(svc)
	rec := call(t, svc.Rotate, http.MethodPost, "/api/console/keys/1/rotate",
		map[string]any{"expire_at": futureExpiry()}, uid, strconv.FormatInt(id, 10))
	if rec.Code != http.StatusCreated {
		t.Fatalf("rotate status = %d body=%s", rec.Code, rec.Body.String())
	}
	if *calls != 1 {
		t.Fatalf("publish calls = %d, want 1 (the replacement needs its own grant)", *calls)
	}
}

func TestUpdateModelScopePublishesSnapshot(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelA := newTestModel(t)
	modelB := newTestModel(t)

	body := createBody("pub-scope-"+futureExpiry(), []int64{modelA}, 1)
	body["ip_allowlist"] = []string{}
	body["model_scope_mode"] = "SELECTED"
	created := call(t, svc.Create, http.MethodPost, "/api/console/keys", body, uid, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	var resp createResponse
	decodeBody(t, created, &resp)
	id := resp.Items[0].ID

	calls := countPublishes(svc)
	rec := call(t, svc.Update, http.MethodPatch, "/api/console/keys/1",
		map[string]any{"model_scope_mode": "SELECTED", "model_ids": []int64{modelA, modelB}},
		uid, strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", rec.Code, rec.Body.String())
	}
	if *calls != 1 {
		t.Fatalf("publish calls = %d, want 1 (the grant changed)", *calls)
	}

	// 只改名字不该触发发布：快照内容没变，重建纯属浪费。
	before := *calls
	rec = call(t, svc.Update, http.MethodPatch, "/api/console/keys/1",
		map[string]any{"key_name": "renamed-" + futureExpiry()}, uid, strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d body=%s", rec.Code, rec.Body.String())
	}
	if *calls != before {
		t.Fatalf("publish calls = %d after rename, want %d（改名不影响授权）", *calls, before)
	}
}

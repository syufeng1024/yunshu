package key

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"yunshu.dev/core/internal/apikey"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
)

func idStr(id int64) string { return strconv.FormatInt(id, 10) }

// A2 / F6-3: the plaintext is returned exactly once; every read is masked and
// the database holds only the digest.
func TestCreatePlaintextOnceAndDetailMasked(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "a2-key", []int64{modelID})

	if !strings.HasPrefix(item.Key, "sk-") || len(item.Key) != 51 {
		t.Fatalf("plaintext = %q, want sk- + 48 characters", item.Key)
	}
	if item.KeyMasked != item.KeyPrefix+"****"+item.KeyLast4 {
		t.Fatalf("mask = %q, parts = %q/%q", item.KeyMasked, item.KeyPrefix, item.KeyLast4)
	}

	rec := call(t, svc.Detail, http.MethodGet, "/api/console/keys/"+idStr(item.ID), nil, uid, idStr(item.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	decodeBody(t, rec, &raw)
	if _, ok := raw["key"]; ok {
		t.Fatal("detail response contains a plaintext key field")
	}
	if got := raw["key_masked"]; got != item.KeyMasked {
		t.Fatalf("detail key_masked = %v, want %v", got, item.KeyMasked)
	}

	var hash, prefix, last4 string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT key_hash, key_prefix, key_last4 FROM t_api_key WHERE id = ?`, item.ID).
		Scan(&hash, &prefix, &last4); err != nil {
		t.Fatalf("read stored key: %v", err)
	}
	if hash != apikey.Hash(item.Key) {
		t.Fatalf("key_hash is not SHA256(plaintext)")
	}
	if hash == item.Key || strings.Contains(hash, item.Key) || strings.Contains(prefix, item.Key) {
		t.Fatal("plaintext is present in t_api_key")
	}
	if len(hash) != 64 {
		t.Fatalf("key_hash length = %d, want 64", len(hash))
	}
}

// A3 / F6-7 / T12 hard constraint 2: the free tier stops at three active keys,
// disabling one frees a slot, and the refusal carries the quota copy — not a
// request-body diagnosis the caller could never act on.
func TestFreeTierThreeActiveKeys(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)

	var created []createdKey
	for i := 0; i < 3; i++ {
		created = append(created, mustCreate(t, svc, uid, "free-"+strconv.Itoa(i), []int64{modelID}))
	}

	rec := call(t, svc.Create, http.MethodPost, "/api/console/keys",
		createBody("free-4", []int64{modelID}, 0), uid, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("fourth create status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	body := errorBody(t, rec)
	t.Logf("fourth create → %d %s", rec.Code, rec.Body.String())
	if body.Error.Code != "key_limit_reached" {
		t.Fatalf("fourth create code = %q, want key_limit_reached", body.Error.Code)
	}
	if body.Error.Type != "conflict_error" {
		t.Fatalf("fourth create type = %q, want conflict_error", body.Error.Type)
	}
	if !strings.Contains(body.Error.Message, "免费档最多 3 个 Key，升级套餐可扩容") {
		t.Fatalf("fourth create message lacks the quota copy: %q", body.Error.Message)
	}
	if strings.Contains(body.Error.Message, "JSON") {
		t.Fatalf("fourth create message must not mention JSON: %q", body.Error.Message)
	}
	if strings.Contains(body.Error.Hint, "JSON") {
		t.Fatalf("fourth create hint must not mention JSON: %q", body.Error.Hint)
	}

	rec = call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(created[0].ID),
		map[string]any{"status": "DISABLED", "confirm": true}, uid, idStr(created[0].ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = call(t, svc.Create, http.MethodPost, "/api/console/keys",
		createBody("free-4", []int64{modelID}, 0), uid, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create after disable status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// Hard constraint 2: a batch may not bypass the free-tier ceiling, and the
// refusal names the ceiling instead of accusing the body of being malformed.
func TestBatchCannotBypassFreeTier(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	mustCreate(t, svc, uid, "batch-free", []int64{modelID})

	rec := call(t, svc.Create, http.MethodPost, "/api/console/keys",
		createBody("batch-free", []int64{modelID}, 3), uid, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("batch status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	body := errorBody(t, rec)
	if body.Error.Code != "key_limit_reached" {
		t.Fatalf("batch code = %q, want key_limit_reached", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "免费档最多 3 个 Key") {
		t.Fatalf("batch message lacks the quota copy: %q", body.Error.Message)
	}
	var n int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_api_key WHERE user_id = ? AND deleted_at IS NULL`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("key rows = %d after rejected batch, want 1", n)
	}
}

// A4 / F7-1: another user's key is indistinguishable from a missing key.
func TestForeignKeyIsNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	owner := newTestUser(t, nil)
	other := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, owner, "owner-key", []int64{modelID})

	for _, tc := range []struct {
		name   string
		h      http.HandlerFunc
		method string
		target string
		body   any
	}{
		{"detail", svc.Detail, http.MethodGet, "/api/console/keys/" + idStr(item.ID), nil},
		{"update", svc.Update, http.MethodPatch, "/api/console/keys/" + idStr(item.ID), map[string]any{"key_name": "stolen"}},
		{"audit", svc.ListAudit, http.MethodGet, "/api/console/keys/" + idStr(item.ID) + "/audit", nil},
		{"rotate", svc.Rotate, http.MethodPost, "/api/console/keys/" + idStr(item.ID) + "/rotate", nil},
		{"delete", svc.Delete, http.MethodDelete, "/api/console/keys/" + idStr(item.ID), map[string]any{"confirm": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := call(t, tc.h, tc.method, tc.target, tc.body, other, idStr(item.ID))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "key_not_found" {
				t.Fatalf("code = %q, want key_not_found", code)
			}
		})
	}
}

// A5 / F7-4: group_code is rejected even on create and even as null. The
// stored group must not move.
func TestGroupCodeIsRejectedAndReadOnly(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "group-key", []int64{modelID})

	body := createBody("group-create", []int64{modelID}, 0)
	body["group_code"] = "internal"
	rec := call(t, svc.Create, http.MethodPost, "/api/console/keys", body, uid, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with group_code status = %d body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "invalid_request_body" {
		t.Fatalf("create code = %q", code)
	}

	for _, value := range []any{"internal", nil} {
		rec = call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(item.ID),
			map[string]any{"group_code": value, "key_name": "changed"}, uid, idStr(item.ID))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("patch group_code=%v status = %d body=%s", value, rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "invalid_request_body" {
			t.Fatalf("patch code = %q", code)
		}
	}
	var group, name string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT group_code, key_name FROM t_api_key WHERE id = ?`, item.ID).Scan(&group, &name); err != nil {
		t.Fatal(err)
	}
	if group != "standard" || name != "group-key" {
		t.Fatalf("group/name = %q/%q after rejected patch", group, name)
	}

	// F6-6: the detail shows the group and ratio, without a switch endpoint.
	rec = call(t, svc.Detail, http.MethodGet, "/api/console/keys/"+idStr(item.ID), nil, uid, idStr(item.ID))
	var detail detailResponse
	decodeBody(t, rec, &detail)
	if detail.GroupCode != "standard" || detail.GroupRatioBP == nil || *detail.GroupRatioBP != 10000 {
		t.Fatalf("detail group = %q ratio=%v", detail.GroupCode, detail.GroupRatioBP)
	}
}

// A6 / F6-2: the four wizard steps are the request; missing required fields
// 有效期不再是必填项（需求方 2026-11-05 裁定）：不传或传 null 表示永不过期，
// K13 的 EXP 记为 0，网关的过期判断（EXP > 0 才校验）天然跳过。
func TestCreateWithoutExpiryIsPermanent(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)

	rec := call(t, svc.Create, http.MethodPost, "/api/console/keys",
		map[string]any{"name": "permanent-" + futureExpiry(), "model_ids": []int64{modelID}}, uid, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp createResponse
	decodeBody(t, rec, &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d", len(resp.Items))
	}
	if resp.Items[0].ExpireAt != nil {
		t.Fatalf("expire_at = %v, want nil (永不过期)", resp.Items[0].ExpireAt)
	}

	var stored sql.NullTime
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT expire_at FROM t_api_key WHERE id = ?`, resp.Items[0].ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Valid {
		t.Fatalf("stored expire_at = %v, want NULL", stored.Time)
	}
}

// name the field, and no group step exists.
func TestCreateRequiredFields(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)

	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"missing name", map[string]any{"model_ids": []int64{modelID}, "expire_at": futureExpiry()}, "name"},
		{"expiry in the past", map[string]any{"name": "x", "model_ids": []int64{modelID}, "expire_at": "2020-01-01T00:00:00Z"}, "expire_at"},
		{"empty models", map[string]any{"name": "x", "model_ids": []int64{}, "expire_at": futureExpiry()}, "model_ids"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := call(t, svc.Create, http.MethodPost, "/api/console/keys", tc.body, uid, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			var apiErr apiErrorBody
			decodeBody(t, rec, &apiErr)
			if !strings.Contains(apiErr.Error.Message, tc.field) {
				t.Fatalf("message %q does not name %q", apiErr.Error.Message, tc.field)
			}
		})
	}
}

// A7 / F6-5: batch 10 creates 10 rows and returns 10 plaintexts in one response.
func TestBatchCreateTen(t *testing.T) {
	svc, _ := newTestService(t)
	paid := planIDByCode(t, "STANDARD")
	uid := newTestUser(t, &paid)
	modelID := newTestModel(t)

	rec := call(t, svc.Create, http.MethodPost, "/api/console/keys",
		createBody("batch-ten", []int64{modelID}, 10), uid, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp createResponse
	decodeBody(t, rec, &resp)
	if len(resp.Items) != 10 || resp.Count != 10 {
		t.Fatalf("items=%d count=%d, want 10", len(resp.Items), resp.Count)
	}
	seen := make(map[string]bool, 10)
	for _, item := range resp.Items {
		if !strings.HasPrefix(item.Key, "sk-") {
			t.Fatalf("item key = %q", item.Key)
		}
		if seen[item.Key] {
			t.Fatal("duplicate plaintext in batch response")
		}
		seen[item.Key] = true
	}
	var rows int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_api_key WHERE user_id = ? AND deleted_at IS NULL`, uid).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 10 {
		t.Fatalf("rows = %d, want 10", rows)
	}
}

// F6-1: name search and status filter are intersected.
func TestListSearchIntersectsStatus(t *testing.T) {
	svc, _ := newTestService(t)
	paid := planIDByCode(t, "STANDARD")
	uid := newTestUser(t, &paid)
	modelID := newTestModel(t)

	rec := call(t, svc.Create, http.MethodPost, "/api/console/keys",
		createBody("env-search", []int64{modelID}, 5), uid, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created createResponse
	decodeBody(t, rec, &created)
	for i := 0; i < 2; i++ {
		rec := call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(created.Items[i].ID),
			map[string]any{"status": "DISABLED", "confirm": true}, uid, idStr(created.Items[i].ID))
		if rec.Code != http.StatusOK {
			t.Fatalf("disable status = %d body=%s", rec.Code, rec.Body.String())
		}
	}

	rec = call(t, svc.List, http.MethodGet, "/api/console/keys?q=env-search&status=disabled", nil, uid, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp listResponse
	decodeBody(t, rec, &resp)
	if resp.Total != 2 || len(resp.Items) != 2 {
		t.Fatalf("total=%d items=%d, want 2", resp.Total, len(resp.Items))
	}
	for _, item := range resp.Items {
		if item.Status != "DISABLED" || !strings.Contains(item.KeyName, "env-search") {
			t.Fatalf("unexpected item %+v", item)
		}
	}
}

// F6-4: the confirmation is enforced server-side. Without confirm=true the
// status must not move; with it the transition is idempotent.
func TestDisableRequiresConfirm(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "confirm-key", []int64{modelID})

	rec := call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(item.ID),
		map[string]any{"status": "DISABLED"}, uid, idStr(item.ID))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("disable without confirm status = %d body=%s", rec.Code, rec.Body.String())
	}
	var status string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT status FROM t_api_key WHERE id = ?`, item.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ENABLED" {
		t.Fatalf("status = %q after a rejected disable, want ENABLED", status)
	}
}

// A12: disabling a key writes st=DISABLED through to K13.
func TestDisableWritesThroughK13(t *testing.T) {
	svc, rdb := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "a12-key", []int64{modelID})

	rec := call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(item.ID),
		map[string]any{"status": "DISABLED", "confirm": true}, uid, idStr(item.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d body=%s", rec.Code, rec.Body.String())
	}
	hash16, err := apikey.HashPrefix16(apikey.Hash(item.Key))
	if err != nil {
		t.Fatal(err)
	}
	rec13, err := redisx.NewKeyAuthCache(rdb).Lookup(context.Background(), hash16)
	if err != nil {
		t.Fatalf("K13 lookup: %v", err)
	}
	if rec13.ST != "DISABLED" {
		t.Fatalf("K13 st = %q, want DISABLED", rec13.ST)
	}

	// F6-4: a second disable is an idempotent no-op, not an error.
	rec = call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(item.ID),
		map[string]any{"status": "DISABLED", "confirm": true}, uid, idStr(item.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent disable status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// A13 / F7-3: changing the model scope leaves a before/after audit row.
func TestModelScopeChangeAudit(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	first := newTestModel(t)
	second := newTestModel(t)
	item := mustCreate(t, svc, uid, "audit-key", []int64{first})

	rec := call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(item.ID),
		map[string]any{"model_ids": []int64{second}}, uid, idStr(item.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = call(t, svc.ListAudit, http.MethodGet,
		"/api/console/keys/"+idStr(item.ID)+"/audit", nil, uid, idStr(item.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("audit status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp auditResponse
	decodeBody(t, rec, &resp)
	found := false
	for _, entry := range resp.Items {
		if entry.Action != "MODEL_SCOPE_CHANGE" {
			continue
		}
		found = true
		var before, after struct {
			ModelIDs []int64 `json:"model_ids"`
		}
		if err := jsonx.Unmarshal(entry.Before, &before); err != nil {
			t.Fatal(err)
		}
		if err := jsonx.Unmarshal(entry.After, &after); err != nil {
			t.Fatal(err)
		}
		if len(before.ModelIDs) != 1 || before.ModelIDs[0] != first {
			t.Fatalf("before model ids = %v, want [%d]", before.ModelIDs, first)
		}
		if len(after.ModelIDs) != 1 || after.ModelIDs[0] != second {
			t.Fatalf("after model ids = %v, want [%d]", after.ModelIDs, second)
		}
	}
	if !found {
		t.Fatal("no MODEL_SCOPE_CHANGE audit row")
	}
}

// A14: page_size is capped; 1000 is rejected rather than truncated silently.
func TestPageSizeCap(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	rec := call(t, svc.List, http.MethodGet, "/api/console/keys?page_size=1000", nil, uid, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "invalid_query_parameter" {
		t.Fatalf("code = %q, want invalid_query_parameter", code)
	}
}

// Hard constraint 9: an empty model array is rejected, an omitted one keeps
// the existing whitelist.
func TestPatchEmptyModelArrayIsRejected(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "patch-key", []int64{modelID})

	rec := call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(item.ID),
		map[string]any{"model_ids": []int64{}}, uid, idStr(item.ID))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty model_ids status = %d body=%s", rec.Code, rec.Body.String())
	}
	var n int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_api_key_model WHERE api_key_id = ?`, item.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("model rows = %d after rejected patch, want 1", n)
	}

	rec = call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(item.ID),
		map[string]any{"key_name": "renamed"}, uid, idStr(item.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_api_key_model WHERE api_key_id = ?`, item.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("model rows = %d after rename, want 1", n)
	}
}

// F7-2: detail returns live RPM/TPM/concurrency from K1/K2/K3.
func TestDetailRealtimeMetrics(t *testing.T) {
	svc, rdb := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "rt-key", []int64{modelID})

	ctx := context.Background()
	minute := time.Now().Unix() / 60
	if err := rdb.Raw().Set(ctx, redisx.RPMLKey(item.ID, minute), 7, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Raw().Set(ctx, redisx.TPMKey(item.ID, minute), 1234, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Raw().ZAdd(ctx, redisx.ConcurrencyKey(item.ID), goredis.Z{
		Score: float64(time.Now().Add(time.Minute).UnixMilli()), Member: "req-1",
	}).Err(); err != nil {
		t.Fatal(err)
	}

	rec := call(t, svc.Detail, http.MethodGet, "/api/console/keys/"+idStr(item.ID), nil, uid, idStr(item.ID))
	var detail detailResponse
	decodeBody(t, rec, &detail)
	if !detail.Realtime.Available {
		t.Fatal("realtime unavailable")
	}
	if detail.Realtime.RPMUsed == nil || *detail.Realtime.RPMUsed != 7 {
		t.Fatalf("rpm used = %v", detail.Realtime.RPMUsed)
	}
	if detail.Realtime.TPMUsed == nil || *detail.Realtime.TPMUsed != 1234 {
		t.Fatalf("tpm used = %v", detail.Realtime.TPMUsed)
	}
	if detail.Realtime.ConcurrencyUsed == nil || *detail.Realtime.ConcurrencyUsed != 1 {
		t.Fatalf("concurrency used = %v", detail.Realtime.ConcurrencyUsed)
	}
}

// Hard constraint 10: an unreachable Redis yields null counters, not zero.
func TestRealtimeUnavailableIsNull(t *testing.T) {
	rt := &Realtime{
		Redis: redisx.New(redisx.Options{
			Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, CommandTimeout: 100 * time.Millisecond,
		}),
		Now: clock.Real{},
		TTL: time.Second,
	}
	defer rt.Redis.Close()
	snap := rt.Snapshot(context.Background(), 42)
	if snap.Available {
		t.Fatal("snapshot claims availability with no Redis")
	}
	if snap.RPM != nil || snap.TPM != nil || snap.Concurrency != nil {
		t.Fatalf("counters = %v/%v/%v, want all nil", snap.RPM, snap.TPM, snap.Concurrency)
	}
}

func TestRotateRevokesOldKey(t *testing.T) {
	svc, rdb := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "rotate-key", []int64{modelID})

	rec := call(t, svc.Rotate, http.MethodPost,
		"/api/console/keys/"+idStr(item.ID)+"/rotate", nil, uid, idStr(item.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("rotate status = %d body=%s", rec.Code, rec.Body.String())
	}
	var rotated rotateResponse
	decodeBody(t, rec, &rotated)
	if rotated.Key == item.Key || rotated.RotatedFrom != item.ID {
		t.Fatalf("rotate response = %+v", rotated)
	}

	var status string
	var revoked sql.NullTime
	var rotatedFrom sql.NullInt64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT status, revoked_at, rotated_from_id FROM t_api_key WHERE id = ?`, item.ID).
		Scan(&status, &revoked, &rotatedFrom); err != nil {
		t.Fatal(err)
	}
	if status != "DISABLED" || !revoked.Valid {
		t.Fatalf("old status=%q revoked=%v", status, revoked)
	}
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT rotated_from_id FROM t_api_key WHERE id = ?`, rotated.ID).
		Scan(&rotatedFrom); err != nil {
		t.Fatal(err)
	}
	if !rotatedFrom.Valid || rotatedFrom.Int64 != item.ID {
		t.Fatalf("new rotated_from_id = %v, want %d", rotatedFrom, item.ID)
	}

	cache := redisx.NewKeyAuthCache(rdb)
	oldHash, _ := apikey.HashPrefix16(apikey.Hash(item.Key))
	if rec13, err := cache.Lookup(context.Background(), oldHash); err != nil || rec13.ST != "DISABLED" {
		t.Fatalf("old K13 = %+v err=%v, want DISABLED", rec13, err)
	}
	newHash, _ := apikey.HashPrefix16(apikey.Hash(rotated.Key))
	if rec13, err := cache.Lookup(context.Background(), newHash); err != nil || rec13.ST != "ENABLED" {
		t.Fatalf("new K13 = %+v err=%v, want ENABLED", rec13, err)
	}
}

// T12 P0 regression: rotating an inactive key manufactures a new active key,
// so the free-tier ceiling still applies. Before the fix this returned
// invalid_request_body pointing at "count", a field a rotate request cannot
// even carry.
func TestRotateInactiveKeyHitsFreeLimit(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)

	var created []createdKey
	for i := 0; i < 3; i++ {
		created = append(created, mustCreate(t, svc, uid, "rot-free-"+strconv.Itoa(i), []int64{modelID}))
	}
	rec := call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(created[0].ID),
		map[string]any{"status": "DISABLED", "confirm": true}, uid, idStr(created[0].ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d body=%s", rec.Code, rec.Body.String())
	}
	// Refill the freed slot: three active keys plus one disabled.
	mustCreate(t, svc, uid, "rot-free-3", []int64{modelID})

	rec = call(t, svc.Rotate, http.MethodPost,
		"/api/console/keys/"+idStr(created[0].ID)+"/rotate", nil, uid, idStr(created[0].ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("rotate status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	body := errorBody(t, rec)
	if body.Error.Code != "key_limit_reached" {
		t.Fatalf("rotate code = %q, want key_limit_reached", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "免费档最多 3 个 Key，升级套餐可扩容") {
		t.Fatalf("rotate message lacks the quota copy: %q", body.Error.Message)
	}
	if strings.Contains(body.Error.Message, "JSON") || strings.Contains(body.Error.Hint, "JSON") {
		t.Fatalf("rotate message/hint must not mention JSON: %q / %q",
			body.Error.Message, body.Error.Hint)
	}
	var rows int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_api_key WHERE user_id = ? AND deleted_at IS NULL`, uid).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 4 {
		t.Fatalf("key rows = %d after rejected rotate, want 4 (no replacement inserted)", rows)
	}
}

// The counterpart of the quota split: a genuine body problem keeps
// invalid_request_body, hint included. These are the two cases the T12 residual
// cited as the model for "the caller can fix this by editing the body".
func TestBodyErrorsStayInvalidRequestBody(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "body-err", []int64{modelID})

	cases := []struct {
		name string
		call func() *httptest.ResponseRecorder
		path string
	}{
		{
			name: "count above the 1-10 batch range",
			path: "count",
			call: func() *httptest.ResponseRecorder {
				return call(t, svc.Create, http.MethodPost, "/api/console/keys",
					createBody("body-err", []int64{modelID}, 11), uid, "")
			},
		},
		{
			name: "delete without confirm",
			path: "confirm",
			call: func() *httptest.ResponseRecorder {
				return call(t, svc.Delete, http.MethodDelete,
					"/api/console/keys/"+idStr(item.ID), map[string]any{}, uid, idStr(item.ID))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.call()
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			body := errorBody(t, rec)
			if body.Error.Code != "invalid_request_body" {
				t.Fatalf("code = %q, want invalid_request_body", body.Error.Code)
			}
			if !strings.Contains(body.Error.Message, tc.path) {
				t.Fatalf("message %q must name the field path %q", body.Error.Message, tc.path)
			}
		})
	}
	// The rejected delete left the key intact.
	var deleted sql.NullTime
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT deleted_at FROM t_api_key WHERE id = ?`, item.ID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.Valid {
		t.Fatal("key was soft-deleted despite the rejected confirm")
	}
}

func TestDeleteSoftDeletesAndFreesSlot(t *testing.T) {
	svc, rdb := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	longName := strings.Repeat("长", 64)
	item := mustCreate(t, svc, uid, longName, []int64{modelID})

	rec := call(t, svc.Delete, http.MethodDelete, "/api/console/keys/"+idStr(item.ID),
		map[string]any{"confirm": true}, uid, idStr(item.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
	}
	var deleted sql.NullTime
	var name, hash string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT deleted_at, key_name, key_hash FROM t_api_key WHERE id = ?`, item.ID).
		Scan(&deleted, &name, &hash); err != nil {
		t.Fatal(err)
	}
	if !deleted.Valid {
		t.Fatal("deleted_at is NULL")
	}
	if len([]rune(name)) > 64 {
		t.Fatalf("soft-deleted name length = %d, column is varchar(64)", len([]rune(name)))
	}
	if !strings.HasPrefix(hash, "#del#") {
		t.Fatalf("key_hash = %q, want the soft-delete sentinel", hash)
	}
	oldHash, _ := apikey.HashPrefix16(apikey.Hash(item.Key))
	if _, err := redisx.NewKeyAuthCache(rdb).Lookup(context.Background(), oldHash); err == nil {
		t.Fatal("deleted key is still present in K13")
	}

	rec = call(t, svc.List, http.MethodGet, "/api/console/keys", nil, uid, "")
	var list listResponse
	decodeBody(t, rec, &list)
	for _, got := range list.Items {
		if got.ID == item.ID {
			t.Fatal("soft-deleted key appears in the list")
		}
	}
}

// A whitelist only works after cfg:snap:ROUTING carries it: P06 treats a
// missing entry as "no whitelist". Every IP-rule write must republish.
func TestIPRuleChangeRepublishesSnapshot(t *testing.T) {
	svc, _ := newTestService(t)
	var calls int32
	svc.PublishRoutes = func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)

	mustCreate(t, svc, uid, "no-ip", []int64{modelID})
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("publish calls = %d after a key without IP rules, want 0", got)
	}

	body := createBody("with-ip", []int64{modelID}, 0)
	body["ip_allowlist"] = []string{"10.0.0.0/8"}
	rec := call(t, svc.Create, http.MethodPost, "/api/console/keys", body, uid, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with IP status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("publish calls = %d after create with IP, want 1", got)
	}
	var created createResponse
	decodeBody(t, rec, &created)

	rec = call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(created.Items[0].ID),
		map[string]any{"ip_allowlist": []string{"192.168.0.0/16"}}, uid, idStr(created.Items[0].ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch IP status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("publish calls = %d after IP change, want 2", got)
	}

	rec = call(t, svc.Update, http.MethodPatch, "/api/console/keys/"+idStr(created.Items[0].ID),
		map[string]any{"key_name": "renamed-only"}, uid, idStr(created.Items[0].ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("publish calls = %d after a rename, want 2", got)
	}
}

// If the snapshot cannot be published, the batch is rolled back so the caller
// does not lose free-tier slots to keys it never received.
func TestCreateWithIPRollsBackOnPublishFailure(t *testing.T) {
	svc, _ := newTestService(t)
	svc.PublishRoutes = func(context.Context) error { return errors.New("snapshot unavailable") }
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	body := createBody("publish-fail", []int64{modelID}, 0)
	body["ip_allowlist"] = []string{"10.0.0.0/8"}

	rec := call(t, svc.Create, http.MethodPost, "/api/console/keys", body, uid, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "config_unavailable" {
		t.Fatalf("code = %q, want config_unavailable", code)
	}
	var n int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_api_key WHERE user_id = ? AND deleted_at IS NULL`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("active keys = %d after rollback, want 0", n)
	}
}

func TestInternalGroupIsHidden(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "internal-key", []int64{modelID})
	// Simulate an operations-side regroup; the console has no endpoint for it.
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_api_key SET group_code = 'internal' WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}

	rec := call(t, svc.Detail, http.MethodGet, "/api/console/keys/"+idStr(item.ID), nil, uid, idStr(item.ID))
	var detail detailResponse
	decodeBody(t, rec, &detail)
	if detail.GroupCode != "" || detail.GroupRatioBP != nil {
		t.Fatalf("internal group leaked: %q %v", detail.GroupCode, detail.GroupRatioBP)
	}

	rec = call(t, svc.List, http.MethodGet, "/api/console/keys", nil, uid, "")
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	decodeBody(t, rec, &raw)
	for _, got := range raw.Items {
		if got["id"] == float64(item.ID) {
			if _, ok := got["group_code"]; ok {
				t.Fatalf("internal group leaked in list: %v", got)
			}
		}
	}
}

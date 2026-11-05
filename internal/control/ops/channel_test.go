package ops

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func channelBody(t *testing.T, name, region string) map[string]any {
	t.Helper()
	return map[string]any{
		"name":         name,
		"protocol_id":  protocolID(t, "OPENAI_COMPAT"),
		"region":       region,
		"base_url":     "https://upstream.example.invalid/v1",
		"upstream_key": "sk-test-abcdefgh1234",
	}
}

// F14-5 / hard constraint 1: an overseas channel is refused with an explicit
// reason, not a bare field error.
func TestChannelRejectsOverseasRegion(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	name := "t15-overseas-" + uniqueSuffix()

	rec := call(t, svc.CreateChannel, http.MethodPost, "/api/admin/channels",
		channelBody(t, name, "OVERSEAS"), testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if eb.Error.Code != "invalid_request_body" {
		t.Fatalf("code=%q, want invalid_request_body", eb.Error.Code)
	}
	if !strings.Contains(eb.Error.Message, "V1.0 仅支持境内") {
		t.Fatalf("message=%q, want an explicit domestic-only reason", eb.Error.Message)
	}
	var n int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_channel WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("overseas channel rows = %d, want 0", n)
	}
	t.Logf("SQL: SELECT COUNT(*) FROM t_channel WHERE name=%q -> %d", name, n)
	t.Logf("overseas refusal: HTTP %d %s", rec.Code, eb.Error.Message)
}

// F14-2 / hard constraint 3: the database holds the v1: envelope, the API
// returns only the mask, and decrypting the stored bytes yields the key.
func TestChannelKeyIsSealedAndNeverReturned(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	name := "t15-key-" + uniqueSuffix()
	const plain = "sk-test-abcdefgh1234"

	view := createTestChannel(t, svc, uid, channelBody(t, name, "DOMESTIC"))
	if view.UpstreamKeyMask != "sk-****1234" {
		t.Fatalf("mask=%q, want sk-****1234", view.UpstreamKeyMask)
	}

	var enc []byte
	var mask string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT upstream_key_enc, upstream_key_mask FROM t_channel WHERE id = ?`,
		view.ID).Scan(&enc, &mask); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(enc, []byte("v1:")) {
		t.Fatalf("stored envelope = %q, want the v1: prefix (not base64 of the envelope)", string(enc))
	}
	if bytes.Contains(enc, []byte(plain)) {
		t.Fatal("plaintext key appears inside upstream_key_enc")
	}
	if bytes.Contains(enc, []byte("v1:djE6")) {
		t.Fatal("envelope was base64-encoded a second time (E-17 bug)")
	}
	if mask != "sk-****1234" {
		t.Fatalf("stored mask=%q, want sk-****1234", mask)
	}
	opened, err := svc.Box.Open(string(enc))
	if err != nil {
		t.Fatalf("open stored envelope: %v", err)
	}
	if string(opened) != plain {
		t.Fatalf("decrypted key=%q, want the submitted key", string(opened))
	}

	// Neither the detail response nor the list response may contain the
	// plaintext.
	rec := call(t, svc.GetChannel, http.MethodGet, "/api/admin/channels/1", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(view.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), plain) {
		t.Fatalf("detail response leaks the plaintext key: %s", rec.Body.String())
	}
	rec = call(t, svc.ListChannels, http.MethodGet, "/api/admin/channels?q="+name, nil,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), plain) {
		t.Fatalf("list response leaks the plaintext key: %s", rec.Body.String())
	}
	var list channelListResponse
	decodeBody(t, rec, &list)
	if len(list.Items) != 1 || list.Items[0].UpstreamKeyMask != "sk-****1234" {
		t.Fatalf("list = %+v, want one row with the mask", list.Items)
	}

	var prefixHex string
	var encLen int64
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT HEX(LEFT(upstream_key_enc, 3)), LENGTH(upstream_key_enc), upstream_key_mask
		  FROM t_channel WHERE id = ?`, view.ID).Scan(&prefixHex, &encLen, &mask); err != nil {
		t.Fatal(err)
	}
	if prefixHex != "76313A" { // "v1:"
		t.Fatalf("stored prefix hex=%s, want 76313A (v1:)", prefixHex)
	}
	t.Logf("SQL: SELECT HEX(LEFT(upstream_key_enc,3))=%s LENGTH=%d upstream_key_mask=%s",
		prefixHex, encLen, mask)
}

// F14-1 / hard constraint 2: a referenced channel cannot be deleted; after the
// route is removed the delete succeeds and the detail is a friendly 404.
func TestChannelDeleteChecksRoutesAndSoftDelete(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	name := "t15-ref-" + uniqueSuffix()
	ch := createTestChannel(t, svc, uid, channelBody(t, name, "DOMESTIC"))

	// Insert a model and a route that references the channel.
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_model
			(model_name, display_name, region, model_type, protocol_id, listing_state,
			 published_at, created_at, updated_at)
		VALUES (?,?, 'DOMESTIC', 'TEXT', ?, 'LISTED', NOW(3), NOW(3), NOW(3))`,
		"t15-route-model-"+uniqueSuffix(), "route model", ch.ProtocolID)
	if err != nil {
		t.Fatalf("insert model: %v", err)
	}
	modelID, _ := res.LastInsertId()
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_route WHERE model_id = ?`, modelID)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_model WHERE id = ?`, modelID)
	})
	routeRes, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_route
			(model_id, channel_id, protocol_id, upstream_model_name, region,
			 weight, priority, stream_supported, max_tokens_ceiling, enabled, created_at, updated_at)
		VALUES (?,?,?,?, 'DOMESTIC', 100, 100, 1, 0, 1, NOW(3), NOW(3))`,
		modelID, ch.ID, ch.ProtocolID, "upstream-x")
	if err != nil {
		t.Fatalf("insert route: %v", err)
	}
	routeID, _ := routeRes.LastInsertId()

	rec := call(t, svc.DeleteChannel, http.MethodDelete, "/api/admin/channels/1", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(ch.ID)}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete referenced status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if eb.Error.Code != "channel_in_use" {
		t.Fatalf("code=%q, want channel_in_use", eb.Error.Code)
	}
	if !strings.Contains(eb.Error.Hint, "停用") {
		t.Fatalf("hint=%q, want the release path", eb.Error.Hint)
	}
	var refs int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_route WHERE channel_id = ?`, ch.ID).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 1 {
		t.Fatalf("route references=%d, want 1", refs)
	}
	t.Logf("SQL: SELECT COUNT(*) FROM t_route WHERE channel_id=%d -> %d", ch.ID, refs)

	if _, err := testDB.ExecContext(context.Background(), `DELETE FROM t_route WHERE id = ?`, routeID); err != nil {
		t.Fatal(err)
	}
	rec = call(t, svc.DeleteChannel, http.MethodDelete, "/api/admin/channels/1", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(ch.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete unreferenced status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = call(t, svc.GetChannel, http.MethodGet, "/api/admin/channels/1", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(ch.ID)}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted detail status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	if code := errorBody(t, rec).Error.Code; code != "channel_not_found" {
		t.Fatalf("deleted detail code=%q, want channel_not_found", code)
	}

	var deleted bool
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT deleted_at IS NOT NULL FROM t_channel WHERE id = ?`, ch.ID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("channel row was hard-deleted or left undeleted")
	}
	t.Logf("channel_in_use hint: %s", eb.Error.Hint)
}

// F14-3: a weight change republishes ROUTING immediately and the audit row
// carries both values.
func TestChannelWeightPublishesAndAuditsBeforeAfter(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	ch := createTestChannel(t, svc, uid, channelBody(t, "t15-weight-"+uniqueSuffix(), "DOMESTIC"))

	published := 0
	svc.PublishRoutes = func(context.Context) error {
		published++
		return nil
	}
	rec := call(t, svc.UpdateChannel, http.MethodPatch, "/api/admin/channels/1",
		map[string]any{"weight": 10},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(ch.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp channelDetailResponse
	decodeBody(t, rec, &resp)
	if resp.Channel.Weight != 10 {
		t.Fatalf("weight=%d, want 10", resp.Channel.Weight)
	}
	if published != 1 {
		t.Fatalf("PublishRoutes calls=%d, want 1 (weight must take effect immediately)", published)
	}

	var before, after []byte
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT before_json, after_json FROM t_admin_audit_log
		 WHERE module = 'CHANNEL' AND action = 'UPDATE' AND target_id = ?
		 ORDER BY id DESC LIMIT 1`, itoa(ch.ID)).Scan(&before, &after); err != nil {
		t.Fatal(err)
	}
	// MySQL normalises JSON columns with a space after the colon.
	if !bytes.Contains(before, []byte(`"weight": 100`)) {
		t.Fatalf("before_json=%s, want weight 100", before)
	}
	if !bytes.Contains(after, []byte(`"weight": 10`)) {
		t.Fatalf("after_json=%s, want weight 10", after)
	}
	t.Logf("weight audit before=%s after=%s", before, after)
}

// Hard constraint 4: the dedicated allowlist is maintained and cleared when the
// scope returns to ALL.
func TestChannelDedicatedScopeUsers(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	u1 := newTestUser(t, "PERSONAL")
	u2 := newTestUser(t, "PERSONAL")
	body := channelBody(t, "t15-dedicated-"+uniqueSuffix(), "DOMESTIC")
	body["dedicated_scope_mode"] = "SELECTED"
	body["dedicated_user_ids"] = []int64{u2, u1, u2}
	ch := createTestChannel(t, svc, uid, body)

	rec := call(t, svc.GetChannel, http.MethodGet, "/api/admin/channels/1", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(ch.ID)}})
	var detail channelDetailResponse
	decodeBody(t, rec, &detail)
	if detail.Channel.DedicatedUserIDs == nil || len(*detail.Channel.DedicatedUserIDs) != 2 {
		t.Fatalf("dedicated ids=%v, want two distinct ids", detail.Channel.DedicatedUserIDs)
	}

	// Switching to ALL clears the rows; otherwise a later SELECTED switch would
	// silently resurrect the old list.
	rec = call(t, svc.UpdateChannel, http.MethodPatch, "/api/admin/channels/1",
		map[string]any{"dedicated_scope_mode": "ALL"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(ch.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var n int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_channel_dedicated_user WHERE channel_id = ?`, ch.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("dedicated rows=%d after switching to ALL, want 0", n)
	}

	// A non-empty list with mode != SELECTED is rejected.
	rec = call(t, svc.UpdateChannel, http.MethodPatch, "/api/admin/channels/1",
		map[string]any{"dedicated_user_ids": []int64{u1}},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(ch.ID)}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ALL + list status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

// The base_url rule: https anywhere, http only for a private host.
func TestChannelBaseURLScheme(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")

	public := channelBody(t, "t15-http-public-"+uniqueSuffix(), "DOMESTIC")
	public["base_url"] = "http://api.example.com/v1"
	rec := call(t, svc.CreateChannel, http.MethodPost, "/api/admin/channels", public,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("public http status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}

	private := channelBody(t, "t15-http-private-"+uniqueSuffix(), "DOMESTIC")
	private["base_url"] = "http://127.0.0.1:8080/v1"
	createTestChannel(t, svc, uid, private)
}

// The JSON column round-trips as an object; a map bound directly fails in
// database/sql, so this test pins the encoding.
func TestChannelUpstreamModelNameMapRoundTrip(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	// 映射的 key 必须是已登记的平台模型：写错或未登记的名字现在是 400（需求方
	// 2026-11-05 裁定），所以先造一条真实模型行。
	modelName := "t15-namemap-model-" + uniqueSuffix()
	seedPlatformModel(t, modelName)
	body := channelBody(t, "t15-namemap-"+uniqueSuffix(), "DOMESTIC")
	body["upstream_model_name_map"] = map[string]string{modelName: "qwen-max-2025"}
	ch := createTestChannel(t, svc, uid, body)

	rec := call(t, svc.GetChannel, http.MethodGet, "/api/admin/channels/1", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(ch.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp channelDetailResponse
	decodeBody(t, rec, &resp)
	if got := resp.Channel.UpstreamModelNames[modelName]; got != "qwen-max-2025" {
		t.Fatalf("map=%v, want the submitted entry", resp.Channel.UpstreamModelNames)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

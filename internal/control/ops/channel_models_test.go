package ops

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestParseUpstreamModels(t *testing.T) {
	t.Run("openai shape keeps provider metadata", func(t *testing.T) {
		items, err := parseUpstreamModels([]byte(`{
			"object":"list",
			"data":[
				{"id":"deepseek-flash","name":"DeepSeek-V4.1-Flash","context_window":1048576,"max_output_tokens":393216},
				{"id":"deepseek-v4-pro"}
			]}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("items = %d, want 2", len(items))
		}
		if items[0].ID != "deepseek-flash" || items[0].ContextWindow != 1048576 {
			t.Fatalf("first item = %+v", items[0])
		}
	})

	t.Run("models key is accepted", func(t *testing.T) {
		items, err := parseUpstreamModels([]byte(`{"models":[{"id":"qwen-plus"}]}`))
		if err != nil || len(items) != 1 || items[0].ID != "qwen-plus" {
			t.Fatalf("items=%+v err=%v", items, err)
		}
	})

	t.Run("blank and duplicate ids are dropped", func(t *testing.T) {
		items, err := parseUpstreamModels([]byte(`{"data":[{"id":"a"},{"id":" a "},{"id":"  "},{"id":"b"}]}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(items) != 2 || items[0].ID != "a" || items[1].ID != "b" {
			t.Fatalf("items = %+v, want [a b]", items)
		}
	})

	t.Run("empty catalogue is an error, not an empty success", func(t *testing.T) {
		// An upstream that answers 200 with no usable id is misconfigured (or
		// answered with an error page). Returning [] would let the UI say
		// "这个渠道没有任何模型" as if that were a fact.
		if _, err := parseUpstreamModels([]byte(`{"data":[]}`)); err == nil {
			t.Fatal("want error for empty catalogue")
		}
		if _, err := parseUpstreamModels([]byte(`<html>gateway</html>`)); err == nil {
			t.Fatal("want error for non-JSON body")
		}
	})
}

func TestModelsEndpointMirrorsDispatchURLRule(t *testing.T) {
	cases := map[string]string{
		"https://api.deepseek.com":       "https://api.deepseek.com/v1/models",
		"https://api.deepseek.com/":      "https://api.deepseek.com/v1/models",
		"https://api.deepseek.com/v1":    "https://api.deepseek.com/v1/models",
		"http://127.0.0.1:18080/v1":      "http://127.0.0.1:18080/v1/models",
		"http://127.0.0.1:18080/bailian": "http://127.0.0.1:18080/bailian/v1/models",
	}
	for in, want := range cases {
		got, err := modelsEndpoint(in)
		if err != nil || got != want {
			t.Fatalf("modelsEndpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x/v1", "not-a-url"} {
		if _, err := modelsEndpoint(bad); err == nil {
			t.Fatalf("modelsEndpoint(%q) should fail", bad)
		}
	}
}

// The operator asks "这个渠道到底能调哪些模型"; the answer must come from the
// provider, with the channel's own key, and must say which names the platform
// already knows.
func TestChannelUpstreamModelsReadsVendorCatalogue(t *testing.T) {
	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"id":"deepseek-flash","name":"DeepSeek-V4.1-Flash","context_window":1048576,"max_output_tokens":393216},
			{"id":"deepseek-v4-pro","name":"DeepSeek-V4-Pro"}]}`)
	}))
	defer upstream.Close()

	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	name := "t15-models-" + uniqueSuffix()
	// 映射 key 必须是已登记的平台模型，否则保存就被拦下（见 validateModelNameMap）。
	platformModel := "t15-models-platform-" + uniqueSuffix()
	seedPlatformModel(t, platformModel)
	ch := createTestChannel(t, svc, uid, map[string]any{
		"name":         name,
		"protocol_id":  protocolID(t, "OPENAI_COMPAT"),
		"region":       "DOMESTIC",
		"base_url":     upstream.URL,
		"upstream_key": "sk-test-abcdefgh1234",
		// The channel already points deepseek-flash at a platform model.
		"upstream_model_name_map": map[string]string{platformModel: "deepseek-flash"},
	})

	rec := call(t, svc.ChannelUpstreamModels, http.MethodGet,
		"/api/admin/channels/"+strconv.FormatInt(ch.ID, 10)+"/upstream-models", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(ch.ID, 10)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/models" {
		t.Fatalf("upstream path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-test-abcdefgh1234" {
		t.Fatalf("upstream auth = %q", gotAuth)
	}

	var res upstreamModelsResponse
	decodeBody(t, rec, &res)
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2 (%s)", len(res.Items), rec.Body.String())
	}
	// Sorted by id: deepseek-flash, deepseek-v4-pro.
	flash := res.Items[0]
	if flash.ID != "deepseek-flash" || flash.Name != "DeepSeek-V4.1-Flash" || flash.ContextWindow != 1048576 {
		t.Fatalf("first item = %+v", flash)
	}
	if flash.PlatformModel != platformModel {
		t.Fatalf("platform_model = %q, want %s", flash.PlatformModel, platformModel)
	}
	if flash.Registered {
		t.Fatalf("deepseek-flash is not a platform model name yet, registered must be false")
	}

	// Registering the upstream id as a platform model flips Registered.
	if _, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_model
			(model_name, display_name, region, model_type, protocol_id, listing_state,
			 published_at, created_at, updated_at)
		VALUES ('deepseek-v4-pro', 'DeepSeek V4 Pro', 'DOMESTIC', 'TEXT', ?, 'LISTED', NOW(3), NOW(3), NOW(3))`,
		ch.ProtocolID); err != nil {
		t.Fatalf("insert model: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(),
			`DELETE FROM t_model WHERE model_name = 'deepseek-v4-pro'`)
	})

	rec = call(t, svc.ChannelUpstreamModels, http.MethodGet,
		"/api/admin/channels/"+strconv.FormatInt(ch.ID, 10)+"/upstream-models", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(ch.ID, 10)}})
	decodeBody(t, rec, &res)
	if !res.Items[1].Registered {
		t.Fatalf("deepseek-v4-pro is registered, got %+v", res.Items[1])
	}
}

// A wrong or expired key is the most likely failure. The message must name the
// HTTP status instead of a generic "上游异常".
func TestChannelUpstreamModelsSurfacesAuthRejection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer upstream.Close()

	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	ch := createTestChannel(t, svc, uid, map[string]any{
		"name":         "t15-models-401-" + uniqueSuffix(),
		"protocol_id":  protocolID(t, "OPENAI_COMPAT"),
		"region":       "DOMESTIC",
		"base_url":     upstream.URL,
		"upstream_key": "sk-wrong-key-0001",
	})

	rec := call(t, svc.ChannelUpstreamModels, http.MethodGet,
		"/api/admin/channels/"+strconv.FormatInt(ch.ID, 10)+"/upstream-models", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(ch.ID, 10)}})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if eb.Error.Code != "upstream_auth_failed" {
		t.Fatalf("code = %q, want upstream_auth_failed", eb.Error.Code)
	}
	if !strings.Contains(eb.Error.Message, "401") {
		t.Fatalf("message = %q, want it to name HTTP 401", eb.Error.Message)
	}
}

func TestChannelUpstreamModelsUnknownChannelIs404(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	rec := call(t, svc.ChannelUpstreamModels, http.MethodGet,
		"/api/admin/channels/999999999/upstream-models", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": "999999999"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

// registerFixture starts a fake vendor catalogue and a channel bound to it.
func registerFixture(t *testing.T, ids ...string) (*Service, int64, channelView) {
	t.Helper()
	items := make([]string, 0, len(ids))
	for i, id := range ids {
		items = append(items, `{"id":"`+id+`","name":"`+id+` 显示名","context_window":`+
			strconv.Itoa(131072+i)+`}`)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[`+strings.Join(items, ",")+`]}`)
	}))
	t.Cleanup(upstream.Close)

	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	ch := createTestChannel(t, svc, uid, map[string]any{
		"name":         "t15-import-" + uniqueSuffix(),
		"protocol_id":  protocolID(t, "OPENAI_COMPAT"),
		"region":       "DOMESTIC",
		"base_url":     upstream.URL,
		"upstream_key": "sk-test-abcdefgh1234",
	})
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range ids {
			_, _ = testDB.ExecContext(ctx, `
				DELETE r FROM t_route r JOIN t_model m ON m.id = r.model_id
				WHERE m.model_name = ?`, id)
			_, _ = testDB.ExecContext(ctx, `DELETE FROM t_model WHERE model_name = ?`, id)
		}
	})
	return svc, uid, ch
}

func TestRegisterUpstreamModelsCreatesCatalogOnlyModelAndRoute(t *testing.T) {
	name := "t15-reg-" + uniqueSuffix()
	svc, uid, ch := registerFixture(t, name)

	rec := call(t, svc.RegisterUpstreamModels, http.MethodPost,
		"/api/admin/channels/"+strconv.FormatInt(ch.ID, 10)+"/register-upstream-models",
		map[string]any{"model_ids": []string{name}},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(ch.ID, 10)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res registerUpstreamModelsResponse
	decodeBody(t, rec, &res)
	if len(res.Created) != 1 || res.Created[0].ModelName != name || !res.Created[0].RouteCreated {
		t.Fatalf("response = %+v", res)
	}

	var (
		listing   string
		display   string
		ctxLen    int64
		channelID sql.NullInt64
	)
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT listing_state, display_name, context_length, primary_channel_id
		FROM t_model WHERE model_name = ?`, name).Scan(&listing, &display, &ctxLen, &channelID); err != nil {
		t.Fatalf("model row: %v", err)
	}
	// Registering must not publish: no price exists yet, and a LISTED model with
	// no price rule would be sellable for free.
	if listing != "CATALOG_ONLY" {
		t.Fatalf("listing_state = %q, want CATALOG_ONLY", listing)
	}
	if display != name+" 显示名" || ctxLen != 131072 {
		t.Fatalf("display/context = %q/%d, want vendor metadata", display, ctxLen)
	}
	if !channelID.Valid || channelID.Int64 != ch.ID {
		t.Fatalf("primary_channel_id = %+v, want %d", channelID, ch.ID)
	}

	var routes int64
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM t_route r JOIN t_model m ON m.id = r.model_id
		WHERE m.model_name = ? AND r.channel_id = ? AND r.upstream_model_name = ?`,
		name, ch.ID, name).Scan(&routes); err != nil {
		t.Fatal(err)
	}
	if routes != 1 {
		t.Fatalf("routes = %d, want 1", routes)
	}

	var audits int64
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM t_admin_audit_log
		WHERE module = 'MODEL' AND action = 'CREATE' AND target_type = 't_model'
		  AND target_id = ?`, strconv.FormatInt(res.Created[0].ModelID, 10)).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("audit rows = %d, want 1", audits)
	}
}

func TestRegisterUpstreamModelsIsIdempotent(t *testing.T) {
	name := "t15-reg-idem-" + uniqueSuffix()
	svc, uid, ch := registerFixture(t, name)
	opts := testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(ch.ID, 10)}}
	path := "/api/admin/channels/" + strconv.FormatInt(ch.ID, 10) + "/register-upstream-models"

	for i := 0; i < 2; i++ {
		rec := call(t, svc.RegisterUpstreamModels, http.MethodPost, path,
			map[string]any{"model_ids": []string{name}}, opts)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
		var res registerUpstreamModelsResponse
		decodeBody(t, rec, &res)
		if i == 0 {
			if len(res.Created) != 1 {
				t.Fatalf("first call created = %+v", res.Created)
			}
			continue
		}
		// Second click must be a no-op, not a duplicate row and not a 500.
		if len(res.Created) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != "already_registered" {
			t.Fatalf("second call = %+v", res)
		}
	}

	var models, routes int64
	ctx := context.Background()
	if err := testDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM t_model WHERE model_name = ?`, name).Scan(&models); err != nil {
		t.Fatal(err)
	}
	if err := testDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM t_route r JOIN t_model m ON m.id = r.model_id
		WHERE m.model_name = ? AND r.channel_id = ?`, name, ch.ID).Scan(&routes); err != nil {
		t.Fatal(err)
	}
	if models != 1 || routes != 1 {
		t.Fatalf("models=%d routes=%d, want 1/1", models, routes)
	}
}

func TestRegisterUpstreamModelsRejectsUnknownIDWithoutWriting(t *testing.T) {
	known := "t15-reg-known-" + uniqueSuffix()
	svc, uid, ch := registerFixture(t, known)

	rec := call(t, svc.RegisterUpstreamModels, http.MethodPost,
		"/api/admin/channels/"+strconv.FormatInt(ch.ID, 10)+"/register-upstream-models",
		map[string]any{"model_ids": []string{known, "no-such-model"}},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(ch.ID, 10)}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if !strings.Contains(eb.Error.Message, "no-such-model") {
		t.Fatalf("message = %q, want it to name the unknown id", eb.Error.Message)
	}
	// The batch is all-or-nothing: the valid id must not have been imported.
	var n int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_model WHERE model_name = ?`, known).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("model rows = %d, want 0 (partial import leaked)", n)
	}
}

func TestRegisterUpstreamModelsRejectsEmptySelection(t *testing.T) {
	svc, uid, ch := registerFixture(t, "t15-reg-empty-"+uniqueSuffix())
	rec := call(t, svc.RegisterUpstreamModels, http.MethodPost,
		"/api/admin/channels/"+strconv.FormatInt(ch.ID, 10)+"/register-upstream-models",
		map[string]any{"model_ids": []string{}},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(ch.ID, 10)}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

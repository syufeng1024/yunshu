package ops

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"yunshu.dev/core/internal/apierr"
)

// routeFixture creates an OPS session, a DOMESTIC channel and a CATALOG_ONLY
// model with no route. Tests add routes either through CreateModel (passing
// upstream_model_name) or through POST /api/admin/routes.
func routeFixture(t *testing.T, withModelRoute bool) (*Service, int64, channelView, modelView) {
	t.Helper()
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	ch := createTestChannel(t, svc, uid, map[string]any{
		"name":         "t45-ch-" + uniqueSuffix(),
		"protocol_id":  protocolID(t, "OPENAI_COMPAT"),
		"region":       "DOMESTIC",
		"base_url":     "http://127.0.0.1:18090/v1",
		"upstream_key": "sk-t45-abcdefgh1234",
	})
	body := map[string]any{
		"model_name":         "t45-model-" + uniqueSuffix(),
		"display_name":       "T45 测试模型",
		"region":             "DOMESTIC",
		"model_type":         "TEXT",
		"protocol_id":        ch.ProtocolID,
		"context_length":     int64(32768),
		"listing_state":      "CATALOG_ONLY",
		"primary_channel_id": ch.ID,
	}
	if withModelRoute {
		body["upstream_model_name"] = "qwen-plus"
	}
	model := createTestModel(t, svc, uid, body)
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(),
			`DELETE FROM t_route WHERE model_id = ?`, model.ID)
	})
	return svc, uid, ch, model
}

func countRoutes(t *testing.T, modelID int64) int64 {
	t.Helper()
	var n int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_route WHERE model_id = ?`, modelID).Scan(&n); err != nil {
		t.Fatalf("count routes: %v", err)
	}
	return n
}

func createRouteFor(t *testing.T, svc *Service, uid, modelID, channelID int64) int64 {
	t.Helper()
	rec := call(t, svc.CreateRoute, http.MethodPost, "/api/admin/routes", map[string]any{
		"model_id":            modelID,
		"channel_id":          channelID,
		"upstream_model_name": "qwen-plus",
	}, testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create route status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res routeDetailResponse
	decodeBody(t, rec, &res)
	return res.Route.ID
}

func TestCreateModelWithUpstreamModelNameCreatesSingleRoute(t *testing.T) {
	svc, _, ch, _ := routeFixture(t, false)
	publishes := 0
	svc.PublishRoutes = func(context.Context) error {
		publishes++
		return nil
	}
	uid := newTestUser(t, "OPS")

	name := "t45-auto-" + uniqueSuffix()
	model := createTestModel(t, svc, uid, map[string]any{
		"model_name":          name,
		"display_name":        "T45 自动路由",
		"region":              "DOMESTIC",
		"model_type":          "TEXT",
		"protocol_id":         ch.ProtocolID,
		"primary_channel_id":  ch.ID,
		"context_length":      int64(32768),
		"listing_state":       "CATALOG_ONLY",
		"upstream_model_name": "qwen-plus",
	})
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(),
			`DELETE FROM t_route WHERE model_id = ?`, model.ID)
	})

	if n := countRoutes(t, model.ID); n != 1 {
		t.Fatalf("routes = %d, want exactly 1", n)
	}
	var (
		region, upstream, modelRegion string
		channelID, protocolID         int64
		weight, priority, ceiling     int64
		stream, enabled               bool
	)
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT r.region, r.upstream_model_name, m.region, r.channel_id, r.protocol_id,
		       r.weight, r.priority, r.max_tokens_ceiling, r.stream_supported, r.enabled
		  FROM t_route r JOIN t_model m ON m.id = r.model_id
		 WHERE r.model_id = ?`, model.ID).Scan(
		&region, &upstream, &modelRegion, &channelID, &protocolID,
		&weight, &priority, &ceiling, &stream, &enabled,
	); err != nil {
		t.Fatalf("route row: %v", err)
	}
	// region must be the channel's, not the model's request value.
	if region != ch.Region || modelRegion != "DOMESTIC" {
		t.Fatalf("route region = %q, channel region = %q", region, ch.Region)
	}
	if upstream != "qwen-plus" || channelID != ch.ID || protocolID != ch.ProtocolID {
		t.Fatalf("route binding = upstream %q channel %d protocol %d", upstream, channelID, protocolID)
	}
	if weight != 100 || priority != 100 || ceiling != 0 || !stream || !enabled {
		t.Fatalf("defaults = weight %d priority %d ceiling %d stream %v enabled %v",
			weight, priority, ceiling, stream, enabled)
	}
	// A route row is invisible to the gateway until the snapshot is republished.
	if publishes != 1 {
		t.Fatalf("publish calls = %d, want 1", publishes)
	}
}

func TestCreateModelUpstreamNameWithoutChannelIs400(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	name := "t45-nochannel-" + uniqueSuffix()
	rec := call(t, svc.CreateModel, http.MethodPost, "/api/admin/models", map[string]any{
		"model_name":          name,
		"display_name":        "T45 无渠道",
		"region":              "DOMESTIC",
		"model_type":          "TEXT",
		"protocol_id":         protocolID(t, "OPENAI_COMPAT"),
		"listing_state":       "CATALOG_ONLY",
		"upstream_model_name": "qwen-plus",
	}, testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if eb.Error.Code != apierr.CodeInvalidRequestBody || !strings.Contains(eb.Error.Message, "primary_channel_id") {
		t.Fatalf("error = %+v, want invalid_request_body naming primary_channel_id", eb.Error)
	}
	// Nothing may be written when the route target is missing.
	var n int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_model WHERE model_name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("model rows = %d, want 0", n)
	}
}

func TestCreateRouteDuplicateUniqueKeyIs409(t *testing.T) {
	svc, uid, ch, model := routeFixture(t, false)
	// region is not part of the request contract; sending it must not leak
	// into the row (the channel's region always wins).
	body := map[string]any{
		"model_id":            model.ID,
		"channel_id":          ch.ID,
		"upstream_model_name": "qwen-plus",
		"region":              "OVERSEAS",
	}
	rec := call(t, svc.CreateRoute, http.MethodPost, "/api/admin/routes", body,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", rec.Code, rec.Body.String())
	}
	var first routeDetailResponse
	decodeBody(t, rec, &first)
	if first.Route.Region != ch.Region {
		t.Fatalf("region = %q, want channel region %q", first.Route.Region, ch.Region)
	}

	rec = call(t, svc.CreateRoute, http.MethodPost, "/api/admin/routes", body,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if eb := errorBody(t, rec); eb.Error.Code != apierr.CodeResourceStateConflict {
		t.Fatalf("error code = %q, want %s", eb.Error.Code, apierr.CodeResourceStateConflict)
	}
	if n := countRoutes(t, model.ID); n != 1 {
		t.Fatalf("routes = %d, want 1 (duplicate must not insert)", n)
	}
}

func TestDeleteLastEnabledRouteOfListedModelIs409(t *testing.T) {
	svc, uid, ch, model := routeFixture(t, false)
	routeID := createRouteFor(t, svc, uid, model.ID, ch.ID)
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_model SET listing_state = 'LISTED' WHERE id = ?`, model.ID); err != nil {
		t.Fatalf("set LISTED: %v", err)
	}

	path := "/api/admin/routes/" + strconv.FormatInt(routeID, 10)
	rec := call(t, svc.DeleteRoute, http.MethodDelete, path, nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(routeID, 10)}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if eb := errorBody(t, rec); eb.Error.Code != apierr.CodeResourceStateConflict {
		t.Fatalf("error code = %q, want %s", eb.Error.Code, apierr.CodeResourceStateConflict)
	}
	if n := countRoutes(t, model.ID); n != 1 {
		t.Fatalf("route was deleted despite refusal: %d rows", n)
	}

	// Taking the model off the shelf makes the delete legitimate.
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_model SET listing_state = 'CATALOG_ONLY' WHERE id = ?`, model.ID); err != nil {
		t.Fatalf("set CATALOG_ONLY: %v", err)
	}
	rec = call(t, svc.DeleteRoute, http.MethodDelete, path, nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(routeID, 10)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("after delist status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if n := countRoutes(t, model.ID); n != 0 {
		t.Fatalf("routes = %d after delete, want 0", n)
	}
}

func TestPatchRouteRepublishesSnapshot(t *testing.T) {
	svc, uid, ch, model := routeFixture(t, false)
	publishes := 0
	svc.PublishRoutes = func(context.Context) error {
		publishes++
		return nil
	}
	routeID := createRouteFor(t, svc, uid, model.ID, ch.ID)
	if publishes != 1 {
		t.Fatalf("publishes after create = %d, want 1", publishes)
	}

	// Reads must not republish.
	if rec := call(t, svc.ListRoutes, http.MethodGet, "/api/admin/routes?model_id="+
		strconv.FormatInt(model.ID, 10), nil, testCallOptions{uid: uid, role: "OPS"}); rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	if publishes != 1 {
		t.Fatalf("list republished: %d", publishes)
	}

	path := "/api/admin/routes/" + strconv.FormatInt(routeID, 10)
	rec := call(t, svc.UpdateRoute, http.MethodPatch, path,
		map[string]any{"enabled": false},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": strconv.FormatInt(routeID, 10)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res routeDetailResponse
	decodeBody(t, rec, &res)
	if res.Route.Enabled {
		t.Fatal("response route still enabled")
	}
	if publishes != 2 {
		t.Fatalf("publishes after patch = %d, want 2", publishes)
	}
	var enabled bool
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT enabled FROM t_route WHERE id = ?`, routeID).Scan(&enabled); err != nil {
		t.Fatalf("read route: %v", err)
	}
	if enabled {
		t.Fatal("stored route still enabled")
	}
}

func TestPatchAndDeleteMissingRouteAre404(t *testing.T) {
	svc, uid, _, _ := routeFixture(t, false)
	missing := "999999999"
	opts := testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": missing}}
	path := "/api/admin/routes/" + missing
	if rec := call(t, svc.UpdateRoute, http.MethodPatch, path,
		map[string]any{"weight": int64(50)}, opts); rec.Code != http.StatusNotFound {
		t.Fatalf("patch status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	if rec := call(t, svc.DeleteRoute, http.MethodDelete, path, nil, opts); rec.Code != http.StatusNotFound {
		t.Fatalf("delete status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

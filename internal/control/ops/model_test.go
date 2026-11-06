package ops

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func modelBody(t *testing.T, name, state string) map[string]any {
	t.Helper()
	return map[string]any{
		"model_name":    name,
		"display_name":  name + " 展示名",
		"region":        "DOMESTIC",
		"model_type":    "TEXT",
		"protocol_id":   protocolID(t, "OPENAI_COMPAT"),
		"listing_state": state,
	}
}

// F14-5 applies to model master data too.
func TestModelRejectsOverseasRegion(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	body := modelBody(t, "t15-model-overseas-"+uniqueSuffix(), "LISTED")
	body["region"] = "OVERSEAS"

	rec := call(t, svc.CreateModel, http.MethodPost, "/api/admin/models", body,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if eb.Error.Code != "invalid_request_body" || !strings.Contains(eb.Error.Message, "仅支持境内") {
		t.Fatalf("body=%+v, want an explicit domestic-only refusal", eb.Error)
	}
}

// F16-7 / F17-1: LISTED → CATALOG_ONLY needs no notice period, zeroes the
// recommendation weight and writes a draft; CATALOG_ONLY → LISTED writes a
// "new listing" draft. Both transitions are audited.
func TestModelListingTransitionsDraftAndZeroWeight(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	body := modelBody(t, "t15-model-state-"+uniqueSuffix(), "CATALOG_ONLY")
	body["aliases"] = []string{"t15-alias-" + uniqueSuffix()}
	m := createTestModel(t, svc, uid, body)
	// F16-7: the model must be priced before it can enter LISTED.
	createTestPrice(t, svc, uid, m.ID, 1000, 1000, 600)
	rec := call(t, svc.SetListingState, http.MethodPost, "/api/admin/models/1/listing-state",
		map[string]any{"listing_state": "LISTED"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("initial LISTED status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = call(t, svc.SetRecommendWeight, http.MethodPost, "/api/admin/models/1/recommend-weight",
		map[string]any{"recommend_weight": 7},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set weight status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The initial CATALOG_ONLY → LISTED already wrote one "new listing" draft.
	drafts := draftCount(t, m.ModelName)
	rec = call(t, svc.SetListingState, http.MethodPost, "/api/admin/models/1/listing-state",
		map[string]any{"listing_state": "CATALOG_ONLY", "reason": "成本调整"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("to CATALOG_ONLY status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp modelDetailResponse
	decodeBody(t, rec, &resp)
	if resp.Model.ListingState != "CATALOG_ONLY" || resp.Model.RecommendWeight != 0 {
		t.Fatalf("state/weight=%s/%d, want CATALOG_ONLY/0", resp.Model.ListingState, resp.Model.RecommendWeight)
	}
	if resp.Model.PublishedAt == nil {
		t.Fatal("published_at was cleared by CATALOG_ONLY; the first listing date must be kept")
	}
	if n := draftCount(t, m.ModelName); n != drafts+1 {
		t.Fatalf("drafts=%d after LISTED→CATALOG_ONLY, want %d", n, drafts+1)
	}
	assertListingAudit(t, m.ID, "成本调整")

	rec = call(t, svc.SetListingState, http.MethodPost, "/api/admin/models/1/listing-state",
		map[string]any{"listing_state": "LISTED"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("to LISTED status=%d body=%s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &resp)
	if resp.Model.ListingState != "LISTED" {
		t.Fatalf("state=%s, want LISTED", resp.Model.ListingState)
	}
	if n := draftCount(t, m.ModelName); n != drafts+2 {
		t.Fatalf("drafts=%d after CATALOG_ONLY→LISTED, want %d", n, drafts+2)
	}

	// Announcements are drafts and must never drive the model state back.
	var state string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT listing_state FROM t_model WHERE id = ?`, m.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "LISTED" {
		t.Fatalf("model state=%s, want LISTED", state)
	}
}

// Hard constraint 11: CATALOG_ONLY / DELISTED models cannot hold a non-zero
// recommendation weight; moving off LISTED zeroes it automatically.
func TestModelRecommendWeightOnlyForListed(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")

	// Create-time refusal.
	body := modelBody(t, "t15-model-weight-"+uniqueSuffix(), "CATALOG_ONLY")
	body["recommend_weight"] = 5
	rec := call(t, svc.CreateModel, http.MethodPost, "/api/admin/models", body,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("create CATALOG_ONLY+weight status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if code := errorBody(t, rec).Error.Code; code != "resource_state_conflict" {
		t.Fatalf("code=%q, want resource_state_conflict", code)
	}

	// Set-endpoint refusal.
	ok := modelBody(t, "t15-model-weight-ok-"+uniqueSuffix(), "CATALOG_ONLY")
	m := createTestModel(t, svc, uid, ok)
	rec = call(t, svc.SetRecommendWeight, http.MethodPost, "/api/admin/models/1/recommend-weight",
		map[string]any{"recommend_weight": 10},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("set weight on CATALOG_ONLY status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec).Error.Message; !strings.Contains(msg, "推荐权重必须为 0") {
		t.Fatalf("message=%q", msg)
	}

	// Zero is accepted, then the model is listed and a non-zero weight works.
	rec = call(t, svc.SetRecommendWeight, http.MethodPost, "/api/admin/models/1/recommend-weight",
		map[string]any{"recommend_weight": 0},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set zero weight status=%d body=%s", rec.Code, rec.Body.String())
	}
	createTestPrice(t, svc, uid, m.ID, 1000, 1000, 600)
	rec = call(t, svc.SetListingState, http.MethodPost, "/api/admin/models/1/listing-state",
		map[string]any{"listing_state": "LISTED"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = call(t, svc.SetRecommendWeight, http.MethodPost, "/api/admin/models/1/recommend-weight",
		map[string]any{"recommend_weight": 10},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set weight on LISTED status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp modelDetailResponse
	decodeBody(t, rec, &resp)
	if resp.Model.RecommendWeight != 10 {
		t.Fatalf("weight=%d, want 10", resp.Model.RecommendWeight)
	}

	// Moving back to CATALOG_ONLY zeroes it without an explicit weight field.
	rec = call(t, svc.UpdateModel, http.MethodPatch, "/api/admin/models/1",
		map[string]any{"listing_state": "CATALOG_ONLY"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch to CATALOG_ONLY status=%d body=%s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &resp)
	if resp.Model.RecommendWeight != 0 {
		t.Fatalf("weight=%d after leaving LISTED, want 0", resp.Model.RecommendWeight)
	}
}

func TestModelAliasConflict(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	alias := "t15-shared-alias-" + uniqueSuffix()

	first := modelBody(t, "t15-model-a-"+uniqueSuffix(), "CATALOG_ONLY")
	first["aliases"] = []string{alias}
	owner := createTestModel(t, svc, uid, first)

	second := modelBody(t, "t15-model-b-"+uniqueSuffix(), "CATALOG_ONLY")
	second["aliases"] = []string{alias}
	rec := call(t, svc.CreateModel, http.MethodPost, "/api/admin/models", second,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate alias status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if eb.Error.Code != "resource_state_conflict" {
		t.Fatalf("code=%q, want resource_state_conflict", eb.Error.Code)
	}
	msg := eb.Error.Message
	if !strings.Contains(msg, alias) ||
		!strings.Contains(msg, owner.ModelName) ||
		!strings.Contains(msg, itoa(owner.ID)) {
		t.Fatalf("message=%q, want alias + conflicting model name and id", msg)
	}
	t.Logf("A14 alias=%q already owned by %s(id=%d) -> 409 %s: %s",
		alias, owner.ModelName, owner.ID, eb.Error.Code, msg)
}

// F16-7 / E-34: a model cannot be created directly as LISTED without an
// effective price, and the refused save leaves no row behind. CATALOG_ONLY and
// DELISTED still create without a price.
func TestModelCreateListedRequiresEffectivePrice(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")

	name := "t15-model-unpriced-" + uniqueSuffix()
	rec := call(t, svc.CreateModel, http.MethodPost, "/api/admin/models",
		modelBody(t, name, "LISTED"), testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("create LISTED without price status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	eb := errorBody(t, rec)
	if eb.Error.Code != "resource_state_conflict" {
		t.Fatalf("code=%q, want resource_state_conflict", eb.Error.Code)
	}
	if !strings.Contains(eb.Error.Message, "模型尚未配置生效价格，不能上架") {
		t.Fatalf("message=%q, want the F16-7 reason", eb.Error.Message)
	}
	if !strings.Contains(eb.Error.Hint, "定价页") {
		t.Fatalf("hint=%q, want the pricing-page path", eb.Error.Hint)
	}
	var n int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_model WHERE model_name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("refused create left %d row(s) behind", n)
	}

	// Unlisting states are not gated: DELISTED needs no price.
	delisted := createTestModel(t, svc, uid, modelBody(t, "t15-model-delisted-"+uniqueSuffix(), "DELISTED"))
	if delisted.ListingState != listingDelisted {
		t.Fatalf("state=%s, want DELISTED", delisted.ListingState)
	}
}

// F16-7 / E-34: CATALOG_ONLY → LISTED is refused until a price is effective;
// once configured, the same transition succeeds.
func TestModelListRequiresEffectivePrice(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	m := createTestModel(t, svc, uid, modelBody(t, "t15-model-list-"+uniqueSuffix(), "CATALOG_ONLY"))

	rec := call(t, svc.SetListingState, http.MethodPost, "/api/admin/models/1/listing-state",
		map[string]any{"listing_state": "LISTED"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("LISTED without price status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if code := errorBody(t, rec).Error.Code; code != "resource_state_conflict" {
		t.Fatalf("code=%q, want resource_state_conflict", code)
	}

	var state string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT listing_state FROM t_model WHERE id = ?`, m.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != listingCatalogOnly {
		t.Fatalf("state=%s after refusal, want CATALOG_ONLY", state)
	}

	createTestPrice(t, svc, uid, m.ID, 1000, 1000, 600)
	rec = call(t, svc.SetListingState, http.MethodPost, "/api/admin/models/1/listing-state",
		map[string]any{"listing_state": "LISTED"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("LISTED after price status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp modelDetailResponse
	decodeBody(t, rec, &resp)
	if resp.Model.ListingState != listingListed {
		t.Fatalf("state=%s, want LISTED", resp.Model.ListingState)
	}
}

// F16-7 uses the same window as pricing.go: a rule that starts in the future
// is not a current price, while a disabled or expired one is not either.
func TestModelListIgnoresFuturePrice(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	m := createTestModel(t, svc, uid, modelBody(t, "t15-model-future-"+uniqueSuffix(), "CATALOG_ONLY"))

	futureID := insertRawPriceRule(t, m.ID, 1000, time.Now().UTC().Add(48*time.Hour))
	rec := call(t, svc.SetListingState, http.MethodPost, "/api/admin/models/1/listing-state",
		map[string]any{"listing_state": "LISTED"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("LISTED with future-only price status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec).Error.Message; !strings.Contains(msg, "模型尚未配置生效价格") {
		t.Fatalf("message=%q", msg)
	}

	// Remove the planted future row before the follow-up write: an enabled row
	// that starts later would trip the A12 conflict check, not the listing gate.
	if _, err := testDB.ExecContext(context.Background(),
		`DELETE FROM t_price_rule WHERE id = ?`, futureID); err != nil {
		t.Fatal(err)
	}
	createTestPrice(t, svc, uid, m.ID, 1000, 1000, 600)
	rec = call(t, svc.SetListingState, http.MethodPost, "/api/admin/models/1/listing-state",
		map[string]any{"listing_state": "LISTED"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("LISTED after effective price status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// The gate must not block anything else: a priced model can be listed directly,
// and edits (including a no-op listing_state=LISTED) stay allowed.
func TestModelPricedListingIsNotBlocked(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	m := createTestModel(t, svc, uid, modelBody(t, "t15-model-priced-"+uniqueSuffix(), "CATALOG_ONLY"))
	createTestPrice(t, svc, uid, m.ID, 1000, 1000, 600)

	rec := call(t, svc.SetListingState, http.MethodPost, "/api/admin/models/1/listing-state",
		map[string]any{"listing_state": "LISTED"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("priced LISTED status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Editing a different field while LISTED must not re-run the price gate.
	rec = call(t, svc.UpdateModel, http.MethodPatch, "/api/admin/models/1",
		map[string]any{"display_name": "改过的展示名"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH while LISTED status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Sending LISTED again is a no-op state, not a transition.
	rec = call(t, svc.UpdateModel, http.MethodPatch, "/api/admin/models/1",
		map[string]any{"listing_state": "LISTED"},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op LISTED PATCH status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func draftCount(t *testing.T, modelName string) int64 {
	t.Helper()
	var n int64
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM t_announcement
		 WHERE state = 'DRAFT' AND content LIKE ?`, "%"+modelName+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertListingAudit(t *testing.T, modelID int64, reason string) {
	t.Helper()
	var (
		before, after []byte
		gotReason     *string
	)
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT before_json, after_json, reason FROM t_admin_audit_log
		 WHERE module = 'MODEL' AND action = 'LISTING_STATE_CHANGE' AND target_id = ?
		 ORDER BY id DESC LIMIT 1`, itoa(modelID)).Scan(&before, &after, &gotReason); err != nil {
		t.Fatal(err)
	}
	// MySQL normalises JSON columns with a space after the colon.
	if !strings.Contains(string(before), `"listing_state": "LISTED"`) {
		t.Fatalf("before_json=%s, want LISTED", before)
	}
	if !strings.Contains(string(after), `"listing_state": "CATALOG_ONLY"`) {
		t.Fatalf("after_json=%s, want CATALOG_ONLY", after)
	}
	if !strings.Contains(string(after), `"announcement_id"`) {
		t.Fatalf("after_json=%s, want the generated announcement id", after)
	}
	// The automatic zeroing is visible in the audit, not silent.
	if !strings.Contains(string(before), `"recommend_weight": 7`) ||
		!strings.Contains(string(after), `"recommend_weight": 0`) {
		t.Fatalf("before/after weight not recorded: before=%s after=%s", before, after)
	}
	if gotReason == nil || *gotReason != reason {
		t.Fatalf("reason=%v, want %q", gotReason, reason)
	}
}

// F22-6: saving the recommendation weight writes the before/after values and
// the operator to t_admin_audit_log.
func TestRecommendWeightWritesAudit(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	m := newPricingModel(t, svc, uid, "TEXT")
	createTestPrice(t, svc, uid, m.ID, 1000, 3000, 600)
	opts := testCallOptions{uid: uid, role: "OPS",
		pathVal: map[string]string{"id": itoa(m.ID)}}

	rec := call(t, svc.SetListingState, http.MethodPost,
		"/api/admin/models/1/listing-state",
		map[string]any{"listing_state": "LISTED", "reason": "测试上架"}, opts)
	if rec.Code != http.StatusOK {
		t.Fatalf("list model status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = call(t, svc.SetRecommendWeight, http.MethodPost,
		"/api/admin/models/1/recommend-weight",
		map[string]any{"recommend_weight": 10}, opts)
	if rec.Code != http.StatusOK {
		t.Fatalf("set weight status=%d body=%s", rec.Code, rec.Body.String())
	}

	var (
		before, after string
		operatorID    int64
	)
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT before_json, after_json, operator_id FROM t_admin_audit_log
		 WHERE module = 'MODEL' AND action = 'UPDATE' AND target_type = 't_model'
		   AND target_id = ?
		 ORDER BY id DESC LIMIT 1`, itoa(m.ID)).Scan(&before, &after, &operatorID); err != nil {
		t.Fatalf("load recommend-weight audit: %v", err)
	}
	if operatorID != uid {
		t.Fatalf("audit operator_id = %d, want %d", operatorID, uid)
	}
	// MySQL normalises JSON columns with a space after the colon.
	if !strings.Contains(before, `"recommend_weight": 0`) {
		t.Fatalf("before_json=%s, want recommend_weight 0", before)
	}
	if !strings.Contains(after, `"recommend_weight": 10`) {
		t.Fatalf("after_json=%s, want recommend_weight 10", after)
	}
}

// T46: max_output_tokens is the model's own output ceiling. POST and PATCH must
// carry it, GET must return it, and it may not exceed context_length — otherwise
// the snapshot publishes a ceiling the model cannot reach and the gateway
// reserves more output than the model can ever produce.
func TestModelMaxOutputTokensCreateReadPatch(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")
	body := modelBody(t, "t46-model-output-"+uniqueSuffix(), "CATALOG_ONLY")
	body["context_length"] = int64(8192)
	body["max_output_tokens"] = int64(4096)
	m := createTestModel(t, svc, uid, body)
	if m.ContextLength != 8192 || m.MaxOutputTokens != 4096 {
		t.Fatalf("create 返回 context_length=%d max_output_tokens=%d，期望 8192/4096（两列相邻，赋值错位会串值）",
			m.ContextLength, m.MaxOutputTokens)
	}

	// GET must read the same pair back; the two int64 columns are adjacent in
	// the SELECT and the scan, so a swap here would be silent.
	rec := call(t, svc.GetModel, http.MethodGet, "/api/admin/models/1", nil,
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rec.Code, rec.Body.String())
	}
	var detail modelDetailResponse
	decodeBody(t, rec, &detail)
	if detail.Model.ContextLength != 8192 || detail.Model.MaxOutputTokens != 4096 {
		t.Fatalf("detail context_length=%d max_output_tokens=%d，期望 8192/4096",
			detail.Model.ContextLength, detail.Model.MaxOutputTokens)
	}

	// PATCH may lower it to a value inside the context window.
	rec = call(t, svc.UpdateModel, http.MethodPatch, "/api/admin/models/1",
		map[string]any{"max_output_tokens": int64(2048)},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &detail)
	if detail.Model.MaxOutputTokens != 2048 {
		t.Fatalf("patch 后 max_output_tokens=%d，期望 2048", detail.Model.MaxOutputTokens)
	}

	// 0 means "not configured" and stays writable.
	rec = call(t, svc.UpdateModel, http.MethodPatch, "/api/admin/models/1",
		map[string]any{"max_output_tokens": int64(0)},
		testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch to 0 status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Above context_length is refused, as is a negative value.
	for _, value := range []int64{8193, -1} {
		rec = call(t, svc.UpdateModel, http.MethodPatch, "/api/admin/models/1",
			map[string]any{"max_output_tokens": value},
			testCallOptions{uid: uid, role: "OPS", pathVal: map[string]string{"id": itoa(m.ID)}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("max_output_tokens=%d status=%d body=%s, want 400", value, rec.Code, rec.Body.String())
		}
		eb := errorBody(t, rec)
		if eb.Error.Code != "invalid_request_body" || !strings.Contains(eb.Error.Message, "max_output_tokens") {
			t.Fatalf("max_output_tokens=%d body=%+v, want invalid_request_body 指向 max_output_tokens", value, eb.Error)
		}
	}
}

// T46: a create request may not record a ceiling above the context length it
// ships in the same body, and an unknown context_length (0) only requires the
// value to be non-negative.
func TestModelMaxOutputTokensCreateValidation(t *testing.T) {
	svc := newTestService(t)
	uid := newTestUser(t, "OPS")

	tooBig := modelBody(t, "t46-model-output-big-"+uniqueSuffix(), "CATALOG_ONLY")
	tooBig["context_length"] = int64(4096)
	tooBig["max_output_tokens"] = int64(8192)
	rec := call(t, svc.CreateModel, http.MethodPost, "/api/admin/models", tooBig,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create 超上下文 status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if eb := errorBody(t, rec); !strings.Contains(eb.Error.Message, "max_output_tokens") {
		t.Fatalf("body=%+v, want message 指向 max_output_tokens", eb.Error)
	}

	negative := modelBody(t, "t46-model-output-neg-"+uniqueSuffix(), "CATALOG_ONLY")
	negative["max_output_tokens"] = int64(-1)
	rec = call(t, svc.CreateModel, http.MethodPost, "/api/admin/models", negative,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create 负数 status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}

	// context_length unset (=0, "unknown") accepts any non-negative ceiling.
	unknown := modelBody(t, "t46-model-output-unknown-"+uniqueSuffix(), "CATALOG_ONLY")
	unknown["max_output_tokens"] = int64(65536)
	created := createTestModel(t, svc, uid, unknown)
	if created.MaxOutputTokens != 65536 || created.ContextLength != 0 {
		t.Fatalf("context_length 未知时 max_output_tokens=%d context_length=%d，期望 65536/0",
			created.MaxOutputTokens, created.ContextLength)
	}
}

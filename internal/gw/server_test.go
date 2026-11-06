package gw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/ratelimit"
	"yunshu.dev/core/internal/redisx"
)

type fakeLedger struct {
	mu       sync.Mutex
	preholds []ledger.PreholdParams
	settles  []ledger.SettleParams
	hold     int64
	err      error
}

func (f *fakeLedger) Prehold(_ context.Context, p ledger.PreholdParams) (ledger.PreholdResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preholds = append(f.preholds, p)
	if f.err != nil {
		return ledger.PreholdResult{}, f.err
	}
	return ledger.PreholdResult{RequestID: p.RequestID, HoldMicro: f.hold, State: ledger.HoldStateHeld}, nil
}

func (f *fakeLedger) Settle(_ context.Context, p ledger.SettleParams) (ledger.SettleResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settles = append(f.settles, p)
	return ledger.SettleResult{RequestID: p.RequestID, State: ledger.HoldStateReleased}, nil
}

func (f *fakeLedger) preholdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.preholds)
}

type fakeLimiter struct {
	mu       sync.Mutex
	verdict  ratelimit.Verdict
	err      error
	acquires int
	releases int
}

func (f *fakeLimiter) Acquire(context.Context, ratelimit.AcquireInput) (ratelimit.Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires++
	if f.err != nil {
		return ratelimit.Verdict{}, f.err
	}
	return f.verdict, nil
}

func (f *fakeLimiter) Release(context.Context, ratelimit.ReleaseInput) (ratelimit.ReleaseResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	return ratelimit.ReleaseResult{}, nil
}

func (f *fakeLimiter) RecordDailyRejection(context.Context, int64, string) error { return nil }

func (f *fakeLimiter) acquireCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acquires
}

func serverDeps(catalog *Catalog, auth AuthReader) *Deps {
	d := staticDeps(catalog)
	d.Auth = auth
	d.Prices = fakePrices{raw: jsonx.RawMessage(
		`{"id":11,"model_type":"TEXT","in_price_micro_per_k":1000,"out_price_micro_per_k":1000}`)}
	d.Balance = fakeBalance{micro: 1_000_000}
	d.Ledger = &fakeLedger{hold: 1}
	d.Limiter = &fakeLimiter{verdict: ratelimit.Verdict{
		Allowed: true, ConcurrencyMode: ratelimit.ConcurrencyRedis,
	}}
	return d
}

func authorizedAuth(key string) fakeAuth {
	return fakeAuth{records: map[string]redisx.KeyAuthRecord{
		KeyHash16(key): {UID: 7, KID: 10, ST: "ENABLED", GRP: "standard", GP: 10000},
	}}
}

func postChat(t *testing.T, handler http.Handler, key, model, rid string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("X-Request-Id", rid)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var decoded map[string]any
	_ = jsonx.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec, decoded
}

func errorBody(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	raw, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %v", decoded)
	}
	return raw
}

// TestServerErrorPathsEndToEnd exercises the full ingress + admission chain for
// every P05/P07 code the card requires.
func TestServerErrorPathsEndToEnd(t *testing.T) {
	key := "sk-ok"
	models := []ModelEntry{
		{ID: 1, ModelName: "listed", Region: RegionDomestic, ModelType: ModelTypeText,
			ContextLength: 8192, ListingState: ListingListed},
		{ID: 2, ModelName: "catalog-only", Region: RegionDomestic, ModelType: ModelTypeText,
			ListingState: ListingCatalogOnly},
		{ID: 3, ModelName: "delisted", Region: RegionDomestic, ModelType: ModelTypeText,
			ListingState: ListingDelisted},
		{ID: 4, ModelName: "unauthorized", Region: RegionDomestic, ModelType: ModelTypeText,
			ListingState: ListingListed},
		{ID: 5, ModelName: "out-of-group", Region: RegionDomestic, ModelType: ModelTypeText,
			ListingState: ListingListed},
	}
	catalog := staticCatalog(
		models,
		[]RouteEntry{{ID: 1, ModelID: 1, ChannelID: 1, Region: RegionDomestic, Weight: 1,
			Priority: 100, Enabled: true, StreamSupported: true}},
		[]ChannelEntry{{ID: 1, Region: RegionDomestic, GroupScopeMode: ScopeAll, Enabled: true,
			StreamSupported: true}},
		map[string]GroupEntry{"standard": {Code: "standard", ModelScopeMode: ScopeSelected,
			ChannelScopeMode: ScopeAll, Models: []int64{1, 2, 3, 4}}},
		map[string]KeyModelGrant{"10": {Mode: ScopeSelected, ModelIDs: []int64{1, 2, 3, 5}}},
	)

	cases := []struct {
		name    string
		model   string
		headers http.Header
		status  int
		code    string
	}{
		{
			name: "no key", model: "listed", status: 401, code: "invalid_api_key",
		},
		{
			name: "catalog only", model: "catalog-only", status: 503, code: "model_not_available",
			headers: http.Header{"Authorization": {"Bearer " + key}},
		},
		{
			name: "delisted", model: "delisted", status: 400, code: "model_not_found",
			headers: http.Header{"Authorization": {"Bearer " + key}},
		},
		{
			name: "key unauthorized", model: "unauthorized", status: 403, code: "model_not_authorized",
			headers: http.Header{"Authorization": {"Bearer " + key}},
		},
		{
			name: "group excludes", model: "out-of-group", status: 403, code: "model_not_in_group",
			headers: http.Header{"Authorization": {"Bearer " + key}},
		},
		{
			name: "conflicting auth", model: "listed", status: 400, code: "conflicting_auth_headers",
			headers: http.Header{"Authorization": {"Bearer " + key}, "X-Api-Key": {"sk-other"}},
		},
	}
	handler := NewHandler(serverDeps(catalog, authorizedAuth(key)), noopTerminal)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rid := "rid-" + strings.ReplaceAll(tc.name, " ", "-")
			body := `{"model":"` + tc.model + `","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			for name, values := range tc.headers {
				for _, v := range values {
					req.Header.Add(name, v)
				}
			}
			req.Header.Set("X-Request-Id", rid)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			var decoded map[string]any
			if err := jsonx.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			eb := errorBody(t, decoded)
			if eb["code"] != tc.code {
				t.Fatalf("code = %v, want %s", eb["code"], tc.code)
			}
			if got := rec.Header().Get("X-Request-Id"); got != rid {
				t.Fatalf("X-Request-Id = %q, want %q", got, rid)
			}
			if eb["request_id"] != rid {
				t.Fatalf("body request_id = %v, header = %q", eb["request_id"], rid)
			}
		})
	}
}

// TestServerModelsEndpointSkipsQuota proves /v1/models never acquires a rate
// limit lease and never writes a prehold.
func TestServerModelsEndpointSkipsQuota(t *testing.T) {
	key := "sk-models"
	catalog := staticCatalog(
		[]ModelEntry{
			{ID: 1, ModelName: "listed", Region: RegionDomestic, ModelType: ModelTypeText, ListingState: ListingListed},
			{ID: 2, ModelName: "catalog-only", Region: RegionDomestic, ModelType: ModelTypeText, ListingState: ListingCatalogOnly},
			{ID: 3, ModelName: "overseas", Region: RegionOverseas, ModelType: ModelTypeText, ListingState: ListingListed},
		},
		nil, nil,
		map[string]GroupEntry{"standard": {Code: "standard", ModelScopeMode: ScopeAll, ChannelScopeMode: ScopeAll}},
		map[string]KeyModelGrant{"10": {Mode: ScopeAll}},
	)
	deps := serverDeps(catalog, authorizedAuth(key))
	handler := NewHandler(deps, noopTerminal)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Request-Id", "rid-models")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Object string        `json:"object"`
		Data   []modelObject `json:"data"`
	}
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Object != "list" || len(body.Data) != 1 || body.Data[0].ID != "listed" {
		t.Fatalf("models = %+v", body)
	}
	if deps.Limiter.(*fakeLimiter).acquireCount() != 0 {
		t.Fatal("/v1/models must not call the rate limiter")
	}
	if deps.Ledger.(*fakeLedger).preholdCount() != 0 {
		t.Fatal("/v1/models must not prehold")
	}
}

// TestServerSuccessReachesTerminalAfterPrehold proves the six-slot pipeline
// runs in production wiring through P10.
func TestServerSuccessReachesTerminalAfterPrehold(t *testing.T) {
	key := "sk-ok"
	catalog := basicCatalog()
	deps := serverDeps(catalog, authorizedAuth(key))
	handler := NewHandler(deps, TerminalFunc(func(w http.ResponseWriter, _ *http.Request, _ *Ctx) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"terminal":true}`))
	}))

	rec, _ := postChat(t, handler, key, "qwen-plus", "rid-success")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if deps.Limiter.(*fakeLimiter).acquireCount() != 1 {
		t.Fatal("rate limiter was not called exactly once")
	}
	if deps.Ledger.(*fakeLedger).preholdCount() != 1 {
		t.Fatal("prehold was not called exactly once")
	}
}

func TestServerUnknownPathIs404(t *testing.T) {
	handler := NewHandler(serverDeps(basicCatalog(), authorizedAuth("sk-ok")), noopTerminal)
	req := httptest.NewRequest(http.MethodGet, "/v1/nonexistent", nil)
	req.Header.Set("X-Request-Id", "rid-404")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestServerOversizedBodyIs413BeforeAuth(t *testing.T) {
	handler := NewHandler(&Deps{}, noopTerminal)
	body := strings.NewReader(strings.Repeat("x", MaxBodyBytes+1))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("X-Request-Id", "rid-413")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestServerRateLimiterUnavailableIs503(t *testing.T) {
	key := "sk-ok"
	deps := serverDeps(basicCatalog(), authorizedAuth(key))
	deps.Limiter = &fakeLimiter{err: ratelimit.ErrUnavailable}
	handler := NewHandler(deps, noopTerminal)
	rec, decoded := postChat(t, handler, key, "qwen-plus", "rid-rl-down")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if errorBody(t, decoded)["code"] != "rate_limiter_unavailable" {
		t.Fatalf("body = %v", decoded)
	}
}

// T50: a client X-Request-Id wider than 36 bytes cannot reach the money path.
//
// t_prehold_hold.request_id and t_usage_record.request_id are char(36) (the
// documented format is a ULID or UUIDv7), while the header sanitiser accepts up
// to 128 bytes. Before this check the 37-128 range got through every stage and
// died inside Prehold's INSERT as MySQL 1406, which P10 reports as
// settlement_unavailable — a 503 carrying retry_after_seconds, so the client
// retries a request that can never succeed.
//
// 36 bytes must keep working: this asserts the boundary, not just the rejection.
func TestServeHTTPRejectsOverlongRequestID(t *testing.T) {
	handler := NewHandler(serverDeps(basicCatalog(), authorizedAuth("sk-ok")), noopTerminal)

	long := strings.Repeat("a", apierr.MaxStoredRequestIDLen+1)
	rec, decoded := postChat(t, handler, "sk-ok", "qwen-plus", long)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status for a %d-byte id = %d, want 400 (body %s)",
			len(long), rec.Code, rec.Body.String())
	}
	e := errorBody(t, decoded)
	if e["code"] != apierr.CodeInvalidRequestID {
		t.Fatalf("body code = %v, want %s", e["code"], apierr.CodeInvalidRequestID)
	}
	if got := rec.Header().Get("X-Request-Id"); got != long {
		t.Fatalf("X-Request-Id echo = %q, want the client value %q", got, long)
	}

	// The widest storable id must still be accepted: a check that rejects 36 too
	// would trade one broken population for another.
	ok := strings.Repeat("a", apierr.MaxStoredRequestIDLen)
	recOK, decodedOK := postChat(t, handler, "sk-ok", "qwen-plus", ok)
	if recOK.Code == http.StatusBadRequest {
		t.Fatalf("a %d-byte id was rejected: %v", len(ok), decodedOK)
	}
}

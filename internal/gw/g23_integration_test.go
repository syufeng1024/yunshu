package gw

import (
	"context"
	"net/http"
	"testing"
	"time"

	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/ratelimit"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/settle"
)

// TestG23FailureUsageLandsInDB is the process-level proof the unit test cannot
// give: a real request that dies at P11 (rate limit) actually produces a row in
// t_usage_record, not just a call to the FailureSink.
//
// It wires a real Limiter (Redis), a real UsageWriter (MySQL), a real auth
// cache and a real catalog with RPM=1 for the key, then drives two requests
// through the full ServeHTTP pipeline:
//
//   - request 1 is admitted and reaches the terminal; terminalRan suppresses
//     the backfill, so it must write no failure row.
//   - request 2 exhausts the RPM=1 window and is rejected 429 at P11; exactly
//     one backfill row must land in t_usage_record with the money columns zero.
func TestG23FailureUsageLandsInDB(t *testing.T) {
	client := newRedisClient(t)
	clk := redisx.NewClock(nil, nil)

	keyID := time.Now().UnixNano() & 0x7fffffff
	userID := keyID + 1
	key := "sk-g23-" + randomID(t)

	limiter := ratelimit.New(client, clk, nil, nil, nil)

	catalog := NewStaticCatalog(&RoutingSnapshot{
		Models: []ModelEntry{{
			ID: 1, ModelName: "qwen-plus", Region: RegionDomestic, ModelType: ModelTypeText,
			ContextLength: 8192, MaxOutputTokens: 4096, ListingState: ListingListed,
			DefaultPriceRuleID: 11,
		}},
		Routes: []RouteEntry{{
			ID: 1, ModelID: 1, ChannelID: 1, ProtocolID: 1, UpstreamModelName: "qwen-plus",
			Region: RegionDomestic, Weight: 100, Priority: 100, StreamSupported: true, Enabled: true,
		}},
		Channels: []ChannelEntry{{
			ID: 1, Name: "ch-1", ProtocolID: 1, Region: RegionDomestic, Weight: 100,
			Priority: 100, GroupScopeMode: ScopeAll, Enabled: true, StreamSupported: true,
		}},
		Groups: map[string]GroupEntry{
			"standard": {Code: "standard", RatioBP: 10000, ModelScopeMode: ScopeAll,
				ChannelScopeMode: ScopeAll, Enabled: true},
		},
		KeyModels:  map[string]KeyModelGrant{strKey(keyID): {Mode: ScopeAll}},
		RateLimits: map[string]LimitPolicy{"KEY:" + strKey(keyID): {RPM: 1, TPM: 1_000_000, Concurrency: 100}},
	})

	cache := redisx.NewKeyAuthCache(client)
	if err := cache.Upsert(t.Context(), KeyHash16(key), redisx.KeyAuthRecord{
		UID: userID, KID: keyID, ST: "ENABLED", GRP: "standard", GP: 10000,
	}); err != nil {
		t.Fatalf("upsert K13: %v", err)
	}

	writer := settle.NewUsageWriter(testDB, nil, clock.Real{})

	deps := serverDeps(catalog, cache)
	deps.Limiter = limiter
	deps.Clock = clk
	deps.CounterReader = RedisCounters{Client: client}
	// Same shape as cmd/gatewayd.rejectedUsageRecord: metadata + status pass
	// through, every money and token column stays at its zero value.
	deps.FailureSink = func(f FailureUsage) {
		writer.WriteUsage(settle.UsageRecord{
			RequestID:     f.RequestID,
			ConfigVersion: f.ConfigVersion,
			UserID:        f.UserID,
			APIKeyID:      f.APIKeyID,
			APIKeyName:    f.APIKeyName,
			ModelID:       f.ModelID,
			ModelName:     f.ModelName,
			RequestModel:  f.RequestModel,
			InboundProto:  f.InboundProto,
			PriceRuleID:   f.PriceRuleID,
			GroupCode:     f.GroupCode,
			PriceRatioBP:  f.PriceRatioBP,
			BillingTier:   "BASE",
			IsStream:      f.IsStream,
			HTTPStatus:    f.HTTPStatus,
			BizCode:       f.BizCode,
			Result:        "FAILED",
			FailStage:     f.FailStage,
			FailOwner:     f.FailOwner,
			UsageSource:   "NONE",
			TotalMS:       f.TotalMS,
			ClientIP:      f.ClientIP,
			UserAgent:     f.UserAgent,
			CreatedAt:     f.CreatedAt,
			RawBody:       f.RawBody,
		})
	}

	handler := NewHandler(deps, noopTerminal)

	ridOK := "g23-ok-" + randomID(t)
	ridFail := "g23-fail-" + randomID(t)
	t.Cleanup(func() {
		// Cleanup runs after t.Context() is cancelled; WithoutCancel keeps the
		// DELETE able to reach the DB (same pattern as seedAccount).
		_, _ = testDB.ExecContext(context.WithoutCancel(t.Context()),
			`DELETE FROM t_usage_record WHERE request_id IN (?, ?)`, ridOK, ridFail)
	})

	// Request 1: admitted (RPM=1), reaches the terminal. terminalRan=true, so no
	// failure row may be written for it.
	recOK, _ := postChat(t, handler, key, "qwen-plus", ridOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200 (body %s)", recOK.Code, recOK.Body.String())
	}

	// Request 2: same key, same minute -> RPM exhausted -> 429 at P11.
	recFail, _ := postChat(t, handler, key, "qwen-plus", ridFail)
	if recFail.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429 (body %s)", recFail.Code, recFail.Body.String())
	}

	// The writer is async; flush it synchronously so the row is durable.
	if err := writer.Flush(t.Context()); err != nil {
		t.Fatalf("flush usage writer: %v", err)
	}

	var (
		httpStatus            int
		bizCode, result       string
		failStage, failOwner  string
		usageSource           string
		prehold, consumed     int64
		recharge, bonus       int64
		refunded, attempts    int64
		tokensIn, tokensOut   int64
	)
	err := testDB.QueryRowContext(t.Context(), `
		SELECT http_status, biz_code, result, fail_stage, fail_owner, usage_source,
		       prehold_micro, consumed_micro, consumed_recharge_micro,
		       consumed_bonus_micro, refunded_micro, upstream_attempts,
		       tokens_in, tokens_out
		FROM t_usage_record WHERE request_id = ?`, ridFail,
	).Scan(&httpStatus, &bizCode, &result, &failStage, &failOwner, &usageSource,
		&prehold, &consumed, &recharge, &bonus, &refunded, &attempts, &tokensIn, &tokensOut)
	if err != nil {
		t.Fatalf("failed request row not found in t_usage_record: %v", err)
	}

	if httpStatus != 429 {
		t.Errorf("http_status = %d, want 429", httpStatus)
	}
	if bizCode != "rpm_limit_exceeded" {
		t.Errorf("biz_code = %q, want rpm_limit_exceeded", bizCode)
	}
	if result != "FAILED" {
		t.Errorf("result = %q, want FAILED", result)
	}
	if failStage != "RATE_LIMIT" {
		t.Errorf("fail_stage = %q, want RATE_LIMIT", failStage)
	}
	if failOwner != "CLIENT" {
		t.Errorf("fail_owner = %q, want CLIENT", failOwner)
	}
	if usageSource != "NONE" {
		t.Errorf("usage_source = %q, want NONE", usageSource)
	}
	for _, m := range []struct {
		name string
		v    int64
	}{
		{"prehold_micro", prehold},
		{"consumed_micro", consumed},
		{"consumed_recharge_micro", recharge},
		{"consumed_bonus_micro", bonus},
		{"refunded_micro", refunded},
		{"upstream_attempts", attempts},
		{"tokens_in", tokensIn},
		{"tokens_out", tokensOut},
	} {
		if m.v != 0 {
			t.Errorf("%s = %d, want 0", m.name, m.v)
		}
	}

	// No double write: the admitted request (terminalRan=true) must not have
	// produced a backfill row, and the failed request must produce exactly one.
	var nOK, nFail int
	if err := testDB.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM t_usage_record WHERE request_id = ?`, ridOK).Scan(&nOK); err != nil {
		t.Fatalf("count success rows: %v", err)
	}
	if nOK != 0 {
		t.Errorf("success request wrote %d rows, want 0 (terminalRan must suppress the backfill)", nOK)
	}
	if err := testDB.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM t_usage_record WHERE request_id = ?`, ridFail).Scan(&nFail); err != nil {
		t.Fatalf("count failure rows: %v", err)
	}
	if nFail != 1 {
		t.Errorf("failed request wrote %d rows, want 1", nFail)
	}
}

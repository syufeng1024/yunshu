package settle

import (
	"context"
	"errors"
	"testing"
	"time"

	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

// The P28/P29 writer is the only path that turns a terminal request into a
// database row. This test drives the real INSERT lists: a column mismatch
// between UsageRecord and t_usage_record fails here, not in production.
func TestP28P29WriterPersistsOneRowEach(t *testing.T) {
	ctx := context.Background()
	rid := newRequestID(t)
	userID := newFundedUser(t)
	now := time.Now().UTC().Truncate(time.Millisecond)

	w := NewUsageWriter(testDB, nil, clock.Real{})
	w.WriteUsage(UsageRecord{
		RequestID:     rid,
		UserID:        userID,
		APIKeyID:      5,
		APIKeyName:    "k",
		ModelID:       0,
		ModelName:     "m",
		RequestModel:  "m",
		InboundProto:  "OPENAI_COMPAT",
		ChannelID:     10,
		ChannelName:   "ch",
		OutboundProto: "OPENAI_COMPAT",
		RouteID:       1,
		PriceRuleID:   2,
		GroupCode:     "standard",
		PriceRatioBP:  10000,
		BillingTier:   "BASE",
		HTTPStatus:    200,
		Result:        "SUCCESS",
		TokensIn:      5,
		TokensOut:     4,
		UsageSource:   "UPSTREAM_API",
		PreholdMicro:  1000,
		ConsumedMicro: 42,
		RefundedMicro: 958,
		TotalMS:       12,
		UpstreamMS:    11,
		CreatedAt:     now,
	})
	w.WriteAttempt(AttemptRecord{
		RequestID:     rid,
		AttemptIndex:  1,
		UserID:        userID,
		ModelID:       1,
		RouteID:       1,
		ChannelID:     10,
		ChannelName:   "ch",
		ProtocolCode:  "OPENAI_COMPAT",
		UpstreamModel: "m",
		Phase:         "P17",
		Result:        "SUCCESS",
		HTTPStatus:    200,
		TotalMS:       11,
		CreatedAt:     now,
	})
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var (
		consumed, prehold, tokensIn, tokensOut int64
		modelID                                *int64
	)
	if err := testDB.QueryRow(
		`SELECT consumed_micro, prehold_micro, tokens_in, tokens_out, model_id
		   FROM t_usage_record WHERE request_id = ?`, rid,
	).Scan(&consumed, &prehold, &tokensIn, &tokensOut, &modelID); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if consumed != 42 || prehold != 1000 || tokensIn != 5 || tokensOut != 4 {
		t.Fatalf("usage row = consumed %d prehold %d tokens %d/%d", consumed, prehold, tokensIn, tokensOut)
	}
	if modelID != nil {
		t.Fatalf("model_id = %v, want NULL for id 0", *modelID)
	}

	attemptRows := countRows(t, `SELECT COUNT(*) FROM t_upstream_attempt WHERE request_id = ?`, rid)
	if attemptRows != 1 {
		t.Fatalf("attempt rows = %d, want 1", attemptRows)
	}
	t.Logf("P28/P29 request=%s usage_rows=1 attempt_rows=%d consumed=%d prehold=%d",
		rid, attemptRows, consumed, prehold)
}

func TestEnqueueIsIdempotent(t *testing.T) {
	ctx := context.Background()
	rid := newRequestID(t)
	t.Cleanup(func() { _, _ = testDB.Exec(`DELETE FROM t_settle_task WHERE request_id = ?`, rid) })

	enq := NewEnqueuer(testDB, clock.Real{})
	for i := 0; i < 2; i++ {
		if err := enq.Enqueue(ctx, EnqueueParams{RequestID: rid, UserID: 1, ConsumedMicro: 10}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if n := countRows(t, `SELECT COUNT(*) FROM t_settle_task WHERE request_id = ?`, rid); n != 1 {
		t.Fatalf("task rows = %d, want 1", n)
	}
}

type failingSettler struct{}

func (failingSettler) Settle(context.Context, ledger.SettleParams) (ledger.SettleResult, error) {
	return ledger.SettleResult{}, errors.New("boom")
}

// A task that exhausts its retry ladder becomes DEAD with the error recorded;
// it must not be silently dropped or retried forever.
func TestWorkerMarksDeadAtMaxAttempts(t *testing.T) {
	ctx := context.Background()
	rid := newRequestID(t)
	t.Cleanup(func() { _, _ = testDB.Exec(`DELETE FROM t_settle_task WHERE request_id = ?`, rid) })

	enq := NewEnqueuer(testDB, clock.Real{})
	if err := enq.Enqueue(ctx, EnqueueParams{RequestID: rid, UserID: 1, ConsumedMicro: 10}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	w := NewWorker(testDB, failingSettler{}, clock.Real{}, nil, "settle-test")
	w.MaxAttempts = 1
	res, err := w.ProcessOne(ctx)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if !res.Processed || res.Settled || res.Err == nil {
		t.Fatalf("result = %+v", res)
	}
	var state, lastError string
	if err := testDB.QueryRow(
		`SELECT state, last_error FROM t_settle_task WHERE request_id = ?`, rid,
	).Scan(&state, &lastError); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if state != TaskStateDead {
		t.Fatalf("state = %s, want %s", state, TaskStateDead)
	}
	if lastError != "boom" {
		t.Fatalf("last_error = %q", lastError)
	}
}

// P30 writes K14 and K15 through one pipeline. The channel counter must be
// incremented once, not once per scope delta.
func TestMetricsFlushWritesK14AndK15(t *testing.T) {
	ctx := context.Background()
	rdb := redistest.NewClient(0)
	t.Cleanup(func() { _ = rdb.Close() })
	epoch := redisx.MinEpochFromMS(time.Now().UnixMilli())

	m := NewMetrics(rdb, nil, clock.Real{})
	m.Add(MetricDelta{
		ScopeType: "USER", ScopeID: 4242, ChannelID: 10, MinEpoch: epoch,
		Requests: 1, Success: 1, TokensIn: 5, TokensOut: 4, ConsumedMicro: 42,
	})
	m.Add(MetricDelta{
		ScopeType: "KEY", ScopeID: 77, MinEpoch: epoch,
		Requests: 1, Success: 1, TokensIn: 5, TokensOut: 4, ConsumedMicro: 42,
	})
	if err := m.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	assertHash := func(key string, wantReq, wantConsumed string) {
		t.Helper()
		got, err := rdb.Raw().HGetAll(ctx, key).Result()
		if err != nil {
			t.Fatalf("hgetall %s: %v", key, err)
		}
		if got["req"] != wantReq || got["consumed_micro"] != wantConsumed {
			t.Fatalf("%s = %v, want req=%s consumed=%s", key, got, wantReq, wantConsumed)
		}
	}
	assertHash(redisx.MetricKey("USER", 4242, epoch), "1", "42")
	assertHash(redisx.MetricKey("KEY", 77, epoch), "1", "42")
	assertHash(redisx.ChannelMetricKey(10, epoch), "1", "42")
}

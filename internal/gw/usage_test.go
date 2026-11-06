package gw

import (
	"testing"
	"time"

	"yunshu.dev/core/internal/apierr"
)

// TestEmitFailureUsage pins the backfill gate. Every "skips" case must not call
// the sink; the two "lands" cases assert the normalised fail_stage and the
// http_status/biz_code passthrough.
func TestEmitFailureUsage(t *testing.T) {
	newSink := func() (*Handler, *FailureUsage) {
		var got FailureUsage
		h := &Handler{d: &Deps{FailureSink: func(f FailureUsage) { got = f }}}
		return h, &got
	}
	baseCtx := func() *Ctx {
		return &Ctx{
			RequestID: "req-1",
			StartedAt: time.Now(),
			Endpoint:  EndpointChat,
			Auth:      &AuthContext{UserID: 1, KeyID: 2, KeyName: "k", GroupCode: "standard"},
			FailStage: "P11",
			ErrCode:   apierr.CodeRPMLimitExceeded,
		}
	}

	t.Run("p11 429 lands", func(t *testing.T) {
		h, got := newSink()
		emitFailureUsage(h, baseCtx(), false)
		if got.RequestID != "req-1" {
			t.Fatalf("RequestID = %q, want req-1", got.RequestID)
		}
		if got.HTTPStatus != 429 {
			t.Fatalf("HTTPStatus = %d, want 429", got.HTTPStatus)
		}
		if got.BizCode != apierr.CodeRPMLimitExceeded {
			t.Fatalf("BizCode = %q, want %s", got.BizCode, apierr.CodeRPMLimitExceeded)
		}
		if got.FailStage != "RATE_LIMIT" {
			t.Fatalf("FailStage = %q, want RATE_LIMIT", got.FailStage)
		}
		if got.FailOwner != "CLIENT" {
			t.Fatalf("FailOwner = %q, want CLIENT", got.FailOwner)
		}
		if got.Result != "FAILED" {
			t.Fatalf("Result = %q, want FAILED", got.Result)
		}
	})

	t.Run("terminalRan skips", func(t *testing.T) {
		h, got := newSink()
		emitFailureUsage(h, baseCtx(), true)
		if got.RequestID != "" {
			t.Fatalf("sink called with terminalRan=true: %+v", *got)
		}
	})

	t.Run("nil auth skips", func(t *testing.T) {
		h, got := newSink()
		c := baseCtx()
		c.Auth = nil
		emitFailureUsage(h, c, false)
		if got.RequestID != "" {
			t.Fatalf("sink called with nil Auth: %+v", *got)
		}
	})

	t.Run("models endpoint skips", func(t *testing.T) {
		h, got := newSink()
		c := baseCtx()
		c.Endpoint = EndpointModels
		emitFailureUsage(h, c, false)
		if got.RequestID != "" {
			t.Fatalf("sink called for /v1/models: %+v", *got)
		}
	})

	t.Run("connection_reset skips", func(t *testing.T) {
		h, got := newSink()
		c := baseCtx()
		c.ErrCode = "connection_reset"
		emitFailureUsage(h, c, false)
		if got.RequestID != "" {
			t.Fatalf("sink called for connection_reset: %+v", *got)
		}
	})

	t.Run("nil sink skips", func(t *testing.T) {
		emitFailureUsage(&Handler{d: &Deps{}}, baseCtx(), false)
	})

	t.Run("empty fail_stage still lands", func(t *testing.T) {
		h, got := newSink()
		c := baseCtx()
		c.FailStage = ""
		c.ErrCode = apierr.CodeRateLimiterUnavailable
		emitFailureUsage(h, c, false)
		if got.RequestID == "" {
			t.Fatal("expected a row despite empty FailStage")
		}
		if got.FailStage != "" {
			t.Fatalf("FailStage = %q, want empty", got.FailStage)
		}
		if got.HTTPStatus != 503 {
			t.Fatalf("HTTPStatus = %d, want 503", got.HTTPStatus)
		}
		if got.BizCode != apierr.CodeRateLimiterUnavailable {
			t.Fatalf("BizCode = %q, want %s", got.BizCode, apierr.CodeRateLimiterUnavailable)
		}
	})

	// A-12 is fixed at the table layer (settle.UsageWriter overwrites a FAILED/0
	// row when the request that really ran settles), so the 409 row is still
	// written here: it is the only record that the id was replayed, and it no
	// longer blocks the real row. This test pins that the failure path is
	// unchanged for duplicate_request_id.
	t.Run("duplicate_request_id still lands", func(t *testing.T) {
		h, got := newSink()
		c := baseCtx()
		c.FailStage = "P10"
		c.ErrCode = apierr.CodeDuplicateRequestID
		emitFailureUsage(h, c, false)
		if got.RequestID == "" {
			t.Fatal("duplicate_request_id must still produce a usage record")
		}
		if got.HTTPStatus != 409 || got.BizCode != apierr.CodeDuplicateRequestID {
			t.Fatalf("row = %d/%s, want 409/%s", got.HTTPStatus, got.BizCode, apierr.CodeDuplicateRequestID)
		}
		if got.Result != "FAILED" {
			t.Fatalf("row = %s, want FAILED", got.Result)
		}
	})

	// The overwrite predicate keys on consumed_micro, not on a code list, so
	// every eager failure code that can steal the key is covered by the same
	// rule; this one is the pre-existing B7b shape.
	t.Run("other 409 still lands", func(t *testing.T) {
		h, got := newSink()
		c := baseCtx()
		c.ErrCode = apierr.CodeIdempotencyKeyReused
		emitFailureUsage(h, c, false)
		if got.RequestID == "" {
			t.Fatal("the skip must not widen to every 409")
		}
		if got.HTTPStatus != 409 || got.BizCode != apierr.CodeIdempotencyKeyReused {
			t.Fatalf("row = %d/%s, want 409/%s", got.HTTPStatus, got.BizCode, apierr.CodeIdempotencyKeyReused)
		}
	})
}

func TestStageForFail(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"P01", "REQUEST"}, {"P02", "REQUEST"}, {"P03", "REQUEST"}, {"P04", "REQUEST"},
		{"P05", "AUTH"}, {"P06", "AUTH"}, {"P06.5", "AUTH"}, {"P07", "AUTH"},
		{"P08", "AUTH"}, {"P08.5", "AUTH"},
		{"P09", "SETTLE"}, {"P10", "SETTLE"},
		{"P11", "RATE_LIMIT"},
		{"P12", "ROUTE"}, {"P13", "ROUTE"},
		{"P14", "UPSTREAM"},
		{"P15", "UPSTREAM"}, {"P16", "UPSTREAM"}, {"P17", "UPSTREAM"},
		{"P18", "UPSTREAM"}, {"P19", "UPSTREAM"},
		{"P20", "UPSTREAM"},
		{"P21", "UPSTREAM"}, {"P22", "UPSTREAM"}, {"P23", "UPSTREAM"},
		{"P24", "CLIENT_ABORT"}, {"P25", "UPSTREAM"},
		{"P26", "SETTLE"}, {"P27", "SETTLE"}, {"P28", "SETTLE"}, {"P29", "SETTLE"},
		{"P30", ""},
		{"P31", ""},
		{"", ""}, {"garbage", ""}, {"p11", ""},
	}
	for _, tc := range cases {
		if got := stageForFail(tc.in); got != tc.want {
			t.Errorf("stageForFail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOwnerForCode(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{apierr.CodeInvalidAPIKey, "CLIENT"},
		{apierr.CodeRPMLimitExceeded, "CLIENT"},
		{apierr.CodeUpstreamTimeout, "UPSTREAM"},
		{apierr.CodeInternalError, "PLATFORM"},
		{apierr.CodeConfigUnavailable, "PLATFORM"},
		{"not_a_code", "PLATFORM"},
	}
	for _, tc := range cases {
		if got := ownerForCode(tc.code); got != tc.want {
			t.Errorf("ownerForCode(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

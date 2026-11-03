package ratelimit

import (
	"testing"

	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

// This benchmark exists because the degradation mode is a startup choice
// (GW_RATELIMIT_DEGRADE_MODE, default off). Before G31, concurrency_only spent
// two extra Redis reads per request on Degrader.Replicas; after G31 the divisor
// is cached and only the in-process local counter adds overhead, so the two
// modes should converge. The cost is part of the decision, so it should be
// measurable rather than guessed:
//
//	go test ./internal/ratelimit/ -run XXX -bench BenchmarkAcquireMode -benchtime=500x
//
// Pre-G31 local result (2026-11-02, Redis 127.0.0.1): off ≈ 0.71 ms/op,
// concurrency_only ≈ 2.47 ms/op — about 1.8 ms and two round trips more per
// request. G31 removes those two round trips from the request path.
func benchAcquire(b *testing.B, mode Mode, keyID int64) {
	b.Helper()
	raw := redistest.Client()
	// Deliberately NOT redistest.NewClient: this benchmark measures per-op
	// latency, so it keeps the production 50ms budget and the degradation path
	// it is benchmarking. It is not a correctness fixture — do not raise it.
	client := redisx.New(redisx.Options{Addr: redistest.Addr(), DB: redistest.DB()})
	clock := redisx.NewStaticClock(baseTime)
	degrader := NewDegrader(client, clock, "bench", mode, 0)
	limiter := New(client, clock, degrader, &captureMetrics{}, nil)
	b.Cleanup(func() {
		_ = client.Close()
		_ = raw.FlushDB(b.Context()).Err()
	})

	limits := Limits{
		RPM: 1_000_000, TPM: 1_000_000_000,
		Concurrency: 1_000_000, DailyQuotaMicro: 1_000_000_000, DailyEnabled: true,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := limiter.Acquire(b.Context(), AcquireInput{
			KeyID: keyID, UserID: keyID, RequestID: "bench", Limits: limits,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAcquireModeOff(b *testing.B)             { benchAcquire(b, ModeOff, 900) }
func BenchmarkAcquireModeConcurrencyOnly(b *testing.B) { benchAcquire(b, ModeConcurrencyOnly, 901) }

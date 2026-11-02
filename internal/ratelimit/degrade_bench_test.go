package ratelimit

import (
	"testing"

	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

// This benchmark exists because the degradation mode is now a startup choice
// (GW_RATELIMIT_DEGRADE_MODE, default concurrency_only). In concurrency_only
// mode every Acquire goes through acquireDegraded, which spends two extra Redis
// reads on Degrader.Replicas before the usual Lua call — the cost is part of the
// decision, so it should be measurable rather than guessed:
//
//	go test ./internal/ratelimit/ -run XXX -bench BenchmarkAcquireMode -benchtime=500x
//
// Local result (2026-11-02, Redis 127.0.0.1): off ≈ 0.71 ms/op,
// concurrency_only ≈ 2.47 ms/op — about 1.8 ms and two round trips more per
// request.
func benchAcquire(b *testing.B, mode Mode, keyID int64) {
	b.Helper()
	raw := redistest.Client()
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

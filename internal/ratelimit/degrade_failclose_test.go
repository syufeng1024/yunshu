package ratelimit

import (
	"errors"
	"testing"
	"time"

	"yunshu.dev/core/internal/redisx"
)

// TestDegradedModeRejectsLocallyWhenRedisIsDown is the G31 acceptance baseline.
//
// Before G31, acquireDegraded's first act was Degrader.Replicas(ctx), a
// ZREMRANGEBYSCORE + ZCARD pair that failed as soon as Redis went down — so the
// concurrency fallback was unreachable and every outage ended in
// ErrUnavailable (503). After G31 the replica count is a cached last-known
// value: the concurrency dimension reaches the node-local counter and rejects
// with 429 once it is full, while the Redis-backed RPM/TPM/daily check still
// fails closed when the local counter is not full.
func TestDegradedModeRejectsLocallyWhenRedisIsDown(t *testing.T) {
	dead := func() *redisx.Client {
		return redisx.New(redisx.Options{
			Addr:           "127.0.0.1:1",
			DB:             15,
			DialTimeout:    50 * time.Millisecond,
			CommandTimeout: 50 * time.Millisecond,
			LuaTimeout:     100 * time.Millisecond,
		})
	}

	t.Run("local concurrency full yields 429", func(t *testing.T) {
		bad := dead()
		defer bad.Close()
		degrader := NewDegrader(bad, redisx.NewStaticClock(baseTime), "n1", ModeConcurrencyOnly, 0)
		if !degrader.Enabled() {
			t.Fatal("degrader is not enabled; the mode was not wired through")
		}
		// Seed the cached divisor as if a prior RefreshReplicas succeeded while
		// Redis was up. Replicas() must serve it without touching Redis now.
		degrader.replicas.Store(3)

		metrics := &captureMetrics{}
		limiter := New(bad, redisx.NewStaticClock(baseTime), degrader, metrics, nil)

		limits := Limits{RPM: 1000, TPM: 1_000_000, Concurrency: 10, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
		perKey := PerKeyLocalLimit(limits.Concurrency, degrader.Replicas())

		// Fill the local counter to the ceiling directly: each full Acquire
		// would pass TryAcquire and then fail the Redis-backed check, releasing
		// its local slot again.
		for i := int64(0); i < perKey; i++ {
			if !degrader.TryAcquire(70, perKey) {
				t.Fatalf("failed to fill local counter at slot %d", i)
			}
		}

		verdict, err := limiter.Acquire(t.Context(), AcquireInput{
			KeyID: 70, UserID: 70, RequestID: "degraded-down-full",
			EstTokens: 10, EstMicro: 1, Limits: limits,
		})
		// Allowed is the only thing that must never happen: it would mean an
		// outage admitted traffic past a fail-close dimension.
		if verdict.Allowed {
			t.Fatalf("admitted while Redis is down and local counter is full: verdict=%+v err=%v", verdict, err)
		}
		if err != nil {
			t.Fatalf("err = %v; want nil (local concurrency rejection, not fail-close)", err)
		}
		if verdict.Code != CodeConcurrency {
			t.Fatalf("code = %q; want %s", verdict.Code, CodeConcurrency)
		}
		if !hasMetric(metrics.rejected, "concurrency") {
			t.Fatalf("rejected dims = %v; want concurrency", metrics.rejected)
		}
	})

	t.Run("local concurrency not full still fails close", func(t *testing.T) {
		bad := dead()
		defer bad.Close()
		degrader := NewDegrader(bad, redisx.NewStaticClock(baseTime), "n1", ModeConcurrencyOnly, 0)
		degrader.replicas.Store(3)

		metrics := &captureMetrics{}
		limiter := New(bad, redisx.NewStaticClock(baseTime), degrader, metrics, nil)

		limits := Limits{RPM: 1000, TPM: 1_000_000, Concurrency: 10, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
		verdict, err := limiter.Acquire(t.Context(), AcquireInput{
			KeyID: 71, UserID: 71, RequestID: "degraded-down-open",
			EstTokens: 10, EstMicro: 1, Limits: limits,
		})
		if verdict.Allowed {
			t.Fatalf("admitted while Redis is down on the RPM/TPM/daily dimensions: verdict=%+v", verdict)
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v; want ErrUnavailable (RPM/TPM/daily fail-close)", err)
		}
	})
}

func hasMetric(dims []string, want string) bool {
	for _, d := range dims {
		if d == want {
			return true
		}
	}
	return false
}

// ModeFromEnv is the startup contract: unset means TDS §2.3's single V1 value,
// a known value passes through, and anything else is an error the process will
// refuse to start on.
func TestModeFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    Mode
		wantErr bool
	}{
		{"unset means off", "", ModeOff, false},
		{"blank is unset", "   ", ModeOff, false},
		{"explicit concurrency_only", "concurrency_only", ModeConcurrencyOnly, false},
		{"explicit off", "off", ModeOff, false},
		{"case is not a synonym", "Concurrency_Only", "", true},
		{"typo fails startup", "concurency_only", "", true},
		{"unknown word fails startup", "degrade", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDegradeMode, tc.value)
			got, err := ModeFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ModeFromEnv(%q) = %q, nil; want an error", tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ModeFromEnv(%q) = %v; want %q", tc.value, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("ModeFromEnv(%q) = %q; want %q", tc.value, got, tc.want)
			}
		})
	}
}

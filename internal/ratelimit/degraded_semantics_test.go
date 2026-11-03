package ratelimit

import (
	"errors"
	"testing"
	"time"

	"yunshu.dev/core/internal/redisx"
)

// deadRedisClient is a client pointed at a closed port: every Redis command
// fails fast, which is exactly the "Redis unavailable" branch.
func deadRedisClient() *redisx.Client {
	return redisx.New(redisx.Options{
		Addr:           "127.0.0.1:1",
		DB:             15,
		DialTimeout:    50 * time.Millisecond,
		CommandTimeout: 50 * time.Millisecond,
		LuaTimeout:     100 * time.Millisecond,
	})
}

// TestDegradedCounterCountsOutagesNotAdmissions is the G29 acceptance baseline.
//
// With mode=concurrency_only every request takes acquireDegraded, and the old
// code incremented gw_ratelimit_degraded_total on that path's *success* branch.
// The counter therefore counted admissions, so "degraded" and "busy" became the
// same number and the outage signal was gone. The counter must move only when
// Redis is actually failing.
func TestDegradedCounterCountsOutagesNotAdmissions(t *testing.T) {
	flushRedis(t)
	clock := redisx.NewStaticClock(baseTime)
	limits := Limits{RPM: 1000, TPM: 1_000_000, Concurrency: 100, DailyQuotaMicro: 1_000_000, DailyEnabled: true}

	// One degrader shared by two limiters, so the outage observation set by the
	// broken client is visible to the next request — the same object graph the
	// gateway uses (one Degrader per process, one client).
	degrader := NewDegrader(nil, clock, "n1", ModeConcurrencyOnly, 0)
	if !degrader.Enabled() {
		t.Fatal("degrader is not enabled; the mode was not wired through")
	}

	t.Run("healthy Redis: no degraded count", func(t *testing.T) {
		flushRedis(t)
		metrics := &captureMetrics{}
		healthy := newTestClient(t)
		limiter := New(healthy, clock, degrader, metrics, nil)

		for _, rid := range []string{"g29-h1", "g29-h2", "g29-h3"} {
			verdict, err := limiter.Acquire(t.Context(), AcquireInput{
				KeyID: 91, UserID: 91, RequestID: rid,
				EstTokens: 10, EstMicro: 1, Limits: limits,
			})
			if err != nil || !verdict.Allowed {
				t.Fatalf("%s: allowed=%v err=%v", rid, verdict.Allowed, err)
			}
			if verdict.ConcurrencyMode != ConcurrencyLocal {
				t.Fatalf("%s: concurrency mode = %q; the mode is on, so it must be local",
					rid, verdict.ConcurrencyMode)
			}
		}
		if len(metrics.degraded) != 0 {
			t.Fatalf("gw_ratelimit_degraded_total moved %d times on a healthy Redis with "+
				"concurrency_only on: %v. It would be an admission counter, not an outage "+
				"counter (G29)", len(metrics.degraded), metrics.degraded)
		}
		if degrader.RedisDown() {
			t.Fatalf("degrader thinks Redis is down after three successful acquisitions")
		}
	})

	t.Run("Redis down: counted only once the outage is observed", func(t *testing.T) {
		dead := deadRedisClient()
		defer dead.Close()
		metrics := &captureMetrics{}
		limiter := New(dead, clock, degrader, metrics, nil)

		// First request discovers the outage but was decided before knowing
		// about it: it must not be retroactively labelled as degraded, and it
		// still fails closed (RPM/TPM/daily are never answered locally).
		_, err := limiter.Acquire(t.Context(), AcquireInput{
			KeyID: 92, UserID: 92, RequestID: "g29-d1",
			EstTokens: 10, EstMicro: 1, Limits: limits,
		})
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("first request with Redis down: err = %v, want ErrUnavailable", err)
		}
		if len(metrics.degraded) != 0 {
			t.Fatalf("degraded counter moved before the outage was observed: %v", metrics.degraded)
		}
		if len(metrics.unavailable) == 0 {
			t.Fatalf("gw_ratelimit_unavailable_total did not move; it is the failure signal")
		}
		if !degrader.RedisDown() {
			t.Fatalf("degrader did not record the Redis failure")
		}

		// Second request runs while Redis is known to be down: this is the
		// only situation the degraded counter is for.
		_, err = limiter.Acquire(t.Context(), AcquireInput{
			KeyID: 92, UserID: 92, RequestID: "g29-d2",
			EstTokens: 10, EstMicro: 1, Limits: limits,
		})
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("second request with Redis down: err = %v, want ErrUnavailable", err)
		}
		if len(metrics.degraded) != 1 || metrics.degraded[0] != "concurrency" {
			t.Fatalf("degraded dims = %v; want exactly [concurrency] for the request that ran "+
				"with Redis known to be down", metrics.degraded)
		}
		t.Logf("degraded=%v unavailable=%d redisDown=%t",
			metrics.degraded, len(metrics.unavailable), degrader.RedisDown())
	})

	t.Run("recovery clears the observation", func(t *testing.T) {
		flushRedis(t)
		healthy := newTestClient(t)
		metrics := &captureMetrics{}
		limiter := New(healthy, clock, degrader, metrics, nil)

		// The documented ±1-request lag: the recovery request is still labelled
		// "degraded" because that label comes from the previous request's
		// outcome, and no request can know Redis is back before calling it.
		// This is the price of keeping a health probe off the hot path; the
		// next request proves the lag is one request long, not permanent.
		verdict, err := limiter.Acquire(t.Context(), AcquireInput{
			KeyID: 93, UserID: 93, RequestID: "g29-r1",
			EstTokens: 10, EstMicro: 1, Limits: limits,
		})
		if err != nil || !verdict.Allowed {
			t.Fatalf("after recovery: allowed=%v err=%v", verdict.Allowed, err)
		}
		if len(metrics.degraded) != 1 {
			t.Fatalf("degraded dims = %v; want exactly 1 (the stale observation carried by the "+
				"first post-recovery request)", metrics.degraded)
		}
		if degrader.RedisDown() {
			t.Fatalf("degrader still reports Redis down after a successful acquisition")
		}

		for _, rid := range []string{"g29-r2", "g29-r3"} {
			if _, err := limiter.Acquire(t.Context(), AcquireInput{
				KeyID: 93, UserID: 93, RequestID: rid,
				EstTokens: 10, EstMicro: 1, Limits: limits,
			}); err != nil {
				t.Fatalf("%s: %v", rid, err)
			}
		}
		if len(metrics.degraded) != 1 {
			t.Fatalf("degraded dims = %v after two more healthy requests; the counter must stop "+
				"at the one stale observation", metrics.degraded)
		}
	})
}

// TestDegradedCounterStillSilentInOffMode keeps the pre-existing behaviour:
// mode=off never takes the degraded path, so the counter cannot move at all.
func TestDegradedCounterStillSilentInOffMode(t *testing.T) {
	flushRedis(t)
	clock := redisx.NewStaticClock(baseTime)
	metrics := &captureMetrics{}
	client := newTestClient(t)
	limiter := New(client, clock, NewDegrader(client, clock, "n1", ModeOff, 0), metrics, nil)

	if _, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 94, UserID: 94, RequestID: "g29-off", EstTokens: 1, EstMicro: 1,
		Limits: Limits{RPM: 100, TPM: 1_000, Concurrency: 10, DailyQuotaMicro: 1_000, DailyEnabled: true},
	}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if len(metrics.degraded) != 0 {
		t.Fatalf("degraded counter moved in off mode: %v", metrics.degraded)
	}
	if limiter.degrader.RedisDown() {
		t.Fatalf("off mode must not observe Redis state at all")
	}
}

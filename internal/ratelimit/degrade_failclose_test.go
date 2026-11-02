package ratelimit

import (
	"errors"
	"testing"
	"time"

	"yunshu.dev/core/internal/redisx"
)

// What this file pins: the degraded path is currently UNREACHABLE while Redis is
// down, so concurrency_only cannot admit anything during an outage. That is a
// statement about today's implementation, not about the spec — and it is the
// baseline G31 has to change.
//
// The judgement order in the code is why, and it is worth writing down because
// the matrix alone does not show it:
//
//  1. Acquire checks degrader.Enabled() first and, when the mode is
//     concurrency_only, goes straight to acquireDegraded — every request, not
//     only the ones that already failed.
//  2. acquireDegraded's first act is Degrader.Replicas(), which itself reads
//     K18' (ZREMRANGEBYSCORE + ZCARD). With Redis down this fails before any
//     local counter is touched: unavailable("concurrency") + ErrUnavailable.
//  3. Even if Replicas returned, step 3 of acquireDegraded calls callAcquire,
//     which still reads the RPM / TPM / daily keys — the same ErrUnavailable.
//
// Step 2 is the defect behind BACKLOG G31: the fallback meant to survive a Redis
// outage needs Redis to compute "nominal concurrency ÷ replicas". That is why
// DefaultMode is off (errata E-38) — enabling it by default would cost two extra
// Redis reads per request and still return 503.
//
// When G31 lands (replica count cached from the last known value), the last hop
// of this test must change: a Redis outage in concurrency_only mode should then
// be rejected by the LOCAL counter — 429 concurrency_limit_exceeded — instead of
// ErrUnavailable. Keep the "Allowed must never be true" assertion in both worlds.
func TestDegradedModeIsUnreachableWhenRedisIsDown(t *testing.T) {
	bad := redisx.New(redisx.Options{
		Addr:           "127.0.0.1:1",
		DB:             15,
		DialTimeout:    50 * time.Millisecond,
		CommandTimeout: 50 * time.Millisecond,
		LuaTimeout:     100 * time.Millisecond,
	})
	defer bad.Close()

	// The same dead client is used for the degrader: Replicas() needs Redis, so
	// this reproduces the real outage rather than a half-broken one.
	degrader := NewDegrader(bad, redisx.NewStaticClock(baseTime), "n1", ModeConcurrencyOnly, 0)
	if !degrader.Enabled() {
		t.Fatal("degrader is not enabled; the mode was not wired through")
	}
	metrics := &captureMetrics{}
	limiter := New(bad, redisx.NewStaticClock(baseTime), degrader, metrics, nil)

	limits := Limits{RPM: 1000, TPM: 1_000_000, Concurrency: 10, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
	for i := 0; i < 3; i++ {
		verdict, err := limiter.Acquire(t.Context(), AcquireInput{
			KeyID: 70, UserID: 70, RequestID: "degraded-down-" + string(rune('a'+i)),
			EstTokens: 10, EstMicro: 1, Limits: limits,
		})
		// Allowed is the only thing that must never happen: it would mean an
		// outage admitted traffic past a fail-close dimension.
		if verdict.Allowed {
			t.Fatalf("request %d admitted while Redis is down in concurrency_only mode: verdict=%+v err=%v", i, verdict, err)
		}
		if err == nil {
			// A rejection without an error is acceptable only if it carries the
			// concurrency code — i.e. the local counter refused it.
			if verdict.Code != CodeConcurrency {
				t.Fatalf("request %d rejected with code %q and no error; want ErrUnavailable or %s",
					i, verdict.Code, CodeConcurrency)
			}
			continue
		}
		if !errors.Is(err, ErrUnavailable) {
			// Today the outage is caught before the local counter (see the file
			// comment). A 429 here would mean G31 landed; update this branch
			// deliberately rather than by accident.
			t.Fatalf("request %d error = %v; want ErrUnavailable while Replicas() still needs Redis", i, err)
		}
	}

	// The fail-close must be attributed: without the metric an operator cannot
	// tell "rejected by policy" from "rejected because Redis is gone".
	if len(metrics.unavailable) == 0 {
		t.Fatal("no unavailable metric recorded; the fail-close branch was not the one taken")
	}
	t.Logf("degraded+down: unavailable dims=%v rejected=%v", metrics.unavailable, metrics.rejected)
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

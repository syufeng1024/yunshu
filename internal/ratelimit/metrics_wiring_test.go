package ratelimit

import (
	"strings"
	"testing"

	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/redisx"
)

// The alarm rules in internal/obs/alert/rules.yaml compute the reject ratio from
// gw_ratelimit_rejected_total; if that counter never moves, the rule is another
// "data source does not exist" and stays green forever. This test drives the
// real limiter into a real 429 against the real Redis and reads the real
// Prometheus export, so the wiring is proven end to end without starting
// gatewayd.
func TestRejectedCounterMovesOnRealRejection(t *testing.T) {
	flushRedis(t)
	client := newTestClient(t)
	clock := redisx.NewStaticClock(baseTime)
	reg := obsmetrics.NewRegistry()
	rec := obsmetrics.NewRecorder(reg)
	limiter := New(client, clock, nil, rec, nil)

	// RPM = 1: the second request inside the same minute must be rejected.
	limits := Limits{RPM: 1, TPM: 1_000_000, Concurrency: 100, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
	first, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 80, UserID: 80, RequestID: "metrics-first", Limits: limits,
	})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !first.Allowed {
		t.Fatalf("first acquire rejected: %+v", first)
	}

	second, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 80, UserID: 80, RequestID: "metrics-second", Limits: limits,
	})
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if second.Allowed {
		t.Fatalf("second acquire admitted with RPM=1: %+v", second)
	}
	if second.Code != CodeRPM {
		t.Fatalf("second acquire code = %q; want %q", second.Code, CodeRPM)
	}

	text := string(reg.PrometheusText())
	const series = `gw_ratelimit_rejected_total{dim="rpm"}`
	var found bool
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, series+" ") {
			found = true
			if !strings.HasSuffix(strings.TrimSpace(line), " 1") {
				t.Fatalf("%s = %q; want exactly 1", series, line)
			}
		}
	}
	if !found {
		t.Fatalf("%s is missing from the export; the alert rule has no data source:\n%s", series, text)
	}
	t.Logf("429 path: code=%s, export has %s 1", second.Code, series)
}

// The degraded counter must stay quiet on the default path (DefaultMode is off):
// it is an outage signal, and a Redis-healthy rejection must not touch it. This
// also documents why gw_ratelimit_degraded_total is not a "we are degraded"
// dashboard while the mode is off.
func TestDegradedCounterStaysQuietInOffMode(t *testing.T) {
	flushRedis(t)
	client := newTestClient(t)
	clock := redisx.NewStaticClock(baseTime)
	reg := obsmetrics.NewRegistry()
	rec := obsmetrics.NewRecorder(reg)
	limiter := New(client, clock, NewDegrader(client, clock, "n1", DefaultMode, 0), rec, nil)

	limits := Limits{RPM: 100, TPM: 1_000_000, Concurrency: 100, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
	if _, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 81, UserID: 81, RequestID: "off-mode", Limits: limits,
	}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if DefaultMode != ModeOff {
		t.Fatalf("DefaultMode = %q; this test documents the off-mode behaviour", DefaultMode)
	}
	text := string(reg.PrometheusText())
	if strings.Contains(text, "gw_ratelimit_degraded_total{") {
		t.Fatalf("degraded counter moved in off mode; it must only count real degradation:\n%s", text)
	}
	if !strings.Contains(text, `gw_ratelimit_rejected_total{`) {
		// Nothing was rejected here; absence is expected and is itself the
		// "no fabricated zero" property.
		t.Logf("no reject series yet (nothing was rejected), as expected")
	}
}

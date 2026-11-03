package breaker

import (
	"os"
	"testing"
	"time"

	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

func TestMain(m *testing.M) { os.Exit(redistest.Setup(m)) }

var breakerNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func flushRedis(t *testing.T) {
	t.Helper()
	if err := redistest.Client().FlushDB(t.Context()).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func newBreaker(t *testing.T, clock *redisx.Clock) *Breaker {
	t.Helper()
	client := redistest.NewClient(0)
	t.Cleanup(func() { _ = client.Close() })
	return New(client, clock, nil)
}

func failureInput(channelID int64, probe bool) RecordInput {
	return RecordInput{
		ChannelID: channelID, Success: false, IsProbe: probe,
		FailThreshold: DefaultFailThreshold,
		BaseOpen:      DefaultBaseOpen,
		MaxOpen:       DefaultMaxOpen,
	}
}

func successInput(channelID int64, probe bool) RecordInput {
	in := failureInput(channelID, probe)
	in.Success = true
	return in
}

func TestA11OpenHalfOpenClosed(t *testing.T) {
	flushRedis(t)
	clock, setNow := redisx.NewAdjustableClock(breakerNow)
	br := newBreaker(t, clock)
	const channel = int64(101)

	for i := 0; i < int(DefaultFailThreshold); i++ {
		result := br.Record(t.Context(), failureInput(channel, false))
		if result.Unavailable {
			t.Fatal("breaker unexpectedly unavailable")
		}
	}
	state, openUntil, err := br.State(t.Context(), channel)
	if err != nil {
		t.Fatal(err)
	}
	if state != StateOpen {
		t.Fatalf("state = %s want OPEN", state)
	}
	if openUntil != breakerNow.Add(DefaultBaseOpen).UnixMilli() {
		t.Fatalf("open_until = %d want %d", openUntil, breakerNow.Add(DefaultBaseOpen).UnixMilli())
	}

	if acquired, _, err := br.TryProbe(t.Context(), channel, "p0"); err != nil || acquired {
		t.Fatalf("probe before backoff: acquired=%v err=%v", acquired, err)
	}

	setNow(breakerNow.Add(DefaultBaseOpen + time.Second))
	acquired, state, err := br.TryProbe(t.Context(), channel, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !acquired || state != StateHalfOpen {
		t.Fatalf("probe after backoff: acquired=%v state=%s", acquired, state)
	}
	if err := br.ReleaseProbe(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	if exists := redistest.Client().Exists(t.Context(), redisx.ProbeKey(channel)).Val(); exists != 0 {
		t.Fatal("probe lease was not returned with DEL")
	}

	for i := 0; i < int(ProbeSuccessTarget); i++ {
		acquired, _, err := br.TryProbe(t.Context(), channel, "p-success")
		if err != nil || !acquired {
			t.Fatalf("probe %d: acquired=%v err=%v", i+1, acquired, err)
		}
		result := br.Record(t.Context(), successInput(channel, true))
		if err := br.ReleaseProbe(t.Context(), channel); err != nil {
			t.Fatal(err)
		}
		if i < int(ProbeSuccessTarget)-1 && result.State != StateHalfOpen {
			t.Fatalf("after probe %d state = %s want HALF_OPEN", i+1, result.State)
		}
		if i == int(ProbeSuccessTarget)-1 && result.State != StateClosed {
			t.Fatalf("after %d probes state = %s want CLOSED", i+1, result.State)
		}
	}
	state, _, err = br.State(t.Context(), channel)
	if err != nil || state != StateClosed {
		t.Fatalf("final state = %s err=%v", state, err)
	}
	if exists := redistest.Client().Exists(t.Context(), redisx.ProbeKey(channel)).Val(); exists != 0 {
		t.Fatal("probe key left behind after closing")
	}
	t.Logf("A11: CLOSED -> OPEN -> HALF_OPEN -> CLOSED after 3 probe successes")
}

func TestHalfOpenFailureDoublesBackoff(t *testing.T) {
	flushRedis(t)
	clock, setNow := redisx.NewAdjustableClock(breakerNow)
	br := newBreaker(t, clock)
	const channel = int64(102)

	for i := 0; i < int(DefaultFailThreshold); i++ {
		br.Record(t.Context(), failureInput(channel, false))
	}
	setNow(breakerNow.Add(DefaultBaseOpen + time.Second))
	if acquired, _, err := br.TryProbe(t.Context(), channel, "probe"); err != nil || !acquired {
		t.Fatalf("probe: acquired=%v err=%v", acquired, err)
	}
	result := br.Record(t.Context(), failureInput(channel, true))
	if result.State != StateOpen {
		t.Fatalf("state = %s want OPEN", result.State)
	}
	if result.BackoffMS != int64(2*DefaultBaseOpen/time.Millisecond) {
		t.Fatalf("backoff = %dms want %dms", result.BackoffMS, 2*DefaultBaseOpen/time.Millisecond)
	}
	wantOpenUntil := breakerNow.Add(DefaultBaseOpen + time.Second + 2*DefaultBaseOpen).UnixMilli()
	if result.OpenUntilMS != wantOpenUntil {
		t.Fatalf("open_until = %d want %d", result.OpenUntilMS, wantOpenUntil)
	}
}

func TestK23CooldownDoesNotFeedBreaker(t *testing.T) {
	flushRedis(t)
	clock := redisx.NewStaticClock(breakerNow)
	br := newBreaker(t, clock)
	const channel = int64(103)

	applied, err := br.SetCooldown(t.Context(), channel, 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if applied != MaxCooldown {
		t.Fatalf("cooldown = %v want capped at %v", applied, MaxCooldown)
	}
	remaining, err := br.CooldownRemaining(t.Context(), channel)
	if err != nil {
		t.Fatal(err)
	}
	if remaining <= 55*time.Second || remaining > MaxCooldown {
		t.Fatalf("cooldown remaining = %v", remaining)
	}

	result := br.Record(t.Context(), failureInput(channel, false))
	if result.State != StateClosed || result.FailScore != 1 {
		t.Fatalf("one failure = %+v; cooldown must not contribute to fc", result)
	}
	if remainingAfter, _ := br.CooldownRemaining(t.Context(), channel); remainingAfter <= 55*time.Second {
		t.Fatalf("Record consumed the cooldown: %v", remainingAfter)
	}
	if acquired, _, err := br.TryProbe(t.Context(), channel, "p"); err != nil || acquired {
		t.Fatalf("probe while CLOSED: acquired=%v err=%v", acquired, err)
	}

	success := br.Record(t.Context(), successInput(channel, false))
	if success.FailScore != 0 {
		t.Fatalf("success left fc = %d want 0", success.FailScore)
	}
}

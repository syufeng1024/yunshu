package ratelimit

import (
	"errors"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

func TestMain(m *testing.M) { os.Exit(redistest.Setup(m)) }

var baseTime = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func flushRedis(t *testing.T) {
	t.Helper()
	if err := redistest.Client().FlushDB(t.Context()).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func newTestClient(t *testing.T) *redisx.Client {
	t.Helper()
	client := redisx.New(redisx.Options{Addr: redistest.Addr(), DB: redistest.DB()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func newTestLimiter(t *testing.T, clock *redisx.Clock, degrader *Degrader, metrics Metrics) (*Limiter, *redisx.Client) {
	t.Helper()
	client := newTestClient(t)
	if clock == nil {
		clock = redisx.NewStaticClock(baseTime)
	}
	return New(client, clock, degrader, metrics, nil), client
}

func readInt(t *testing.T, key string) (int64, bool) {
	t.Helper()
	value, err := redistest.Client().Get(t.Context(), key).Int64()
	if errors.Is(err, goredis.Nil) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return value, true
}

type captureMetrics struct {
	rejected    []string
	degraded    []string
	unavailable []string
}

func (m *captureMetrics) RatelimitRejected(dim string)    { m.rejected = append(m.rejected, dim) }
func (m *captureMetrics) RatelimitDegraded(dim string)    { m.degraded = append(m.degraded, dim) }
func (m *captureMetrics) RatelimitUnavailable(dim string) { m.unavailable = append(m.unavailable, dim) }

func TestA2FourDimensionsIndividually(t *testing.T) {
	clock := redisx.NewStaticClock(baseTime)
	minEpoch := clock.MinEpoch()
	nowSec := baseTime.Unix()

	t.Run("rpm", func(t *testing.T) {
		flushRedis(t)
		limiter, _ := newTestLimiter(t, clock, nil, nil)
		limits := Limits{RPM: 2, TPM: 1_000_000, Concurrency: 100, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
		in := AcquireInput{KeyID: 1, UserID: 1, RequestID: "r1", EstTokens: 10, EstMicro: 10, Limits: limits}
		for _, requestID := range []string{"r1", "r2"} {
			in.RequestID = requestID
			verdict, err := limiter.Acquire(t.Context(), in)
			if err != nil || !verdict.Allowed {
				t.Fatalf("request %s: allowed=%v err=%v", requestID, verdict.Allowed, err)
			}
		}
		in.RequestID = "r3"
		verdict, err := limiter.Acquire(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if verdict.Allowed || verdict.Code != CodeRPM {
			t.Fatalf("verdict = %+v, want rpm rejection", verdict)
		}
		if verdict.Limit != 2 || verdict.Total != 2 || verdict.Remaining != 0 {
			t.Fatalf("limit/total/remaining = %d/%d/%d, want 2/2/0",
				verdict.Limit, verdict.Total, verdict.Remaining)
		}
		if verdict.ResetAt != (minEpoch+1)*60 {
			t.Fatalf("reset_at = %d want %d", verdict.ResetAt, (minEpoch+1)*60)
		}
	})

	t.Run("tpm", func(t *testing.T) {
		flushRedis(t)
		limiter, _ := newTestLimiter(t, clock, nil, nil)
		limits := Limits{RPM: 100, TPM: 100, Concurrency: 100, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
		in := AcquireInput{KeyID: 2, UserID: 2, RequestID: "t1", EstTokens: 60, EstMicro: 10, Limits: limits}
		if verdict, err := limiter.Acquire(t.Context(), in); err != nil || !verdict.Allowed {
			t.Fatalf("first request: %+v err=%v", verdict, err)
		}
		in.RequestID = "t2"
		verdict, err := limiter.Acquire(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if verdict.Allowed || verdict.Code != CodeTPM {
			t.Fatalf("verdict = %+v, want tpm rejection", verdict)
		}
		if verdict.Limit != 100 || verdict.Total != 60 || verdict.Remaining != 40 {
			t.Fatalf("limit/total/remaining = %d/%d/%d, want 100/60/40",
				verdict.Limit, verdict.Total, verdict.Remaining)
		}
	})

	t.Run("concurrency", func(t *testing.T) {
		flushRedis(t)
		limiter, _ := newTestLimiter(t, clock, nil, nil)
		limits := Limits{RPM: 100, TPM: 1_000_000, Concurrency: 1, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
		in := AcquireInput{KeyID: 3, UserID: 3, RequestID: "c1", EstTokens: 1, EstMicro: 1, Limits: limits}
		if verdict, err := limiter.Acquire(t.Context(), in); err != nil || !verdict.Allowed {
			t.Fatalf("first request: %+v err=%v", verdict, err)
		}
		in.RequestID = "c2"
		verdict, err := limiter.Acquire(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if verdict.Allowed || verdict.Code != CodeConcurrency {
			t.Fatalf("verdict = %+v, want concurrency rejection", verdict)
		}
		if verdict.Limit != 1 || verdict.Total != 1 || verdict.Remaining != 0 {
			t.Fatalf("limit/total/remaining = %d/%d/%d, want 1/1/0",
				verdict.Limit, verdict.Total, verdict.Remaining)
		}
		if verdict.ResetAt != nowSec+3 {
			t.Fatalf("reset_at = %d want now+3 = %d", verdict.ResetAt, nowSec+3)
		}
	})

	t.Run("daily", func(t *testing.T) {
		flushRedis(t)
		limiter, _ := newTestLimiter(t, clock, nil, nil)
		limits := Limits{RPM: 100, TPM: 1_000_000, Concurrency: 100, DailyQuotaMicro: 1000, DailyEnabled: true}
		in := AcquireInput{KeyID: 4, UserID: 4, RequestID: "d1", EstTokens: 1, EstMicro: 600, Limits: limits}
		if verdict, err := limiter.Acquire(t.Context(), in); err != nil || !verdict.Allowed {
			t.Fatalf("first request: %+v err=%v", verdict, err)
		}
		in.RequestID = "d2"
		verdict, err := limiter.Acquire(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if verdict.Allowed || verdict.Code != CodeDailyQuota {
			t.Fatalf("verdict = %+v, want daily rejection", verdict)
		}
		if verdict.Limit != 1000 || verdict.Total != 600 || verdict.Remaining != 400 {
			t.Fatalf("limit/total/remaining = %d/%d/%d, want 1000/600/400",
				verdict.Limit, verdict.Total, verdict.Remaining)
		}
		if verdict.ResetAt != NextResetUnix(baseTime) {
			t.Fatalf("reset_at = %d want %d", verdict.ResetAt, NextResetUnix(baseTime))
		}
	})
}

// A3: all four dimensions are saturated. [2]code stays the priority winner
// (concurrency) while [3]-[6] describe the latest recovery (daily, next
// 00:00 UTC+8), i.e. the dimension the outward 429 will name. That split is
// the whole point of the 8-slot return (T06 hard constraint 4, errata E-13).
func TestA3AllFourHit(t *testing.T) {
	flushRedis(t)
	clock := redisx.NewStaticClock(baseTime)
	limiter, _ := newTestLimiter(t, clock, nil, nil)
	nowMS := baseTime.UnixMilli()
	minEpoch := clock.MinEpoch()
	day := Day(baseTime)
	raw := redistest.Client()

	// Distinct per-dimension values so a slot attributed to the wrong
	// dimension cannot pass by coincidence: concurrency 1/1, RPM 10/10,
	// TPM 100/100, daily 1000 limit with 700 spent.
	mustDo(t, raw.Set(t.Context(), redisx.RPMLKey(9, minEpoch), 10, 0).Err())
	mustDo(t, raw.Set(t.Context(), redisx.TPMKey(9, minEpoch), 100, 0).Err())
	mustDo(t, raw.Do(t.Context(), "ZADD", redisx.ConcurrencyKey(9), nowMS+5000, "existing").Err())
	mustDo(t, raw.Set(t.Context(), redisx.DailyQuotaKey(9, day), 700, 0).Err())

	verdict, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 9, UserID: 9, RequestID: "full", EstTokens: 1, EstMicro: 400,
		Limits: Limits{RPM: 10, TPM: 100, Concurrency: 1, DailyQuotaMicro: 1000, DailyEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Allowed {
		t.Fatal("all four dimensions are full but the request was admitted")
	}
	if verdict.Code != CodeConcurrency {
		t.Fatalf("[2]code = %q, want the priority winner %q", verdict.Code, CodeConcurrency)
	}
	wantHits := []string{CodeConcurrency, CodeRPM, CodeTPM, CodeDailyQuota}
	if len(verdict.AllHit) != len(wantHits) {
		t.Fatalf("all_hit = %v, want 4 dimensions", verdict.AllHit)
	}
	for i, want := range wantHits {
		if verdict.AllHit[i] != want {
			t.Fatalf("all_hit = %v, want %v", verdict.AllHit, wantHits)
		}
	}
	if got := verdict.OutwardCode(); got != CodeDailyQuota {
		t.Fatalf("outward code = %q want %q (latest recovery); [2]code stays %q",
			got, CodeDailyQuota, CodeConcurrency)
	}
	// [3]-[6] must describe daily (1000 limit / 700 total / 300 remaining),
	// not the priority winner concurrency (1/1/0).
	if verdict.Limit != 1000 || verdict.Total != 700 || verdict.Remaining != 300 {
		t.Fatalf("[3][4][5] = %d/%d/%d, want daily 1000/700/300 (not concurrency 1/1/0)",
			verdict.Limit, verdict.Total, verdict.Remaining)
	}
	if verdict.ResetAt != NextResetUnix(baseTime) {
		t.Fatalf("[6]reset_at = %d, want next 00:00 UTC+8 = %d (not concurrency now+3s)",
			verdict.ResetAt, NextResetUnix(baseTime))
	}
	if verdict.ResetAtMax != NextResetUnix(baseTime) {
		t.Fatalf("[8]reset_at_max = %d, want next 00:00 UTC+8 = %d", verdict.ResetAtMax, NextResetUnix(baseTime))
	}
	t.Logf("A3: allowed=%v [2]code=%s outward_code=%s limit/total/remaining=%d/%d/%d reset_at=%d all_hit=%v reset_at_max=%d (next 00:00 UTC+8)",
		verdict.Allowed, verdict.Code, verdict.OutwardCode(),
		verdict.Limit, verdict.Total, verdict.Remaining, verdict.ResetAt, verdict.AllHit, verdict.ResetAtMax)

	// A rejected request must not have written any counter.
	if got := mustGet(t, raw, redisx.RPMLKey(9, minEpoch)); got != "10" {
		t.Fatalf("K1 changed to %q", got)
	}
	if got := mustGet(t, raw, redisx.TPMKey(9, minEpoch)); got != "100" {
		t.Fatalf("K2 changed to %q", got)
	}
	if got := mustGet(t, raw, redisx.DailyQuotaKey(9, day)); got != "700" {
		t.Fatalf("K4 changed to %q", got)
	}
}

// Only concurrency is saturated. [2] and [3]-[6] must both describe
// concurrency; the latest-recovery rule must not disturb the single-hit case.
func TestOutwardSlotsSingleConcurrency(t *testing.T) {
	flushRedis(t)
	clock := redisx.NewStaticClock(baseTime)
	limiter, _ := newTestLimiter(t, clock, nil, nil)
	nowSec := baseTime.Unix()

	limits := Limits{RPM: 100, TPM: 1_000_000, Concurrency: 1, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
	in := AcquireInput{KeyID: 11, UserID: 11, RequestID: "c1", EstTokens: 1, EstMicro: 1, Limits: limits}
	if verdict, err := limiter.Acquire(t.Context(), in); err != nil || !verdict.Allowed {
		t.Fatalf("first request: %+v err=%v", verdict, err)
	}
	in.RequestID = "c2"
	verdict, err := limiter.Acquire(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Allowed || verdict.Code != CodeConcurrency || verdict.OutwardCode() != CodeConcurrency {
		t.Fatalf("verdict = %+v, want concurrency rejection", verdict)
	}
	if verdict.Limit != 1 || verdict.Total != 1 || verdict.Remaining != 0 {
		t.Fatalf("[3][4][5] = %d/%d/%d, want concurrency 1/1/0",
			verdict.Limit, verdict.Total, verdict.Remaining)
	}
	if verdict.ResetAt != nowSec+3 || verdict.ResetAtMax != nowSec+3 {
		t.Fatalf("[6]/[8] = %d/%d, want concurrency now+3s = %d",
			verdict.ResetAt, verdict.ResetAtMax, nowSec+3)
	}
	if len(verdict.AllHit) != 1 || verdict.AllHit[0] != CodeConcurrency {
		t.Fatalf("all_hit = %v, want only concurrency", verdict.AllHit)
	}
	t.Logf("single-concurrency: [2]=%s outward=%s limit/total/remaining=%d/%d/%d reset_at=%d reset_at_max=%d",
		verdict.Code, verdict.OutwardCode(), verdict.Limit, verdict.Total, verdict.Remaining,
		verdict.ResetAt, verdict.ResetAtMax)
}

// RPM and TPM both recover at the next minute boundary. On an equal reset
// instant the description follows the evaluation priority (RPM before TPM),
// and OutwardCode does the same.
func TestOutwardSlotsRPMAndTPMTie(t *testing.T) {
	flushRedis(t)
	clock := redisx.NewStaticClock(baseTime)
	limiter, _ := newTestLimiter(t, clock, nil, nil)
	minEpoch := clock.MinEpoch()
	nextMinute := (minEpoch + 1) * 60
	raw := redistest.Client()

	mustDo(t, raw.Set(t.Context(), redisx.RPMLKey(12, minEpoch), 10, 0).Err())
	mustDo(t, raw.Set(t.Context(), redisx.TPMKey(12, minEpoch), 1000, 0).Err())

	verdict, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 12, UserID: 12, RequestID: "tie", EstTokens: 100, EstMicro: 1,
		Limits: Limits{RPM: 10, TPM: 1000, Concurrency: 100, DailyQuotaMicro: 1_000_000, DailyEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Allowed || verdict.Code != CodeRPM {
		t.Fatalf("verdict = %+v, want RPM as the priority-first hit", verdict)
	}
	if got := verdict.OutwardCode(); got != CodeRPM {
		t.Fatalf("outward code = %q, want RPM (tie-break by priority order)", got)
	}
	if verdict.Limit != 10 || verdict.Total != 10 || verdict.Remaining != 0 {
		t.Fatalf("[3][4][5] = %d/%d/%d, want RPM 10/10/0 (not TPM 1000/1000/0)",
			verdict.Limit, verdict.Total, verdict.Remaining)
	}
	if verdict.ResetAt != nextMinute || verdict.ResetAtMax != nextMinute {
		t.Fatalf("[6]/[8] = %d/%d, want next minute = %d",
			verdict.ResetAt, verdict.ResetAtMax, nextMinute)
	}
	wantHits := []string{CodeRPM, CodeTPM}
	if len(verdict.AllHit) != len(wantHits) || verdict.AllHit[0] != CodeRPM || verdict.AllHit[1] != CodeTPM {
		t.Fatalf("all_hit = %v, want %v", verdict.AllHit, wantHits)
	}
	t.Logf("rpm+tpm tie: [2]=%s outward=%s limit/total/remaining=%d/%d/%d reset_at=%d",
		verdict.Code, verdict.OutwardCode(), verdict.Limit, verdict.Total, verdict.Remaining, verdict.ResetAt)
}

// At 23:59:30 CST the RPM next_minute and the daily 00:00 reset are the same
// instant. [2]code must stay the evaluation-first hit (RPM), but [3]-[6] must
// follow OutwardCode()'s order (daily > RPM > TPM > concurrency), otherwise
// the 429 would name RPM while describing daily's counters.
func TestOutwardSlotsDailyAndRPMTie(t *testing.T) {
	flushRedis(t)
	late := time.Date(2026, 10, 26, 23, 59, 30, 0, cst)
	clock := redisx.NewStaticClock(late)
	limiter, _ := newTestLimiter(t, clock, nil, nil)
	minEpoch := clock.MinEpoch()
	day := Day(late)
	raw := redistest.Client()

	nextMinute := (minEpoch + 1) * 60
	nextReset := NextResetUnix(late)
	if nextMinute != nextReset {
		t.Fatalf("precondition: next_minute %d != daily reset %d, tie not exercised",
			nextMinute, nextReset)
	}

	mustDo(t, raw.Set(t.Context(), redisx.RPMLKey(13, minEpoch), 10, 0).Err())
	mustDo(t, raw.Set(t.Context(), redisx.DailyQuotaKey(13, day), 700, 0).Err())

	verdict, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 13, UserID: 13, RequestID: "daily-rpm-tie", EstTokens: 1, EstMicro: 400,
		Limits: Limits{RPM: 10, TPM: 1_000_000, Concurrency: 100, DailyQuotaMicro: 1000, DailyEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Allowed {
		t.Fatal("request admitted with RPM and daily both saturated")
	}
	if verdict.Code != CodeRPM {
		t.Fatalf("[2]code = %q, want the evaluation-first hit %q", verdict.Code, CodeRPM)
	}
	if got := verdict.OutwardCode(); got != CodeDailyQuota {
		t.Fatalf("outward code = %q, want %q (rank order, not evaluation order)", got, CodeDailyQuota)
	}
	if verdict.Limit != 1000 || verdict.Total != 700 || verdict.Remaining != 300 {
		t.Fatalf("[3][4][5] = %d/%d/%d, want daily 1000/700/300 (not RPM 10/10/0)",
			verdict.Limit, verdict.Total, verdict.Remaining)
	}
	if verdict.ResetAt != nextReset || verdict.ResetAtMax != nextReset {
		t.Fatalf("[6]/[8] = %d/%d, want tied reset = %d",
			verdict.ResetAt, verdict.ResetAtMax, nextReset)
	}
	wantHits := []string{CodeRPM, CodeDailyQuota}
	if len(verdict.AllHit) != len(wantHits) || verdict.AllHit[0] != CodeRPM || verdict.AllHit[1] != CodeDailyQuota {
		t.Fatalf("all_hit = %v, want %v", verdict.AllHit, wantHits)
	}
	t.Logf("daily+rpm tie @23:59:30 CST: [2]=%s outward=%s limit/total/remaining=%d/%d/%d reset_at=%d reset_at_max=%d",
		verdict.Code, verdict.OutwardCode(), verdict.Limit, verdict.Total, verdict.Remaining,
		verdict.ResetAt, verdict.ResetAtMax)
}

// In the last 3s of a minute, concurrency's now+3s and RPM's next_minute are
// the same instant. [2]code stays concurrency; [3]-[6] must describe RPM,
// which OutwardCode() ranks above concurrency.
func TestOutwardSlotsConcurrencyAndRPMTie(t *testing.T) {
	flushRedis(t)
	lastSecond := time.Date(2026, 10, 26, 12, 34, 57, 0, time.UTC)
	clock := redisx.NewStaticClock(lastSecond)
	limiter, _ := newTestLimiter(t, clock, nil, nil)
	nowMS := lastSecond.UnixMilli()
	minEpoch := clock.MinEpoch()
	raw := redistest.Client()

	nextMinute := (minEpoch + 1) * 60
	ccReset := (nowMS + 3000) / 1000
	if ccReset != nextMinute {
		t.Fatalf("precondition: concurrency reset %d != next minute %d, tie not exercised",
			ccReset, nextMinute)
	}

	mustDo(t, raw.Set(t.Context(), redisx.RPMLKey(14, minEpoch), 10, 0).Err())
	mustDo(t, raw.Do(t.Context(), "ZADD", redisx.ConcurrencyKey(14), nowMS+5000, "existing").Err())

	verdict, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 14, UserID: 14, RequestID: "cc-rpm-tie", EstTokens: 1, EstMicro: 1,
		Limits: Limits{RPM: 10, TPM: 1_000_000, Concurrency: 1, DailyQuotaMicro: 1_000_000, DailyEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Allowed {
		t.Fatal("request admitted with concurrency and RPM both saturated")
	}
	if verdict.Code != CodeConcurrency {
		t.Fatalf("[2]code = %q, want the evaluation-first hit %q", verdict.Code, CodeConcurrency)
	}
	if got := verdict.OutwardCode(); got != CodeRPM {
		t.Fatalf("outward code = %q, want %q (rank order, not evaluation order)", got, CodeRPM)
	}
	if verdict.Limit != 10 || verdict.Total != 10 || verdict.Remaining != 0 {
		t.Fatalf("[3][4][5] = %d/%d/%d, want RPM 10/10/0 (not concurrency 1/1/0)",
			verdict.Limit, verdict.Total, verdict.Remaining)
	}
	if verdict.ResetAt != nextMinute || verdict.ResetAtMax != nextMinute {
		t.Fatalf("[6]/[8] = %d/%d, want tied reset = %d",
			verdict.ResetAt, verdict.ResetAtMax, nextMinute)
	}
	wantHits := []string{CodeConcurrency, CodeRPM}
	if len(verdict.AllHit) != len(wantHits) || verdict.AllHit[0] != CodeConcurrency || verdict.AllHit[1] != CodeRPM {
		t.Fatalf("all_hit = %v, want %v", verdict.AllHit, wantHits)
	}
	t.Logf("concurrency+rpm tie @minute-end: [2]=%s outward=%s limit/total/remaining=%d/%d/%d reset_at=%d reset_at_max=%d",
		verdict.Code, verdict.OutwardCode(), verdict.Limit, verdict.Total, verdict.Remaining,
		verdict.ResetAt, verdict.ResetAtMax)
}

func TestA4ReleaseIdempotentAndLazyCleanup(t *testing.T) {
	flushRedis(t)
	clock := redisx.NewStaticClock(baseTime)
	limiter, _ := newTestLimiter(t, clock, nil, nil)
	limits := Limits{RPM: 100, TPM: 1_000_000, Concurrency: 5, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
	minEpoch := clock.MinEpoch()

	in := AcquireInput{KeyID: 20, UserID: 20, RequestID: "req", EstTokens: 100, EstMicro: 1, Limits: limits}
	if verdict, err := limiter.Acquire(t.Context(), in); err != nil || !verdict.Allowed {
		t.Fatalf("acquire: %+v err=%v", verdict, err)
	}
	release := ReleaseInput{
		KeyID: 20, UserID: 20, RequestID: "req",
		AcquireMinEpoch: minEpoch, CurrentMinEpoch: minEpoch,
		TPMDelta: -40, HadLease: true,
	}
	if _, err := limiter.Release(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(t, redisx.TPMKey(20, minEpoch)); got != 60 {
		t.Fatalf("TPM after first release = %d want 60", got)
	}
	if _, err := limiter.Release(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(t, redisx.TPMKey(20, minEpoch)); got != 60 {
		t.Fatalf("duplicate release compensated again: TPM = %d want 60", got)
	}
	zcard := redistest.Client().ZCard(t.Context(), redisx.ConcurrencyKey(20)).Val()
	if zcard != 0 {
		t.Fatalf("ZCARD after two releases = %d want 0", zcard)
	}

	// An expired member is removed by the next acquire's lazy cleanup.
	mustDo(t, redistest.Client().Do(t.Context(), "ZADD", redisx.ConcurrencyKey(21),
		baseTime.UnixMilli()-1000, "ancient").Err())
	limiter2, _ := newTestLimiter(t, clock, nil, nil)
	in.KeyID = 21
	in.RequestID = "fresh"
	if verdict, err := limiter2.Acquire(t.Context(), in); err != nil || !verdict.Allowed {
		t.Fatalf("acquire after stale member: %+v err=%v", verdict, err)
	}
	if _, err := redistest.Client().ZScore(t.Context(), redisx.ConcurrencyKey(21), "ancient").Result(); !errors.Is(err, goredis.Nil) {
		t.Fatalf("expired member still present: %v", err)
	}
	if zcard := redistest.Client().ZCard(t.Context(), redisx.ConcurrencyKey(21)).Val(); zcard != 1 {
		t.Fatalf("ZCARD = %d want 1", zcard)
	}
}

func TestA5TPMPlaceholderSettleAndCrossMinute(t *testing.T) {
	flushRedis(t)
	clock := redisx.NewStaticClock(baseTime)
	limiter, _ := newTestLimiter(t, clock, nil, nil)
	minEpoch := clock.MinEpoch()
	limits := Limits{RPM: 100, TPM: 100_000, Concurrency: 100, DailyQuotaMicro: 1_000_000, DailyEnabled: true}

	in := AcquireInput{KeyID: 30, UserID: 30, RequestID: "tpm", EstTokens: 1000, EstMicro: 1, MinEpoch: minEpoch, Limits: limits}
	if verdict, err := limiter.Acquire(t.Context(), in); err != nil || !verdict.Allowed {
		t.Fatalf("acquire: %+v err=%v", verdict, err)
	}
	if got, _ := readInt(t, redisx.TPMKey(30, minEpoch)); got != 1000 {
		t.Fatalf("placeholder = %d want 1000", got)
	}

	delta := SettleAfterRelease(Usage{Present: true, PromptTokens: 500, CompletionTokens: 300}, 0, 0, 1000)
	if delta != -200 {
		t.Fatalf("settle delta = %d want -200", delta)
	}
	if _, err := limiter.Release(t.Context(), ReleaseInput{
		KeyID: 30, UserID: 30, RequestID: "tpm",
		AcquireMinEpoch: minEpoch, CurrentMinEpoch: minEpoch,
		TPMDelta: delta, HadLease: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(t, redisx.TPMKey(30, minEpoch)); got != 800 {
		t.Fatalf("settled bucket = %d want 800", got)
	}

	// Cross-minute settlement is dropped, never written into the old bucket.
	flushRedis(t)
	in.RequestID = "cross"
	if verdict, err := limiter.Acquire(t.Context(), in); err != nil || !verdict.Allowed {
		t.Fatalf("acquire cross: %+v err=%v", verdict, err)
	}
	if _, err := limiter.Release(t.Context(), ReleaseInput{
		KeyID: 30, UserID: 30, RequestID: "cross",
		AcquireMinEpoch: minEpoch, CurrentMinEpoch: minEpoch + 1,
		TPMDelta: -200, HadLease: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(t, redisx.TPMKey(30, minEpoch)); got != 1000 {
		t.Fatalf("cross-minute compensation changed the old bucket to %d want 1000", got)
	}

	// A bucket that already expired must not be recreated by a late settle.
	flushRedis(t)
	in.RequestID = "expired"
	if verdict, err := limiter.Acquire(t.Context(), in); err != nil || !verdict.Allowed {
		t.Fatalf("acquire expired: %+v err=%v", verdict, err)
	}
	mustDo(t, redistest.Client().Del(t.Context(), redisx.TPMKey(30, minEpoch)).Err())
	if _, err := limiter.Release(t.Context(), ReleaseInput{
		KeyID: 30, UserID: 30, RequestID: "expired",
		AcquireMinEpoch: minEpoch, CurrentMinEpoch: minEpoch,
		TPMDelta: -200, HadLease: true,
	}); err != nil {
		t.Fatal(err)
	}
	if exists := redistest.Client().Exists(t.Context(), redisx.TPMKey(30, minEpoch)).Val(); exists != 0 {
		t.Fatal("late settlement recreated an expired TPM bucket")
	}
}

func TestA6NoUsageNeverReleasesPlaceholder(t *testing.T) {
	occupied := PlaceholderTokens(200, 0, 4096)
	if occupied != 200+4096 {
		t.Fatalf("placeholder = %d want 4296", occupied)
	}
	actual, delta := SettleTokens(Usage{Present: false}, 200, 100, occupied)
	if actual != 300 {
		t.Fatalf("no-usage actual = %d want 300 (emitted 100 + estimated input 200)", actual)
	}
	if delta != 300-occupied {
		t.Fatalf("no-usage delta = %d want %d", delta, 300-occupied)
	}
	if delta == -occupied {
		t.Fatal("no-usage settlement released the whole placeholder")
	}

	flushRedis(t)
	clock := redisx.NewStaticClock(baseTime)
	limiter, _ := newTestLimiter(t, clock, nil, nil)
	minEpoch := clock.MinEpoch()
	limits := Limits{RPM: 100, TPM: 100_000, Concurrency: 100, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
	in := AcquireInput{KeyID: 31, UserID: 31, RequestID: "nousage", EstTokens: 1000, EstMicro: 1, Limits: limits}
	if verdict, err := limiter.Acquire(t.Context(), in); err != nil || !verdict.Allowed {
		t.Fatalf("acquire: %+v err=%v", verdict, err)
	}
	delta = SettleAfterRelease(Usage{Present: false}, 200, 100, 1000)
	if _, err := limiter.Release(t.Context(), ReleaseInput{
		KeyID: 31, UserID: 31, RequestID: "nousage",
		AcquireMinEpoch: minEpoch, CurrentMinEpoch: minEpoch,
		TPMDelta: delta, HadLease: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(t, redisx.TPMKey(31, minEpoch)); got != 300 {
		t.Fatalf("no-usage bucket = %d want 300", got)
	}
}

func TestA7FailCloseWhenRedisIsDown(t *testing.T) {
	bad := redisx.New(redisx.Options{
		Addr:           "127.0.0.1:1",
		DB:             15,
		DialTimeout:    50 * time.Millisecond,
		CommandTimeout: 50 * time.Millisecond,
		LuaTimeout:     100 * time.Millisecond,
	})
	defer bad.Close()
	limiter := New(bad, redisx.NewStaticClock(baseTime), nil, &captureMetrics{}, nil)
	verdict, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 40, UserID: 40, RequestID: "down",
		Limits: Limits{RPM: 100, TPM: 1000, Concurrency: 10, DailyQuotaMicro: 1000, DailyEnabled: true},
	})
	if err == nil {
		t.Fatal("Redis is down but Acquire returned no error (fail-open)")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if verdict.Allowed {
		t.Fatal("fail-close returned Allowed=true")
	}
	t.Logf("A7 fail-close: allowed=%v err=%v", verdict.Allowed, err)
}

func TestA8ConcurrencyDegradation(t *testing.T) {
	flushRedis(t)
	clock := redisx.NewStaticClock(baseTime)
	nowMS := baseTime.UnixMilli()
	raw := redistest.Client()
	mustDo(t, raw.Do(t.Context(), "ZADD", redisx.NodeSetKey(),
		nowMS, "n1", nowMS, "n2", nowMS, "n3").Err())

	client := newTestClient(t)
	degrader := NewDegrader(client, clock, "n1", ModeConcurrencyOnly, 0)
	// Replicas() is cached after G31; seed it from the nodes written above so
	// the divisor is 3 instead of the floor value of 1.
	if err := degrader.RefreshReplicas(t.Context()); err != nil {
		t.Fatal(err)
	}
	metrics := &captureMetrics{}
	limiter := New(client, clock, degrader, metrics, nil)
	limits := Limits{RPM: 100, TPM: 1_000_000, Concurrency: 10, DailyQuotaMicro: 1_000_000, DailyEnabled: true}
	minEpoch := clock.MinEpoch()

	for i := 0; i < 4; i++ {
		verdict, err := limiter.Acquire(t.Context(), AcquireInput{
			KeyID: 50, UserID: 50, RequestID: "deg-" + string(rune('a'+i)),
			EstTokens: 10, EstMicro: 1, Limits: limits,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !verdict.Allowed || verdict.ConcurrencyMode != ConcurrencyLocal {
			t.Fatalf("request %d: %+v", i, verdict)
		}
	}
	verdict, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 50, UserID: 50, RequestID: "deg-over",
		EstTokens: 10, EstMicro: 1, Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Allowed || verdict.Code != CodeConcurrency {
		t.Fatalf("5th request = %+v, want local concurrency rejection", verdict)
	}
	if verdict.Limit != PerKeyLocalLimit(10, 3) || verdict.Limit != 4 {
		t.Fatalf("perKeyLocal = %d want ceil(10/3) = 4", verdict.Limit)
	}
	// K1/K2 are still enforced in Redis even while concurrency is local.
	if got, _ := readInt(t, redisx.RPMLKey(50, minEpoch)); got != 4 {
		t.Fatalf("K1 = %d want 4 (RPM still on Redis)", got)
	}
	if got, _ := readInt(t, redisx.TPMKey(50, minEpoch)); got != 40 {
		t.Fatalf("K2 = %d want 40 (TPM still on Redis)", got)
	}
	if exists := raw.Exists(t.Context(), redisx.ConcurrencyKey(50)).Val(); exists != 0 {
		t.Fatal("degraded path wrote K3")
	}
	if len(metrics.degraded) == 0 || metrics.degraded[0] != "concurrency" {
		t.Fatalf("degraded metric = %v", metrics.degraded)
	}
	if _, err := limiter.Release(t.Context(), ReleaseInput{
		KeyID: 50, UserID: 50, RequestID: "deg-a",
		AcquireMinEpoch: minEpoch, CurrentMinEpoch: minEpoch,
		TPMDelta: -10, HadLease: false,
	}); err != nil {
		t.Fatal(err)
	}
	if got := degrader.LocalCount(50); got != 3 {
		t.Fatalf("local count after release = %d want 3", got)
	}
	t.Logf("A8: perKeyLocal=%d K1=%d K2=%d K3_exists=%d local=%d",
		verdict.Limit, 4, 40, 0, degrader.LocalCount(50))
}

func TestA12DailyQuotaCrossesCSTDay(t *testing.T) {
	flushRedis(t)
	cst := time.FixedZone("CST", 8*3600)
	firstDay := time.Date(2026, 10, 26, 23, 59, 0, 0, cst)
	nextDay := time.Date(2026, 10, 27, 0, 1, 0, 0, cst)
	clock, setNow := redisx.NewAdjustableClock(firstDay)
	limiter, _ := newTestLimiter(t, clock, nil, nil)
	limits := Limits{RPM: 100, TPM: 1_000_000, Concurrency: 100, DailyQuotaMicro: 1000, DailyEnabled: true}

	if verdict, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 60, UserID: 60, RequestID: "late", EstTokens: 1, EstMicro: 100, Limits: limits,
	}); err != nil || !verdict.Allowed {
		t.Fatalf("23:59 acquire: %+v err=%v", verdict, err)
	}
	setNow(nextDay)
	if verdict, err := limiter.Acquire(t.Context(), AcquireInput{
		KeyID: 61, UserID: 61, RequestID: "early", EstTokens: 1, EstMicro: 100, Limits: limits,
	}); err != nil || !verdict.Allowed {
		t.Fatalf("00:01 acquire: %+v err=%v", verdict, err)
	}
	raw := redistest.Client()
	oldKey := redisx.DailyQuotaKey(60, "20261026")
	newKey := redisx.DailyQuotaKey(61, "20261027")
	if exists := raw.Exists(t.Context(), oldKey).Val(); exists != 1 {
		t.Fatalf("23:59 charge is not in %s", oldKey)
	}
	if exists := raw.Exists(t.Context(), newKey).Val(); exists != 1 {
		t.Fatalf("00:01 charge is not in %s", newKey)
	}
	ttl := raw.TTL(t.Context(), oldKey).Val()
	if ttl <= 0 {
		t.Fatalf("old daily key has no TTL")
	}
	t.Logf("A12: old=%s (%s) new=%s (%s)", oldKey, Day(firstDay), newKey, Day(nextDay))
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustGet(t *testing.T, raw *goredis.Client, key string) string {
	t.Helper()
	value, err := raw.Get(t.Context(), key).Result()
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return value
}

func TestK5DailyRejectionCounter(t *testing.T) {
	flushRedis(t)
	clock := redisx.NewStaticClock(baseTime)
	limiter, _ := newTestLimiter(t, clock, nil, nil)
	day := Day(baseTime)
	if err := limiter.RecordDailyRejection(t.Context(), 70, day); err != nil {
		t.Fatal(err)
	}
	if err := limiter.RecordDailyRejection(t.Context(), 70, day); err != nil {
		t.Fatal(err)
	}
	key := redisx.DailyQuotaRejectedKey(70, day)
	if got, _ := readInt(t, key); got != 2 {
		t.Fatalf("K5 = %d want 2", got)
	}
	if ttl := redistest.Client().PTTL(t.Context(), key).Val(); ttl <= 0 {
		t.Fatal("K5 has no TTL")
	}
}

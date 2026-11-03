// Package ratelimit implements the four-dimension gate from PRD §3.2 on the
// 26-key namespace from TDS §2.1.
//
// The gate is an admission check, not a money check. Redis losing a key can
// admit a request that should have been rejected; it can never change a
// ledger row (GR-02). The opposite failure — Redis unavailable — is
// fail-close for RPM/TPM/daily because those protect quota and money, while
// concurrency alone may fall back to a node-local counter.
package ratelimit

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/redisx"
)

// ErrUnavailable is the fail-close marker. Callers must translate it to
// HTTP 503 rate_limiter_unavailable and must not admit the request.
var ErrUnavailable = errors.New("rate_limiter_unavailable")

//go:embed scripts/rl_acquire.lua
var acquireScriptSource string

var acquireScript = goredis.NewScript(acquireScriptSource)

// Code values are the 429 sub-codes from PRD §3.3. T10 chooses the outward
// code. L1 reports the priority-first hit in [2] and the latest-recovering
// hit in Limit/Total/Remaining/ResetAt.
const (
	CodeConcurrency = "concurrency_limit_exceeded"
	CodeRPM         = "rpm_limit_exceeded"
	CodeTPM         = "tpm_limit_exceeded"
	CodeDailyQuota  = "daily_quota_exceeded"
)

// DefaultLeaseMS is the K3 lease for a request whose caller does not supply
// one. Streaming callers should pass a budget that covers the stream.
const DefaultLeaseMS = int64(30_000)

// Limits are the resolved policy values for one request. They come from the
// configuration snapshot; the Lua script never reads configuration (T06 §7).
type Limits struct {
	RPM             int64
	TPM             int64
	Concurrency     int64
	DailyQuotaMicro int64
	DailyEnabled    bool
}

// AcquireInput is one admission attempt.
type AcquireInput struct {
	KeyID  int64
	UserID int64

	// NowMS and MinEpoch must be corrected timestamps. Zero means "use the
	// rate limiter's clock"; a caller that already sampled the clock should
	// pass the same values it will pass to Release.
	NowMS    int64
	MinEpoch int64

	RequestID string
	LeaseMS   int64

	// EstTokens is the TPM placeholder (input + max_tokens), EstMicro the
	// daily-quota hold. Negative values are clamped to zero at the boundary.
	EstTokens int64
	EstMicro  int64

	// Day is the Asia/Shanghai yyyymmdd; zero means derive it from NowMS.
	Day string

	Limits Limits
}

// Verdict is the L1 result. ResetAt and ResetAtMax are unix seconds.
//
// Code is the priority-first hit, but Limit/Total/Remaining/ResetAt describe
// the hit that recovers last (the OutwardCode dimension). The two differ when
// several dimensions are saturated; see the package-level discussion in
// scripts/rl_acquire.lua.
type Verdict struct {
	Allowed    bool
	Code       string
	Limit      int64
	Total      int64
	Remaining  int64
	ResetAt    int64
	AllHit     []string
	ResetAtMax int64

	ConcurrencyMode ConcurrencyMode
}

// OutwardCode returns the 429 sub-code T10 must emit: the hit dimension with
// the latest recovery (PRD §3.3). RPM beats TPM on a tie because the
// dimension declaration order fixes that tie-break.
//
// Code is the *main* (priority) dimension and is deliberately not the outward
// code. Keeping the two separate is what stops an all-four-hit request from
// reporting concurrency, whose 3-second recovery would mislead the client.
func (v Verdict) OutwardCode() string {
	if v.Allowed {
		return ""
	}
	has := func(code string) bool {
		for _, hit := range v.AllHit {
			if hit == code {
				return true
			}
		}
		return false
	}
	switch {
	case has(CodeDailyQuota):
		return CodeDailyQuota
	case has(CodeRPM):
		return CodeRPM
	case has(CodeTPM):
		return CodeTPM
	case has(CodeConcurrency):
		return CodeConcurrency
	default:
		return v.Code
	}
}

// Metrics is the exporter surface. It is optional and must not block.
type Metrics interface {
	RatelimitRejected(dim string)
	RatelimitDegraded(dim string)
	RatelimitUnavailable(dim string)
}

// Limiter is stateless apart from the optional degradation counter and the
// pending-release buffer, so one instance is safe for concurrent requests.
type Limiter struct {
	client   *redisx.Client
	clock    *redisx.Clock
	degrader *Degrader
	metrics  Metrics
	logger   redisx.Logger
	pending  *pendingQueue
}

// New builds a limiter. clock may be nil in tests that always pass NowMS.
func New(client *redisx.Client, clock *redisx.Clock, degrader *Degrader, metrics Metrics, logger redisx.Logger) *Limiter {
	if clock == nil {
		clock = redisx.NewClock(nil, nil)
	}
	return &Limiter{
		client:   client,
		clock:    clock,
		degrader: degrader,
		metrics:  metrics,
		logger:   logger,
		pending:  newPendingQueue(10_000),
	}
}

// Acquire performs the single atomic four-dimension decision.
func (l *Limiter) Acquire(ctx context.Context, in AcquireInput) (Verdict, error) {
	if in.RequestID == "" {
		return Verdict{}, errors.New("ratelimit: empty request_id")
	}
	if in.LeaseMS <= 0 {
		in.LeaseMS = DefaultLeaseMS
	}
	in.EstTokens = nonNegative(in.EstTokens)
	in.EstMicro = nonNegative(in.EstMicro)

	nowMS := in.NowMS
	if nowMS == 0 {
		nowMS = l.clock.NowMS()
	}
	minEpoch := in.MinEpoch
	if minEpoch == 0 {
		minEpoch = redisx.MinEpochFromMS(nowMS)
	}
	day := in.Day
	if day == "" {
		day = Day(time.UnixMilli(nowMS))
	}

	if l.degrader != nil && l.degrader.Enabled() {
		// gw_ratelimit_degraded_total means "this admission was decided by the
		// node-local counter *because Redis was failing*", not "the mode is
		// concurrency_only". Recording it here unconditionally turned the
		// outage counter into an admission counter: with the mode switched on
		// it moved on every healthy request, so it could no longer be used to
		// tell an outage from a quiet minute (G29). The flag is set by the
		// previous request's Redis outcome, which is the only per-request
		// evidence available without paying for a health probe on the hot path.
		if l.degrader.RedisDown() {
			l.degraded("concurrency")
		}
		return l.acquireDegraded(ctx, in, nowMS, minEpoch, day)
	}
	verdict, err := l.callAcquire(ctx, in, nowMS, minEpoch, day, true)
	if err != nil {
		return Verdict{}, err
	}
	verdict.ConcurrencyMode = ConcurrencyRedis
	return verdict, nil
}

func (l *Limiter) acquireDegraded(ctx context.Context, in AcquireInput, nowMS, minEpoch int64, day string) (Verdict, error) {
	// The divisor is a cached last-known value refreshed in the background, so
	// the concurrency dimension stays reachable while Redis is down. The
	// Redis-backed RPM/TPM/daily check in callAcquire below still fails closed.
	replicas := l.degrader.Replicas()
	perKey := PerKeyLocalLimit(in.Limits.Concurrency, replicas)
	if !l.degrader.TryAcquire(in.KeyID, perKey) {
		l.rejected("concurrency")
		nowSec := nowMS / 1000
		return Verdict{
			Allowed:         false,
			Code:            CodeConcurrency,
			Limit:           perKey,
			Total:           l.degrader.LocalCount(in.KeyID),
			Remaining:       0,
			ResetAt:         nowSec + 3,
			AllHit:          []string{CodeConcurrency},
			ResetAtMax:      nowSec + 3,
			ConcurrencyMode: ConcurrencyLocal,
		}, nil
	}

	verdict, err := l.callAcquire(ctx, in, nowMS, minEpoch, day, false)
	if err != nil {
		// Remember that Redis, not the mode, is why this request took the
		// local path. The request itself still fails closed: RPM/TPM/daily
		// protect quota and money and are never answered locally (T06 #7).
		l.degrader.MarkRedisDown()
		l.degrader.ReleaseLocal(in.KeyID, in.RequestID)
		return Verdict{}, err
	}
	l.degrader.MarkRedisUp()
	if !verdict.Allowed {
		l.degrader.ReleaseLocal(in.KeyID, in.RequestID)
		return verdict, nil
	}
	verdict.ConcurrencyMode = ConcurrencyLocal
	return verdict, nil
}

func (l *Limiter) callAcquire(ctx context.Context, in AcquireInput, nowMS, minEpoch int64, day string, ccEnabled bool) (Verdict, error) {
	keys := []string{
		redisx.RPMLKey(in.KeyID, minEpoch),
		redisx.TPMKey(in.KeyID, minEpoch),
		redisx.ConcurrencyKey(in.KeyID),
		redisx.DailyQuotaKey(in.UserID, day),
	}
	dqEnabled := int64(0)
	if in.Limits.DailyEnabled {
		dqEnabled = 1
	}
	ccFlag := int64(0)
	if ccEnabled {
		ccFlag = 1
	}
	args := []interface{}{
		nowMS,
		in.Limits.RPM,
		in.Limits.TPM,
		in.Limits.Concurrency,
		in.Limits.DailyQuotaMicro,
		in.EstTokens,
		in.EstMicro,
		in.RequestID,
		in.LeaseMS,
		dqEnabled,
		minEpoch,
		NextResetUnix(time.UnixMilli(nowMS)),
		ccFlag,
		TTLMillis(time.UnixMilli(nowMS)),
	}

	lctx, cancel := l.client.LuaContext(ctx)
	defer cancel()
	raw, err := acquireScript.Run(lctx, l.client.Raw(), keys, args...).Result()
	if err != nil {
		l.unavailable(ctx, "ratelimit")
		return Verdict{}, fmt.Errorf("%w: acquire: %v", ErrUnavailable, err)
	}
	values, ok := raw.([]interface{})
	if !ok || len(values) != 8 {
		l.unavailable(ctx, "ratelimit")
		return Verdict{}, fmt.Errorf("%w: acquire returned %T with %d slots", ErrUnavailable, raw, len(values))
	}

	verdict := Verdict{
		Allowed:    asInt64(values[0]) == 1,
		Code:       asString(values[1]),
		Limit:      asInt64(values[2]),
		Total:      asInt64(values[3]),
		Remaining:  asInt64(values[4]),
		ResetAt:    asInt64(values[5]),
		AllHit:     splitHits(asString(values[6])),
		ResetAtMax: asInt64(values[7]),
	}
	if !verdict.Allowed {
		l.rejected(dimOfCode(verdict.Code))
	}
	return verdict, nil
}

// RecordDailyRejection increments K5. K5 is display-only, so it is outside
// L1's atomic boundary (TDS §2.1: non-atomic is acceptable). T10 calls this
// after a verdict whose all_hit contains daily_quota_exceeded; the pipeline is
// best-effort and must not change the 429 result.
func (l *Limiter) RecordDailyRejection(ctx context.Context, userID int64, day string) error {
	if day == "" {
		day = Day(l.clock.Now())
	}
	key := redisx.DailyQuotaRejectedKey(userID, day)
	ttl := TTLMillis(l.clock.Now())
	cctx, cancel := l.client.CommandContext(ctx)
	defer cancel()
	pipe := l.client.Raw().Pipeline()
	pipe.Incr(cctx, key)
	pipe.PExpire(cctx, key, time.Duration(ttl)*time.Millisecond)
	_, err := pipe.Exec(cctx)
	return err
}

func splitHits(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func dimOfCode(code string) string {
	switch code {
	case CodeRPM:
		return "rpm"
	case CodeTPM:
		return "tpm"
	case CodeConcurrency:
		return "concurrency"
	case CodeDailyQuota:
		return "daily"
	default:
		return "ratelimit"
	}
}

func asInt64(v interface{}) int64 {
	switch value := v.(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case string:
		parsed, _ := strconv.ParseInt(value, 10, 64)
		return parsed
	case []byte:
		parsed, _ := strconv.ParseInt(string(value), 10, 64)
		return parsed
	default:
		return 0
	}
}

func asString(v interface{}) string {
	switch value := v.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}

func (l *Limiter) rejected(dim string) {
	if l.metrics != nil {
		l.metrics.RatelimitRejected(dim)
	}
}

func (l *Limiter) degraded(dim string) {
	if l.metrics != nil {
		l.metrics.RatelimitDegraded(dim)
	}
}

func (l *Limiter) unavailable(ctx context.Context, dim string) {
	if l.metrics != nil {
		l.metrics.RatelimitUnavailable(dim)
	}
	if l.logger != nil {
		// The caller turns this into 503; the log line makes the fail-close
		// visible even when the caller swallows the error.
		l.logger.Error(ctx, "rate_limiter_unavailable", log.Fields{Stage: "ratelimit"})
	}
}

package gw

import (
	"context"
	"time"

	"yunshu.dev/core/internal/billing"
	"yunshu.dev/core/internal/breaker"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/idemstore"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/ledger"
	obslog "yunshu.dev/core/internal/obs/log"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/ratelimit"
	"yunshu.dev/core/internal/redisx"
)

// The stage dependencies are interfaces rather than concrete T04/T06 types so
// every failure path can be unit-tested without MySQL or Redis. Production
// wiring lives in cmd/gatewayd.
type (
	// AuthReader is K13. A miss must answer 401, never a database lookup.
	AuthReader interface {
		Lookup(ctx context.Context, hash16 string) (redisx.KeyAuthRecord, error)
	}

	// BalanceReader is K26.
	BalanceReader interface {
		Get(ctx context.Context, userID int64) (int64, error)
	}

	// PriceSource is K19.
	PriceSource interface {
		GetStandard(ctx context.Context, modelID int64) (jsonx.RawMessage, error)
	}

	// LedgerStore is the money kernel surface the inbound half uses.
	LedgerStore interface {
		Prehold(ctx context.Context, p ledger.PreholdParams) (ledger.PreholdResult, error)
		Settle(ctx context.Context, p ledger.SettleParams) (ledger.SettleResult, error)
	}

	// Limiter is the four-dimension gate.
	Limiter interface {
		Acquire(ctx context.Context, in ratelimit.AcquireInput) (ratelimit.Verdict, error)
		Release(ctx context.Context, in ratelimit.ReleaseInput) (ratelimit.ReleaseResult, error)
		RecordDailyRejection(ctx context.Context, userID int64, day string) error
	}

	// IdemStore is the K6/K7 + t_idempotency_record surface.
	IdemStore interface {
		Begin(ctx context.Context, in idemstore.BeginInput) (idemstore.BeginResult, error)
		Complete(ctx context.Context, rec idemstore.Record, state idemstore.State, body []byte) error
		// DuplicateWindow is idem.duplicate_window_seconds (T41). Zero or
		// negative turns automatic de-duplication off for requests that carry no
		// Idempotency-Key.
		DuplicateWindow() time.Duration
	}

	// BreakerReader is the read-only K16 surface used by P12. It never records
	// an outcome: attempts belong to T08.
	BreakerReader interface {
		State(ctx context.Context, channelID int64) (breaker.State, int64, error)
	}

	// CounterReader supplies the counter values that fill both groups of
	// rate-limit headers on a 429. It is best-effort: a Redis error yields
	// zero, and the headers then describe the limit without a remaining value.
	CounterReader interface {
		UsedRPM(ctx context.Context, keyID, minEpoch int64) int64
		UsedDaily(ctx context.Context, userID int64, day string) int64
	}

	// BacklogReader is the P08.5 async-settlement gate (GR-03). It reports
	// whether t_settle_task's PENDING count is above 5000. The implementation
	// caches the count for one second, so a request never costs a COUNT(*).
	BacklogReader interface {
		Exceeded(ctx context.Context) bool
	}
)

// Deps is the assembled dependency set. A nil optional dependency disables the
// feature it backs; the required ones are checked by Validate at startup so a
// missing wire is a boot failure, not a 500 at the first request.
type Deps struct {
	Config  *config.Store
	Catalog *Catalog

	Auth    AuthReader
	Balance BalanceReader
	Prices  PriceSource
	Ledger  LedgerStore
	Limiter Limiter
	Idem    IdemStore
	Breaker BreakerReader

	Tokens        billing.InputCounter
	CounterReader CounterReader

	Backpressure *Backpressure
	ConnGuard    *ConnGuard
	// SettleBacklog is optional: nil keeps T08 behaviour (no GR-03 gate).
	SettleBacklog BacklogReader

	Logger *obslog.Logger
	Clock  *redisx.Clock

	// Metrics is optional. Recording never changes a decision: every method is
	// nil-safe and does not return an error.
	Metrics *obsmetrics.Recorder

	// PolicyChecker is the P06.5 content-safety gate (PRD §10.1). nil means the
	// gate is explicitly not configured; it is not replaced by a permissive
	// implementation. WarnDisabled reports the state at startup.
	PolicyChecker PolicyChecker
	// PolicyHits records a hit into t_content_policy_hit. A nil store still
	// refuses the request but records no evidence; WarnDisabled reports it.
	// Unlike the required dependencies this is not a boot failure: refusing is
	// the safe direction, losing the row is the visible cost.
	PolicyHits PolicyHitStore

	// NodeID is written to t_prehold_hold.node_id.
	NodeID string

	// FailureSink receives one FailureUsage row for a request that failed
	// before the upstream terminal ran (G23). A nil sink means T08 behaviour:
	// no backfill rows, no error. The sink must be non-blocking; the usage
	// writer drops rows rather than block the request path.
	FailureSink func(FailureUsage)
}

// Validate reports the first missing required dependency. It is called before
// the listener starts.
func (d *Deps) Validate() error {
	switch {
	case d == nil:
		return errDeps("deps")
	case d.Catalog == nil:
		return errDeps("catalog")
	case d.Auth == nil:
		return errDeps("auth")
	case d.Prices == nil:
		return errDeps("prices")
	case d.Balance == nil:
		return errDeps("balance")
	case d.Ledger == nil:
		return errDeps("ledger")
	case d.Limiter == nil:
		return errDeps("limiter")
	case d.Tokens == nil:
		return errDeps("tokenizer")
	case d.Backpressure == nil:
		return errDeps("backpressure")
	}
	return nil
}

type depsError string

func (e depsError) Error() string { return "gw: missing dependency: " + string(e) }

func errDeps(name string) error { return depsError(name) }

// WarnDisabled reports optional wiring that is absent, once per process. It is
// called from cmd/gatewayd after Validate. A nil PolicyChecker is a deliberate
// configuration state (the request path skips the gate) and must be visible in
// the log rather than inferred from an empty table.
func (d *Deps) WarnDisabled(ctx context.Context) {
	if d == nil || d.Logger == nil {
		return
	}
	if d.PolicyChecker == nil {
		d.Logger.Warn(ctx, "content_policy_checker_disabled: Deps.PolicyChecker is nil; requests are not screened",
			obslog.Fields{Stage: StageP065})
	}
	if d.PolicyHits == nil {
		d.Logger.Warn(ctx, "content_policy_hit_store_disabled: Deps.PolicyHits is nil; hits will be refused without an evidence row",
			obslog.Fields{Stage: StageP065})
	}
}

// NewDefaultPipeline builds the six-slot execution order. The slice order is
// the contract; see Pipeline.Run for why P12 precedes P11.
func NewDefaultPipeline(d *Deps) *Pipeline {
	return NewPipeline(d.Logger,
		NewAdmissionStage(
			NewAuthStage(d),
			NewContentPolicyStage(d),
			NewIPStage(d),
			NewModelStage(d),
			NewIdemStage(d),
		),
		NewBackpressureStage(d),
		NewRouterStage(d),
		NewEstimateStage(d),
		NewRateLimitStage(d),
		NewPreholdStage(d),
	)
}

// NewAuthOnlyPipeline is the /v1/models path: authentication, IP rules and
// the process-level backpressure gate. It has no idempotency, no four-dimension
// rate limit and no prehold (TDS §6.1 "不消耗额度"). Backpressure is capacity
// protection, not quota: at >90% a listing request is shed like any other
// request (TDS §4.3), but it never consumes a rate-limit lease or a hold.
func NewAuthOnlyPipeline(d *Deps) *Pipeline {
	return NewPipeline(d.Logger, NewAuthStage(d), NewIPStage(d), NewBackpressureStage(d))
}

// ClockNowMS returns the corrected unix milliseconds.
func (d *Deps) ClockNowMS() int64 {
	if d.Clock != nil {
		return d.Clock.NowMS()
	}
	return redisx.NewClock(nil, nil).NowMS()
}

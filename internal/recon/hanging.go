package recon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"yunshu.dev/core/internal/billing"
	"yunshu.dev/core/internal/ledger"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/settle"
)

// Hanging recovery defaults from TDS §5.3.
const (
	HangingBatchSize     = 200
	HangingRatePerSecond = 100
	HangingLedgerTTL     = 300 * time.Second
	HangingBacklogRounds = 3
)

// Hanging types from TDS §5.3. They are deliberately distinct: only A and B
// may release; C must never be silently released.
const (
	HangingTypeA = "A" // prehold written, upstream never started
	HangingTypeB = "B" // upstream started and failed
	HangingTypeC = "C" // upstream succeeded or stream body started
)

// Settler is the subset of *ledger.Store hanging recovery needs.
type Settler interface {
	Settle(ctx context.Context, p ledger.SettleParams) (ledger.SettleResult, error)
}

// Enqueuer is the subset of *settle.Enqueuer hanging recovery needs.
type Enqueuer interface {
	Enqueue(ctx context.Context, p settle.EnqueueParams) error
}

// CostFunc prices tokens for a type-C recovery. The production implementation
// reads t_price_rule through RuleCost; tests inject a fixed value.
type CostFunc func(ctx context.Context, modelID, tokensIn, tokensOut int64, at time.Time) (int64, error)

// Hanging scans expired HELD rows and resolves the three TDS §5.3 states.
//
// 资金真相只在 DB：A/B 走 ledger.Settle（写 t_ledger_entry），C 只入队，绝不
// 直接改余额。K12 hold:recover:{rid} 是唯一的去重闸门。
type Hanging struct {
	DB            *sql.DB
	Redis         *redisx.Client
	Ledger        Settler
	Enqueuer      Enqueuer
	Logger        *obslog.Logger
	Clock         clock.Clock
	Recorder      *Recorder
	Cost          CostFunc
	BatchSize     int
	RatePerSecond int
	// Metrics is optional. Every recovered expired hold is reason=ttl_expired;
	// db_sweep is reserved for a future sweep path.
	Metrics Metrics
	// UserLo/UserHi limit the scan to [UserLo, UserHi). Zero, zero scans the
	// whole table, which is the only shape production uses. Tests sharing a
	// schema set them so unrelated stale holds cannot enter their counters.
	UserLo, UserHi int64

	backlogRounds  int
	backlogAlerted bool
}

// HangingResult is one round's counters.
type HangingResult struct {
	Scanned   int
	Processed int
	Skipped   int
	TypeA     int
	TypeB     int
	TypeC     int
	Failed    int
	Backlog   bool
}

// NewHanging builds the recovery task. Cost defaults to RuleCost.
func NewHanging(db *sql.DB, rdb *redisx.Client, st Settler, en Enqueuer, logger *obslog.Logger, clk clock.Clock) *Hanging {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Hanging{
		DB:            db,
		Redis:         rdb,
		Ledger:        st,
		Enqueuer:      en,
		Logger:        logger,
		Clock:         clk,
		Recorder:      NewRecorder(db, logger, clk),
		Cost:          RuleCost(db, clk),
		BatchSize:     HangingBatchSize,
		RatePerSecond: HangingRatePerSecond,
	}
}

type hangingFacts struct {
	requestID string
	userID    int64
	holdMicro int64
	modelID   int64
	createdAt time.Time
	state     string

	hasUsage       bool
	usageAttempts  int
	usageResult    string
	usageConsumed  int64
	usageTokensIn  int64
	usageTokensOut int64

	attemptCount     int
	attemptFailed    int
	attemptSuccess   int
	attemptStream    int
	attemptTokensIn  int64
	attemptTokensOut int64

	hasSettleTask bool
	hasSettleRow  bool
}

// Round runs one 60s scan. It is called by jobsd's 60s task period; the method
// itself does not sleep between rounds.
func (h *Hanging) Round(ctx context.Context) (HangingResult, error) {
	var out HangingResult
	if h == nil || h.DB == nil || h.Ledger == nil || h.Enqueuer == nil {
		return out, fmt.Errorf("recon: hanging requires db/ledger/enqueuer")
	}
	if h.BatchSize <= 0 {
		h.BatchSize = HangingBatchSize
	}
	rids, err := h.scan(ctx)
	if err != nil {
		return out, err
	}
	out.Scanned = len(rids)

	processedThisSecond := 0
	for _, rid := range rids {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		ok, err := h.claim(ctx, rid)
		if err != nil {
			h.logf(ctx, obslog.LevelWarn, "hold recover lock failed: "+err.Error(), rid)
			out.Failed++
			continue
		}
		if !ok {
			out.Skipped++
			continue
		}
		facts, typ, err := h.resolve(ctx, rid)
		if err != nil {
			h.logf(ctx, obslog.LevelWarn, "hold recover classify failed: "+err.Error(), rid)
			out.Failed++
			continue
		}
		if typ == "" {
			out.Skipped++
			continue
		}
		switch typ {
		case HangingTypeA:
			if err := h.recoverA(ctx, rid, facts); err != nil {
				h.logf(ctx, obslog.LevelError, "prehold_crash_recover failed: "+err.Error(), rid)
				out.Failed++
				continue
			}
			out.TypeA++
		case HangingTypeB:
			if err := h.recoverB(ctx, rid, facts); err != nil {
				h.logf(ctx, obslog.LevelError, "prehold_upstream_fail failed: "+err.Error(), rid)
				out.Failed++
				continue
			}
			out.TypeB++
		case HangingTypeC:
			enqueued, err := h.recoverC(ctx, rid, facts)
			if err != nil {
				h.logf(ctx, obslog.LevelError, "prehold_half_completed failed: "+err.Error(), rid)
				out.Failed++
				continue
			}
			if enqueued {
				out.TypeC++
			} else {
				out.Skipped++
			}
		}
		out.Processed++
		if h.Metrics != nil {
			h.Metrics.PreholdOrphan("ttl_expired")
		}

		processedThisSecond++
		if h.RatePerSecond > 0 && processedThisSecond >= h.RatePerSecond {
			// TDS §5.3: cap at 100 rows/s. Sleeping one second per 100 rows is
			// a strict upper bound and protects the primary during an outage.
			time.Sleep(time.Second)
			processedThisSecond = 0
		}
	}

	if out.Scanned >= h.BatchSize {
		h.backlogRounds++
		if h.backlogRounds >= HangingBacklogRounds && !h.backlogAlerted {
			h.backlogAlerted = true
			out.Backlog = true
			if h.Logger != nil {
				h.Logger.Error(ctx, fmt.Sprintf("reconcile_backlog rounds=%d batch=%d", h.backlogRounds, h.BatchSize),
					obslog.Fields{Stage: "P29"})
			}
		}
	} else {
		h.backlogRounds = 0
		h.backlogAlerted = false
	}
	return out, nil
}

func (h *Hanging) scan(ctx context.Context) ([]string, error) {
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("recon: begin hanging scan: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only scan; rollback releases SKIP LOCKED rows

	q := `SELECT request_id
	        FROM t_prehold_hold
	       WHERE state = 'HELD' AND expires_at < UTC_TIMESTAMP(3)`
	args := []any{}
	if h.UserLo != 0 || h.UserHi != 0 {
		lo, hi := h.UserLo, h.UserHi
		if lo < 0 {
			lo = 0
		}
		if hi <= lo {
			hi = math.MaxInt64
		}
		q += ` AND user_id >= ? AND user_id < ?`
		args = append(args, lo, hi)
	}
	q += ` ORDER BY expires_at ASC
	       LIMIT ?
	       FOR UPDATE SKIP LOCKED`
	args = append(args, h.BatchSize)
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("recon: scan hanging holds: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("recon: scan hanging row: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recon: scan hanging rows: %w", err)
	}
	return ids, nil
}

// claim is the K12 SET NX guard. A false result means another replica (or an
// earlier 300s window that is still in flight) owns this request.
func (h *Hanging) claim(ctx context.Context, rid string) (bool, error) {
	if h.Redis == nil {
		return false, fmt.Errorf("recon: hold:recover requires redis (hard constraint 6)")
	}
	cctx, cancel := h.Redis.CommandContext(ctx)
	defer cancel()
	return h.Redis.Raw().SetNX(cctx, redisx.HoldRecoverKey(rid), "1", HangingLedgerTTL).Result()
}

func (h *Hanging) resolve(ctx context.Context, rid string) (hangingFacts, string, error) {
	var f hangingFacts
	f.requestID = rid
	var modelID sql.NullInt64
	err := h.DB.QueryRowContext(ctx,
		`SELECT user_id, hold_micro, model_id, created_at, state
		   FROM t_prehold_hold WHERE request_id = ?`, rid,
	).Scan(&f.userID, &f.holdMicro, &modelID, &f.createdAt, &f.state)
	if errors.Is(err, sql.ErrNoRows) {
		return f, "", nil
	}
	if err != nil {
		return f, "", fmt.Errorf("recon: read hold %s: %w", rid, err)
	}
	if f.state != ledger.HoldStateHeld {
		return f, "", nil
	}
	f.modelID = modelID.Int64

	var attempts int
	err = h.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_upstream_attempt WHERE request_id = ?`, rid).Scan(&attempts)
	if err != nil {
		return f, "", fmt.Errorf("recon: count attempts %s: %w", rid, err)
	}
	f.attemptCount = attempts
	if attempts > 0 {
		rows, err := h.DB.QueryContext(ctx,
			`SELECT result, phase, COALESCE(tokens_in,0), COALESCE(tokens_out,0)
			   FROM t_upstream_attempt WHERE request_id = ? ORDER BY attempt_index`, rid)
		if err != nil {
			return f, "", fmt.Errorf("recon: read attempts %s: %w", rid, err)
		}
		for rows.Next() {
			var result, phase string
			var in, out int64
			if err := rows.Scan(&result, &phase, &in, &out); err != nil {
				rows.Close()
				return f, "", fmt.Errorf("recon: scan attempt %s: %w", rid, err)
			}
			switch result {
			case "SUCCESS":
				f.attemptSuccess++
			case "FAILED":
				f.attemptFailed++
			}
			if phase == "STREAM_BODY" {
				f.attemptStream++
			}
			if in > f.attemptTokensIn {
				f.attemptTokensIn = in
			}
			if out > f.attemptTokensOut {
				f.attemptTokensOut = out
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return f, "", fmt.Errorf("recon: iterate attempts %s: %w", rid, err)
		}
		rows.Close()
	}

	var usageTokensIn, usageTokensOut sql.NullInt64
	err = h.DB.QueryRowContext(ctx,
		`SELECT upstream_attempts, result, consumed_micro,
		        COALESCE(tokens_in,0), COALESCE(tokens_out,0)
		   FROM t_usage_record WHERE request_id = ?`, rid,
	).Scan(&f.usageAttempts, &f.usageResult, &f.usageConsumed, &usageTokensIn, &usageTokensOut)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		f.hasUsage = false
	case err != nil:
		return f, "", fmt.Errorf("recon: read usage %s: %w", rid, err)
	default:
		f.hasUsage = true
		f.usageTokensIn, f.usageTokensOut = usageTokensIn.Int64, usageTokensOut.Int64
	}

	// DEAD tasks are excluded on purpose: a task that exhausted its retries
	// still has a row, and counting it would short-circuit classifyHanging
	// forever ("a settlement task exists, nothing to do") while the prehold
	// stays HELD. Excluding them lets the row reach the classifier, which
	// either releases it (A/B) or re-enqueues and alerts (C).
	err = h.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_settle_task
		  WHERE request_id = ? AND task_type = ? AND state <> ?`,
		rid, settle.TaskTypeSettle, settle.TaskStateDead).Scan(&f.hasSettleTask)
	if err != nil {
		return f, "", fmt.Errorf("recon: read settle task %s: %w", rid, err)
	}
	var settleRows int
	err = h.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_ledger_entry
		  WHERE request_id = ? AND biz_type = ?`,
		rid, ledger.BizTypeConsumeSettle).Scan(&settleRows)
	if err != nil {
		return f, "", fmt.Errorf("recon: read settle entry %s: %w", rid, err)
	}
	f.hasSettleRow = settleRows > 0

	return f, classifyHanging(f), nil
}

// classifyHanging follows TDS §5.3 verbatim. Unknown shapes fall through to C:
// releasing an ambiguous hold is the one outcome the card forbids.
func classifyHanging(f hangingFacts) string {
	if f.hasSettleRow || f.hasSettleTask {
		return ""
	}
	// A: no upstream attempt row at all. The TDS literal first branch is
	// "usage exists with upstream_attempts=0"; an actual attempt row wins over
	// a missing usage counter, because releasing after an upstream call is the
	// expensive mistake this table exists to prevent.
	if f.attemptCount == 0 {
		return HangingTypeA
	}
	// C: an attempt succeeded or a stream body started, or usage says SUCCESS.
	if f.attemptSuccess > 0 || f.attemptStream > 0 ||
		(f.hasUsage && f.usageResult == "SUCCESS") || f.usageConsumed > 0 {
		return HangingTypeC
	}
	// B: attempts exist and every one failed.
	if f.attemptCount > 0 && f.attemptFailed == f.attemptCount {
		return HangingTypeB
	}
	return HangingTypeC
}

func (h *Hanging) recoverA(ctx context.Context, rid string, f hangingFacts) error {
	if _, err := h.Ledger.Settle(ctx, ledger.SettleParams{
		RequestID: rid, UserID: f.userID, ConsumedMicro: 0, ReleaseReason: "CRASH_RECOVER",
	}); err != nil {
		return err
	}
	if f.hasUsage {
		if _, err := h.DB.ExecContext(ctx,
			`UPDATE t_usage_record
			    SET result='FAILED', fail_stage='SETTLE', fail_owner='PLATFORM', consumed_micro=0
			  WHERE request_id=?`, rid); err != nil {
			return fmt.Errorf("recon: correct usage for %s: %w", rid, err)
		}
	}
	h.logf(ctx, obslog.LevelInfo, "prehold_crash_recover released", rid)
	return nil
}

func (h *Hanging) recoverB(ctx context.Context, rid string, f hangingFacts) error {
	if _, err := h.Ledger.Settle(ctx, ledger.SettleParams{
		RequestID: rid, UserID: f.userID, ConsumedMicro: 0, ReleaseReason: "UPSTREAM_FAIL",
	}); err != nil {
		return err
	}
	h.logf(ctx, obslog.LevelInfo, "prehold_upstream_fail released", rid)
	return nil
}

// recoverC enqueues settlement and alerts operations. It returns false when a
// settlement task already exists (nothing new to do) or when the request has
// already been revived too often.
func (h *Hanging) recoverC(ctx context.Context, rid string, f hangingFacts) (bool, error) {
	if f.hasSettleTask {
		return false, nil
	}

	// A DEAD task that is reclassified becomes PENDING again with a fresh attempt
	// budget, and the worker's retries take ~13 minutes to exhaust. Nothing stops
	// the next scan from doing it again, so a deterministic failure (an
	// ErrInsufficientBatches from a data problem, for instance) would loop
	// forever, one operator notification per round, while DEAD — the "this needs
	// a human" signal — silently stops meaning anything. The counter caps it;
	// the durable equivalent (t_settle_task.revive_count) belongs with the
	// suppressed-usage migration, not here.
	revives, capped := h.countRevive(ctx, rid)
	if capped {
		// The notification is attempted across a short retry window, NOT once at the
		// crossing round. `capped` stays true for the whole 24h TTL and this branch
		// runs once per K12 window (300s), so pinning Notify to
		// `revives == MaxHangingRevives+1` turned at-least-once into at-most-once:
		// one transient error at that single round lost the operator hand-off for
		// the rest of the day, because the next round sees revives=5 and never
		// matches again. This branch is the only hand-off there is (no Enqueue
		// below, by design), so a lost notification means a DEAD request nobody
		// ever hears about. Notify is idempotent per body, so the retries collapse
		// onto one row rather than producing three.
		//
		// The ERROR alert stays pinned to the crossing round: an unconditional one
		// would emit ~288 lines per day per stuck request — the log equivalent of
		// the notification flood, and enough to bury real alerts. Rounds after the
		// window log nothing, deliberately: "still capped" is not new information,
		// it is exactly `t_settle_task.state='DEAD' AND revive_count > 3`, which the
		// table can answer and the notification row already reports.
		// The ERROR line is emitted BEFORE the notification and independently of its
		// outcome. It is the channel that cannot be lost: if Notify fails, returning
		// early would drop both the durable row and the log line, which is the
		// original bug in a new place. Pinned to the crossing round so it fires
		// exactly once — an unconditional one would emit ~288 lines per day per
		// stuck request, the log equivalent of the notification flood.
		if revives == int64(MaxHangingRevives)+1 && h.Logger != nil {
			h.Logger.Error(ctx, fmt.Sprintf("prehold_revive_capped rid=%s user=%d revives=%d",
				rid, f.userID, revives), obslog.Fields{Stage: "P29", RequestID: rid})
		}
		if revives <= int64(MaxHangingRevives)+3 {
			// The body carries the UTC day, so a request that is still stuck
			// tomorrow produces exactly one more notice — a daily reminder rather
			// than a silent permanent failure — while every round inside the same
			// day collapses onto the same row (Notify is idempotent per body). The
			// live counter is deliberately NOT in the body: it changes every round
			// and would defeat that collapse. Expect one row per request per day,
			// not one per lifetime: ReviveTTL expires daily.
			body := fmt.Sprintf("request_id=%s user_id=%d manual=true revive_capped=true revive_limit=%d revive_day=%s",
				rid, f.userID, MaxHangingRevives, h.now().Format("2006-01-02"))
			if err := h.Recorder.Notify(ctx, "RECONCILE_MANUAL",
				"悬挂预扣复活次数超限，需人工处理", body, 3); err != nil {
				return false, fmt.Errorf("recon: notify revive-capped %s: %w", rid, err)
			}
		}
		// Deliberately no Enqueue: the task stays DEAD, which is the signal that a
		// retry will not help. The notification above is the hand-off.
		return false, nil
	}

	tokensIn, tokensOut := f.usageTokensIn, f.usageTokensOut
	if f.attemptTokensIn > tokensIn {
		tokensIn = f.attemptTokensIn
	}
	if f.attemptTokensOut > tokensOut {
		tokensOut = f.attemptTokensOut
	}
	consumed := f.usageConsumed
	if consumed == 0 && tokensOut > 0 {
		if h.Cost != nil {
			at := f.createdAt
			if at.IsZero() {
				at = h.now()
			}
			var err error
			consumed, err = h.Cost(ctx, f.modelID, tokensIn, tokensOut, at)
			if err != nil {
				return false, fmt.Errorf("recon: price half-completed %s: %w", rid, err)
			}
		}
	}
	manual := tokensOut == 0 && !f.hasUsage
	if manual {
		// TDS §5.3: zero-credit fallback is allowed, but it must be paired with
		// an operations task. It is never a silent release.
		consumed = 0
	}
	if err := h.Enqueuer.Enqueue(ctx, settle.EnqueueParams{
		RequestID: rid, UserID: f.userID, ConsumedMicro: consumed,
	}); err != nil {
		return false, fmt.Errorf("recon: enqueue half-completed %s: %w", rid, err)
	}

	body := fmt.Sprintf("request_id=%s user_id=%d tokens_in=%d tokens_out=%d consumed_micro=%d manual=%t revive=%d",
		rid, f.userID, tokensIn, tokensOut, consumed, manual, revives)
	if err := h.Recorder.Notify(ctx, "RECONCILE_MANUAL",
		"悬挂预扣半完成，需要人工核查", body, 3); err != nil {
		return false, fmt.Errorf("recon: notify half-completed %s: %w", rid, err)
	}
	if h.Logger != nil {
		h.Logger.Error(ctx, fmt.Sprintf("prehold_half_completed rid=%s user=%d tokens_in=%d tokens_out=%d consumed=%d manual=%t revive=%d",
			rid, f.userID, tokensIn, tokensOut, consumed, manual, revives),
			obslog.Fields{Stage: "P29", RequestID: rid})
	}
	return true, nil
}

// MaxHangingRevives is how many times one request_id may be put back in the
// settlement queue before recon stops trying and hands it to an operator.
//
// Three is enough for the transient causes (a failover, a lock timeout) and far
// too many for the deterministic ones: a batch/account mismatch fails
// identically on every attempt, so the fourth revive only spends ~13 minutes of
// worker time and one notification. The counter lives in Redis with a TTL so a
// rid is never locked out for good.
const MaxHangingRevives = 3

// ReviveTTL bounds how long revive attempts are remembered. After it expires the
// rid starts from zero, which is the right behaviour for a long-lived process:
// yesterday's failures should not block today's recovery.
const ReviveTTL = 24 * time.Hour

// reviveCounterKey is the per-request revive counter. It is separate from K12
// (hold:recover:{rid}, 300s) on purpose: K12 serialises replicas within one
// window, this counts across windows.
func reviveCounterKey(requestID string) string { return "recon:revive:" + requestID }

// countRevive increments and returns the revive counter for one request, and
// reports whether the cap has been reached.
//
// Redis being unavailable is treated as "not capped": the money path must not
// stall on a counter, and the K12 claim that guards this call already requires a
// working Redis, so a failure here is a narrow window rather than a mode.
func (h *Hanging) countRevive(ctx context.Context, rid string) (int64, bool) {
	if h == nil || h.Redis == nil {
		return 0, false
	}
	cctx, cancel := h.Redis.CommandContext(ctx)
	defer cancel()
	key := reviveCounterKey(rid)
	n, err := h.Redis.Raw().Incr(cctx, key).Result()
	if err != nil {
		if h.Logger != nil {
			h.Logger.Warn(ctx, "prehold_revive_count_failed: "+err.Error(),
				obslog.Fields{Stage: "P29", RequestID: rid})
		}
		return 0, false
	}
	if n == 1 {
		// TTL is set once, on creation: refreshing it on every increment would keep
		// a permanently failing rid counted forever.
		if err := h.Redis.Raw().Expire(cctx, key, ReviveTTL).Err(); err != nil && h.Logger != nil {
			h.Logger.Warn(ctx, "prehold_revive_ttl_failed: "+err.Error(),
				obslog.Fields{Stage: "P29", RequestID: rid})
		}
	}
	return n, n > MaxHangingRevives
}

func (h *Hanging) now() time.Time {
	if h == nil || h.Clock == nil {
		return time.Now().UTC()
	}
	return h.Clock.Now().UTC()
}

func (h *Hanging) logf(ctx context.Context, level obslog.Level, msg, rid string) {
	if h.Logger == nil {
		return
	}
	switch level {
	case obslog.LevelError:
		h.Logger.Error(ctx, msg, obslog.Fields{Stage: "P29", RequestID: rid})
	case obslog.LevelWarn:
		h.Logger.Warn(ctx, msg, obslog.Fields{Stage: "P29", RequestID: rid})
	default:
		h.Logger.Info(ctx, msg, obslog.Fields{Stage: "P29", RequestID: rid})
	}
}

// RuleCost prices tokens from the active MODEL-level t_price_rule row at the
// given instant. It is the default for type C when usage has no consumed value.
// A missing rule returns 0; the caller has already raised RECONCILE_MANUAL.
func RuleCost(db *sql.DB, clk clock.Clock) CostFunc {
	return func(ctx context.Context, modelID, tokensIn, tokensOut int64, at time.Time) (int64, error) {
		if db == nil || modelID <= 0 {
			return 0, nil
		}
		var inPrice, outPrice int64
		err := db.QueryRowContext(ctx,
			`SELECT in_price_micro_per_k, out_price_micro_per_k
			   FROM t_price_rule
			  WHERE scope_level='MODEL' AND scope_id=? AND enabled=1
			    AND effective_from <= ?
			    AND (effective_to IS NULL OR effective_to > ?)
			  ORDER BY effective_from DESC
			  LIMIT 1`,
			modelID, at.UTC(), at.UTC(),
		).Scan(&inPrice, &outPrice)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		return billing.CostMicro(tokensIn, tokensOut, inPrice, outPrice), nil
	}
}

package settle

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"yunshu.dev/core/internal/platform/clock"
)

// BacklogThreshold is the GR-03 backpressure line. Above it the gateway rejects
// NEW requests with 503 backpressure_rejected; requests already in flight are
// never rejected (T09 hard constraint 5).
const BacklogThreshold = 5000

// BacklogWarnThreshold is the early-warning line exported as
// gw_settle_backlog_tasks: 90% of BacklogThreshold. Backpressure is a lagging
// signal — by the time it fires the queue is already refusing traffic — so the
// alert has to sit strictly below the hard line to be actionable (G24).
//
// The number is derived, not chosen: 5000 * 9 / 10 = 4500. The Prometheus rule
// in internal/obs/alert/rules.yaml repeats the literal (PromQL cannot import
// Go); TestBacklogWarnThresholdMatchesAlertRule keeps the two equal, so this
// constant is the single place to change.
const BacklogWarnThreshold = BacklogThreshold * 9 / 10

// PendingCountSQL counts the settlement backlog. It is the ONE definition of
// "how much settlement work is queued": the gateway's capacity gate
// (Backlog.SettleBacklog) and the jobsd gauge producer (jobs.ObsSample) both
// execute it, so the number the alert shows and the number that rejects
// traffic can never drift apart (G24).
//
// It is a const so a test can assert both call sites use it instead of
// growing a second query.
const PendingCountSQL = `SELECT COUNT(*) FROM t_settle_task WHERE state = ?`

// CountPending runs PendingCountSQL against db. db may be nil (a process
// without a database), in which case the count is zero and no error is
// returned: a gauge producer must not fail because a dependency is absent from
// the wiring.
func CountPending(ctx context.Context, db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	var n int64
	if err := db.QueryRowContext(ctx, PendingCountSQL, TaskStatePending).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Backlog counts PENDING settlement tasks.
//
// Counting on every request would put a SELECT COUNT(*) on the gateway's hot
// path. The value is cached for one second instead: at most one query per
// second per gateway process, and a burst of 5000 tasks is still visible within
// the second. A database error returns the last known value (fail-open for the
// capacity gate — refusing traffic because a COUNT failed would turn a DB
// hiccup into a total outage).
type Backlog struct {
	DB    *sql.DB
	Clock clock.Clock
	TTL   time.Duration

	mu      sync.Mutex
	value   int64
	checked time.Time
}

// NewBacklog builds a one-second-cached backlog counter.
func NewBacklog(db *sql.DB, clk clock.Clock) *Backlog {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Backlog{DB: db, Clock: clk, TTL: time.Second}
}

// SettleBacklog returns the number of PENDING tasks, cached for TTL.
func (b *Backlog) SettleBacklog(ctx context.Context) (int64, error) {
	if b == nil {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	if !b.checked.IsZero() && now.Sub(b.checked) < b.ttl() {
		return b.value, nil
	}
	if b.DB == nil {
		return b.value, nil
	}
	n, err := CountPending(ctx, b.DB)
	if err != nil {
		// Keep the stale value rather than reporting zero: a zero would
		// silently disable backpressure.
		return b.value, err
	}
	b.value = n
	b.checked = now
	return n, nil
}

// Exceeded reports whether the backlog is above BacklogThreshold.
func (b *Backlog) Exceeded(ctx context.Context) bool {
	n, err := b.SettleBacklog(ctx)
	return err == nil && n > BacklogThreshold
}

// Invalidate drops the cached value. Tests call it after fixtures change.
func (b *Backlog) Invalidate() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.checked = time.Time{}
	b.mu.Unlock()
}

func (b *Backlog) ttl() time.Duration {
	if b.TTL <= 0 {
		return time.Second
	}
	return b.TTL
}

func (b *Backlog) now() time.Time {
	if b.Clock == nil {
		return time.Now().UTC()
	}
	return b.Clock.Now().UTC()
}

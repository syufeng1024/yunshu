package jobs

import (
	"context"
	"database/sql"
	"fmt"

	obslog "yunshu.dev/core/internal/obs/log"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/settle"
)

// ObsSample produces the point-in-time gauges that no request-scoped producer
// can export: how deep the settlement queue is, and how many support tickets
// have blown their first-response deadline. Both are counts over a table, not
// events, so they can only come from a process that holds a DB handle and runs
// on a timer — jobsd (T22 §5.3 notes; G24/G25).
//
// Every method is nil-safe and read-only. A failed sample keeps the previously
// exported value: overwriting a gauge with 0 because MySQL hiccuped would make
// an alert graph report "queue empty" during the one moment it is not.
type ObsSample struct {
	DB      *sql.DB
	Metrics *obsmetrics.Recorder
	Logger  *obslog.Logger
}

// NewObsSample builds a sampler. Any field may be nil; the corresponding
// method then becomes a no-op (used by tests that only want the metric).
func NewObsSample(db *sql.DB, metrics *obsmetrics.Recorder, logger *obslog.Logger) *ObsSample {
	return &ObsSample{DB: db, Metrics: metrics, Logger: logger}
}

// SettleBacklogTaskName is the registry name of the settlement-backlog
// sampler. The task only reads, so it runs on every replica instead of behind
// the leader lease: a sole leader would stop exporting the gauge the moment the
// lease moved, and the alert would go quiet exactly when the job process is
// being replaced.
const SettleBacklogTaskName = "obs_settle_backlog"

// RunSettleBacklog samples the PENDING settlement-task count and exports it as
// gw_settle_backlog_tasks.
//
// The query is settle.PendingCountSQL — the same statement the gateway's
// capacity gate uses — so the alert number and the number that actually sheds
// traffic cannot disagree. It is executed directly here (not through
// settle.Backlog) because the gauge must be current: the gateway cache answers
// from a value up to one second old, which is fine for shedding but pointless
// for a 30s scrape.
func (s *ObsSample) RunSettleBacklog(ctx context.Context) error {
	if s == nil {
		return nil
	}
	n, err := settle.CountPending(ctx, s.DB)
	if err != nil {
		s.warn(ctx, "obs_settle_backlog_failed: "+err.Error())
		return fmt.Errorf("jobs: sample settle backlog: %w", err)
	}
	if s.Metrics != nil {
		s.Metrics.SettleBacklogTasks(n)
	}
	return nil
}

func (s *ObsSample) warn(ctx context.Context, msg string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Warn(ctx, msg, obslog.Fields{Stage: "obs_sample"})
}

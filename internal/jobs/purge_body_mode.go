package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control/audit"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// PurgeBodyModeTaskName is the registry name of the "closing the switch deletes
// what was already stored" task (TDS 4318 / B-67「用户关闭即删」); G2.
const PurgeBodyModeTaskName = "purge_body_mode"

// PurgeBodyModeAction is the t_admin_audit_log.action for a mode-driven purge.
// It is a distinct action from the retention sweep on purpose: an operator
// asking "why did the body of request X disappear after 3 days" must find a
// different answer for "retention expired" and "the product owner switched
// full logging off". Length is checked by a test against the column width.
const PurgeBodyModeAction = "LOG_BODY_PURGED_ON_MODE_DISABLED"

// PurgeBodyModeAuditModule is the t_admin_audit_log.module. body_mode is a
// configuration key, so CONFIG (not a data module) is the honest bucket.
const PurgeBodyModeAuditModule = "CONFIG"

// purgeAllBodiesSQL deletes every stored body, one batch at a time. There is
// deliberately no WHERE clause: under meta_only *every* remaining row is a
// violation, and adding a predicate would let a row hide from the delete.
// ORDER BY id keeps the delete on the primary key so it stays a range scan.
const purgeAllBodiesSQL = `DELETE FROM t_request_body ORDER BY id LIMIT ?`

// BodyModeReader resolves the *effective* log.body_mode. An error means "we
// cannot tell", and every caller must treat that as "do not delete": keeping a
// body one more minute is recoverable, deleting a body that opt_in promised to
// keep is not.
type BodyModeReader func(ctx context.Context) (string, error)

// PurgeBodiesOnMetaOnly deletes retained request bodies while full logging is
// switched off.
//
// The rule is the product promise behind `log.body_mode = meta_only`: "用户关闭
// 即删" (TDS 4318 / B-67). Before this task the only body deletion was the
// 180-day retention sweep, so a tenant that switched the feature off kept
// yesterday's bodies for up to six months — a retention promise nobody could
// keep and nobody was told about.
//
// It is state-free: the trigger is "the current mode is meta_only AND rows
// exist", not "the mode changed since the last run". That is strictly safer
// than remembering the previous mode —
//
//   - a restart or a leader handover loses no memory, because there is none;
//   - a configuration rollback (opt_in → meta_only → opt_in) cannot leave a
//     window in which bodies survive because "we already ran the transition";
//   - a node that was down during the switch deletes on its next tick instead
//     of needing to have observed the switch.
//
// The deletes are plain idempotent DELETE statements, so they can overlap with
// the 180-day sweep (PurgeLog.purgeExpiredBodies) safely: each deletes the rows
// its own predicate matches, and a row already gone simply matches nothing.
type PurgeBodiesOnMetaOnly struct {
	DB *sql.DB
	// Purger supplies the batch loop (BodyPurgeBatchSize, 200ms pacing,
	// ctx cancellation) and tableExists. Reusing it is deliberate: a second
	// batched-delete loop would drift from the first one on the first
	// retention change.
	Purger *PurgeLog
	// Audit is required. A deletion of stored user content with no trail is
	// worse than a deletion that never ran.
	Audit  AuditInserter
	Logger *obslog.Logger
	Clock  clock.Clock
	// BodyMode is the configuration seam. Nil reads t_config_item through
	// config.NewStoreFromDB, i.e. the same registry defaults the gateway uses.
	BodyMode BodyModeReader
	// BatchSize bounds one DELETE statement. Zero means BodyPurgeBatchSize —
	// the same constant the retention sweep uses, so both paths page the table
	// the same way. It is a field only so a test can exercise paging on a small
	// fixture.
	BatchSize int
}

// PurgeBodiesOnMetaOnlyResult is one run.
type PurgeBodiesOnMetaOnlyResult struct {
	// Mode is the effective log.body_mode this run acted on.
	Mode string
	// Skipped is true when the mode is opt_in: nothing was even counted.
	Skipped bool
	// Deleted is the number of body rows removed by this run.
	Deleted int
	// Batches is how many DELETE statements ran.
	Batches int
	// Truncated reports that MaxBatches was hit; the next run continues.
	Truncated bool
}

// NewPurgeBodiesOnMetaOnly builds the task with production retention settings.
func NewPurgeBodiesOnMetaOnly(db *sql.DB, purger *PurgeLog, auditWriter AuditInserter, logger *obslog.Logger, clk clock.Clock) *PurgeBodiesOnMetaOnly {
	if clk == nil {
		clk = clock.Real{}
	}
	if purger == nil {
		purger = NewPurgeLog(db, logger, clk)
	}
	return &PurgeBodiesOnMetaOnly{
		DB: db, Purger: purger, Audit: auditWriter, Logger: logger, Clock: clk,
	}
}

// Run deletes every retained body when the effective mode is meta_only.
func (p *PurgeBodiesOnMetaOnly) Run(ctx context.Context) (PurgeBodiesOnMetaOnlyResult, error) {
	var out PurgeBodiesOnMetaOnlyResult
	if p == nil || p.DB == nil {
		return out, fmt.Errorf("jobs: purge_body_mode requires a database")
	}
	if p.Audit == nil {
		return out, fmt.Errorf("jobs: purge_body_mode requires an audit writer")
	}
	if p.Purger == nil {
		return out, fmt.Errorf("jobs: purge_body_mode requires a purger")
	}

	mode, err := p.bodyMode(ctx)
	if err != nil {
		// Fail safe: an unreadable mode must never be read as "meta_only".
		// Returning the error (instead of deleting nothing quietly) makes the
		// runner report task_failed, so a compliance gap is visible.
		p.warn(ctx, "purge_body_mode_config_unreadable: "+err.Error())
		return out, fmt.Errorf("jobs: purge_body_mode: resolve log.body_mode: %w", err)
	}
	out.Mode = mode
	if mode != config.EnumMetaOnly {
		// opt_in means the product owner asked us to keep bodies. Nothing to do,
		// and nothing to count: the row count is not ours to report here.
		out.Skipped = true
		if p.Logger != nil {
			p.Logger.Info(ctx, "purge_body_mode skipped: log.body_mode="+mode,
				obslog.Fields{Stage: PurgeBodyModeTaskName})
		}
		return out, nil
	}

	exists, err := p.Purger.tableExists(ctx, "t_request_body")
	if err != nil {
		return out, err
	}
	if !exists {
		// Pre-0022 deployment: nothing was ever stored.
		return out, nil
	}

	batch := p.BatchSize
	if batch <= 0 {
		batch = BodyPurgeBatchSize
	}
	deleted, more, batches, err := p.Purger.purge(ctx, purgeAllBodiesSQL, batch)
	out.Deleted, out.Batches, out.Truncated = deleted, batches, more
	if err != nil {
		return out, err
	}

	// Audit only when something was actually removed: a run that deleted 0 rows
	// would otherwise write one row per minute forever, and the trail would
	// stop being read.
	if deleted > 0 {
		if err := p.audit(ctx, deleted); err != nil {
			// The rows are already gone (the batches committed); the audit is
			// the only evidence that this was us and not a bug. Surface it as a
			// task failure rather than a WARN nobody reads.
			p.warn(ctx, "purge_body_mode_audit_failed: "+err.Error())
			return out, fmt.Errorf("jobs: purge_body_mode: audit: %w", err)
		}
	}

	if p.Logger != nil {
		p.Logger.Info(ctx, fmt.Sprintf("purge_body_mode done deleted=%d batches=%d truncated=%t",
			out.Deleted, out.Batches, out.Truncated), obslog.Fields{Stage: PurgeBodyModeTaskName})
	}
	return out, nil
}

// audit writes the one row that makes this deletion attributable.
func (p *PurgeBodiesOnMetaOnly) audit(ctx context.Context, deleted int) error {
	_, after, err := audit.MarshalBeforeAfter(nil, map[string]any{
		"body_mode": config.EnumMetaOnly,
		"deleted":   deleted,
		"table":     "t_request_body",
	})
	if err != nil {
		return err
	}
	return p.Audit.Insert(ctx, p.DB, audit.Entry{
		// 0 / SYSTEM is the convention for every system-initiated write in this
		// codebase (callback.go, refund.go, close_expired_order.go). A mode
		// property change is performed by the operator who edited
		// t_config_item; this task is the system executing its consequence.
		OperatorID:   0,
		OperatorRole: "SYSTEM",
		Module:       PurgeBodyModeAuditModule,
		Action:       PurgeBodyModeAction,
		TargetType:   "t_request_body",
		TargetID:     "t_request_body",
		After:        after,
		Reason:       "BODY_MODE_META_ONLY",
		// The count is the evidence, so it goes in Result rather than a generic
		// "OK". "n=" plus a decimal fits varchar(16) for any plausible table
		// size; the structured copy lives in after_json.
		Result: "n=" + strconv.Itoa(deleted),
	})
}

// bodyMode reads the effective log.body_mode.
func (p *PurgeBodiesOnMetaOnly) bodyMode(ctx context.Context) (string, error) {
	if p.BodyMode != nil {
		return p.BodyMode(ctx)
	}
	return effectiveBodyMode(ctx, p.DB)
}

// effectiveBodyMode resolves log.body_mode from t_config_item through the same
// registry the gateway uses, so an unparsable row falls back to the built-in
// default (meta_only) exactly as it does on the request path. Reading the row
// directly would be a second config implementation with its own fallback rules.
func effectiveBodyMode(ctx context.Context, db *sql.DB) (string, error) {
	store, err := config.NewStoreFromDB(ctx, db)
	if err != nil {
		return "", err
	}
	mode, ok := store.String(config.KeyLogBodyMode)
	if !ok {
		// Cannot happen with the shipped registry (every key has a default),
		// but "no value" must not silently mean meta_only.
		return "", fmt.Errorf("log.body_mode has no effective value")
	}
	return mode, nil
}

func (p *PurgeBodiesOnMetaOnly) warn(ctx context.Context, msg string) {
	if p == nil || p.Logger == nil {
		return
	}
	p.Logger.Warn(ctx, msg, obslog.Fields{Stage: PurgeBodyModeTaskName})
}

package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"yunshu.dev/core/internal/control/audit"
	obslog "yunshu.dev/core/internal/obs/log"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/platform/clock"
)

// CloseExpiredBatchSize bounds one page of expired orders. The row count of an
// unpaid backlog is unbounded, but one transaction per order keeps each lock
// short; the page size only decides how many ids are read at a time.
const CloseExpiredBatchSize = 500

// MaxCloseExpiredPages bounds one run. A page that closes nothing already ends
// the run, so this is the second belt: an implementation bug that keeps
// returning fresh ids must still leave the runner with time to report, instead
// of running until the task timeout kills it.
const MaxCloseExpiredPages = 20

// Failure stages for gw_order_auto_close_failed_total. They are closed-set
// values, not error strings: a stage is how an operator decides whether to look
// at MySQL or at the audit trail.
const (
	closeExpiredStageScan   = "scan"
	closeExpiredStageDB     = "db"
	closeExpiredStageAudit  = "audit"
	autoCloseReasonExpireAt = "expire_at"
)

// AuditInserter is the one method this task needs from the admin audit trail.
//
// It is an interface rather than *audit.Writer so a test can drive the failure
// path (an audit write that keeps failing) without corrupting the shared
// schema; *audit.Writer satisfies it unchanged.
type AuditInserter interface {
	Insert(ctx context.Context, q audit.Execer, e audit.Entry) error
}

// CloseExpiredOrder voids recharge orders whose payment window has passed.
//
// 78a005d added the operator void (fund.CloseOrder, ORDER_CLOSED_BY_OPERATOR);
// it left the finance queue dependent on a human noticing each stale order. A
// customer who never transfers leaves a PENDING row forever, so the queue only
// ever grew. This task is the automatic half: expire_at has passed, nobody
// paid, the order becomes CLOSED.
//
// It deliberately writes nothing but the state and one audit row: an unpaid
// order never held money, so there is no ledger entry, no batch and no balance
// to touch — the same shape as the operator path, with a different actor.
type CloseExpiredOrder struct {
	DB     *sql.DB
	Audit  AuditInserter
	Logger *obslog.Logger
	Clock  clock.Clock
	// Metrics is optional; the counters are registered by jobsd.
	Metrics   *obsmetrics.Recorder
	BatchSize int
}

// CloseExpiredOrderResult is one run.
//
// Closed and Skipped are different outcomes and are reported separately: a
// skip means "not ours" (a callback paid the order first, or it was already
// closed), a failure means "we tried and could not". Collapsing both into one
// counter is how a database or audit problem used to look like a quiet night.
type CloseExpiredOrderResult struct {
	Scanned int
	Closed  int
	// Skipped counts orders the CAS found no longer PENDING.
	Skipped int
	// Failed counts orders this run could not close because of a real error.
	Failed int
	// Pages is the number of scan batches read.
	Pages int
}

// NewCloseExpiredOrder builds the task. auditWriter is required: a void that
// nobody can attribute is not worth keeping, so Run refuses to start without
// it rather than close orders silently.
func NewCloseExpiredOrder(db *sql.DB, auditWriter AuditInserter, logger *obslog.Logger, clk clock.Clock) *CloseExpiredOrder {
	if clk == nil {
		clk = clock.Real{}
	}
	return &CloseExpiredOrder{
		DB: db, Audit: auditWriter, Logger: logger, Clock: clk,
		BatchSize: CloseExpiredBatchSize,
	}
}

// Run closes every expired PENDING order, in pages.
//
// It returns an error when at least one order failed for a real reason, so the
// runner reports task_failed and the alert fires; a run that only skipped
// orders the CAS refused returns nil. The error is returned after the page
// loop, so one bad order cannot stop the rest of the backlog.
func (c *CloseExpiredOrder) Run(ctx context.Context) (CloseExpiredOrderResult, error) {
	var out CloseExpiredOrderResult
	if c == nil || c.DB == nil {
		return out, fmt.Errorf("jobs: close_expired_order requires a database")
	}
	if c.Audit == nil {
		return out, fmt.Errorf("jobs: close_expired_order requires an audit writer")
	}
	if c.BatchSize <= 0 {
		c.BatchSize = CloseExpiredBatchSize
	}

	var firstErr error
	for out.Pages < MaxCloseExpiredPages {
		ids, err := c.scan(ctx)
		if err != nil {
			c.countFailure(closeExpiredStageScan)
			out.Failed++
			return out, err
		}
		out.Pages++
		out.Scanned += len(ids)
		if len(ids) == 0 {
			break
		}

		pageClosed, pageFailed := 0, 0
		for _, id := range ids {
			closed, err := c.closeOne(ctx, id)
			if err != nil {
				// One bad order must not stop the rest: the next run is
				// idempotent and a row that failed here is still eligible.
				if c.Logger != nil {
					c.Logger.Error(ctx, fmt.Sprintf("close_expired_order_failed order_id=%d: %v", id, err),
						obslog.Fields{Stage: "close_expired_order"})
				}
				if firstErr == nil {
					firstErr = err
				}
				pageFailed++
				continue
			}
			if !closed {
				// A callback paid the order between the scan and the CAS, or
				// the order is already CLOSED. Either way it is not ours, and
				// it is not a failure.
				out.Skipped++
				continue
			}
			pageClosed++
			out.Closed++
			if c.Metrics != nil {
				c.Metrics.OrderAutoClosed(autoCloseReasonExpireAt)
			}
		}
		out.Failed += pageFailed

		// No order on this page could be closed, but the ids were eligible a
		// moment ago: the next scan would hand back the same rows and spin
		// until the task timeout. Stop and let the error (or the log) surface.
		if pageClosed == 0 {
			if c.Logger != nil && pageFailed > 0 {
				c.Logger.Warn(ctx, fmt.Sprintf(
					"close_expired_order no_progress page=%d failed=%d skipped=%d: stopping early instead of re-reading the same page",
					out.Pages, pageFailed, out.Skipped), obslog.Fields{Stage: "close_expired_order"})
			}
			break
		}
		// The closed rows drop out of the scan predicate, so a full page means
		// there may be more; a short page means the queue is empty.
		if len(ids) < c.BatchSize {
			break
		}
	}

	if c.Logger != nil {
		c.Logger.Info(ctx, fmt.Sprintf("close_expired_order done closed=%d failed=%d skipped=%d pages=%d scanned=%d",
			out.Closed, out.Failed, out.Skipped, out.Pages, out.Scanned),
			obslog.Fields{Stage: "close_expired_order"})
	}
	if out.Failed > 0 {
		return out, fmt.Errorf("jobs: close_expired_order closed=%d failed=%d: %w",
			out.Closed, out.Failed, firstErr)
	}
	return out, nil
}

// countFailure records one failed unit. stage is the closed-set label an
// operator uses to pick the next place to look.
func (c *CloseExpiredOrder) countFailure(stage string) {
	if c == nil || c.Metrics == nil {
		return
	}
	c.Metrics.OrderAutoCloseFailed(stage)
}

// scan lists expired unpaid orders. The predicate matches idx_ro_expire
// (state, expire_at), which the DDL created for exactly this task.
func (c *CloseExpiredOrder) scan(ctx context.Context) ([]int64, error) {
	rows, err := c.DB.QueryContext(ctx,
		`SELECT id FROM t_recharge_order
		  WHERE state = 'PENDING'
		    AND expire_at IS NOT NULL
		    AND expire_at <= UTC_TIMESTAMP(3)
		  ORDER BY expire_at ASC
		  LIMIT ?`, c.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("jobs: scan expired recharge orders: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("jobs: scan expired recharge orders: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: scan expired recharge orders: %w", err)
	}
	return ids, nil
}

// closeOne returns true when this call moved the order to CLOSED.
//
// The CAS is the whole safety story: it re-checks state = 'PENDING' inside the
// update, so an order the channel callback paid a millisecond earlier (state
// now PAID) is left alone — voiding it would orphan a credit that really
// exists. RowsAffected == 0 therefore means "not ours", never an error.
func (c *CloseExpiredOrder) closeOne(ctx context.Context, orderID int64) (bool, error) {
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		c.countFailure(closeExpiredStageDB)
		return false, fmt.Errorf("jobs: begin close expired order %d: %w", orderID, err)
	}
	defer tx.Rollback() //nolint:errcheck // commit decides

	now := c.now()
	res, err := tx.ExecContext(ctx,
		`UPDATE t_recharge_order
		    SET state = 'CLOSED', updated_at = ?
		  WHERE id = ? AND state = 'PENDING'`, now, orderID)
	if err != nil {
		c.countFailure(closeExpiredStageDB)
		return false, fmt.Errorf("jobs: close expired order %d: %w", orderID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		c.countFailure(closeExpiredStageDB)
		return false, fmt.Errorf("jobs: close expired order %d rows: %w", orderID, err)
	}
	if affected != 1 {
		return false, nil
	}

	// Read the payload back inside the same transaction: the row is locked by
	// the update above, so these values are the ones the customer saw.
	var (
		orderNo  string
		userID   int64
		expireAt time.Time
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT order_no, user_id, expire_at FROM t_recharge_order WHERE id = ?`, orderID).
		Scan(&orderNo, &userID, &expireAt); err != nil {
		c.countFailure(closeExpiredStageDB)
		return false, fmt.Errorf("jobs: read closed order %d: %w", orderID, err)
	}

	before, after, jerr := audit.MarshalBeforeAfter(
		map[string]any{"state": "PENDING"},
		map[string]any{
			"state":          "CLOSED",
			"previous_state": "PENDING",
			"order_no":       orderNo,
			"user_id":        userID,
			"expire_at":      expireAt.UTC().Format(time.RFC3339),
			"reason":         autoCloseReasonExpireAt,
		})
	if jerr != nil {
		c.countFailure(closeExpiredStageAudit)
		return false, fmt.Errorf("jobs: audit payload order %d: %w", orderID, jerr)
	}

	if aerr := c.Audit.Insert(ctx, tx, audit.Entry{
		// 0 / SYSTEM is how every system-initiated write in this codebase
		// identifies itself (callback.go, refund.go, quota_apply.go); the
		// column is NOT NULL, and a system void has no operator to name.
		OperatorID:   0,
		OperatorRole: "SYSTEM",
		Module:       "FUND",
		Action:       "ORDER_CLOSED_BY_SYSTEM",
		TargetType:   "t_recharge_order",
		TargetID:     strconv.FormatInt(orderID, 10),
		Before:       before,
		After:        after,
		Reason:       "EXPIRED_UNPAID",
		Result:       "OK",
	}); aerr != nil {
		// Same rule as the operator path: a void nobody can audit must not
		// commit.
		c.countFailure(closeExpiredStageAudit)
		return false, fmt.Errorf("jobs: audit closed order %d: %w", orderID, aerr)
	}

	if err := tx.Commit(); err != nil {
		c.countFailure(closeExpiredStageDB)
		return false, fmt.Errorf("jobs: commit close expired order %d: %w", orderID, err)
	}
	return true, nil
}

func (c *CloseExpiredOrder) now() time.Time {
	if c == nil || c.Clock == nil {
		return time.Now().UTC()
	}
	return c.Clock.Now().UTC()
}

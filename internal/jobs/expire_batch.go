package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"yunshu.dev/core/internal/ledger"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// Expire defaults.
const (
	ExpireBatchSize = 500
)

// ExpireLocker is the K22 surface from internal/lock.
type ExpireLocker interface {
	AcquireExpireBatch(ctx context.Context, owner string) (bool, error)
	RenewExpireBatch(ctx context.Context, owner string) (bool, error)
}

// ExpireBatch clears expired non-refundable ("bonus family") batches.
// RECHARGE batches have no expiry and are never selected; the two validity
// rules are deliberately separate (T09 hard constraint 16 / PRD §3.2.1).
//
// The clearing is a DEBIT ledger entry plus remaining_micro=0. consumed_micro
// is increased by the same amount so R-04
// (remaining == face - consumed) still holds after expiry.
type ExpireBatch struct {
	DB        *sql.DB
	Locker    ExpireLocker
	Logger    *obslog.Logger
	Clock     clock.Clock
	Owner     string
	BatchSize int
	Entries   *ledger.EntryNoGenerator
}

// ExpireResult is one run.
type ExpireResult struct {
	Locked       bool
	Scanned      int
	Cleared      int
	ClearedMicro int64
	Skipped      int
}

// NewExpireBatch builds the task.
func NewExpireBatch(db *sql.DB, locker ExpireLocker, logger *obslog.Logger, clk clock.Clock, owner string) *ExpireBatch {
	if clk == nil {
		clk = clock.Real{}
	}
	return &ExpireBatch{
		DB: db, Locker: locker, Logger: logger, Clock: clk, Owner: owner,
		BatchSize: ExpireBatchSize, Entries: ledger.NewEntryNoGenerator(),
	}
}

// Run takes K22 and clears every expired BONUS batch, in pages.
func (e *ExpireBatch) Run(ctx context.Context) (ExpireResult, error) {
	var out ExpireResult
	if e == nil || e.DB == nil {
		return out, fmt.Errorf("jobs: expire_batch requires a database")
	}
	if e.Locker != nil {
		ok, err := e.Locker.AcquireExpireBatch(ctx, e.Owner)
		if err != nil {
			return out, fmt.Errorf("jobs: expire_batch K22: %w", err)
		}
		if !ok {
			return out, nil
		}
		out.Locked = true
	}
	if e.BatchSize <= 0 {
		e.BatchSize = ExpireBatchSize
	}
	for {
		ids, err := e.scan(ctx)
		if err != nil {
			return out, err
		}
		out.Scanned += len(ids)
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			cleared, err := e.clearOne(ctx, id)
			if err != nil {
				// One bad batch must not stop the rest: the next run is
				// idempotent and the failed row remains eligible.
				if e.Logger != nil {
					e.Logger.Error(ctx, fmt.Sprintf("expire_batch_failed batch_id=%d: %v", id, err),
						obslog.Fields{Stage: "expire_batch"})
				}
				out.Skipped++
				continue
			}
			if cleared == 0 {
				out.Skipped++
				continue
			}
			out.Cleared++
			out.ClearedMicro += cleared
		}
		if e.Locker != nil {
			// K22 has a 300s TTL; a large backlog must keep the guard alive.
			if ok, err := e.Locker.RenewExpireBatch(ctx, e.Owner); err == nil && !ok {
				return out, fmt.Errorf("jobs: expire_batch lost K22")
			}
		}
		if len(ids) < e.BatchSize {
			break
		}
	}
	if e.Logger != nil {
		e.Logger.Info(ctx, fmt.Sprintf("expire_batch done cleared=%d micro=%d skipped=%d",
			out.Cleared, out.ClearedMicro, out.Skipped), obslog.Fields{Stage: "expire_batch"})
	}
	return out, nil
}

// expiringBatchTypes is the explicit allow-list of batch families that carry an
// expire_at. Every one of them is a non-refundable credit that lands in
// t_credit_account.balance_bonus_micro, so all of them must be cleared on
// expiry: skipping them would leave the account balance holding credits that
// deduct.go refuses to spend (it filters expire_at > now) while admission still
// counts them, and every settlement would then fail with
// ErrInsufficientBatches.
//
// It is an allow-list on purpose. `batch_type <> 'RECHARGE'` would look
// equivalent today and become wrong the moment a new permanent batch type is
// added: the default has to be "keep the user's credits", so a type must opt
// in to being cleared.
func expiringBatchTypes() []string {
	return []string{
		ledger.BatchTypeBonus,
		ledger.BatchTypeRedeem,
		ledger.BatchTypeCompensation,
		ledger.BatchTypePlanGrant,
	}
}

// expiringBatchTypeFilter renders the allow-list as an IN (...) clause plus its
// bind arguments. scan and clearOne must agree on it: a row selected by scan
// but rejected by clearOne would never be cleared, and the paging loop in Run
// would rescan it forever.
func expiringBatchTypeFilter() (string, []any) {
	types := expiringBatchTypes()
	clause := "("
	args := make([]any, 0, len(types))
	for i, t := range types {
		if i > 0 {
			clause += ","
		}
		clause += "?"
		args = append(args, t)
	}
	return clause + ")", args
}

func (e *ExpireBatch) scan(ctx context.Context) ([]int64, error) {
	typeClause, typeArgs := expiringBatchTypeFilter()
	q := `SELECT id FROM t_credit_batch
	       WHERE batch_type IN ` + typeClause + `
	         AND expire_at IS NOT NULL
	         AND expire_at <= UTC_TIMESTAMP(3)
	         AND remaining_micro > 0
	       ORDER BY expire_at ASC
	       LIMIT ?`
	args := make([]any, 0, len(typeArgs)+1)
	args = append(args, typeArgs...)
	args = append(args, e.BatchSize)
	rows, err := e.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("jobs: scan expired batches: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// clearOne returns the cleared amount, or 0 when the row is no longer eligible.
func (e *ExpireBatch) clearOne(ctx context.Context, batchID int64) (int64, error) {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("jobs: begin expire batch %d: %w", batchID, err)
	}
	defer tx.Rollback() //nolint:errcheck // commit decides

	typeClause, typeArgs := expiringBatchTypeFilter()
	lockQ := `SELECT user_id, remaining_micro
	            FROM t_credit_batch
	           WHERE id = ? AND batch_type IN ` + typeClause + `
	             AND expire_at IS NOT NULL AND expire_at <= UTC_TIMESTAMP(3)
	             AND remaining_micro > 0
	           FOR UPDATE`
	lockArgs := make([]any, 0, len(typeArgs)+1)
	lockArgs = append(lockArgs, batchID)
	lockArgs = append(lockArgs, typeArgs...)
	var userID, remaining int64
	err = tx.QueryRowContext(ctx, lockQ, lockArgs...).Scan(&userID, &remaining)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("jobs: lock batch %d: %w", batchID, err)
	}

	acct, err := ledger.LockAccount(ctx, tx, userID)
	if err != nil {
		return 0, err
	}
	before := acct.TotalMicro()
	now := e.now()
	batch := batchID
	// "BONUS_EXPIRED" is the historical reason string and stays unchanged: the
	// clear debits balance_bonus_micro for every one of expiringBatchTypes, so
	// the value keeps naming the account column rather than the batch family.
	// The exact batch (and therefore its batch_type) is in batch_id.
	reason := "BONUS_EXPIRED"
	entryID, err := ledger.AppendEntry(ctx, tx, e.Entries, now, ledger.Entry{
		UserID:              userID,
		Direction:           ledger.DirectionDebit,
		BizType:             ledger.BizTypeBatchExpire,
		AmountBonusMicro:    -remaining,
		AmountRechargeMicro: 0,
		AmountTotalMicro:    -remaining,
		BalanceBeforeMicro:  before,
		BalanceAfterMicro:   before - remaining,
		BatchID:             &batch,
		IdemKey:             fmt.Sprintf("expire:%d", batchID),
		Reason:              &reason,
		CreatedAt:           now,
	})
	if err != nil {
		if errors.Is(err, ledger.ErrDuplicateIdem) {
			return 0, nil
		}
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE t_credit_batch
		    SET remaining_micro = 0,
		        consumed_micro = consumed_micro + ?,
		        cleared_at = ?,
		        updated_at = ?
		  WHERE id = ? AND remaining_micro = ?`,
		remaining, now, now, batchID, remaining,
	)
	if err != nil {
		return 0, fmt.Errorf("jobs: clear batch %d: %w", batchID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, fmt.Errorf("jobs: batch %d CAS conflict", batchID)
	}
	if err := acct.Apply(ctx, tx, ledger.BalanceDelta{BonusMicro: -remaining}, entryID, nil, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("jobs: commit expire batch %d: %w", batchID, err)
	}
	return remaining, nil
}

func (e *ExpireBatch) now() time.Time {
	if e == nil || e.Clock == nil {
		return time.Now().UTC()
	}
	return e.Clock.Now().UTC()
}

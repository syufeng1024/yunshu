package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"yunshu.dev/core/internal/platform/clock"
)

type stubExpireLocker struct {
	ok    bool
	calls int
}

func (s *stubExpireLocker) AcquireExpireBatch(context.Context, string) (bool, error) {
	s.calls++
	return s.ok, nil
}

func (s *stubExpireLocker) RenewExpireBatch(context.Context, string) (bool, error) {
	return s.ok, nil
}

// A10: only expired BONUS batches with a remaining balance are cleared. RECHARGE
// batches have no expiry and must never be selected.
func TestExpireBatchClearsOnlyExpiredBonus(t *testing.T) {
	ctx := context.Background()
	const user = int64(940001)
	cleanupExpireUser(t, user)
	t.Cleanup(func() { cleanupExpireUser(t, user) })

	now := time.Now().UTC()
	if _, err := testDB.Exec(
		`INSERT INTO t_credit_account
		   (user_id, balance_recharge_micro, balance_bonus_micro, balance_micro,
		    overdraft_limit_micro, version, created_at, updated_at)
		 VALUES (?,2000,1500,3500,500000,0,?,?)`, user, now, now); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	past := now.Add(-24 * time.Hour)
	future := now.Add(24 * time.Hour)
	batch := func(no, typ string, face, remaining, consumed int64, expire time.Time, refundable int) int64 {
		t.Helper()
		res, err := testDB.Exec(
			`INSERT INTO t_credit_batch
			   (user_id, batch_no, batch_type, face_value_micro, remaining_micro,
			    consumed_micro, refundable, effective_at, expire_at, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			user, no, typ, face, remaining, consumed, refundable, past, expire, now, now)
		if err != nil {
			t.Fatalf("insert batch %s: %v", no, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("batch id: %v", err)
		}
		return id
	}
	bonusID := batch("20261020000000000001", "BONUS", 1000, 1000, 0, past, 0)
	rechargeID := batch("20261020000000000002", "RECHARGE", 2000, 2000, 0, past, 1)
	zeroID := batch("20261020000000000003", "BONUS", 500, 0, 500, past, 0)
	futureID := batch("20261020000000000004", "BONUS", 500, 500, 0, future, 0)

	// K22 is taken before any row is read: a replica that does not own the lock
	// must not clear anything.
	blocked := &stubExpireLocker{ok: false}
	if res, err := NewExpireBatch(testDB, blocked, nil, clock.Real{}, "n1").Run(ctx); err != nil {
		t.Fatalf("blocked run: %v", err)
	} else if res.Locked || res.Cleared != 0 {
		t.Fatalf("blocked run = %+v; want locked=false cleared=0", res)
	}
	if remaining := batchRemaining(t, bonusID); remaining != 1000 {
		t.Fatalf("blocked run changed bonus remaining to %d", remaining)
	}

	locker := &stubExpireLocker{ok: true}
	task := NewExpireBatch(testDB, locker, nil, clock.Real{}, "n1")
	res, err := task.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Locked || res.Cleared != 1 || res.ClearedMicro != 1000 {
		t.Fatalf("run = %+v; want locked cleared=1 micro=1000", res)
	}

	var remaining, consumed int64
	var clearedAt sql.NullTime
	if err := testDB.QueryRow(
		`SELECT remaining_micro, consumed_micro, cleared_at FROM t_credit_batch WHERE id = ?`, bonusID,
	).Scan(&remaining, &consumed, &clearedAt); err != nil {
		t.Fatalf("read bonus batch: %v", err)
	}
	if remaining != 0 || consumed != 1000 || !clearedAt.Valid {
		t.Fatalf("bonus batch remaining=%d consumed=%d cleared=%v; want 0/1000/set",
			remaining, consumed, clearedAt.Valid)
	}

	if got := batchRemaining(t, rechargeID); got != 2000 {
		t.Fatalf("RECHARGE remaining = %d; want 2000 (never expires)", got)
	}
	if got := batchRemaining(t, zeroID); got != 0 {
		t.Fatalf("zero-remaining BONUS = %d; want 0", got)
	}
	if got := batchRemaining(t, futureID); got != 500 {
		t.Fatalf("future BONUS = %d; want 500", got)
	}

	var bizType, direction, idem string
	var amount int64
	if err := testDB.QueryRow(
		`SELECT biz_type, direction, amount_total_micro, idem_key
		   FROM t_ledger_entry WHERE idem_key = ?`, fmt.Sprintf("expire:%d", bonusID),
	).Scan(&bizType, &direction, &amount, &idem); err != nil {
		t.Fatalf("read expire entry: %v", err)
	}
	if bizType != "BATCH_EXPIRE" || direction != "DEBIT" || amount != -1000 {
		t.Fatalf("expire entry = %s/%s/%d; want BATCH_EXPIRE/DEBIT/-1000", bizType, direction, amount)
	}

	var bonus, balance int64
	if err := testDB.QueryRow(
		`SELECT balance_bonus_micro, balance_micro FROM t_credit_account WHERE user_id = ?`, user,
	).Scan(&bonus, &balance); err != nil {
		t.Fatalf("read account: %v", err)
	}
	// Account bonus was 1500: expired 1000 cleared, future 500 remains.
	if bonus != 500 || balance != 2500 {
		t.Fatalf("account bonus=%d balance=%d; want 500/2500", bonus, balance)
	}

	var broken int
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM t_credit_batch
		  WHERE user_id = ? AND remaining_micro <> face_value_micro - consumed_micro`, user,
	).Scan(&broken); err != nil {
		t.Fatalf("R04 check: %v", err)
	}
	if broken != 0 {
		t.Fatalf("R-04 broken rows = %d after expiry", broken)
	}

	if res, err := task.Run(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	} else if res.Cleared != 0 {
		t.Fatalf("second run cleared = %d; want 0", res.Cleared)
	}
	t.Logf("A10 cleared=%d micro=%d entry=%s/%d account_bonus=%d R04_broken=%d",
		res.Cleared, res.ClearedMicro, bizType, amount, bonus, broken)
}

// B3: every bonus-family batch type expires, not just BONUS. REDEEM,
// COMPENSATION and PLAN_GRANT all carry an expire_at (grant.go, adjust.go,
// redeem.go) and all land in balance_bonus_micro, but only BONUS was scanned.
// Their remaining balance therefore stayed in the account while deduct.go
// refused to spend it — admission passed and settlement failed with
// ErrInsufficientBatches.
//
// The trailing RECHARGE row with a past expire_at is deliberate: it must never
// be selected, because its validity rule is not time-based.
func TestExpireBatchClearsEveryExpiringBatchType(t *testing.T) {
	ctx := context.Background()
	const user = int64(940002)
	cleanupExpireUser(t, user)
	t.Cleanup(func() { cleanupExpireUser(t, user) })

	now := time.Now().UTC()
	past := now.Add(-24 * time.Hour)
	future := now.Add(24 * time.Hour)
	// bonus = 700 + 800 + 900 (expired) + 400 (still valid) = 2800.
	if _, err := testDB.Exec(
		`INSERT INTO t_credit_account
		   (user_id, balance_recharge_micro, balance_bonus_micro, balance_micro,
		    overdraft_limit_micro, version, created_at, updated_at)
		 VALUES (?,600,2800,3400,500000,0,?,?)`, user, now, now); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	batch := func(no, typ string, face, remaining, consumed int64, expire time.Time, refundable int) int64 {
		t.Helper()
		res, err := testDB.Exec(
			`INSERT INTO t_credit_batch
			   (user_id, batch_no, batch_type, face_value_micro, remaining_micro,
			    consumed_micro, refundable, effective_at, expire_at, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			user, no, typ, face, remaining, consumed, refundable, past, expire, now, now)
		if err != nil {
			t.Fatalf("insert batch %s: %v", no, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("batch id: %v", err)
		}
		return id
	}

	redeemID := batch("20261020000000000011", "REDEEM", 700, 700, 0, past, 0)
	compID := batch("20261020000000000012", "COMPENSATION", 800, 800, 0, past, 0)
	planID := batch("20261020000000000013", "PLAN_GRANT", 900, 900, 0, past, 0)
	rechargeID := batch("20261020000000000014", "RECHARGE", 600, 600, 0, past, 1)
	futureID := batch("20261020000000000015", "REDEEM", 400, 400, 0, future, 0)

	// BatchSize 2 forces the paging loop to run twice over three eligible rows:
	// a row that scan selects but clearOne rejects would spin there forever.
	task := NewExpireBatch(testDB, &stubExpireLocker{ok: true}, nil, clock.Real{}, "n1")
	task.BatchSize = 2
	res, err := task.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Cleared != 3 || res.ClearedMicro != 2400 || res.Skipped != 0 {
		t.Fatalf("run = %+v; want cleared=3 micro=2400 skipped=0", res)
	}
	for name, id := range map[string]int64{
		"REDEEM": redeemID, "COMPENSATION": compID, "PLAN_GRANT": planID,
	} {
		if got := batchRemaining(t, id); got != 0 {
			t.Fatalf("%s remaining = %d; want 0", name, got)
		}
	}
	if got := batchRemaining(t, rechargeID); got != 600 {
		t.Fatalf("RECHARGE remaining = %d; want 600 (never expires)", got)
	}
	if got := batchRemaining(t, futureID); got != 400 {
		t.Fatalf("future REDEEM remaining = %d; want 400", got)
	}

	var bonus, recharge, balance int64
	if err := testDB.QueryRow(
		`SELECT balance_bonus_micro, balance_recharge_micro, balance_micro
		   FROM t_credit_account WHERE user_id = ?`, user,
	).Scan(&bonus, &recharge, &balance); err != nil {
		t.Fatalf("read account: %v", err)
	}
	if bonus != 400 || recharge != 600 || balance != 1000 {
		t.Fatalf("account bonus=%d recharge=%d balance=%d; want 400/600/1000", bonus, recharge, balance)
	}

	var broken int
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM t_credit_batch
		  WHERE user_id = ? AND remaining_micro <> face_value_micro - consumed_micro`, user,
	).Scan(&broken); err != nil {
		t.Fatalf("R04 check: %v", err)
	}
	if broken != 0 {
		t.Fatalf("R-04 broken rows = %d after expiry", broken)
	}

	if res, err := task.Run(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	} else if res.Cleared != 0 {
		t.Fatalf("second run cleared = %d; want 0", res.Cleared)
	}
	t.Logf("B3 cleared=%d micro=%d account_bonus=%d replenished_batches=3", res.Cleared, res.ClearedMicro, bonus)
}

func batchRemaining(t *testing.T, id int64) int64 {
	t.Helper()
	var n int64
	if err := testDB.QueryRow(`SELECT remaining_micro FROM t_credit_batch WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("read batch %d: %v", id, err)
	}
	return n
}

func cleanupExpireUser(t *testing.T, user int64) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM t_credit_batch WHERE user_id = ?`,
		`DELETE FROM t_ledger_entry WHERE user_id = ?`,
		`DELETE FROM t_credit_account WHERE user_id = ?`,
	} {
		if _, err := testDB.ExecContext(ctx, q, user); err != nil {
			t.Fatalf("cleanup expire user: %v\nquery: %s", err, q)
		}
	}
}

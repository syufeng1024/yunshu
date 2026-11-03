package ctltest

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PlanGrantLock is the MySQL advisory lock that serialises the plan-grant write
// path (`fund.applyPlanTx`) across test binaries.
//
// Why it is needed. applyPlanTx reads the account's current rate-limit policy
// with `SELECT ... FOR UPDATE` and then inserts the new one. For a brand-new
// account the read matches nothing, and InnoDB takes a next-key lock on
// `uk_rlp_scope_active (scope_type, scope_id, effective_from)` that spans to the
// index supremum; the following INSERT then waits for an insert-intention lock
// on that same supremum. Two grants running at the same time — even for two
// different accounts, because the locked range is "everything above the last
// USER row" — therefore deadlock:
//
//	TRANSACTION (1) holds X on supremum of uk_rlp_scope_active,
//	                waits for the record of scope_id 910609
//	TRANSACTION (2) `INSERT INTO t_rate_limit_policy ... 'USER', 910610, 'TRIAL'`
//	                holds X on supremum, waits for insert intention
//	*** WE ROLL BACK TRANSACTION (2)
//
// (SHOW ENGINE INNODB STATUS, reproduced while running `go test ./...`.)
//
// In production the two grants belong to two different requests and the loser
// gets a retryable 1213. In the test suite they belong to two different package
// binaries — internal/acceptance (G-09) and internal/control/fund — that share
// one yunshu_test schema and run concurrently under `go test ./...`. The test
// asserts the grant contract, not MySQL's deadlock victim selection, so the
// exercisers take this lock instead of hoping the two never overlap.
//
// The lock is an advisory (GET_LOCK) lock: it is connection scoped, so the
// returned release function must be called on the same goroutine's flow that
// acquired it (TestMain deferring it is the intended use).
const PlanGrantLock = "yunshu_test_plan_grant"

// LockNamed takes a MySQL advisory lock for the caller and returns the release
// function. Unlike redistest.AcquireLock (a filesystem lock that guards one
// Redis logical database) this one lives in MySQL, which is the resource the
// plan-grant path contends on.
func LockNamed(ctx context.Context, db *sql.DB, name string, timeout time.Duration) (func(), error) {
	if db == nil {
		return nil, fmt.Errorf("ctltest: lock %q: no database", name)
	}
	if name == "" {
		return nil, fmt.Errorf("ctltest: lock: empty name")
	}
	seconds := int(timeout / time.Second)
	if seconds <= 0 {
		seconds = 1
	}
	// GET_LOCK is scoped to the session, so the connection must stay open for
	// as long as the lock is held: take one connection out of the pool and
	// return it only on release.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("ctltest: lock %q: take connection: %w", name, err)
	}
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`, name, seconds).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ctltest: lock %q: GET_LOCK: %w", name, err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		_ = conn.Close()
		return nil, fmt.Errorf("ctltest: lock %q: not acquired within %s; another test binary is holding it", name, timeout)
	}
	// The release must run even after the caller's context is done (TestMain
	// cancels the acquisition context as soon as it has the lock), so it derives
	// from the acquisition context with the cancellation detached.
	base := context.WithoutCancel(ctx)
	release := func() {
		rctx, cancel := context.WithTimeout(base, 10*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(rctx, `SELECT RELEASE_LOCK(?)`, name)
		_ = conn.Close()
	}
	return release, nil
}

// LockPlanGrant takes PlanGrantLock. The caller must release it (defer).
func LockPlanGrant(ctx context.Context, db *sql.DB, timeout time.Duration) (func(), error) {
	return LockNamed(ctx, db, PlanGrantLock, timeout)
}

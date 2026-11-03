package fund

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// G33: two *different* accounts buying at the same instant used to deadlock on
// uk_rlp_scope_active. The policy read in applyPlanTx carried a FOR UPDATE, and
// under REPEATABLE READ a locking read that matches no row (every fresh user)
// takes a gap lock all the way to supremum; two such transactions then hold the
// same gap and wait for each other's insert intention. InnoDB rolled one back
// with 1213 and that buyer saw a 500.
//
// The lock was redundant: applyPlanTx locks the t_user row FOR UPDATE before
// touching the policy, so grants for the same user are already serialised, and
// policy rows of different users cannot conflict. Removing it — not adding a
// retry — is the fix this test pins.
func TestConcurrentPlanGrantDifferentUsersDoNotDeadlock(t *testing.T) {
	svc, _ := newTestService(t)
	// newTestService passes a nil Logger, which would swallow the reason behind
	// the internal_error the assertion sees.
	svc.Logger = obslog.NewWithWriter("fund", "test", os.Stderr, clock.Real{})
	planID := planIDByCode(t, "PRO")

	// 40 rounds of the exact interleaving. One round can get lucky; the bug was
	// reported at roughly 1–2 failures per 100 runs, so the loop is the test.
	const rounds = 40
	for round := 0; round < rounds; round++ {
		uidA := newTestUser(t, nil)
		uidB := newTestUser(t, nil)

		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		for i, uid := range []int64{uidA, uidB} {
			go func(idx int, userID int64) {
				defer wg.Done()
				ctx := context.Background()
				tx, err := svc.DB.BeginTx(ctx, nil)
				if err != nil {
					errs[idx] = err
					return
				}
				defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit
				if _, apiErr := svc.applyPlanTx(ctx, tx, userID, planID, time.Now()); apiErr != nil {
					errs[idx] = apiErr
					return
				}
				errs[idx] = tx.Commit()
			}(i, uid)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, grant %d: %v", round, i, err)
			}
		}

		// Both users must actually hold the plan: "no deadlock" that also
		// granted nothing would pass the loop for the wrong reason.
		for i, uid := range []int64{uidA, uidB} {
			var planIDNow int64
			if err := testDB.QueryRowContext(context.Background(),
				`SELECT plan_id FROM t_user WHERE id = ?`, uid).Scan(&planIDNow); err != nil {
				t.Fatalf("round %d, read back user %d: %v", round, i, err)
			}
			if planIDNow != planID {
				t.Fatalf("round %d, user %d plan_id=%d, want %d", round, uid, planIDNow, planID)
			}
		}
	}
}

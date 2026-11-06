package recon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
	"yunshu.dev/core/internal/settle"
)

// B2: a DEAD settle task must not short-circuit hanging recovery.
//
// A task that exhausted its retries keeps its row, so counting
// t_settle_task without a state filter made classifyHanging answer "a
// settlement task exists, nothing to do" forever: the expired HELD prehold was
// never released and the user's credits stayed locked. After the fix the row
// reaches the classifier again.
//
// Both halves of the consequence are pinned here:
//   - all-attempts-failed (B): the hold is released, which is the shape that
//     was purely broken before;
//   - upstream-succeeded (C): the hold is NOT released (releasing a consumed
//     request would forgive the charge) and the operator is alerted instead.
func TestHangingIgnoresDeadSettleTask(t *testing.T) {
	ctx := context.Background()
	rdb := redistest.NewClient(0)
	t.Cleanup(func() { _ = rdb.Close() })

	store := ledger.NewStore(testDB, clock.Real{})
	h := NewHanging(testDB, rdb, store, settle.NewEnqueuer(testDB, clock.Real{}), nil, clock.Real{})
	h.Cost = func(context.Context, int64, int64, int64, time.Time) (int64, error) {
		return 5000, nil
	}
	h.UserLo, h.UserHi = 900020, 900030

	const (
		userB = int64(900021) // DEAD task, every upstream attempt failed
		userC = int64(900022) // DEAD task, the upstream attempt succeeded
		hold  = int64(120000)
	)
	rb, rc := rid("dthB", 2), rid("dthC", 2)
	expired := time.Now().UTC().Add(-10 * time.Minute)

	// Notify is idempotent per (event_code, body) and the body is derived from
	// fixture values, so a row left by an interrupted earlier run would mask
	// this run's insert. Clear the channel first.
	_, _ = testDB.Exec(`DELETE FROM t_notification WHERE event_code = 'RECONCILE_MANUAL'`)

	for i, u := range []int64{userB, userC} {
		deleteAccount(t, u)
		// The prehold already debited the account, so the balance is -hold and
		// the prehold ledger row exists for the A8 release guard.
		insertAccount(t, u, -hold, 0)
		req := []string{rb, rc}[i]
		insertLedgerEntry(t, u, entryNo("HD", i+1), ledger.BizTypeConsumePrehold,
			ledger.DirectionDebit, 0, -hold, -hold, 0, -hold, "prehold:"+req, &req)
		execFixture(t,
			`INSERT INTO t_prehold_hold
			   (request_id, user_id, api_key_id, hold_micro, estimated_micro, model_id,
			    state, expires_at, node_id, created_at)
			 VALUES (?,?,1,?,?,910001,'HELD',?, 'test-node', ?)`,
			req, u, hold, hold, expired, expired)
		insertDeadSettleTask(t, req, u, expired)
	}
	t.Cleanup(func() {
		for _, u := range []int64{userB, userC} {
			deleteAccount(t, u)
		}
		_, _ = testDB.Exec(`DELETE FROM t_notification WHERE event_code = 'RECONCILE_MANUAL'`)
		_ = rdb.Raw().Del(context.Background(),
			redisx.HoldRecoverKey(rb), redisx.HoldRecoverKey(rc)).Err()
	})

	// B: every attempt failed, so nothing was delivered and the hold must go.
	insertAttempt(t, rb, userB, 910001, 910002, 1, "CONNECT", "FAILED", "HTTP_5XX", expired)
	insertUsage(t, rb, userB, expired, "FAILED", 0)

	// C: the attempt succeeded, so the request was served and the money is owed.
	insertAttempt(t, rc, userC, 910001, 910002, 1, "COMPLETE", "SUCCESS", "", expired)
	execFixture(t,
		`UPDATE t_upstream_attempt SET tokens_in = 100, tokens_out = 50 WHERE request_id = ?`, rc)
	insertUsage(t, rc, userC, expired, "SUCCESS", 0)

	res, err := h.Round(ctx)
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if res.Scanned != 2 || res.Failed != 0 {
		t.Fatalf("round = %+v; want scanned=2 failed=0", res)
	}
	if res.TypeB != 1 || res.TypeC != 1 || res.TypeA != 0 {
		t.Fatalf("round = %+v; want B=1 C=1 A=0 (a DEAD task must not suppress classification)", res)
	}

	holdState := func(req string) string {
		t.Helper()
		var state string
		if err := testDB.QueryRow(
			`SELECT state FROM t_prehold_hold WHERE request_id = ?`, req).Scan(&state); err != nil {
			t.Fatalf("read hold %s: %v", req, err)
		}
		return state
	}

	// B: released. This is the state that used to stay HELD forever.
	if got := holdState(rb); got != ledger.HoldStateReleased {
		t.Fatalf("B hold state = %s; want RELEASED", got)
	}
	var sum int64
	if err := testDB.QueryRow(
		`SELECT COALESCE(SUM(amount_total_micro),0) FROM t_ledger_entry WHERE request_id = ?`, rb,
	).Scan(&sum); err != nil {
		t.Fatalf("sum ledger %s: %v", rb, err)
	}
	if sum != 0 {
		t.Fatalf("B ledger sum = %d; want 0 (prehold + release)", sum)
	}

	// C: never released, and the operator is told. The DEAD task is revived by
	// the re-enqueue (uk_st_request_type keeps one row per request, so without
	// the revival the insert would be a no-op and the task would stay DEAD
	// while the prehold stayed HELD). It comes back as PENDING with a fresh
	// attempt budget and the recomputed payload, and the worker can claim it.
	if got := holdState(rc); got != ledger.HoldStateHeld {
		t.Fatalf("C hold state = %s; C must never be silently released", got)
	}
	var cState, cPayload string
	var cAttempts int
	if err := testDB.QueryRow(
		`SELECT state, attempt_count, payload FROM t_settle_task
		  WHERE request_id = ? AND task_type = ?`,
		rc, settle.TaskTypeSettle).Scan(&cState, &cAttempts, &cPayload); err != nil {
		t.Fatalf("read C task: %v", err)
	}
	if cState != settle.TaskStatePending {
		t.Fatalf("C task state = %s; want PENDING (a DEAD task must be revived, not left dead)", cState)
	}
	if cAttempts != 0 {
		t.Fatalf("C task attempt_count = %d; want 0 (fresh retry budget)", cAttempts)
	}
	var decoded settle.SettlePayload
	if err := json.Unmarshal([]byte(cPayload), &decoded); err != nil {
		t.Fatalf("decode revived payload: %v", err)
	}
	if decoded.RequestID != rc || decoded.UserID != userC || decoded.ConsumedMicro != 5000 {
		t.Fatalf("revived payload = %+v; want rid=%s user=%d consumed=5000", decoded, rc, userC)
	}
	var notices int
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM t_notification
		  WHERE event_code = 'RECONCILE_MANUAL' AND body LIKE ?`, "%request_id="+rc+"%",
	).Scan(&notices); err != nil {
		t.Fatalf("C notification: %v", err)
	}
	if notices != 1 {
		t.Fatalf("C RECONCILE_MANUAL rows = %d; want 1 (a DEAD task must reach the operator)", notices)
	}
	// The next round re-derives the identical body; Notify is now idempotent
	// per (event_code, body), so the operator channel does not fill up with one
	// row per minute per hung request. Re-send the body the round actually wrote
	// instead of rebuilding it here: a hand-built copy would drift from the
	// production format and the assertion would quietly stop testing anything.
	var writtenBody, writtenTitle string
	if err := testDB.QueryRow(
		`SELECT body, title FROM t_notification
		  WHERE event_code = 'RECONCILE_MANUAL' AND body LIKE ?`, "%request_id="+rc+"%",
	).Scan(&writtenBody, &writtenTitle); err != nil {
		t.Fatalf("read C notification body: %v", err)
	}
	if err := h.Recorder.Notify(ctx, "RECONCILE_MANUAL", writtenTitle, writtenBody, 3); err != nil {
		t.Fatalf("repeat notify: %v", err)
	}
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM t_notification
		  WHERE event_code = 'RECONCILE_MANUAL' AND body LIKE ?`, "%request_id="+rc+"%",
	).Scan(&notices); err != nil {
		t.Fatalf("C notification recount: %v", err)
	}
	if notices != 1 {
		t.Fatalf("C RECONCILE_MANUAL rows after an identical repeat = %d; want 1", notices)
	}
	t.Logf("B2: DEAD+FAILED => %s, DEAD+SUCCESS => %s (task %s attempts=%d, notices=%d)",
		holdState(rb), holdState(rc), cState, cAttempts, notices)
}

// insertDeadSettleTask writes the row a settle worker leaves after maxAttempts.
func insertDeadSettleTask(t *testing.T, requestID string, userID int64, at time.Time) {
	t.Helper()
	execFixture(t,
		`INSERT INTO t_settle_task
		   (request_id, user_id, task_type, payload, state, attempt_count,
		    next_retry_at, last_error, created_at, updated_at)
		 VALUES (?,?,?,?,?,5,?,?,?,?)`,
		requestID, userID, settle.TaskTypeSettle, []byte(`{"request_id":"`+requestID+`"}`),
		settle.TaskStateDead, at, "settle: credit batches cannot cover the debit", at, at)
	var state string
	if err := testDB.QueryRow(
		`SELECT state FROM t_settle_task WHERE request_id = ?`, requestID).Scan(&state); err != nil {
		t.Fatalf("verify dead task %s: %v", requestID, err)
	}
	if state != settle.TaskStateDead {
		t.Fatalf("seeded task state = %s; want DEAD", state)
	}
}

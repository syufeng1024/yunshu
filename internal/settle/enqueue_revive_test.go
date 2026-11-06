package settle

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/clock"
)

// B2 companion: re-enqueueing a DEAD task must put it back in the dispatch
// queue.
//
// The hanging scanner now ignores DEAD rows when it asks "does a settlement
// task already exist", so a half-completed request reaches recoverC again. The
// enqueue that follows lands on uk_st_request_type (request_id, task_type).
// Without revival the duplicate branch only touched updated_at: the task stayed
// DEAD, the worker only claims PENDING rows, and the prehold stayed HELD
// forever.
func TestEnqueueRevivesDeadTask(t *testing.T) {
	ctx := context.Background()
	rid := newRequestID(t)
	userID := newFundedUser(t)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM t_settle_task WHERE request_id = ?`, rid)
	})

	past := time.Now().UTC().Add(-time.Hour)
	deadPayload, err := jsonx.Marshal(SettlePayload{
		RequestID: rid, UserID: userID, ConsumedMicro: 1,
	})
	if err != nil {
		t.Fatalf("encode dead payload: %v", err)
	}
	if _, err := testDB.Exec(
		`INSERT INTO t_settle_task
		   (request_id, user_id, task_type, payload, state, attempt_count,
		    next_retry_at, last_error, created_at, updated_at)
		 VALUES (?,?,?,?,?,5,?,?,?,?)`,
		rid, userID, TaskTypeSettle, deadPayload, TaskStateDead, past,
		"settle: credit batches cannot cover the debit", past, past); err != nil {
		t.Fatalf("seed dead task: %v", err)
	}

	enq := NewEnqueuer(testDB, clock.Real{})
	if err := enq.Enqueue(ctx, EnqueueParams{
		RequestID: rid, UserID: userID, ConsumedMicro: 60000,
	}); err != nil {
		t.Fatalf("enqueue over dead task: %v", err)
	}

	var state, rawPayload string
	var attempts int
	var createdAt time.Time
	var lastErr sql.NullString
	if err := testDB.QueryRow(
		`SELECT state, attempt_count, payload, created_at, last_error FROM t_settle_task
		  WHERE request_id = ? AND task_type = ?`, rid, TaskTypeSettle,
	).Scan(&state, &attempts, &rawPayload, &createdAt, &lastErr); err != nil {
		t.Fatalf("read revived task: %v", err)
	}
	if state != TaskStatePending {
		t.Fatalf("state = %s; want PENDING", state)
	}
	if attempts != 0 {
		t.Fatalf("attempt_count = %d; want 0 (a revived task gets a fresh budget)", attempts)
	}
	// The dead-letter cause names an attempt that no longer describes this row.
	if lastErr.Valid && lastErr.String != "" {
		t.Fatalf("last_error = %q; want NULL on a revived task", lastErr.String)
	}
	var got SettlePayload
	if err := jsonx.Unmarshal([]byte(rawPayload), &got); err != nil {
		t.Fatalf("decode revived payload: %v", err)
	}
	if got.ConsumedMicro != 60000 || got.RequestID != rid || got.UserID != userID {
		t.Fatalf("payload = %+v; want the re-enqueued settlement input", got)
	}
	if !createdAt.UTC().Truncate(time.Second).Equal(past.Truncate(time.Second)) {
		t.Fatalf("created_at moved to %s; the row must be revived, not replaced", createdAt)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM t_settle_task WHERE request_id = ?`, rid); n != 1 {
		t.Fatalf("task rows = %d; want 1", n)
	}

	// A PENDING row is NOT revived a second time: a replay must not reset a
	// task that is already queued or running, or it would never exhaust its
	// attempt budget.
	if err := enq.Enqueue(ctx, EnqueueParams{
		RequestID: rid, UserID: userID, ConsumedMicro: 999,
	}); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if err := testDB.QueryRow(
		`SELECT state, attempt_count, payload FROM t_settle_task
		  WHERE request_id = ? AND task_type = ?`, rid, TaskTypeSettle,
	).Scan(&state, &attempts, &rawPayload); err != nil {
		t.Fatalf("re-read task: %v", err)
	}
	if state != TaskStatePending || attempts != 0 {
		t.Fatalf("after a second enqueue state=%s attempts=%d; want PENDING/0", state, attempts)
	}
	if err := jsonx.Unmarshal([]byte(rawPayload), &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got.ConsumedMicro != 60000 {
		t.Fatalf("consumed = %d; a live task's payload must not be overwritten by a replay", got.ConsumedMicro)
	}
}

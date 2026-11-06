package recon

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"yunshu.dev/core/internal/ledger"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
	"yunshu.dev/core/internal/settle"
)

// F4: reviving a DEAD settlement task must be bounded.
//
// The chain was unbounded: DEAD → the hanging scan classifies it as C →
// Enqueue revives it with a fresh attempt budget → the worker burns five
// retries (~13 minutes) → DEAD again → the next 60s scan revives it again. For a
// deterministic failure (a batch/account mismatch fails identically every time)
// that never converges, writes ~110 notifications per day per stuck request, and
// destroys the meaning of DEAD — the state that is supposed to say "a human has
// to look at this".
//
// The cap lives in Redis (no migration) and counts revives per request_id with a
// TTL so a rid can never be locked out permanently.
func TestHangingReviveIsCapped(t *testing.T) {
	ctx := context.Background()
	rdb := redistest.NewClient(0)
	t.Cleanup(func() { _ = rdb.Close() })

	store := ledger.NewStore(testDB, clock.Real{})
	logBuf := &bytes.Buffer{}
	clk := &mutableClock{t: time.Now().UTC()}
	h := NewHanging(testDB, rdb, store, settle.NewEnqueuer(testDB, clock.Real{}),
		obslog.NewWithWriter("jobsd", "test", logBuf, nil), clk)
	h.Cost = func(context.Context, int64, int64, int64, time.Time) (int64, error) {
		return 5000, nil
	}
	h.UserLo, h.UserHi = 900030, 900040

	const (
		user = int64(900031)
		hold = int64(120000)
	)
	rid := rid("capC", 3)
	expired := time.Now().UTC().Add(-10 * time.Minute)

	deleteAccount(t, user)
	t.Cleanup(func() {
		deleteAccount(t, user)
		_, _ = testDB.Exec(`DELETE FROM t_notification WHERE event_code = 'RECONCILE_MANUAL'`)
		_ = rdb.Raw().Del(context.Background(),
			redisx.HoldRecoverKey(rid), reviveCounterKey(rid)).Err()
	})
	_ = rdb.Raw().Del(ctx, reviveCounterKey(rid), redisx.HoldRecoverKey(rid)).Err()
	_, _ = testDB.Exec(`DELETE FROM t_notification WHERE event_code = 'RECONCILE_MANUAL'`)

	// A held prehold whose upstream attempt succeeded: class C, the shape that
	// gets re-enqueued rather than released.
	insertAccount(t, user, -hold, 0)
	insertLedgerEntry(t, user, entryNo("CAP", 1), ledger.BizTypeConsumePrehold,
		ledger.DirectionDebit, 0, -hold, -hold, 0, -hold, "prehold:"+rid, &rid)
	execFixture(t,
		`INSERT INTO t_prehold_hold
		   (request_id, user_id, api_key_id, hold_micro, estimated_micro, model_id,
		    state, expires_at, node_id, created_at)
		 VALUES (?,?,1,?,?,910001,'HELD',?, 'test-node', ?)`,
		rid, user, hold, hold, expired, expired)
	insertAttempt(t, rid, user, 910001, 910002, 1, "COMPLETE", "SUCCESS", "", expired)
	execFixture(t, `UPDATE t_upstream_attempt SET tokens_in = 100, tokens_out = 50 WHERE request_id = ?`, rid)
	insertUsage(t, rid, user, expired, "SUCCESS", 0)

	taskState := func() string {
		t.Helper()
		var state string
		if err := testDB.QueryRow(
			`SELECT state FROM t_settle_task WHERE request_id = ? AND task_type = ?`,
			rid, settle.TaskTypeSettle).Scan(&state); err != nil {
			t.Fatalf("read task: %v", err)
		}
		return state
	}
	// round simulates one scan period: the worker has just marked the task DEAD
	// again and K12's 300s window has elapsed.
	round := func() {
		t.Helper()
		execFixture(t,
			`UPDATE t_settle_task SET state = ?, attempt_count = 5
			  WHERE request_id = ? AND task_type = ?`,
			settle.TaskStateDead, rid, settle.TaskTypeSettle)
		if err := rdb.Raw().Del(ctx, redisx.HoldRecoverKey(rid)).Err(); err != nil {
			t.Fatalf("clear K12: %v", err)
		}
		if _, err := h.Round(ctx); err != nil {
			t.Fatalf("round: %v", err)
		}
	}

	for i := 1; i <= MaxHangingRevives; i++ {
		round()
		if got := taskState(); got != settle.TaskStatePending {
			t.Fatalf("after revive %d the task state = %s; want PENDING", i, got)
		}
		count, err := rdb.Raw().Get(ctx, reviveCounterKey(rid)).Int64()
		if err != nil {
			t.Fatalf("read revive counter: %v", err)
		}
		if count != int64(i) {
			t.Fatalf("revive counter = %d after %d rounds; want %d", count, i, i)
		}
	}

	// Round 4 exceeds the cap: the task is left DEAD (the "needs a human" state)
	// and the operator gets a capped notice instead of another silent retry.
	round()
	if got := taskState(); got != settle.TaskStateDead {
		t.Fatalf("after the cap the task state = %s; want DEAD (a retry will not help)", got)
	}
	cappedNotices := func() int64 {
		t.Helper()
		return countRows(t,
			`SELECT COUNT(*) FROM t_notification
			  WHERE event_code = 'RECONCILE_MANUAL' AND body LIKE ?`,
			"%request_id="+rid+"%revive_capped=true%")
	}
	if n := cappedNotices(); n != 1 {
		t.Fatalf("capped notices = %d; want 1", n)
	}

	// Two more rounds stay capped and must not add a second notice. This is the
	// assertion that distinguishes "notify on the crossing round" from "notify
	// every round": a per-round implementation also leaves exactly one ROW (Notify
	// is idempotent on a stable body) but writes an ERROR line every time, so the
	// log counter is the discriminator.
	round()
	round()
	if got := taskState(); got != settle.TaskStateDead {
		t.Fatalf("task state = %s after further rounds; want DEAD", got)
	}
	if n := cappedNotices(); n != 1 {
		t.Fatalf("capped notices after two more rounds = %d; want 1", n)
	}

	// "Only once" is only meaningful together with "it was attempted more than
	// once": an ERROR count of 1 is also what you get if recoverC is never reached
	// (for example if the hasSettleTask query went back to counting DEAD rows, in
	// which case this branch never runs at all). The counter is incremented only
	// after that early return, so its value proves the capped branch was entered
	// on every round below.
	count, err := rdb.Raw().Get(ctx, reviveCounterKey(rid)).Int64()
	if err != nil {
		t.Fatalf("read revive counter after capped rounds: %v", err)
	}
	if count != int64(MaxHangingRevives)+3 {
		t.Fatalf("revive counter = %d after %d rounds; want %d "+
			"(proves recoverC was reached on the capped rounds)",
			count, MaxHangingRevives+3, MaxHangingRevives+3)
	}
	if n := int64(bytes.Count(logBuf.Bytes(), []byte("prehold_revive_capped"))); n != 1 {
		t.Fatalf("prehold_revive_capped log lines = %d; want 1 (capped must not log every round)", n)
	}
	// Later capped rounds must not add a line at any level. "Still capped" is not
	// new information: it is exactly `state='DEAD' AND revive_count > 3`, which
	// the table answers and the notification row already reports. A per-round
	// Info line here cost ~51k rows per stuck request over the 180-day retention
	// while telling an operator nothing they could not query.
	if n := int64(bytes.Count(logBuf.Bytes(), []byte("prehold_revive_still_capped"))); n != 0 {
		t.Fatalf("prehold_revive_still_capped log lines = %d; want 0 "+
			"(rounds past the window must be silent, not Info-per-round)", n)
	}

	// The counter carries a TTL: a rid must never be locked out for good.
	ttl, err := rdb.Raw().TTL(ctx, reviveCounterKey(rid)).Result()
	if err != nil {
		t.Fatalf("read revive TTL: %v", err)
	}
	if ttl <= 0 || ttl > ReviveTTL {
		t.Fatalf("revive counter TTL = %s; want 0 < ttl <= %s", ttl, ReviveTTL)
	}

	// TTL expiry (and therefore a new UTC day) resets the counter, and one more
	// notice is allowed: a request that is still stuck tomorrow must resurface
	// rather than go silent forever. Same day -> the body is identical -> the
	// existing row absorbs it.
	if err := rdb.Raw().Del(ctx, reviveCounterKey(rid)).Err(); err != nil {
		t.Fatalf("expire revive counter: %v", err)
	}
	clk.Advance(24 * time.Hour)
	for i := 0; i < MaxHangingRevives+1; i++ {
		round()
	}
	if n := cappedNotices(); n != 2 {
		t.Fatalf("capped notices after the day rollover = %d; want 2 (one per day)", n)
	}
	round()
	round()
	if n := cappedNotices(); n != 2 {
		t.Fatalf("capped notices = %d after more same-day rounds; want 2", n)
	}

	// The hold is untouched throughout: capping changes only how often we retry,
	// never the money.
	var state string
	if err := testDB.QueryRow(
		`SELECT state FROM t_prehold_hold WHERE request_id = ?`, rid).Scan(&state); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if state != ledger.HoldStateHeld {
		t.Fatalf("hold state = %s; want HELD (capping must not release anything)", state)
	}
	t.Logf("F4: %d revives then capped; task stays %s, notices=%d, ttl=%s",
		MaxHangingRevives, taskState(), cappedNotices(), ttl)
}

// F4-d: the capped notice must survive a transient failure at the crossing
// round.
//
// The branch that hands a DEAD request to an operator is the only hand-off there
// is (recoverC deliberately does not re-enqueue), and `capped` stays true for
// the whole 24h TTL. Gating Notify on `revives == MaxHangingRevives+1` made it
// at-most-once: that exact round occurs once, and a single error there meant no
// notice for the rest of the day — the request would sit DEAD and unmentioned
// until the TTL expired. The gate is now a retry window
// (`revives <= MaxHangingRevives+3`), so the next rounds re-attempt it, and
// Notify's per-body idempotency keeps the retries at one row.
//
// This test fails on the old gate: round 5 does not match `== Max+1`, so the row
// stays at 0.
func TestHangingCappedNotifyRetriesAfterFailure(t *testing.T) {
	ctx := context.Background()
	rdb := redistest.NewClient(0)
	t.Cleanup(func() { _ = rdb.Close() })

	store := ledger.NewStore(testDB, clock.Real{})
	logBuf := &bytes.Buffer{}
	clk := &mutableClock{t: time.Now().UTC()}
	h := NewHanging(testDB, rdb, store, settle.NewEnqueuer(testDB, clock.Real{}),
		obslog.NewWithWriter("jobsd", "test", logBuf, nil), clk)
	h.Cost = func(context.Context, int64, int64, int64, time.Time) (int64, error) {
		return 5000, nil
	}
	h.UserLo, h.UserHi = 900050, 900060

	const (
		user = int64(900051)
		hold = int64(120000)
	)
	rid := rid("capD", 4)
	expired := time.Now().UTC().Add(-10 * time.Minute)

	deleteAccount(t, user)
	t.Cleanup(func() {
		deleteAccount(t, user)
		_, _ = testDB.Exec(`DELETE FROM t_notification WHERE event_code = 'RECONCILE_MANUAL'`)
		_ = rdb.Raw().Del(context.Background(),
			redisx.HoldRecoverKey(rid), reviveCounterKey(rid)).Err()
	})
	_ = rdb.Raw().Del(ctx, reviveCounterKey(rid), redisx.HoldRecoverKey(rid)).Err()
	_, _ = testDB.Exec(`DELETE FROM t_notification WHERE event_code = 'RECONCILE_MANUAL'`)

	insertAccount(t, user, -hold, 0)
	insertLedgerEntry(t, user, entryNo("CAPD", 1), ledger.BizTypeConsumePrehold,
		ledger.DirectionDebit, 0, -hold, -hold, 0, -hold, "prehold:"+rid, &rid)
	execFixture(t,
		`INSERT INTO t_prehold_hold
		   (request_id, user_id, api_key_id, hold_micro, estimated_micro, model_id,
		    state, expires_at, node_id, created_at)
		 VALUES (?,?,1,?,?,910001,'HELD',?, 'test-node', ?)`,
		rid, user, hold, hold, expired, expired)
	insertAttempt(t, rid, user, 910001, 910002, 1, "COMPLETE", "SUCCESS", "", expired)
	execFixture(t, `UPDATE t_upstream_attempt SET tokens_in = 100, tokens_out = 50 WHERE request_id = ?`, rid)
	insertUsage(t, rid, user, expired, "SUCCESS", 0)

	round := func() error {
		t.Helper()
		execFixture(t,
			`UPDATE t_settle_task SET state = ?, attempt_count = 5
			  WHERE request_id = ? AND task_type = ?`,
			settle.TaskStateDead, rid, settle.TaskTypeSettle)
		if err := rdb.Raw().Del(ctx, redisx.HoldRecoverKey(rid)).Err(); err != nil {
			t.Fatalf("clear K12: %v", err)
		}
		_, err := h.Round(ctx)
		return err
	}
	cappedNotices := func() int64 {
		t.Helper()
		return countRows(t,
			`SELECT COUNT(*) FROM t_notification
			  WHERE event_code = 'RECONCILE_MANUAL' AND body LIKE ?`,
			"%request_id="+rid+"%revive_capped=true%")
	}

	for i := 1; i <= MaxHangingRevives; i++ {
		if err := round(); err != nil {
			t.Fatalf("revive round %d: %v", i, err)
		}
	}
	if n := cappedNotices(); n != 0 {
		t.Fatalf("capped notices before the cap = %d; want 0", n)
	}

	// The crossing round fails to notify. Pointing Recorder at no database is the
	// narrowest injection available: Notify's own guard rejects it, so nothing
	// else in Round is affected (Notify is the only Recorder call).
	//
	// Round aggregates per-request failures into its counters instead of returning
	// them, so the observable proof that the injection worked is the missing row
	// at a round that otherwise writes one — not the return value.
	savedDB := h.Recorder.DB
	h.Recorder.DB = nil
	_ = round()
	h.Recorder.DB = savedDB
	if n := cappedNotices(); n != 0 {
		t.Fatalf("capped notices after the failed crossing round = %d; want 0 "+
			"(the injection did not take effect)", n)
	}
	// The log channel must not be lost along with the row: the ERROR is emitted
	// before Notify precisely so one failure cannot silence both.
	if n := int64(bytes.Count(logBuf.Bytes(), []byte("prehold_revive_capped"))); n != 1 {
		t.Fatalf("prehold_revive_capped ERROR lines after a failed notice = %d; want 1 "+
			"(the log line is the channel that must never be skipped)", n)
	}

	// The retry window is what makes the loss recoverable: revives is 5 here, and
	// the old `== MaxHangingRevives+1` gate would skip Notify for the rest of the
	// TTL.
	if err := round(); err != nil {
		t.Fatalf("retry round after a failed notice: %v", err)
	}
	if n := cappedNotices(); n != 1 {
		t.Fatalf("capped notices after the retry window = %d; want 1 "+
			"(a transient Notify failure must not lose the whole day's alert)", n)
	}
	if n := int64(bytes.Count(logBuf.Bytes(), []byte("prehold_revive_capped"))); n != 1 {
		t.Fatalf("prehold_revive_capped ERROR lines = %d; want 1 "+
			"(the alert stays pinned to the crossing round; only Notify retries)", n)
	}

	// The retries inside the window collapse: the body is identical within a UTC
	// day, so one more round adds no row and no line.
	if err := round(); err != nil {
		t.Fatalf("third capped round: %v", err)
	}
	if n := cappedNotices(); n != 1 {
		t.Fatalf("capped notices after another round = %d; want 1 (idempotent per body)", n)
	}
}

// mutableClock lets the test move "now" without sleeping, which the daily
// rollover case needs.
type mutableClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *mutableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// countRows is a local helper: the settle package has one, recon does not.
func countRows(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := testDB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

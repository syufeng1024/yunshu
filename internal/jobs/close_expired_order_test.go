package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yunshu.dev/core/internal/control/audit"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/platform/clock"
)

// jobOrderSeq keeps order_no unique across runs: uk_ro_order_no is (28) chars
// of yyyymmddHHmmss + sequence, the same shape the console uses.
var jobOrderSeq atomic.Int64

// insertJobOrder inserts one recharge order in the state a test needs. The
// table has no foreign key, so a synthetic user id is enough — this test is
// about the state machine, not about account creation.
func insertJobOrder(t *testing.T, userID int64, state string, expireAt time.Time, paid bool) int64 {
	t.Helper()
	now := time.Now().UTC()
	seq := jobOrderSeq.Add(1)
	orderNo := fmt.Sprintf("%s%014d", now.Format("20060102150405"), seq)
	var paidAt any
	if paid {
		paidAt = now
	}
	res, err := testDB.Exec(
		`INSERT INTO t_recharge_order
		   (order_no, user_id, order_type, plan_id, pay_amount_fen, recharge_micro,
		    bonus_micro, pay_channel, pay_txn_id, state, paid_at, refundable_micro,
		    expire_at, created_at, updated_at)
		 VALUES (?,?,'CUSTOM',NULL,10000,100000000,0,'WECHAT',?,?,?,0,?,?,?)`,
		orderNo, userID, nil, state, paidAt, expireAt, now, now)
	if err != nil {
		t.Fatalf("insert order %s: %v", orderNo, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("order id: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM t_admin_audit_log WHERE target_id = ?`, fmt.Sprint(id))
		_, _ = testDB.Exec(`DELETE FROM t_recharge_order WHERE id = ?`, id)
	})
	return id
}

func newCloseExpiredTask(t *testing.T) *CloseExpiredOrder {
	t.Helper()
	return NewCloseExpiredOrder(testDB, &audit.Writer{DB: testDB, Now: clock.Real{}},
		nil, clock.Real{})
}

func jobOrderState(t *testing.T, orderID int64) string {
	t.Helper()
	var state string
	if err := testDB.QueryRow(`SELECT state FROM t_recharge_order WHERE id = ?`, orderID).
		Scan(&state); err != nil {
		t.Fatalf("read order %d: %v", orderID, err)
	}
	return state
}

// countSystemClose counts the audit rows this task writes for one order. The
// operator action is counted separately on purpose: the two must not be
// interchangeable.
func countSystemClose(t *testing.T, orderID int64) int64 {
	t.Helper()
	var n int64
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM t_admin_audit_log
		  WHERE module = 'FUND' AND action = 'ORDER_CLOSED_BY_SYSTEM' AND target_id = ?`,
		fmt.Sprint(orderID)).Scan(&n); err != nil {
		t.Fatalf("count system close audit: %v", err)
	}
	return n
}

// G21: an expired unpaid order leaves the queue by itself, with an audit row
// that says the system did it; an order still inside its window and an order
// that was actually paid are both left untouched.
func TestCloseExpiredOrderClosesOnlyExpiredPending(t *testing.T) {
	ctx := context.Background()
	task := newCloseExpiredTask(t)

	now := time.Now().UTC()
	expired := insertJobOrder(t, 950001, "PENDING", now.Add(-1*time.Minute), false)
	live := insertJobOrder(t, 950002, "PENDING", now.Add(20*time.Minute), false)
	paid := insertJobOrder(t, 950003, "PAID", now.Add(-1*time.Minute), true)

	res, err := task.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Closed == 0 {
		t.Fatalf("run = %+v; want at least the one expired order closed", res)
	}

	if st := jobOrderState(t, expired); st != "CLOSED" {
		t.Fatalf("expired PENDING order state = %q; want CLOSED", st)
	}
	if got := countSystemClose(t, expired); got != 1 {
		t.Fatalf("expired order audit rows = %d; want 1", got)
	}
	// The audit row must name the system actor, not an operator.
	var operatorID int64
	var role string
	if err := testDB.QueryRow(
		`SELECT operator_id, operator_role FROM t_admin_audit_log
		  WHERE action = 'ORDER_CLOSED_BY_SYSTEM' AND target_id = ?
		  ORDER BY id DESC LIMIT 1`, fmt.Sprint(expired)).
		Scan(&operatorID, &role); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if operatorID != 0 || role != "SYSTEM" {
		t.Fatalf("audit actor = %d/%q; want 0/SYSTEM", operatorID, role)
	}

	if st := jobOrderState(t, live); st != "PENDING" {
		t.Fatalf("unexpired PENDING order state = %q; want PENDING (untouched)", st)
	}
	if got := countSystemClose(t, live); got != 0 {
		t.Fatalf("unexpired order audit rows = %d; want 0", got)
	}

	// The paid order's expire_at is in the past, which is exactly the shape
	// the CAS has to refuse: money really moved, so it may never be voided.
	if st := jobOrderState(t, paid); st != "PAID" {
		t.Fatalf("paid order state = %q; want PAID (never voided)", st)
	}
	if got := countSystemClose(t, paid); got != 0 {
		t.Fatalf("paid order audit rows = %d; want 0", got)
	}

	// Idempotency: the second run finds nothing it may close.
	before := countSystemClose(t, expired)
	again, err := task.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again.Closed != 0 {
		t.Fatalf("second run closed = %d; want 0", again.Closed)
	}
	if got := countSystemClose(t, expired); got != before {
		t.Fatalf("audit rows after second run = %d; want %d (no second audit)", got, before)
	}
	if st := jobOrderState(t, expired); st != "CLOSED" {
		t.Fatalf("state after second run = %q; want CLOSED", st)
	}
	t.Logf("first run closed=%d scanned=%d; second run closed=%d", res.Closed, res.Scanned, again.Closed)
}

// The task refuses to run without an audit writer: a silent void is worse than
// no void at all.
func TestCloseExpiredOrderRequiresAuditWriter(t *testing.T) {
	task := NewCloseExpiredOrder(testDB, nil, nil, clock.Real{})
	if _, err := task.Run(context.Background()); err == nil {
		t.Fatal("run without an audit writer = nil error; want a refusal")
	}
}

// A paid order must survive even when the CAS predicate is the only thing
// standing between it and the void: PENDING -> CLOSED is the sole transition
// this task performs, and the state it reads is re-checked inside the update.
func TestCloseExpiredOrderLeavesRefundedPendingStatesAlone(t *testing.T) {
	ctx := context.Background()
	task := newCloseExpiredTask(t)

	now := time.Now().UTC()
	closed := insertJobOrder(t, 950011, "CLOSED", now.Add(-1*time.Minute), false)
	refunded := insertJobOrder(t, 950012, "REFUNDED", now.Add(-1*time.Minute), true)

	if _, err := task.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, tc := range []struct {
		id   int64
		want string
	}{{closed, "CLOSED"}, {refunded, "REFUNDED"}} {
		if st := jobOrderState(t, tc.id); st != tc.want {
			t.Fatalf("order %d state = %q; want %q", tc.id, st, tc.want)
		}
		if got := countSystemClose(t, tc.id); got != 0 {
			t.Fatalf("order %d audit rows = %d; want 0", tc.id, got)
		}
	}
}

// The scan predicate and the CAS guard different failure modes. The scan keeps
// non-PENDING rows out; the CAS covers the window between the scan and the
// update, which is exactly when a channel callback can pay the order. This test
// reproduces that window by paying the order after it has been scanned, so the
// CAS is the only thing that can still refuse the void.
func TestCloseExpiredOrderSkipsOrderPaidAfterScan(t *testing.T) {
	ctx := context.Background()
	task := newCloseExpiredTask(t)

	orderID := insertJobOrder(t, 950031, "PENDING", time.Now().UTC().Add(-1*time.Minute), false)

	ids, err := task.scan(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("scan returned no id; the fixture must be eligible")
	}

	// The callback wins the race: the row is PAID before this task updates it.
	if _, err := testDB.Exec(
		`UPDATE t_recharge_order SET state = 'PAID', paid_at = UTC_TIMESTAMP(3) WHERE id = ?`,
		orderID); err != nil {
		t.Fatalf("simulate callback: %v", err)
	}

	closed, err := task.closeOne(ctx, orderID)
	if err != nil {
		t.Fatalf("closeOne: %v", err)
	}
	if closed {
		t.Fatal("closeOne reported a void for an order that was already PAID")
	}
	if st := jobOrderState(t, orderID); st != "PAID" {
		t.Fatalf("state = %q; want PAID (a paid order is never voided)", st)
	}
	if got := countSystemClose(t, orderID); got != 0 {
		t.Fatalf("audit rows = %d; want 0 (no void, no audit)", got)
	}
}

// scan must not offer a row without expire_at. The column is NOT NULL in the
// schema, so this guards the predicate's IS NOT NULL clause against a future
// nullable migration rather than an existing row.
func TestCloseExpiredOrderScanIgnoresUnexpiredWindow(t *testing.T) {
	task := newCloseExpiredTask(t)
	insertJobOrder(t, 950021, "PENDING", time.Now().UTC().Add(48*time.Hour), false)
	ids, err := task.scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, id := range ids {
		var expireAt sql.NullTime
		if err := testDB.QueryRow(`SELECT expire_at FROM t_recharge_order WHERE id = ?`, id).
			Scan(&expireAt); err != nil {
			t.Fatalf("read expire_at: %v", err)
		}
		if !expireAt.Valid || expireAt.Time.After(time.Now().UTC()) {
			t.Fatalf("scan returned order %d with expire_at %v; want only expired rows", id, expireAt)
		}
	}
}

// failingAuditor is an audit trail that is down: every insert fails. It is the
// production failure this task has to report instead of swallowing.
type failingAuditor struct {
	calls int
}

func (f *failingAuditor) Insert(context.Context, audit.Execer, audit.Entry) error {
	f.calls++
	return errors.New("audit trail unavailable")
}

// G21 robustness (QA): a real failure must fail the run so the runner reports
// task_failed instead of task_done, and a page that closes nothing must end the
// run instead of re-reading the same ids until the task timeout.
func TestCloseExpiredOrderFailsLoudlyAndStopsOnNoProgress(t *testing.T) {
	ctx := context.Background()
	orderID := insertJobOrder(t, 950041, "PENDING", time.Now().UTC().Add(-1*time.Minute), false)

	reg := obsmetrics.NewRegistry()
	rec := obsmetrics.NewRecorder(reg)
	auditor := &failingAuditor{}
	task := NewCloseExpiredOrder(testDB, auditor, nil, clock.Real{})
	task.Metrics = rec

	start := time.Now()
	res, err := task.Run(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("run = %+v with a nil error; want an error so the runner reports task_failed", res)
	}
	if res.Closed != 0 || res.Failed == 0 {
		t.Fatalf("run = %+v; want closed=0 and failed>0", res)
	}
	if res.Pages != 1 {
		t.Fatalf("pages = %d; want 1 (a page that closes nothing must end the run)", res.Pages)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("run took %s; want it to stop on no progress, not spin", elapsed)
	}
	// The audit failure rolls the state change back: the order is untouched.
	if st := jobOrderState(t, orderID); st != "PENDING" {
		t.Fatalf("state = %q; want PENDING (an unauditable void must not commit)", st)
	}
	if got := countSystemClose(t, orderID); got != 0 {
		t.Fatalf("audit rows = %d; want 0", got)
	}
	if auditor.calls == 0 {
		t.Fatal("the audit writer was never called; the failure path was not exercised")
	}
	text := string(reg.PrometheusText())
	// "At least one" rather than "exactly one": the shared test schema may hold
	// stale expired orders from another fixture, and a second failed order in
	// the same page would legitimately raise the count.
	if got := counterValue(t, text, `gw_order_auto_close_failed_total{stage="audit"}`); got < 1 {
		t.Fatalf("failure counter = %d, want >= 1; missing from export:\n%s", got, text)
	}
	t.Logf("run returned error in %s: %v", elapsed, err)
}

// counterValue parses one sample out of the Prometheus text export. A missing
// series returns -1 so "absent" is distinguishable from "zero".
func counterValue(t *testing.T, text, series string) int64 {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, series+" ") {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, series)), 10, 64)
		if err != nil {
			t.Fatalf("parse %s: %v", series, err)
		}
		return value
	}
	return -1
}

// G21 robustness (QA): the CAS missing is not a failure. A run that only skips
// orders somebody else already moved returns nil, and nothing is counted as
// failed.
func TestCloseExpiredOrderCasMissIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	orderID := insertJobOrder(t, 950042, "PENDING", time.Now().UTC().Add(-1*time.Minute), false)

	task := newCloseExpiredTask(t)
	ids, err := task.scan(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("scan returned no id; the fixture must be eligible")
	}
	if _, err := testDB.Exec(
		`UPDATE t_recharge_order SET state = 'PAID', paid_at = UTC_TIMESTAMP(3) WHERE id = ?`,
		orderID); err != nil {
		t.Fatalf("simulate callback: %v", err)
	}

	closed, err := task.closeOne(ctx, orderID)
	if err != nil {
		t.Fatalf("closeOne returned %v; a CAS miss must not be an error", err)
	}
	if closed {
		t.Fatal("closeOne reported a void for an order that was already PAID")
	}

	res, err := task.Run(ctx)
	if err != nil {
		t.Fatalf("run = %+v, %v; want a nil error when the only outcome is skips", res, err)
	}
	if res.Failed != 0 {
		t.Fatalf("failed = %d; want 0 (a CAS miss is a normal skip)", res.Failed)
	}
}

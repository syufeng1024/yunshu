package recon

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
	"yunshu.dev/core/internal/settle"
)

// A5: one expired HELD row per TDS §5.3 state. A and B release the full hold; C
// never releases and must leave a SETTLE task plus a RECONCILE_MANUAL notice.
func TestHangingRecoveryThreeStates(t *testing.T) {
	ctx := context.Background()
	rdb := redistest.NewClient(0)
	t.Cleanup(func() { _ = rdb.Close() })

	store := ledger.NewStore(testDB, clock.Real{})
	h := NewHanging(testDB, rdb, store, settle.NewEnqueuer(testDB, clock.Real{}), nil, clock.Real{})
	h.Cost = func(context.Context, int64, int64, int64, time.Time) (int64, error) {
		return 5000, nil
	}
	// yunshu_test retains expired HELD rows from other packages; only this
	// test's users belong to the round.
	h.UserLo, h.UserHi = 900000, 900010

	const (
		userA = int64(900001)
		userB = int64(900002)
		userC = int64(900003)
		hold  = int64(120000)
	)
	ra, rb, rc := rid("hngA", 1), rid("hngB", 1), rid("hngC", 1)
	expired := time.Now().UTC().Add(-10 * time.Minute)

	for i, u := range []int64{userA, userB, userC} {
		deleteAccount(t, u)
		insertAccount(t, u, -hold, 0)
		req := []string{ra, rb, rc}[i]
		insertLedgerEntry(t, u, entryNo("HP", i+1), ledger.BizTypeConsumePrehold,
			ledger.DirectionDebit, 0, -hold, -hold, 0, -hold, "prehold:"+req, &req)
		execFixture(t,
			`INSERT INTO t_prehold_hold
			   (request_id, user_id, api_key_id, hold_micro, estimated_micro, model_id,
			    state, expires_at, node_id, created_at)
			 VALUES (?,?,1,?,?,910001,'HELD',?, 'test-node', ?)`,
			req, u, hold, hold, expired, expired)
	}
	t.Cleanup(func() {
		for _, u := range []int64{userA, userB, userC} {
			deleteAccount(t, u)
		}
		_, _ = testDB.Exec(`DELETE FROM t_notification WHERE event_code = 'RECONCILE_MANUAL'`)
		_, _ = testDB.Exec(`DELETE FROM t_notification WHERE event_code = 'RECONCILE_ALERT'`)
		_, _ = testDB.Exec(`DELETE FROM t_reconcile_diff`)
		_ = rdb.Raw().Del(context.Background(),
			redisx.HoldRecoverKey(ra), redisx.HoldRecoverKey(rb), redisx.HoldRecoverKey(rc)).Err()
	})

	// A: the gateway wrote the prehold and died before any upstream call. The
	// usage row must be corrected, not merely released.
	insertUsage(t, ra, userA, expired, "SUCCESS", 0)

	// B: every upstream attempt failed.
	insertAttempt(t, rb, userB, 910001, 910002, 1, "CONNECT", "FAILED", "HTTP_5XX", expired)

	// C: the upstream attempt succeeded; the local settlement never ran.
	insertAttempt(t, rc, userC, 910001, 910002, 1, "COMPLETE", "SUCCESS", "", expired)
	execFixture(t,
		`UPDATE t_upstream_attempt SET tokens_in = 100, tokens_out = 50 WHERE request_id = ?`, rc)

	res, err := h.Round(ctx)
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if res.Scanned != 3 || res.TypeA != 1 || res.TypeB != 1 || res.TypeC != 1 || res.Failed != 0 {
		t.Fatalf("round = %+v; want scanned=3 A=B=C=1 failed=0", res)
	}

	// K12: the 300s hold:recover key blocks a second replica from redoing any
	// of the three rows.
	for _, req := range []string{ra, rb, rc} {
		ok, err := h.claim(ctx, req)
		if err != nil {
			t.Fatalf("claim %s: %v", req, err)
		}
		if ok {
			t.Fatalf("claim %s succeeded twice; K12 must dedupe replicas", req)
		}
	}

	type holdRow struct {
		state  string
		reason string
	}
	readHold := func(req string) holdRow {
		var out holdRow
		var reason sql.NullString
		if err := testDB.QueryRow(
			`SELECT state, release_reason FROM t_prehold_hold WHERE request_id = ?`, req,
		).Scan(&out.state, &reason); err != nil {
			t.Fatalf("read hold %s: %v", req, err)
		}
		out.reason = reason.String
		return out
	}

	if got := readHold(ra); got.state != ledger.HoldStateReleased || got.reason != "CRASH_RECOVER" {
		t.Fatalf("A hold = %+v; want RELEASED/CRASH_RECOVER", got)
	}
	if got := readHold(rb); got.state != ledger.HoldStateReleased || got.reason != "UPSTREAM_FAIL" {
		t.Fatalf("B hold = %+v; want RELEASED/UPSTREAM_FAIL", got)
	}
	if got := readHold(rc); got.state != ledger.HoldStateHeld {
		t.Fatalf("C hold state = %q; C must never be silently released", got.state)
	}

	var usageResult, failStage, failOwner string
	var consumed int64
	if err := testDB.QueryRow(
		`SELECT result, COALESCE(fail_stage,''), COALESCE(fail_owner,''), consumed_micro
		   FROM t_usage_record WHERE request_id = ?`, ra,
	).Scan(&usageResult, &failStage, &failOwner, &consumed); err != nil {
		t.Fatalf("read A usage: %v", err)
	}
	if usageResult != "FAILED" || failStage != "SETTLE" || failOwner != "PLATFORM" || consumed != 0 {
		t.Fatalf("A usage = %s/%s/%s consumed=%d; want FAILED/SETTLE/PLATFORM/0",
			usageResult, failStage, failOwner, consumed)
	}

	var payload []byte
	if err := testDB.QueryRow(
		`SELECT payload FROM t_settle_task WHERE request_id = ? AND task_type = ?`,
		rc, settle.TaskTypeSettle,
	).Scan(&payload); err != nil {
		t.Fatalf("C settle task: %v", err)
	}
	var decoded settle.SettlePayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode C payload: %v", err)
	}
	if decoded.ConsumedMicro != 5000 || decoded.UserID != userC {
		t.Fatalf("C payload = %+v; want consumed=5000 user=%d", decoded, userC)
	}

	var notices int
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM t_notification
		  WHERE event_code = 'RECONCILE_MANUAL' AND body LIKE ?`, "%request_id="+rc+"%",
	).Scan(&notices); err != nil {
		t.Fatalf("C notification: %v", err)
	}
	if notices != 1 {
		t.Fatalf("C RECONCILE_MANUAL rows = %d; want 1", notices)
	}

	// Ledger truth for A and B: prehold -hold plus release +hold nets to zero.
	for _, req := range []string{ra, rb} {
		var sum int64
		if err := testDB.QueryRow(
			`SELECT COALESCE(SUM(amount_total_micro),0) FROM t_ledger_entry WHERE request_id = ?`, req,
		).Scan(&sum); err != nil {
			t.Fatalf("sum ledger %s: %v", req, err)
		}
		if sum != 0 {
			t.Fatalf("%s ledger sum = %d; want 0", req, sum)
		}
	}
	t.Logf("A5 A=%s B=%s C=%s consumed=%d notices=%d",
		readHold(ra).state, readHold(rb).state, readHold(rc).state, decoded.ConsumedMicro, notices)
}

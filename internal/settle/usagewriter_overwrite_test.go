package settle

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"yunshu.dev/core/internal/platform/clock"
)

// A-12: a row that actually consumed credits must win the request_id.
//
// t_usage_record's unique key is request_id and the batch INSERT resolves a
// conflict with a deliberate no-op, so whichever row lands first owns the key
// forever — including the FAILED/0 row an eager rejection (409 duplicate id, 402
// quota, 429 rate limit, P06/P07) writes when a client reuses an id that another
// request is still executing. The request that really ran then settles, its row
// is swallowed, and the record says "failed, 0 credits" while the ledger holds a
// charge. These cases pin the ownership rule: a row with consumed_micro > 0 may
// replace a row with result='FAILED' AND consumed_micro = 0, and nothing else.

type usageRow struct {
	Result       string
	BizCode      string
	UserID       int64
	Consumed     int64
	TokensIn     int64
	TokensOut    int64
	HTTPStatus   int64
	CreatedAt    time.Time
	PreholdMicro int64
}

func writeUsage(t *testing.T, w *UsageWriter, rows ...UsageRecord) {
	t.Helper()
	for _, r := range rows {
		w.WriteUsage(r)
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("flush usage: %v", err)
	}
}

func readUsageRow(t *testing.T, requestID string) usageRow {
	t.Helper()
	var row usageRow
	var biz sql.NullString
	if err := testDB.QueryRow(
		`SELECT result, biz_code, user_id, consumed_micro, tokens_in, tokens_out,
		        http_status, created_at, prehold_micro
		   FROM t_usage_record WHERE request_id = ?`, requestID,
	).Scan(&row.Result, &biz, &row.UserID, &row.Consumed, &row.TokensIn, &row.TokensOut,
		&row.HTTPStatus, &row.CreatedAt, &row.PreholdMicro); err != nil {
		t.Fatalf("read usage %s: %v", requestID, err)
	}
	row.BizCode = biz.String
	return row
}

func countUsageRows(t *testing.T, requestID string) int64 {
	t.Helper()
	return countRows(t, `SELECT COUNT(*) FROM t_usage_record WHERE request_id = ?`, requestID)
}

func cleanupUsage(t *testing.T, requestIDs ...string) {
	t.Helper()
	for _, rid := range requestIDs {
		_, _ = testDB.Exec(`DELETE FROM t_usage_record WHERE request_id = ?`, rid)
	}
}

// settleRow is the row the settling worker writes for a request that ran.
func settleRow(requestID string, userID, consumed, tokensIn, tokensOut int64, at time.Time) UsageRecord {
	return UsageRecord{
		RequestID: requestID, UserID: userID, APIKeyID: 1,
		ModelID: 1, ModelName: "overwrite-test", RequestModel: "overwrite-test",
		InboundProto: "OPENAI_COMPAT", GroupCode: "standard", PriceRatioBP: 10000,
		BillingTier: "BASE", HTTPStatus: 200, Result: "SUCCESS", UsageSource: "UPSTREAM",
		TokensIn: tokensIn, TokensOut: tokensOut,
		PreholdMicro: consumed, ConsumedMicro: consumed,
		ConsumedRechargeMicro: consumed, UpstreamAttempts: 1, CreatedAt: at,
	}
}

// rejectRow is the row an eager rejection writes: no execution, no money.
func rejectRow(requestID string, userID int64, bizCode string, at time.Time) UsageRecord {
	return UsageRecord{
		RequestID: requestID, UserID: userID, APIKeyID: 1,
		ModelID: 1, ModelName: "overwrite-test", RequestModel: "overwrite-test",
		InboundProto: "OPENAI_COMPAT", GroupCode: "standard", PriceRatioBP: 10000,
		BillingTier: "BASE", HTTPStatus: 409, BizCode: bizCode, Result: "FAILED",
		UsageSource: "NONE", CreatedAt: at,
	}
}

// The A-12 shape itself: the rejection lands first (while the request is still
// in flight), the settlement lands second and must take the row back.
func TestInsertUsageOverwritesStaleFailure(t *testing.T) {
	rid := newRequestID(t)
	const user = int64(970001)
	cleanupUsage(t, rid)
	t.Cleanup(func() { cleanupUsage(t, rid) })

	w := NewUsageWriter(testDB, nil, clock.Real{})
	// created_at is datetime(3): keep the fixture at millisecond precision so the
	// comparison below is about the value, not about rounding.
	firstAt := time.Now().UTC().Add(-90 * time.Second).Truncate(time.Millisecond)
	writeUsage(t, w, rejectRow(rid, user, "duplicate_request_id", firstAt))

	if got := readUsageRow(t, rid); got.Result != "FAILED" || got.Consumed != 0 || got.BizCode != "duplicate_request_id" {
		t.Fatalf("stale row = %+v; want FAILED/0/duplicate_request_id", got)
	}

	settledAt := firstAt.Add(30 * time.Second)
	writeUsage(t, w, settleRow(rid, user, 1024, 30, 50, settledAt))

	if n := countUsageRows(t, rid); n != 1 {
		t.Fatalf("usage rows = %d; want 1 (the key keeps one row per request)", n)
	}
	got := readUsageRow(t, rid)
	if got.Result != "SUCCESS" || got.BizCode != "" {
		t.Fatalf("result = %s/%q; a paid row must replace the failure", got.Result, got.BizCode)
	}
	if got.Consumed != 1024 || got.TokensIn != 30 || got.TokensOut != 50 || got.HTTPStatus != 200 {
		t.Fatalf("overwritten row = %+v; want the settlement values", got)
	}
	// created_at must stay the first-write value: R-05 compares the day's quota
	// against the day's SUM(consumed), so moving a long stream's usage across
	// midnight would invent an R-05 diff nobody can explain.
	if !got.CreatedAt.UTC().Truncate(time.Millisecond).Equal(firstAt) {
		t.Fatalf("created_at moved to %s; want the first write %s", got.CreatedAt, firstAt)
	}
	t.Logf("A-12: FAILED/0 -> %s consumed=%d tokens=%d/%d, created_at preserved at %s",
		got.Result, got.Consumed, got.TokensIn, got.TokensOut, got.CreatedAt.Format(time.RFC3339))
}

// B7b X shape: a 40-character id is truncated to 36 at the insert boundary, so a
// stranger's long id can collide with someone else's id. The row that ran (and
// paid) must win, including its user_id — otherwise the wrong tenant's usage is
// attributed to the account that was merely rejected.
func TestInsertUsageOverwriteTakesTheOwningTenant(t *testing.T) {
	rid := newRequestID(t) // exactly 36 characters, the column width
	longID := rid + "abcd" // 40 characters: cut() keeps the first 36
	const (
		victim = int64(970002) // the stranger who owns the long id
		payer  = int64(970003) // the account that actually ran and paid
	)
	cleanupUsage(t, rid)
	t.Cleanup(func() { cleanupUsage(t, rid) })

	w := NewUsageWriter(testDB, nil, clock.Real{})
	at := time.Now().UTC().Add(-2 * time.Minute)
	writeUsage(t, w, rejectRow(longID, victim, "model_not_found", at))
	if got := readUsageRow(t, rid); got.UserID != victim || got.Result != "FAILED" {
		t.Fatalf("poisoned row = %+v; want user %d FAILED", got, victim)
	}

	writeUsage(t, w, settleRow(rid, payer, 116, 12, 8, at.Add(time.Minute)))

	got := readUsageRow(t, rid)
	if got.UserID != payer || got.Result != "SUCCESS" || got.Consumed != 116 {
		t.Fatalf("row = %+v; want user %d SUCCESS/116", got, payer)
	}
	if n := countUsageRows(t, rid); n != 1 {
		t.Fatalf("usage rows = %d; want 1", n)
	}
}

// TestUsageOverwritePredicateP1 pins WHICH existing rows the A-12 overwrite may
// replace. Six cells, one per boundary:
//
//	#  old result/consumed   new consumed   covered   guards
//	1  FAILED  / 0           116            yes       the only reachable poison shape
//	2  PARTIAL / 0           116            no        `result = 'FAILED'`
//	3  SUCCESS / 0           116            no        `result = 'FAILED'`
//	4  FAILED  / 0             0            no        new-row trigger `consumed > 0`
//	5  FAILED  / 116         200            no        old-row `consumed_micro = 0`
//	6  SUCCESS / 500         116            no        boundary: two charged rows never
//	                                                  replace each other (first wins)
//
// Completeness rule: every conjunct of the predicate must have at least one cell
// that turns red when that conjunct is deleted — delete `result = 'FAILED'` and
// cells 2/3 fail; delete `consumed_micro = 0` and cell 5 fails; delete the
// new-row trigger and cell 4 fails. Without cell 5 the third conjunct would be
// untested: cells 1-4 all stay green without it.
//
// Reachability caveat: cells 2, 3 and 5 are synthetic. A request that succeeds
// leaves a SETTLED hold, so a second write on the same id is refused by P10
// before it can reach this table — there is no production path that puts a
// zero-cost row in front of a charged one. These cells lock the DIRECTION of the
// predicate, not a claim that the collision is reachable. They will NOT fail if
// reachability changes (a new zero-cost write path, a longer hold lifecycle):
// that change must be reviewed by hand, because the assertions encode what we
// decided, not what the world is.
func TestUsageOverwritePredicateP1(t *testing.T) {
	cases := []struct {
		name        string
		oldResult   string
		oldConsume  int64
		newConsume  int64
		covered     bool
		wantResult  string
		wantConsume int64
	}{
		{"1 FAILED/0 replaced by a charge", "FAILED", 0, 116, true, "SUCCESS", 116},
		{"2 PARTIAL/0 kept (zero-price group)", "PARTIAL", 0, 116, false, "PARTIAL", 0},
		{"3 SUCCESS/0 kept (zero-price group)", "SUCCESS", 0, 116, false, "SUCCESS", 0},
		{"4 no override when the new row is free", "FAILED", 0, 0, false, "FAILED", 0},
		{"5 charged FAILED row is never rewritten", "FAILED", 116, 200, false, "FAILED", 116},
		{"6 two charged rows: first wins", "SUCCESS", 500, 116, false, "SUCCESS", 500},
	}
	w := NewUsageWriter(testDB, nil, clock.Real{})
	at := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rid := newRequestID(t)
			cleanupUsage(t, rid)
			t.Cleanup(func() { cleanupUsage(t, rid) })
			user := int64(970100 + i)

			old := settleRow(rid, user, tc.oldConsume, 0, 0, at)
			old.Result = tc.oldResult
			old.BizCode = ""
			if tc.oldResult == "FAILED" {
				old.BizCode = "duplicate_request_id"
				old.HTTPStatus = 409
			}
			writeUsage(t, w, old)

			writeUsage(t, w, settleRow(rid, user, tc.newConsume, 7, 3, at.Add(time.Minute)))

			if n := countUsageRows(t, rid); n != 1 {
				t.Fatalf("usage rows = %d; want 1 (the key keeps one row per request)", n)
			}
			got := readUsageRow(t, rid)
			if got.Result != tc.wantResult || got.Consumed != tc.wantConsume {
				t.Fatalf("row = %s/%d; want %s/%d", got.Result, got.Consumed,
					tc.wantResult, tc.wantConsume)
			}
			if !got.CreatedAt.UTC().Truncate(time.Millisecond).Equal(at) {
				t.Fatalf("created_at = %s; want the first write %s (R-05 depends on it)",
					got.CreatedAt, at)
			}
			// The third completeness dimension: the SET clause must carry EVERY
			// INSERT column, not just the ones the other assertions happen to
			// look at. A column missing from usageUpdateSet leaves a mixed row
			// (result=SUCCESS with tokens 0) that passes result/consumed. The
			// old rows are written with tokens 0/0 and the new row with 7/3, so
			// this pair distinguishes "was overwritten" from "was left alone".
			wantIn, wantOut := int64(0), int64(0)
			if tc.covered {
				wantIn, wantOut = 7, 3
			}
			if got.TokensIn != wantIn || got.TokensOut != wantOut {
				t.Fatalf("tokens = %d/%d; want %d/%d (usageUpdateSet must cover every INSERT column)",
					got.TokensIn, got.TokensOut, wantIn, wantOut)
			}
		})
	}
}

// V3 green case: a zero-cost SUCCESS row is a legitimate internal-group record,
// so it survives a later charged write on the same id (cell 3 of P1, spelled out
// with its own rationale). Synthetic — see the reachability caveat on P1.
func TestInsertUsageKeepsZeroCostSuccess(t *testing.T) {
	rid := newRequestID(t)
	const user = int64(970004)
	cleanupUsage(t, rid)
	t.Cleanup(func() { cleanupUsage(t, rid) })

	w := NewUsageWriter(testDB, nil, clock.Real{})
	at := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Millisecond)
	free := settleRow(rid, user, 0, 0, 0, at)
	writeUsage(t, w, free)
	// A later row for the same id, this time with money. The predicate must not
	// fire: the existing row is SUCCESS, not a failure.
	writeUsage(t, w, settleRow(rid, user, 900, 99, 99, at.Add(time.Minute)))

	got := readUsageRow(t, rid)
	if got.Consumed != 0 || got.TokensIn != 0 || got.Result != "SUCCESS" {
		t.Fatalf("row = %+v; a zero-cost SUCCESS row must not be overwritten", got)
	}
}

// The new-row side accepts any result as long as it consumed credits: an
// interrupted stream settles as PARTIAL and must still replace a stale
// FAILED/0 row.
func TestInsertUsagePartialSettlementOverwrites(t *testing.T) {
	rid := newRequestID(t)
	const user = int64(970005)
	cleanupUsage(t, rid)
	t.Cleanup(func() { cleanupUsage(t, rid) })

	w := NewUsageWriter(testDB, nil, clock.Real{})
	at := time.Now().UTC().Add(-4 * time.Minute)
	writeUsage(t, w, rejectRow(rid, user, "rate_limited", at))

	partial := settleRow(rid, user, 300, 10, 4, at.Add(time.Minute))
	partial.Result = "PARTIAL"
	partial.HTTPStatus = 200
	writeUsage(t, w, partial)

	got := readUsageRow(t, rid)
	if got.Result != "PARTIAL" || got.Consumed != 300 {
		t.Fatalf("row = %+v; want PARTIAL/300", got)
	}
}

// The ordinary path must be untouched: one fresh row, no overwrite in play.
func TestInsertUsageFreshRowUnaffected(t *testing.T) {
	rid := newRequestID(t)
	const user = int64(970006)
	cleanupUsage(t, rid)
	t.Cleanup(func() { cleanupUsage(t, rid) })

	w := NewUsageWriter(testDB, nil, clock.Real{})
	// created_at is datetime(3): keep the fixture at millisecond precision so the
	// comparison is about the value, not about rounding.
	at := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Millisecond)
	writeUsage(t, w, settleRow(rid, user, 500, 20, 30, at))

	got := readUsageRow(t, rid)
	if got.Result != "SUCCESS" || got.Consumed != 500 || got.UserID != user || got.TokensIn != 20 {
		t.Fatalf("row = %+v; want the row as written", got)
	}
	if !got.CreatedAt.UTC().Truncate(time.Millisecond).Equal(at) {
		t.Fatalf("created_at = %s; want %s", got.CreatedAt, at)
	}
}

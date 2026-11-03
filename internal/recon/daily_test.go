package recon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/lock"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

// wantDiff is one expected row from a check.
type wantDiff struct {
	DiffType, Subject, MatchMode, Level string
	Local, Upstream, Delta              int64
}

// cleanReconScope removes every row the daily fixtures can leave behind for
// the half-open user range [lo, hi). It runs before every case and again from
// t.Cleanup, so a case that fails halfway through cannot poison the next run.
//
// The table list is the union of every table this package's fixtures insert
// into — insertAccount, insertLedgerEntry, insertUsage, insertAttempt, the
// inline R-04..R-08 inserts — plus the two tables a Daily run writes. Metric
// tables are keyed by dimension id, not user_id, so they get their own
// predicates. t_reconcile_diff has neither user_id nor subject columns; it is
// bounded by biz_date (see the comment at the DELETE).
func cleanReconScope(t *testing.T, lo, hi int64, bizDates ...string) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{
		"t_upstream_attempt",
		"t_usage_record",
		"t_settle_task",
		"t_prehold_hold",
		"t_ledger_entry",
		"t_credit_batch",
		"t_credit_account",
		"t_user_daily_quota",
		"t_idempotency_record",
		"t_notification",
	} {
		if _, err := testDB.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE user_id >= ? AND user_id < ?`, lo, hi); err != nil {
			t.Fatalf("cleanReconScope %s: %v", table, err)
		}
	}
	for _, q := range []string{
		`DELETE FROM t_metric_model_daily WHERE model_id >= ? AND model_id < ?`,
		`DELETE FROM t_metric_minute WHERE scope_id >= ? AND scope_id < ?`,
		`DELETE FROM t_metric_daily WHERE scope_id >= ? AND scope_id < ?`,
		`DELETE FROM t_metric_channel_health WHERE channel_id >= ? AND channel_id < ?`,
	} {
		if _, err := testDB.ExecContext(ctx, q, lo, hi); err != nil {
			t.Fatalf("cleanReconScope metrics: %v\nquery: %s", err, q)
		}
	}
	for _, bizDate := range bizDates {
		// t_reconcile_diff has no subject column: the subject is folded into
		// diff_key = sha256(biz_date|diff_type|subject), and R-03's subject is
		// a ledger entry id, not a user id, so no numeric range exists to
		// filter on. Only this package writes the table and every test owns a
		// distinct biz_date, so the date is the tightest usable bound.
		if _, err := testDB.ExecContext(ctx,
			`DELETE FROM t_reconcile_diff WHERE biz_date = ?`, bizDate); err != nil {
			t.Fatalf("cleanReconScope t_reconcile_diff: %v", err)
		}
		// Daily writes its alert rows with user_id = 0 and puts biz_date first
		// in the body. Matching both keeps another package's notifications.
		if _, err := testDB.ExecContext(ctx,
			`DELETE FROM t_notification
			  WHERE user_id = 0
			    AND event_code IN ('RECONCILE_ALERT', 'RECONCILE_P0')
			    AND body LIKE CONCAT('biz_date=', ?, ' %')`, bizDate); err != nil {
			t.Fatalf("cleanReconScope t_notification: %v", err)
		}
	}
}

// A7: R-01..R-08 are table-driven. Each case seeds exactly one violation,
// runs its check, asserts the stored shape (diff_type / match_mode / level /
// delta) and removes the fixture before the next case.
func TestDailyChecksR01toR08(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, CST)
	bizDate := day.Format("2006-01-02")
	dayStart, dayEnd := day, day.AddDate(0, 0, 1)
	d := NewDaily(testDB, nil, nil, clock.Real{}, "a7")
	d.IdemThreshold = 1
	// R-01..R-07 scan whole tables. yunshu_test accumulates fixtures from other
	// packages, so every case is limited to this test's user band.
	d.UserLo, d.UserHi = 901000, 901100

	checks := map[string]Check{}
	for _, c := range d.Checks() {
		checks[c.Code] = c
	}

	cases := []struct {
		name  string
		code  string
		setup func(t *testing.T) []wantDiff
	}{
		{
			name: "R01 balance != ledger sum", code: DiffR01,
			setup: func(t *testing.T) []wantDiff {
				insertAccount(t, 901001, 1000, 0)
				return []wantDiff{{DiffR01, "901001", MatchModeInternal, LevelCritical, 1000, 0, 1000}}
			},
		},
		{
			name: "R02 balance != recharge+bonus", code: DiffR02,
			setup: func(t *testing.T) []wantDiff {
				insertAccountValues(t, 901002, 600, 300, 1000)
				// Batch sum matches the split columns (900), so only the §2-2
				// assertion fires; §2-3 is a separate case.
				insertBatch(t, 901002, 900, 900, 0)
				return []wantDiff{{DiffR02, "901002", MatchModeInternal, LevelCritical, 1000, 900, 100}}
			},
		},
		{
			name: "R03 ledger continuity break", code: DiffR03,
			setup: func(t *testing.T) []wantDiff {
				req := rid("r03x", 1)
				insertLedgerEntry(t, 901003, entryNo("R03", 1), "RECHARGE",
					"CREDIT", 0, 5000, 5000, 0, 5000, "r03:1", &req)
				insertLedgerEntry(t, 901003, entryNo("R03", 2), "CONSUME_SETTLE",
					"DEBIT", 0, 0, 0, 4000, 4000, "r03:2", &req)
				var prevID int64
				if err := testDB.QueryRow(`SELECT id FROM t_ledger_entry WHERE entry_no = ?`,
					entryNo("R03", 1)).Scan(&prevID); err != nil {
					t.Fatalf("read R03 entry: %v", err)
				}
				return []wantDiff{{DiffR03, fmt.Sprintf("%d", prevID), MatchModeInternal, LevelCritical, 5000, 4000, -1000}}
			},
		},
		{
			name: "R04 batch remaining != face - consumed", code: DiffR04,
			setup: func(t *testing.T) []wantDiff {
				insertAccount(t, 901004, 5000, 0)
				insertLedgerEntry(t, 901004, entryNo("R04", 1), "RECHARGE",
					"CREDIT", 0, 5000, 5000, 0, 5000, "r04:1", nil)
				execFixture(t,
					`INSERT INTO t_credit_batch
					   (user_id, batch_no, batch_type, face_value_micro, remaining_micro,
					    consumed_micro, refundable, effective_at, created_at, updated_at)
					 VALUES (?, '20261020000000000001', 'BONUS', 10000, 5000, 3000, 0, ?, ?, ?)`,
					901004, time.Now().UTC(), time.Now().UTC(), time.Now().UTC())
				var id int64
				if err := testDB.QueryRow(`SELECT id FROM t_credit_batch WHERE user_id = ?`, 901004).Scan(&id); err != nil {
					t.Fatalf("read R04 batch: %v", err)
				}
				return []wantDiff{{DiffR04, fmt.Sprintf("%d", id), MatchModeInternal, LevelHigh, 5000, 7000, -2000}}
			},
		},
		{
			name: "R05 quota vs usage two tiers", code: DiffR05,
			setup: func(t *testing.T) []wantDiff {
				now := dayStart.Add(10 * time.Hour)
				for _, row := range []struct {
					user  int64
					used  int64
					usage int64
				}{
					{901005, 500, 300}, // delta 200  -> HIGH
					{901007, 20000, 0}, // delta 20000 -> CRITICAL
					{901008, 50, 0},    // delta 50   -> below the 100 threshold
				} {
					execFixture(t,
						`INSERT INTO t_user_daily_quota
						   (user_id, stat_date, limit_micro, used_total_micro, reset_at, created_at, updated_at)
						 VALUES (?,?,1000000,?,?,?,?)`,
						row.user, bizDate, row.used, dayEnd.UTC(), now.UTC(), now.UTC())
					if row.usage > 0 {
						insertUsage(t, rid("r05u", int(row.user%1000)), row.user, now, "SUCCESS", row.usage)
					}
				}
				return []wantDiff{
					{DiffR05, "901005", MatchModeInternal, LevelHigh, 500, 300, 200},
					{DiffR05, "901007", MatchModeInternal, LevelCritical, 20000, 0, 20000},
				}
			},
		},
		{
			name: "R06 expired HELD older than one hour", code: DiffR06,
			setup: func(t *testing.T) []wantDiff {
				execFixture(t,
					`INSERT INTO t_prehold_hold
					   (request_id, user_id, api_key_id, hold_micro, estimated_micro, model_id,
					    state, expires_at, node_id, created_at)
					 VALUES (?, 901006, 1, 1000, 1000, 1, 'HELD', ?, 'a7', ?)`,
					rid("r06x", 1), time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-2*time.Hour))
				return []wantDiff{{DiffR06, "GLOBAL", MatchModeInternal, LevelHigh, 1, 0, 1}}
			},
		},
		{
			name: "R07 expired idempotency above threshold", code: DiffR07,
			setup: func(t *testing.T) []wantDiff {
				for i := 1; i <= 2; i++ {
					execFixture(t,
						`INSERT INTO t_idempotency_record
						   (scope, idem_key, user_id, api_key_id, request_id, state, created_at, expires_at)
						 VALUES ('CLIENT_REQUEST', ?, 901009, 1, ?, 'DONE', ?, ?)`,
						fmt.Sprintf("r07-%d-%s", i, randomSuffix(t)),
						rid("r07x", i), time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-time.Hour))
				}
				return []wantDiff{{DiffR07, "GLOBAL", MatchModeInternal, LevelMedium, 2, 1, 1}}
			},
		},
		{
			name: "R08 gross != consumed - cost", code: DiffR08,
			setup: func(t *testing.T) []wantDiff {
				execFixture(t,
					`INSERT INTO t_metric_model_daily
					   (stat_date, model_id, request_total, consumed_micro,
					    cost_micro, cost_configured, gross_profit_micro)
					 VALUES (?, 901010, 1, 10000, 3000, 1, 8000)`, bizDate)
				return []wantDiff{{DiffR08, "901010", MatchModeInternal, LevelHigh, 8000, 7000, 1000}}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Every case starts from an empty band and leaves one behind, so
			// -count=3 and a run that dies mid-case both start clean.
			cleanReconScope(t, d.UserLo, d.UserHi, bizDate)
			t.Cleanup(func() { cleanReconScope(t, d.UserLo, d.UserHi, bizDate) })
			wants := tc.setup(t)
			check, ok := checks[tc.code]
			if !ok {
				t.Fatalf("check %s not registered", tc.code)
			}
			diffs, err := check.Run(ctx, d, bizDate, dayStart, dayEnd)
			if err != nil {
				t.Fatalf("%s: %v", tc.code, err)
			}
			if len(diffs) != len(wants) {
				t.Fatalf("%s diffs = %d (%+v); want %d", tc.code, len(diffs), diffs, len(wants))
			}
			for i, want := range wants {
				got := diffs[i]
				if got.DiffType != want.DiffType || got.Subject != want.Subject ||
					got.MatchMode != want.MatchMode || got.Level != want.Level ||
					got.LocalValue != want.Local || got.UpstreamValue != want.Upstream ||
					got.DeltaMicro != want.Delta {
					t.Fatalf("%s diff[%d] = %+v; want %+v", tc.code, i, got, want)
				}
				if len(got.DiffType) > 24 {
					t.Fatalf("%s diff_type %q exceeds varchar(24)", tc.code, got.DiffType)
				}
			}
			t.Logf("%s stored diff_type=%s match_mode=%s level=%s subject=%s delta=%d",
				tc.code, wants[0].DiffType, wants[0].MatchMode, wants[0].Level, wants[0].Subject, wants[0].Delta)
		})
	}
}

// A8: rerun convergence and the K21 guard.
func TestDailyRerunConvergesAndKeepsClosedRowsClosed(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 9, 15, 0, 0, 0, 0, CST)
	bizDate := day.Format("2006-01-02")
	rdb := redistest.NewClient(0)
	t.Cleanup(func() {
		_ = rdb.Raw().Del(context.Background(), redisx.ReconLockKey(bizDate)).Err()
		_ = rdb.Close()
	})

	const user = int64(902001)
	cleanReconScope(t, 902000, 902100, bizDate)
	insertAccount(t, user, 5000, 0)
	insertBatch(t, user, 5000, 5000, 0) // keep settlement §2-3 satisfied
	key := DiffKey(bizDate, DiffR01, "902001")
	t.Cleanup(func() { cleanReconScope(t, 902000, 902100, bizDate) })

	locker := lock.New(rdb, nil)
	run := func(owner string) DailyResult {
		t.Helper()
		d := NewDaily(testDB, locker, nil, clock.Real{}, owner)
		d.UserLo, d.UserHi = 902000, 902100
		res, err := d.Run(ctx, day)
		if err != nil {
			t.Fatalf("run %s: %v", owner, err)
		}
		return res
	}
	resetLock := func() {
		t.Helper()
		if err := rdb.Raw().Del(context.Background(), redisx.ReconLockKey(bizDate)).Err(); err != nil {
			t.Fatalf("reset K21: %v", err)
		}
	}

	first := run("instance-1")
	if first.Skipped || first.Recorded != 1 || first.Changed != 1 {
		t.Fatalf("first run = %+v; want recorded=1 changed=1", first)
	}
	second := run("instance-2")
	if !second.Skipped {
		t.Fatalf("second run did not observe K21: %+v", second)
	}
	resetLock()
	third := run("instance-3")
	if third.Skipped || third.Recorded != 1 || third.Changed != 0 {
		t.Fatalf("third run = %+v; want recorded=1 changed=0", third)
	}

	execFixture(t, `UPDATE t_reconcile_diff SET status='CLOSED' WHERE diff_key = ?`, key)
	resetLock()
	fourth := run("instance-4")
	if fourth.Changed != 0 {
		t.Fatalf("identical rerun changed a CLOSED row: %+v", fourth)
	}
	var status string
	var count int
	if err := testDB.QueryRow(
		`SELECT status FROM t_reconcile_diff WHERE diff_key = ?`, key).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM t_reconcile_diff WHERE biz_date = ?`, bizDate).Scan(&count); err != nil {
		t.Fatalf("count diffs: %v", err)
	}
	if status != "CLOSED" || count != 1 {
		t.Fatalf("after identical rerun status=%s rows=%d; want CLOSED/1", status, count)
	}

	// A changed delta reopens the row in place; it must not insert a second one.
	// Re-seed the account instead of updating the balance in place.
	deleteAccount(t, user)
	insertAccountValues(t, user, 6000, 0, 6000)
	insertBatch(t, user, 6000, 6000, 0) // keep settlement §2-3 satisfied
	resetLock()
	fifth := run("instance-5")
	if fifth.Changed != 1 {
		t.Fatalf("changed delta run = %+v; want changed=1", fifth)
	}
	var opened string
	var local int64
	if err := testDB.QueryRow(
		`SELECT status, local_value FROM t_reconcile_diff WHERE diff_key = ?`, key).Scan(&opened, &local); err != nil {
		t.Fatalf("read reopened row: %v", err)
	}
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM t_reconcile_diff WHERE biz_date = ?`, bizDate).Scan(&count); err != nil {
		t.Fatalf("count diffs: %v", err)
	}
	if opened != "OPEN" || local != 6000 || count != 1 {
		t.Fatalf("after delta change status=%s local=%d rows=%d; want OPEN/6000/1", opened, local, count)
	}
	t.Logf("A8 first(changed=%d) replay(changed=%d skipped=%t) closed-kept status=%s reopened=%s rows=%d",
		first.Changed, third.Changed, second.Skipped, status, opened, count)
}

// A14: 1,000 settled requests, 3,000+ ledger entries, one matching quota
// snapshot. The kernel must report zero differences.
func TestDailyThousandRequestsZeroDiffs(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 8, 20, 0, 0, 0, 0, CST)
	bizDate := day.Format("2006-01-02")
	const user = int64(903001)

	cleanReconScope(t, 903000, 903100, bizDate)
	insertAccount(t, user, 300000, 0)
	t.Cleanup(func() { cleanReconScope(t, 903000, 903100, bizDate) })

	// Opening balance must itself be a ledger row; otherwise R-01 cannot hold.
	insertLedgerEntry(t, user, entryNo("A14", 0), "RECHARGE", "CREDIT",
		0, 1000000, 1000000, 0, 1000000, "a14:open", nil)
	// settlement §2-3: the recharge is backed by one batch whose remaining
	// value equals the final balance (1,000,000 - 700,000 consumed).
	insertBatch(t, user, 1000000, 300000, 700000)

	const requests = 1000
	var ledger []ledgerFixture
	balance := int64(1_000_000)
	for i := 0; i < requests; i++ {
		req := rid("a14r", i)
		// request_id is set on every request-scoped row, as ledger.Settle and
		// ledger.Prehold do in production. settlement §2-4 (business doc) and
		// §2-5 (unsettled success) both key on it.
		ledger = append(ledger,
			ledgerFixture{entryNo("A14", 3*i+1), "CONSUME_PREHOLD", "DEBIT", "a14:pre:" + req,
				-1000, -1000, balance, balance - 1000, &req},
			ledgerFixture{entryNo("A14", 3*i+2), "PREHOLD_RELEASE", "CREDIT", "a14:rel:" + req,
				1000, 1000, balance - 1000, balance, &req},
			ledgerFixture{entryNo("A14", 3*i+3), "CONSUME_SETTLE", "DEBIT", "a14:set:" + req,
				-700, -700, balance, balance - 700, &req},
		)
		balance -= 700
	}
	insertLedgerBatch(t, user, ledger)
	insertUsageBatch(t, user, requests, day.Add(12*time.Hour), 700)

	execFixture(t,
		`INSERT INTO t_user_daily_quota
		   (user_id, stat_date, limit_micro, used_recharge_micro, used_total_micro,
		    reset_at, created_at, updated_at)
		 VALUES (?,?,1000000,700000,700000,?,?,?)`,
		user, bizDate, day.AddDate(0, 0, 1).UTC(), time.Now().UTC(), time.Now().UTC())

	d := NewDaily(testDB, nil, nil, clock.Real{}, "a14")
	d.UserLo, d.UserHi = 903000, 903100
	res, err := d.Run(ctx, day)
	if err != nil {
		t.Fatalf("daily run: %v", err)
	}
	if res.Internal != 0 || res.External != 0 || res.Recorded != 0 || res.P0 {
		t.Fatalf("A14 result = %+v; want zero diffs", res)
	}
	var stored int
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM t_reconcile_diff WHERE biz_date = ?`, bizDate).Scan(&stored); err != nil {
		t.Fatalf("count diffs: %v", err)
	}
	if stored != 0 {
		t.Fatalf("A14 stored diffs = %d; want 0", stored)
	}
	var sum, account int64
	if err := testDB.QueryRow(
		`SELECT COALESCE(SUM(amount_total_micro),0) FROM t_ledger_entry WHERE user_id = ?`, user).Scan(&sum); err != nil {
		t.Fatalf("sum ledger: %v", err)
	}
	if err := testDB.QueryRow(
		`SELECT balance_micro FROM t_credit_account WHERE user_id = ?`, user).Scan(&account); err != nil {
		t.Fatalf("read account: %v", err)
	}
	if sum != account || sum != 300000 {
		t.Fatalf("ledger sum=%d account=%d; want 300000", sum, account)
	}
	t.Logf("A14 requests=%d ledger_entries=%d internal=%d external=%d stored_diffs=%d sum=%d",
		requests, len(ledger)+1, res.Internal, res.External, stored, sum)
}

type ledgerFixture struct {
	no, biz, dir, idem             string
	recharge, total, before, after int64
	requestID                      *string
}

func insertLedgerBatch(t *testing.T, userID int64, rows []ledgerFixture) {
	t.Helper()
	const perBatch = 500
	now := time.Now().UTC()
	for start := 0; start < len(rows); start += perBatch {
		end := start + perBatch
		if end > len(rows) {
			end = len(rows)
		}
		var b strings.Builder
		b.WriteString(`INSERT INTO t_ledger_entry
		  (user_id, entry_no, account_type, direction, biz_type,
		   amount_bonus_micro, amount_recharge_micro, amount_total_micro,
		   balance_before_micro, balance_after_micro, request_id, idem_key, created_at) VALUES `)
		args := make([]any, 0, (end-start)*13)
		for i := start; i < end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			b.WriteString(`(?,?,'RECHARGE',?,?,0,?,?,?,?,?,?,?)`)
			r := rows[i]
			args = append(args, userID, r.no, r.dir, r.biz, r.recharge, r.total,
				r.before, r.after, r.requestID, r.idem, now)
		}
		if _, err := testDB.ExecContext(context.Background(), b.String(), args...); err != nil {
			t.Fatalf("insert ledger batch: %v", err)
		}
	}
}

func insertUsageBatch(t *testing.T, userID int64, n int, at time.Time, consumed int64) {
	t.Helper()
	const perBatch = 500
	for start := 0; start < n; start += perBatch {
		end := start + perBatch
		if end > n {
			end = n
		}
		var b strings.Builder
		b.WriteString(`INSERT INTO t_usage_record
		  (request_id, user_id, api_key_id, api_key_name_snap, model_name_snap,
		   request_model_req, inbound_protocol, http_status, result,
		   consumed_micro, total_ms, created_at) VALUES `)
		args := make([]any, 0, (end-start)*5)
		for i := start; i < end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			b.WriteString(`(?,?,1,'fixture','fixture-model','fixture-model','OPENAI_COMPAT',200,'SUCCESS',?,1,?)`)
			args = append(args, rid("a14r", i), userID, consumed, at.UTC())
		}
		if _, err := testDB.ExecContext(context.Background(), b.String(), args...); err != nil {
			t.Fatalf("insert usage batch: %v", err)
		}
	}
}

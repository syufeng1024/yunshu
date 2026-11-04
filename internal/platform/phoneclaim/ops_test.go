package phoneclaim

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/platform/phonehmac"
)

// opsUser is one fixture account for the operator commands.
type opsUser struct {
	uid   int64
	hmac  string
	phone string
}

// insertOpsUser creates a t_user row shaped like the states the commands care
// about, with a ledger row only when withClaim is true.
func insertOpsUser(t *testing.T, status, source, phone string, withClaim bool, trialAt sql.NullTime) opsUser {
	t.Helper()
	var hmac string
	if phone != "" {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatalf("random hmac: %v", err)
		}
		hmac = hex.EncodeToString(b[:])
	}
	return insertOpsUserWithHMAC(t, status, source, phone, hmac, withClaim, trialAt)
}

// insertOpsUserWithHMAC is insertOpsUser for a fixture whose hash must match a
// known key (the support-lookup test).
func insertOpsUserWithHMAC(t *testing.T, status, source, phone, hmac string, withClaim bool, trialAt sql.NullTime) opsUser {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	userNo := fmt.Sprintf("O%011d", now.UnixNano()%100000000000)
	var cancelAt, disabledAt any
	if status == "DISABLED" {
		cancelAt, disabledAt = now, now
	}
	res, err := testDB.ExecContext(ctx, `
		INSERT INTO t_user
			(user_no, phone, phone_hmac, password_hash, password_algo, password_updated_at,
			 nickname, role, status, disable_source, cancel_apply_at, disabled_at,
			 trial_claimed_at, created_at, updated_at)
		VALUES (?,?,?,'x','bcrypt',?,'ops', 'PERSONAL', ?, ?, ?, ?, ?, ?, ?)`,
		userNo, nullable(phone), nullable(hmac), now, status, nullable(source),
		cancelAt, disabledAt, nullTime(trialAt), now, now)
	if err != nil {
		t.Fatalf("insert ops user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("ops user id: %v", err)
	}
	u := opsUser{uid: uid, hmac: hmac, phone: phone}
	if withClaim && hmac != "" {
		if _, err := testDB.ExecContext(ctx, `
			INSERT INTO t_phone_claim
			  (phone_hmac, first_user_id, last_claim_user_id, last_claim_at,
			   cancel_count, created_at, updated_at)
			VALUES (?,?,?,?,0,?,?)`, hmac, uid, uid, now, now, now); err != nil {
			t.Fatalf("seed claim: %v", err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = testDB.ExecContext(c, `DELETE FROM t_phone_claim WHERE first_user_id = ? OR last_claim_user_id = ? OR last_trial_user_id = ?`, uid, uid, uid)
		if hmac != "" {
			_, _ = testDB.ExecContext(c, `DELETE FROM t_phone_claim WHERE phone_hmac = ?`, hmac)
		}
		_, _ = testDB.ExecContext(c, `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return u
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func mustNullTime(t *testing.T) sql.NullTime {
	t.Helper()
	// datetime(3) keeps milliseconds: truncate here so Equal() compares what the
	// column can actually store.
	return sql.NullTime{Time: time.Now().UTC().AddDate(0, 0, -5).Truncate(time.Millisecond), Valid: true}
}

// AC-11 / AC-13 for backfill: detect sees the missing row, dry run writes
// nothing, commit writes the history, and a rerun is a no-op.
func TestBackfillCopiesHistoryAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	trialAt := mustNullTime(t)
	u := insertOpsUser(t, "NORMAL", "", "8613800700001", false, trialAt)

	before, err := Detect(ctx, testDB)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if before.PendingBackfill < 1 {
		t.Fatalf("pending backfill = %d, want ≥ 1 for the new row", before.PendingBackfill)
	}

	dry, err := Backfill(ctx, testDB, Options{})
	if err != nil {
		t.Fatalf("backfill dry-run: %v", err)
	}
	if dry.Changed != 0 || dry.Pending < 1 {
		t.Fatalf("dry run changed=%d pending=%d, want 0/≥1", dry.Changed, dry.Pending)
	}
	if _, err := Get(ctx, testDB, u.hmac); err == nil {
		t.Fatal("dry run wrote a ledger row")
	}

	committed, err := Backfill(ctx, testDB, Options{Commit: true})
	if err != nil {
		t.Fatalf("backfill commit: %v", err)
	}
	if committed.Changed < 1 {
		t.Fatalf("commit changed=%d, want ≥ 1", committed.Changed)
	}
	claim, err := Get(ctx, testDB, u.hmac)
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if claim.FirstUserID != u.uid {
		t.Fatalf("first_user_id = %d, want %d", claim.FirstUserID, u.uid)
	}
	if !claim.TrialGrantedAt.Valid || !claim.TrialGrantedAt.Time.Equal(trialAt.Time) {
		t.Fatalf("trial_granted_at = %+v, want %s", claim.TrialGrantedAt, trialAt.Time)
	}
	if !claim.LastTrialUserID.Valid || claim.LastTrialUserID.Int64 != u.uid {
		t.Fatalf("last_trial_user_id = %+v, want %d", claim.LastTrialUserID, u.uid)
	}

	again, err := Backfill(ctx, testDB, Options{Commit: true})
	if err != nil {
		t.Fatalf("backfill rerun: %v", err)
	}
	if again.Changed != 0 {
		t.Fatalf("rerun changed=%d, want 0 (idempotent)", again.Changed)
	}
	if pending, err := countPendingBackfill(ctx, testDB); err != nil || pending != 0 {
		t.Fatalf("pending after backfill = %d err=%v, want 0", pending, err)
	}
}

// The gate: release refuses while any number is missing from the ledger, and
// says how many.
func TestReleaseRefusesBeforeBackfill(t *testing.T) {
	ctx := context.Background()
	u := insertOpsUser(t, "DISABLED", "SELF_CANCEL", "8613800700002", false, sql.NullTime{})

	_, err := Release(ctx, testDB, Options{Commit: true})
	var order *OrderViolation
	if !errors.As(err, &order) {
		t.Fatalf("release err = %v, want OrderViolation", err)
	}
	if order.ExitCode() != ExitOrderViolation {
		t.Fatalf("exit code = %d, want %d", order.ExitCode(), ExitOrderViolation)
	}

	// Clean up the fixture so later assertions in this package are not affected.
	if _, err := Backfill(ctx, testDB, Options{Commit: true}); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	released, err := Release(ctx, testDB, Options{Commit: true})
	if err != nil {
		t.Fatalf("release after backfill: %v", err)
	}
	if released.Changed < 1 {
		t.Fatalf("release changed=%d, want ≥ 1 (uid=%d)", released.Changed, u.uid)
	}
	assertReleased(t, u.uid)
}

// AC-11: a released row loses both phone columns, and the ledger keeps the
// history — including the trial that the number consumed.
func TestReleaseRecordsHistoryAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	trialAt := mustNullTime(t)
	u := insertOpsUser(t, "DISABLED", "SELF_CANCEL", "8613800700003", true, trialAt)

	first, err := Release(ctx, testDB, Options{Commit: true})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if first.Changed < 1 {
		t.Fatalf("changed=%d, want ≥ 1", first.Changed)
	}
	assertReleased(t, u.uid)

	claim, err := Get(ctx, testDB, u.hmac)
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if claim.CancelCount != 1 {
		t.Fatalf("cancel_count = %d, want 1", claim.CancelCount)
	}
	if !claim.TrialGrantedAt.Valid || !claim.TrialGrantedAt.Time.Equal(trialAt.Time) {
		t.Fatalf("trial_granted_at = %+v, want the account's own claim time", claim.TrialGrantedAt)
	}
	if !claim.LastCancelAt.Valid || claim.LastCancelAt.Time.IsZero() {
		t.Fatalf("last_cancel_at = %+v, want a timestamp", claim.LastCancelAt)
	}

	second, err := Release(ctx, testDB, Options{Commit: true})
	if err != nil {
		t.Fatalf("release rerun: %v", err)
	}
	if second.Changed != 0 {
		t.Fatalf("rerun changed=%d, want 0 (idempotent)", second.Changed)
	}
	if claim2, err := Get(ctx, testDB, u.hmac); err != nil || claim2.CancelCount != 1 {
		t.Fatalf("cancel_count after rerun = %d err=%v, want 1", claim2.CancelCount, err)
	}
}

// AC-12: the boundary. Only SELF_CANCEL is released; an operator ban, a risk
// ban and an overdraft freeze all keep the number occupied.
func TestReleaseLeavesNonSelfCancelRowsOccupied(t *testing.T) {
	ctx := context.Background()
	ops := insertOpsUser(t, "DISABLED", "OPS_MANUAL", "8613800700004", true, sql.NullTime{})
	risk := insertOpsUser(t, "DISABLED", "RISK_AUTO", "8613800700005", true, sql.NullTime{})
	frozen := insertOpsUser(t, "FROZEN_OVERDRAFT", "", "8613800700006", true, sql.NullTime{})

	report, err := Release(ctx, testDB, Options{Commit: true})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	for _, u := range []opsUser{ops, risk, frozen} {
		var hmac sql.NullString
		if err := testDB.QueryRowContext(ctx,
			`SELECT phone_hmac FROM t_user WHERE id = ?`, u.uid).Scan(&hmac); err != nil {
			t.Fatalf("read uid %d: %v", u.uid, err)
		}
		if !hmac.Valid || hmac.String != u.hmac {
			t.Fatalf("uid %d lost its phone_hmac (got %v); only SELF_CANCEL may be released",
				u.uid, hmac)
		}
	}
	if report.Changed != 0 {
		t.Fatalf("changed=%d for a batch with no SELF_CANCEL row, want 0", report.Changed)
	}

	// verify must report the over-release that a hand edit produces: the hash was
	// cleared while the plaintext stayed behind. (Clearing both columns is not
	// assertable — see overReleasedWhere.)
	if _, err := testDB.ExecContext(ctx,
		`UPDATE t_user SET phone_hmac = NULL WHERE id = ?`, ops.uid); err != nil {
		t.Fatalf("simulate over-release: %v", err)
	}
	before, err := Verify(ctx, testDB)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if before.OverReleased < 1 || !before.Blocking() {
		t.Fatalf("verify over_released=%d blocking=%v, want ≥ 1 and blocking",
			before.OverReleased, before.Blocking())
	}
}

// QA-B (red line): a banned account that never had a number is NOT an
// over-release.
//
// Every e-mail account an operator bans carries disable_source='OPS_MANUAL'
// (internal/control/ops/users.go), and a release leaves exactly the same two
// NULLs. Counting both-NULL made the runbook's "四条断言全 0" unsatisfiable on
// any deployment with one banned e-mail account, which is how a gate stops being
// read. The predicate therefore requires the plaintext to still be there.
func TestBannedAccountWithoutPhoneIsNotOverReleased(t *testing.T) {
	ctx := context.Background()
	beforeDetect, err := Detect(ctx, testDB)
	if err != nil {
		t.Fatalf("detect before: %v", err)
	}
	beforeVerify, err := Verify(ctx, testDB)
	if err != nil {
		t.Fatalf("verify before: %v", err)
	}

	uid := insertEmailOnlyUser(t, "OPS_MANUAL")
	riskUID := insertEmailOnlyUser(t, "RISK_AUTO")

	afterDetect, err := Detect(ctx, testDB)
	if err != nil {
		t.Fatalf("detect after: %v", err)
	}
	if afterDetect.OverReleased != beforeDetect.OverReleased {
		t.Fatalf("detect over_released moved %d → %d after adding e-mail-only bans",
			beforeDetect.OverReleased, afterDetect.OverReleased)
	}
	for _, a := range afterDetect.Anomalies {
		if a.UID == uid || a.UID == riskUID {
			t.Fatalf("uid %d reported as %s; it never had a number", a.UID, a.Kind)
		}
	}

	afterVerify, err := Verify(ctx, testDB)
	if err != nil {
		t.Fatalf("verify after: %v", err)
	}
	if afterVerify.OverReleased != beforeVerify.OverReleased {
		t.Fatalf("verify over_released moved %d → %d after adding e-mail-only bans",
			beforeVerify.OverReleased, afterVerify.OverReleased)
	}
}

// insertEmailOnlyUser is a banned account with no handset at all: the shape the
// over-release predicate must ignore.
func insertEmailOnlyUser(t *testing.T, source string) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	userNo := fmt.Sprintf("E%011d", now.UnixNano()%100000000000)
	res, err := testDB.ExecContext(ctx, `
		INSERT INTO t_user
			(user_no, email, password_hash, password_algo, password_updated_at,
			 nickname, role, status, disable_source, disabled_at, created_at, updated_at)
		VALUES (?,?, 'x', 'bcrypt', ?, 'ops', 'PERSONAL', 'DISABLED', ?, ?, ?, ?)`,
		userNo, userNo+"@test.local", now, source, now, now, now)
	if err != nil {
		t.Fatalf("insert e-mail-only user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("id: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

// --uid is the recovery path for the "plaintext without hash" state: it must
// release exactly one account, even when other candidates exist.
func TestReleaseUIDTargetsOneRow(t *testing.T) {
	ctx := context.Background()
	target := insertOpsUser(t, "DISABLED", "SELF_CANCEL", "8613800700007", true, sql.NullTime{})
	other := insertOpsUser(t, "DISABLED", "SELF_CANCEL", "8613800700008", true, sql.NullTime{})

	report, err := Release(ctx, testDB, Options{Commit: true, UID: target.uid})
	if err != nil {
		t.Fatalf("release --uid: %v", err)
	}
	if report.Changed != 1 {
		t.Fatalf("changed=%d, want exactly 1", report.Changed)
	}
	assertReleased(t, target.uid)
	var hmac sql.NullString
	if err := testDB.QueryRowContext(ctx,
		`SELECT phone_hmac FROM t_user WHERE id = ?`, other.uid).Scan(&hmac); err != nil {
		t.Fatalf("read other: %v", err)
	}
	if !hmac.Valid {
		t.Fatal("--uid released a different account")
	}
}

// Lookup answers the support question and prints only masked values. The
// fixture's hash is computed with the key the command is given, because that is
// the only way a support lookup can ever find a row: the key is the address.
func TestLookupReturnsLedgerAndHolder(t *testing.T) {
	ctx := context.Background()
	key := "phoneclaim-test-key-32-bytes-long!!"
	phone := "8613800700009"
	hmac, err := phonehmac.Hash(key, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u := insertOpsUserWithHMAC(t, "NORMAL", "", phone, hmac, true, mustNullTime(t))

	report, err := Lookup(ctx, testDB, key, phone)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !report.Found {
		t.Fatal("lookup found no ledger row for a row that exists")
	}
	if report.PhoneMasked == phone || strings.Contains(report.PhoneMasked, "0700009") {
		t.Fatalf("masked phone = %q, want the digits hidden", report.PhoneMasked)
	}
	if len(report.HMACShort) != 6 {
		t.Fatalf("short hmac = %q, want 6 characters", report.HMACShort)
	}
	if !report.HolderUID.Valid || report.HolderUID.Int64 != u.uid {
		t.Fatalf("holder = %+v, want uid %d", report.HolderUID, u.uid)
	}
	if report.HolderStatus != "NORMAL" {
		t.Fatalf("holder status = %q, want NORMAL", report.HolderStatus)
	}
}

func assertReleased(t *testing.T, uid int64) {
	t.Helper()
	var phone, hmac sql.NullString
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT phone, phone_hmac FROM t_user WHERE id = ?`, uid).Scan(&phone, &hmac); err != nil {
		t.Fatalf("read uid %d: %v", uid, err)
	}
	if phone.Valid || hmac.Valid {
		t.Fatalf("uid %d still holds phone=%v hmac=%v", uid, phone, hmac)
	}
}

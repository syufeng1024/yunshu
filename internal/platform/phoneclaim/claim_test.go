package phoneclaim

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"yunshu.dev/core/internal/control/ctltest"
)

var testDB *sql.DB

func TestMain(m *testing.M) {
	db, err := ctltest.SetupDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "phoneclaim tests: %v\n", err)
		os.Exit(1)
	}
	testDB = db
	code := m.Run()
	_ = db.Close()
	os.Exit(code)
}

// newHMAC returns a 64-character lowercase hex string shaped exactly like
// HMAC-SHA256(phone, PHONE_HMAC_KEY). The ledger never verifies the digest, so a
// random value is enough — and it keeps this package free of an import of auth
// (which imports this package).
func newHMAC(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random hmac: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func cleanupClaim(t *testing.T, hmac string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(),
			`DELETE FROM t_phone_claim WHERE phone_hmac = ?`, hmac)
	})
}

// inTx runs fn inside a transaction and commits it, so the test exercises the
// same statement shapes the handlers use.
func inTx(t *testing.T, fn func(tx *sql.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := testDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// The boundary is left-open: exactly 90 days old no longer blocks, one
// millisecond younger does. Both edges live in WindowHit and nowhere else.
func TestWindowHitBoundaries(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value sql.NullTime
		want  bool
	}{
		{"never granted", sql.NullTime{}, false},
		{"just granted", sql.NullTime{Time: now, Valid: true}, true},
		{"89d23h", sql.NullTime{Time: now.Add(-(WindowDays*24*time.Hour - time.Hour)), Valid: true}, true},
		{"exactly 90d", sql.NullTime{Time: now.AddDate(0, 0, -WindowDays), Valid: true}, false},
		{"90d minus 1ms", sql.NullTime{Time: now.AddDate(0, 0, -WindowDays).Add(time.Millisecond), Valid: true}, true},
		{"91d", sql.NullTime{Time: now.AddDate(0, 0, -WindowDays-1), Valid: true}, false},
	}
	for _, tc := range cases {
		claim := Claim{TrialGrantedAt: tc.value}
		if got := claim.WindowHit(now); got != tc.want {
			t.Fatalf("%s: WindowHit=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// First wins: a second registration of the same number advances last_claim_*
// only. first_user_id, created_at and trial_granted_at must not move, or the
// number's history would be rewritten by every re-registration.
func TestUpsertOnRegisterIsFirstWins(t *testing.T) {
	ctx := context.Background()
	hmac := newHMAC(t)
	cleanupClaim(t, hmac)

	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	second := first.Add(48 * time.Hour)

	inTx(t, func(tx *sql.Tx) {
		if err := UpsertOnRegisterTx(ctx, tx, hmac, 101, first); err != nil {
			t.Fatalf("first upsert: %v", err)
		}
	})
	// Simulate a grant recorded between the two registrations.
	if _, err := testDB.ExecContext(ctx,
		`UPDATE t_phone_claim SET trial_granted_at = ?, last_trial_user_id = 101 WHERE phone_hmac = ?`,
		first.Add(time.Hour), hmac); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	inTx(t, func(tx *sql.Tx) {
		if err := UpsertOnRegisterTx(ctx, tx, hmac, 202, second); err != nil {
			t.Fatalf("second upsert: %v", err)
		}
	})

	claim, err := Get(ctx, testDB, hmac)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if claim.FirstUserID != 101 {
		t.Fatalf("first_user_id = %d, want 101 (first claim wins)", claim.FirstUserID)
	}
	if !claim.CreatedAt.Equal(first) {
		t.Fatalf("created_at = %s, want %s", claim.CreatedAt, first)
	}
	if claim.LastClaimUserID.Int64 != 202 || !claim.LastClaimUserID.Valid {
		t.Fatalf("last_claim_user_id = %+v, want 202", claim.LastClaimUserID)
	}
	if !claim.LastClaimAt.Time.Equal(second) {
		t.Fatalf("last_claim_at = %s, want %s", claim.LastClaimAt.Time, second)
	}
	if !claim.TrialGrantedAt.Valid || !claim.TrialGrantedAt.Time.Equal(first.Add(time.Hour)) {
		t.Fatalf("trial_granted_at = %+v, want the seeded grant (upsert must not touch it)",
			claim.TrialGrantedAt)
	}
	if claim.CancelCount != 0 {
		t.Fatalf("cancel_count = %d, want 0", claim.CancelCount)
	}
}

// A cancellation of a number with no row creates one: the row is the only place
// the release is remembered once t_user.phone is gone.
func TestRecordCancelCreatesRowAndStampsGrant(t *testing.T) {
	ctx := context.Background()
	hmac := newHMAC(t)
	cleanupClaim(t, hmac)

	cancelAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	claimedAt := cancelAt.Add(-72 * time.Hour)

	var got Claim
	inTx(t, func(tx *sql.Tx) {
		claim, err := RecordCancelTx(ctx, tx, hmac, 77, cancelAt,
			sql.NullTime{Time: claimedAt, Valid: true}, TrialGrantUserID(sql.NullTime{Time: claimedAt, Valid: true}, 77))
		if err != nil {
			t.Fatalf("record cancel: %v", err)
		}
		got = claim
	})

	if got.FirstUserID != 77 || got.CancelCount != 1 {
		t.Fatalf("first_user_id=%d cancel_count=%d, want 77/1", got.FirstUserID, got.CancelCount)
	}
	if !got.TrialGrantedAt.Valid || !got.TrialGrantedAt.Time.Equal(claimedAt) {
		t.Fatalf("trial_granted_at = %+v, want %s (self-healed from the account row)",
			got.TrialGrantedAt, claimedAt)
	}
	if !got.LastTrialUserID.Valid || got.LastTrialUserID.Int64 != 77 {
		t.Fatalf("last_trial_user_id = %+v, want 77", got.LastTrialUserID)
	}
	if !got.LastCancelAt.Time.Equal(cancelAt) {
		t.Fatalf("last_cancel_at = %s, want %s", got.LastCancelAt.Time, cancelAt)
	}
}

// COALESCE: an earlier grant on the ledger wins over the value on the account
// row, so cancel + re-register cannot restart the 90-day window.
func TestRecordCancelKeepsExistingGrant(t *testing.T) {
	ctx := context.Background()
	hmac := newHMAC(t)
	cleanupClaim(t, hmac)

	grantedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := grantedAt.Add(240 * time.Hour)
	if _, err := testDB.ExecContext(ctx, `
		INSERT INTO t_phone_claim
		  (phone_hmac, first_user_id, last_claim_user_id, last_claim_at, cancel_count,
		   trial_granted_at, last_trial_user_id, created_at, updated_at)
		VALUES (?, 11, 11, ?, 0, ?, 11, ?, ?)`,
		hmac, grantedAt, grantedAt, grantedAt, grantedAt); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	var got Claim
	inTx(t, func(tx *sql.Tx) {
		claim, err := RecordCancelTx(ctx, tx, hmac, 22, cancelAt, sql.NullTime{Time: cancelAt, Valid: true}, sql.NullInt64{})
		if err != nil {
			t.Fatalf("record cancel: %v", err)
		}
		got = claim
	})

	if !got.TrialGrantedAt.Time.Equal(grantedAt) {
		t.Fatalf("trial_granted_at = %s, want %s (an existing grant must not move)",
			got.TrialGrantedAt.Time, grantedAt)
	}
	if got.LastTrialUserID.Int64 != 11 {
		t.Fatalf("last_trial_user_id = %d, want 11 (the account that actually claimed)",
			got.LastTrialUserID.Int64)
	}
	if got.FirstUserID != 11 {
		t.Fatalf("first_user_id = %d, want 11", got.FirstUserID)
	}
	if got.CancelCount != 1 {
		t.Fatalf("cancel_count = %d, want 1", got.CancelCount)
	}

	// A second release of the same number increments instead of inserting.
	inTx(t, func(tx *sql.Tx) {
		claim, err := RecordCancelTx(ctx, tx, hmac, 33, cancelAt.Add(time.Hour), sql.NullTime{}, sql.NullInt64{})
		if err != nil {
			t.Fatalf("second record cancel: %v", err)
		}
		got = claim
	})
	if got.CancelCount != 2 {
		t.Fatalf("cancel_count = %d, want 2 after the second release", got.CancelCount)
	}
}

func TestTrialWindowTxReportsHitAndMissing(t *testing.T) {
	ctx := context.Background()
	hmac := newHMAC(t)
	cleanupClaim(t, hmac)

	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	if _, err := testDB.ExecContext(ctx, `
		INSERT INTO t_phone_claim
		  (phone_hmac, first_user_id, last_claim_user_id, last_claim_at, cancel_count,
		   trial_granted_at, last_trial_user_id, created_at, updated_at)
		VALUES (?, 51, 51, ?, 0, ?, 51, ?, ?)`,
		hmac, now, now.AddDate(0, 0, -1), now, now); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	inTx(t, func(tx *sql.Tx) {
		claim, hit, err := TrialWindowTx(ctx, tx, hmac, now)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		if !hit {
			t.Fatalf("hit = false, want true (granted one day ago)")
		}
		if claim.CancelCount != 0 {
			t.Fatalf("cancel_count = %d, want 0", claim.CancelCount)
		}
	})
	inTx(t, func(tx *sql.Tx) {
		if _, hit, err := TrialWindowTx(ctx, tx, hmac, now.AddDate(0, 0, WindowDays+1)); err != nil || hit {
			t.Fatalf("hit=%v err=%v at day 91, want false/nil", hit, err)
		}
	})

	missing := newHMAC(t)
	inTx(t, func(tx *sql.Tx) {
		if _, _, err := TrialWindowTx(ctx, tx, missing, now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing row err = %v, want ErrNotFound", err)
		}
	})
}

func TestMarkTrialGrantedWritesWindow(t *testing.T) {
	ctx := context.Background()
	hmac := newHMAC(t)
	cleanupClaim(t, hmac)

	now := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	inTx(t, func(tx *sql.Tx) {
		if err := UpsertOnRegisterTx(ctx, tx, hmac, 91, now); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	})
	grantedAt := now.Add(time.Hour)
	inTx(t, func(tx *sql.Tx) {
		if err := MarkTrialGrantedTx(ctx, tx, hmac, 91, grantedAt); err != nil {
			t.Fatalf("mark granted: %v", err)
		}
	})
	claim, err := Get(ctx, testDB, hmac)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !claim.TrialGrantedAt.Valid || !claim.TrialGrantedAt.Time.Equal(grantedAt) {
		t.Fatalf("trial_granted_at = %+v, want %s", claim.TrialGrantedAt, grantedAt)
	}
	if claim.LastTrialUserID.Int64 != 91 {
		t.Fatalf("last_trial_user_id = %d, want 91", claim.LastTrialUserID.Int64)
	}
	// Re-stamping the identical instant must not be reported as a drift: MySQL
	// counts changed rows, not matched ones.
	inTx(t, func(tx *sql.Tx) {
		if err := MarkTrialGrantedTx(ctx, tx, hmac, 91, grantedAt); err != nil {
			t.Fatalf("re-stamp: %v", err)
		}
	})
	inTx(t, func(tx *sql.Tx) {
		if err := MarkTrialGrantedTx(ctx, tx, newHMAC(t), 91, grantedAt); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing row err = %v, want ErrNotFound", err)
		}
	})
}

func TestGetMissingIsNotFound(t *testing.T) {
	if _, err := Get(context.Background(), testDB, newHMAC(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

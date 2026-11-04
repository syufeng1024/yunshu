// Package phoneclaim owns t_phone_claim, the phone-number ledger.
//
// The ledger exists because the phone number outlives the account row. Once
// cancellation releases t_user.phone and t_user.phone_hmac (migration 0029),
// the account row keeps no memory of the handset, so the two facts that must
// survive it — "how many times was this number released" and "when was a trial
// last granted to it" — live here, one row per number, addressed by the HMAC.
//
// Rules this package enforces so no caller has to remember them:
//
//   - Every statement is addressed by phone_hmac. The ledger is never scanned
//     and never asked "is this number taken" — that question belongs to
//     t_user.uk_user_phone_hmac, which answers it atomically on INSERT.
//   - The first row wins: first_user_id, created_at and trial_granted_at are
//     written on INSERT only and never overwritten by a later claim. A
//     re-registration advances last_claim_* and nothing else.
//   - The 90-day window has exactly one boundary, Claim.WindowHit. SQL here
//     only selects; the threshold is passed in from Go so a test can stand on
//     the 90th day without touching the clock of the server.
//   - Lock order is fixed: t_user first, t_phone_claim last. Nothing in this
//     package takes a t_user lock, and no caller may lock a claim row before
//     the account row it belongs to (design §6.1).
package phoneclaim

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WindowDays is the anti-arbitrage window for the trial grant, counted from the
// last grant to the *number* (not to the account, and not from cancellation).
// It is 90 because that is the lifetime of a bonus credit batch
// (bonus_valid_days = 90): one number may collect a trial once per gift-credit
// lifetime. Defined once here; fund aliases it instead of repeating the literal.
const WindowDays = 90

// ErrNotFound reports "this phone number has no ledger row". Callers must treat
// it as a blocking fault, not as "no window": the only states that produce it
// are a backfill that has not run or a hand-edited database, and granting a
// trial on a number with no memory is exactly the free grant the ledger exists
// to prevent (design §6.3).
var ErrNotFound = errors.New("phoneclaim: no ledger row for this phone_hmac")

// Claim is one row of t_phone_claim.
type Claim struct {
	PhoneHMAC       string
	FirstUserID     int64
	LastClaimUserID sql.NullInt64
	LastClaimAt     sql.NullTime
	LastCancelAt    sql.NullTime
	CancelCount     uint32
	TrialGrantedAt  sql.NullTime
	LastTrialUserID sql.NullInt64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// WindowHit reports whether a trial granted to this number is still inside the
// anti-arbitrage window at now. The boundary is left-open: a grant exactly
// WindowDays old no longer blocks, so the window is "strictly less than 90 days
// ago". This is the only place the comparison exists.
func (c Claim) WindowHit(now time.Time) bool {
	if !c.TrialGrantedAt.Valid {
		return false
	}
	return c.TrialGrantedAt.Time.After(now.UTC().AddDate(0, 0, -WindowDays))
}

// Querier is the subset of *sql.DB / *sql.Tx these reads need. The pre-flight
// checks in fund run outside a transaction (a database handle) while the write
// paths run inside one (a transaction).
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// columns is the projection every statement in this file scans, in one place so
// adding a column cannot leave a caller scanning a different shape.
const columns = `phone_hmac, first_user_id, last_claim_user_id, last_claim_at,
	last_cancel_at, cancel_count, trial_granted_at, last_trial_user_id,
	created_at, updated_at`

// UpsertOnRegisterTx records "this number is claimed again" inside the caller's
// registration transaction.
//
// It must run after the t_user INSERT: the INSERT is what decides whether the
// number is free (1062 on uk_user_phone_hmac), and this statement is only the
// bookkeeping that follows a successful claim. A failure here rolls the whole
// registration back — an account without a ledger row is the state the trial
// check fails closed on, so it must never be committed.
func UpsertOnRegisterTx(ctx context.Context, tx *sql.Tx, hmac string, uid int64, now time.Time) error {
	if hmac == "" {
		return errors.New("phoneclaim: register upsert needs a phone_hmac")
	}
	now = now.UTC()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO t_phone_claim
		  (phone_hmac, first_user_id, last_claim_user_id, last_claim_at, cancel_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, ?)
		ON DUPLICATE KEY UPDATE
		  last_claim_user_id = VALUES(last_claim_user_id),
		  last_claim_at      = VALUES(last_claim_at),
		  updated_at         = VALUES(updated_at)`,
		hmac, uid, uid, now, now, now)
	if err != nil {
		return fmt.Errorf("phoneclaim: upsert on register: %w", err)
	}
	return nil
}

// TrialGrantUserID is the "who received it" half of a self-healed
// trial_granted_at: an account that claimed a trial before the ledger existed
// carries the fact in t_user.trial_claimed_at, and uid is the only candidate.
// NULL in, NULL out — an account that never claimed must not be named.
func TrialGrantUserID(trialClaimedAt sql.NullTime, uid int64) sql.NullInt64 {
	if !trialClaimedAt.Valid {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: uid, Valid: true}
}

// RecordCancelTx writes "this number was released by cancellation" into the
// ledger and returns the row as it stands afterwards.
//
// Two of the columns are deliberately COALESCE-guarded:
//
//   - trial_granted_at keeps an existing value and only falls back to the value
//     on the account row (trial_claimed_at). An earlier grant must not be moved
//     forward by a later cancellation, otherwise the window would restart every
//     time the user cancels.
//   - last_trial_user_id is written on INSERT only, for the same reason: the
//     existing one names the account that actually received the grant, and
//     overwriting it with NULL would destroy that record.
//
// last_trial_user_id is an argument rather than something derived here because
// the "was there a grant" fact lives on the caller's already-locked t_user row;
// TrialGrantUserID turns it into the value this statement wants.
func RecordCancelTx(ctx context.Context, tx *sql.Tx, hmac string, uid int64,
	cancelAt time.Time, trialClaimedAt sql.NullTime, lastTrialUserID sql.NullInt64) (Claim, error) {
	if hmac == "" {
		return Claim{}, errors.New("phoneclaim: cancel record needs a phone_hmac")
	}
	cancelAt = cancelAt.UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO t_phone_claim
		  (phone_hmac, first_user_id, last_claim_user_id, last_claim_at, last_cancel_at,
		   cancel_count, trial_granted_at, last_trial_user_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  last_cancel_at   = VALUES(last_cancel_at),
		  cancel_count     = cancel_count + 1,
		  trial_granted_at = COALESCE(trial_granted_at, VALUES(trial_granted_at)),
		  updated_at       = VALUES(updated_at)`,
		hmac, uid, uid, cancelAt, cancelAt,
		nullTime(trialClaimedAt), lastTrialUserID, cancelAt, cancelAt); err != nil {
		return Claim{}, fmt.Errorf("phoneclaim: record cancel: %w", err)
	}
	// Re-read inside the same transaction: the row is locked by the statement
	// above, so this is free, and it gives the caller the post-increment
	// cancel_count the audit row wants without a second round trip later.
	return Get(ctx, tx, hmac)
}

// TrialWindowTx re-reads the window under a row lock, immediately before a
// grant is written. It is the authority the pre-flight check cannot be: two
// claims racing on the same number both pass an unlocked read.
//
// hit == true means the number collected a trial less than WindowDays ago and
// the caller must refuse. The row lock is what serialises the two claims, so it
// is taken even though the read is short.
func TrialWindowTx(ctx context.Context, tx *sql.Tx, hmac string, now time.Time) (Claim, bool, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT `+columns+`
		  FROM t_phone_claim WHERE phone_hmac = ? FOR UPDATE`, hmac)
	claim, err := scanClaim(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, false, ErrNotFound
	}
	if err != nil {
		return Claim{}, false, fmt.Errorf("phoneclaim: read window: %w", err)
	}
	return claim, claim.WindowHit(now), nil
}

// MarkTrialGrantedTx stamps the grant after the credit has been applied in the
// caller's transaction. The ledger row and t_user.trial_claimed_at must commit
// together: a window that outlives its grant (or a grant with no window) is
// either a free second trial or a lockout nobody can explain.
//
// A missing row is reported as ErrNotFound rather than swallowed. The caller is
// already holding the row lock from TrialWindowTx, so "gone" means something
// deleted it inside this transaction — a drift no retry can fix.
func MarkTrialGrantedTx(ctx context.Context, tx *sql.Tx, hmac string, uid int64, now time.Time) error {
	if hmac == "" {
		return errors.New("phoneclaim: mark trial needs a phone_hmac")
	}
	now = now.UTC()
	res, err := tx.ExecContext(ctx, `
		UPDATE t_phone_claim
		   SET trial_granted_at = ?, last_trial_user_id = ?, updated_at = ?
		 WHERE phone_hmac = ?`, now, uid, now, hmac)
	if err != nil {
		return fmt.Errorf("phoneclaim: mark trial granted: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("phoneclaim: mark trial granted rows: %w", err)
	}
	if n == 1 {
		return nil
	}
	// RowsAffected counts *changed* rows, so an UPDATE that writes the values
	// the row already holds reports 0. Distinguish that from a vanished row
	// instead of failing a legitimate re-stamp (a retried grant in a test that
	// pins the clock is exactly this case).
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_phone_claim WHERE phone_hmac = ?`, hmac).Scan(&count); err != nil {
		return fmt.Errorf("phoneclaim: mark trial granted verify: %w", err)
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}

// Get reads one ledger row by HMAC. It never locks: use TrialWindowTx inside a
// transaction that is about to decide something.
func Get(ctx context.Context, q Querier, hmac string) (Claim, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+columns+` FROM t_phone_claim WHERE phone_hmac = ?`, hmac)
	claim, err := scanClaim(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, ErrNotFound
	}
	if err != nil {
		return Claim{}, fmt.Errorf("phoneclaim: get: %w", err)
	}
	return claim, nil
}

// nullTime keeps a zero time.Time out of the database: writing the zero value
// would store 0001-01-01 and make "never" indistinguishable from "a long time
// ago" for every reader and every report.
func nullTime(v sql.NullTime) sql.NullTime {
	if !v.Valid || v.Time.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: v.Time.UTC(), Valid: true}
}

func scanClaim(row *sql.Row) (Claim, error) {
	var c Claim
	err := row.Scan(&c.PhoneHMAC, &c.FirstUserID, &c.LastClaimUserID, &c.LastClaimAt,
		&c.LastCancelAt, &c.CancelCount, &c.TrialGrantedAt, &c.LastTrialUserID,
		&c.CreatedAt, &c.UpdatedAt)
	return c, err
}

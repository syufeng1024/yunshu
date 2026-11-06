// Package ledger is the append-only money kernel. It owns credit accounts,
// batches, preholds, settlement, arrears and refunds.
//
// The package talks to MySQL transactions directly, but it never imports
// HTTP, Redis or the gateway: the ledger truth must be testable
// without the gateway or Redis. Every timestamp comes from the injected
// clock.Clock; every threshold (prehold ratio, overdraft limit, safety margin)
// is a parameter, never a read of t_config_item.
package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

	"yunshu.dev/core/internal/platform/clock"
)

// Ledger directions.
const (
	DirectionCredit = "CREDIT"
	DirectionDebit  = "DEBIT"
)

// Account types recorded in t_ledger_entry.account_type.
const (
	AccountTypeRecharge = "RECHARGE"
	AccountTypeBonus    = "BONUS"
	AccountTypeMixed    = "MIXED"
)

// Business types recorded in t_ledger_entry.biz_type.
const (
	BizTypeRecharge        = "RECHARGE"
	BizTypePlanGrant       = "PLAN_GRANT"
	BizTypeRedeem          = "REDEEM"
	BizTypeConsumePrehold  = "CONSUME_PREHOLD"
	BizTypeConsumeSettle   = "CONSUME_SETTLE"
	BizTypePreholdRelease  = "PREHOLD_RELEASE"
	BizTypeRefund          = "REFUND"
	BizTypeCompensation    = "COMPENSATION"
	BizTypeBatchExpire     = "BATCH_EXPIRE"
	BizTypeOverdraftOffset = "OVERDRAFT_OFFSET"
	BizTypeAdjust          = "ADJUST"
)

// Prehold states.
const (
	HoldStateHeld      = "HELD"
	HoldStateSettled   = "SETTLED"
	HoldStateReleased  = "RELEASED"
	HoldStateSuspended = "SUSPENDED"
)

// User statuses relevant to the money kernel.
const (
	UserStatusNormal          = "NORMAL"
	UserStatusFrozenOverdraft = "FROZEN_OVERDRAFT"
)

// Sentinel errors. Callers use errors.Is; the wrapped text carries the
// account/request values needed by an operator without leaking SQL.
var (
	ErrInvalidParam         = errors.New("ledger: invalid parameter")
	ErrNoAccount            = errors.New("ledger: credit account not found")
	ErrCASConflict          = errors.New("ledger: account version CAS conflict")
	ErrBelowOverdraft       = errors.New("ledger: account would exceed the overdraft floor")
	ErrInsufficientCredits  = errors.New("ledger: insufficient credits")
	ErrPreholdNotFound      = errors.New("ledger: prehold not found")
	ErrPreholdExists        = errors.New("ledger: a prehold for this request_id already exists")
	ErrPreholdLedgerMissing = errors.New("ledger: prehold ledger entry missing; releasing would create credits from nothing")
	ErrPreholdSuspended     = errors.New("ledger: prehold is suspended and cannot be settled")
	ErrInsufficientBatches  = errors.New("ledger: credit batches cannot cover the debit")
	ErrBatchCAS             = errors.New("ledger: batch remaining CAS conflict")
	ErrDuplicateIdem        = errors.New("ledger: duplicate idem key")
	ErrRefundIneligible     = errors.New("ledger: refund is not eligible")
	// ErrDuplicateRequestID means the request_id presented to Prehold is
	// already bound to a request that has finished (SETTLED / RELEASED /
	// SUSPENDED) or to another account. request_id is the prehold's unique key
	// and therefore the money idempotency key: replaying it would let a caller
	// re-run a paid request without a second hold, so the reuse is refused
	// instead of being answered idempotently.
	ErrDuplicateRequestID = errors.New("ledger: request_id already belongs to a finished request")
)

// Store wires the ledger to a database, a clock and an entry-number source.
// It is safe for concurrent use: every operation owns its transaction.
type Store struct {
	DB      *sql.DB
	Clock   clock.Clock
	Entries *EntryNoGenerator

	PreholdTimeout time.Duration
	SettleTimeout  time.Duration
	DefaultTimeout time.Duration

	// DeductPolicy is injected rather than read from t_config_item. V1.0 only
	// implements fifo_expiry; an unknown policy is rejected.
	DeductPolicy DeductPolicy
}

// NewStore builds a store with production defaults. Tests may override the
// timeouts and the clock.
func NewStore(db *sql.DB, clk clock.Clock) *Store {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Store{
		DB:             db,
		Clock:          clk,
		Entries:        NewEntryNoGenerator(),
		PreholdTimeout: TxnTimeout,
		SettleTimeout:  SettleTxnTimeout,
		DefaultTimeout: 5 * time.Second,
		DeductPolicy:   DeductPolicyFIFOExpiry,
	}
}

func (s *Store) now() time.Time { return s.Clock.Now().UTC() }

// txMaxAttempts bounds how many times one logical transaction is replayed
// after InnoDB aborted it. Three matches the entry_no retry budget.
const txMaxAttempts = 3

// withTx runs fn inside one transaction, replaying the whole transaction when
// InnoDB aborts it as a deadlock victim (1213) or because the lock wait timed
// out (1205). Both mean "you lost a race, re-issue the transaction", which is
// what MySQL asks of every client. The replay is safe because fn writes only
// through tx, and every failed attempt is rolled back before the next begins.
//
// nightly #2 is why this is not optional: the prehold insert deadlocked (1213)
// on roughly 0.7% of a 200 QPS load, and every one of them surfaced to the
// caller as a 503 settlement_unavailable. The gap-lock root cause is fixed in
// preholdTx; this is the net for the next lock ordering mistake, wherever it
// is.
func (s *Store) withTx(ctx context.Context, timeout time.Duration, fn func(context.Context, *sql.Tx) error) error {
	if s.DB == nil {
		return fmt.Errorf("%w: database is nil", ErrInvalidParam)
	}
	if timeout <= 0 {
		timeout = s.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= txMaxAttempts; attempt++ {
		if attempt > 1 {
			// Growing backoff inside the caller's budget. A deadlock is
			// detected in microseconds, so the winner has normally committed
			// before 2ms have passed.
			select {
			case <-ctx.Done():
				return lastErr
			case <-time.After(time.Duration(attempt-1) * 2 * time.Millisecond):
			}
		}
		err := s.runTx(ctx, fn)
		if err == nil {
			return nil
		}
		lastErr = err
		// database/sql rolls the transaction back itself when ctx expires, so
		// the Commit that follows reports sql.ErrTxDone: "transaction has
		// already been committed or rolled back". That reads like a double
		// commit bug but means "the 200ms budget ran out" - surface the
		// deadline instead, which is what a reader needs.
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, sql.ErrTxDone) {
			return fmt.Errorf("ledger: transaction deadline: %w", ctxErr)
		}
		if !isRetryableTxError(err) {
			return err
		}
	}
	return lastErr
}

// runTx is one attempt: begin, fn, commit.
func (s *Store) runTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit: %w", err)
	}
	return nil
}

// Entry is one append-only t_ledger_entry row. Amounts are signed:
// CREDIT entries are positive, DEBIT entries negative.
type Entry struct {
	UserID              int64
	AccountType         string
	Direction           string
	BizType             string
	AmountBonusMicro    int64
	AmountRechargeMicro int64
	AmountTotalMicro    int64
	BalanceBeforeMicro  int64
	BalanceAfterMicro   int64
	BatchID             *int64
	RequestID           *string
	RechargeOrderID     *int64
	RefundOrderID       *int64
	IdemKey             string
	OperatorID          *int64
	Reason              *string
	CreatedAt           time.Time
}

// AppendEntry writes one immutable ledger row. The caller must pass the same
// transaction that updates the account: a ledger row without the matching
// balance change (or vice versa) is never committed.
func AppendEntry(ctx context.Context, tx *sql.Tx, gen *EntryNoGenerator, now time.Time, e Entry) (int64, error) {
	if e.UserID <= 0 || e.IdemKey == "" {
		return 0, fmt.Errorf("%w: user_id and idem_key are required", ErrInvalidParam)
	}
	if e.AmountTotalMicro != e.AmountBonusMicro+e.AmountRechargeMicro {
		return 0, fmt.Errorf("%w: amount_total != bonus + recharge", ErrInvalidParam)
	}
	if e.BalanceAfterMicro != e.BalanceBeforeMicro+e.AmountTotalMicro {
		return 0, fmt.Errorf("%w: balance_after != balance_before + amount_total", ErrInvalidParam)
	}
	switch e.Direction {
	case DirectionCredit:
		if e.AmountTotalMicro < 0 {
			return 0, fmt.Errorf("%w: CREDIT entry has a negative total", ErrInvalidParam)
		}
	case DirectionDebit:
		if e.AmountTotalMicro > 0 {
			return 0, fmt.Errorf("%w: DEBIT entry has a positive total", ErrInvalidParam)
		}
	default:
		return 0, fmt.Errorf("%w: unknown direction %q", ErrInvalidParam, e.Direction)
	}
	if e.AccountType == "" {
		switch {
		case e.AmountBonusMicro != 0 && e.AmountRechargeMicro != 0:
			e.AccountType = AccountTypeMixed
		case e.AmountBonusMicro != 0:
			e.AccountType = AccountTypeBonus
		default:
			e.AccountType = AccountTypeRecharge
		}
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now.UTC()
	}
	if gen == nil {
		return 0, fmt.Errorf("%w: entry number generator is nil", ErrInvalidParam)
	}

	const insertSQL = `INSERT INTO t_ledger_entry (
		user_id, entry_no, account_type, direction, biz_type,
		amount_bonus_micro, amount_recharge_micro, amount_total_micro,
		balance_before_micro, balance_after_micro, batch_id, request_id,
		recharge_order_id, refund_order_id, idem_key, operator_id, reason,
		created_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		entryNo := gen.Next(e.CreatedAt)
		res, err := tx.ExecContext(ctx, insertSQL,
			e.UserID, entryNo, e.AccountType, e.Direction, e.BizType,
			e.AmountBonusMicro, e.AmountRechargeMicro, e.AmountTotalMicro,
			e.BalanceBeforeMicro, e.BalanceAfterMicro, e.BatchID, e.RequestID,
			e.RechargeOrderID, e.RefundOrderID, e.IdemKey, e.OperatorID, e.Reason,
			e.CreatedAt.UTC(),
		)
		if err == nil {
			return res.LastInsertId()
		}
		lastErr = err
		if !isDuplicate(err) {
			return 0, fmt.Errorf("ledger: insert entry: %w", err)
		}
		// A duplicate entry_no is retried with a fresh ULID. A duplicate
		// idem_key is the idempotency guard and must be surfaced, not retried.
		if isDuplicateIdem(err) {
			return 0, fmt.Errorf("%w: %s", ErrDuplicateIdem, e.IdemKey)
		}
	}
	return 0, fmt.Errorf("ledger: insert entry: %w", lastErr)
}

// isDuplicate reports a MySQL duplicate-key error (1062).
func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// isDuplicateIdem distinguishes a duplicate idem_key from a duplicate
// entry_no. The driver message contains the conflicting key name.
func isDuplicateIdem(err error) bool {
	if !isDuplicate(err) {
		return false
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return strings.Contains(me.Message, "uk_le_idem")
	}
	return false
}

// isRetryableTxError reports the two InnoDB errors that mean "this transaction
// lost a race, run it again": 1213 deadlock and 1205 lock wait timeout. Every
// write path in this package is a single transaction over the same few rows,
// so the victim is always correct to re-issue.
func isRetryableTxError(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}

// EntryNoGenerator produces monotonic ULID-shaped 26-character numbers. It
// uses the injected time for the 48-bit millisecond prefix and crypto/rand for
// the 80-bit suffix; within the same millisecond the suffix is incremented, so
// entry_no is strictly increasing even under concurrency.
type EntryNoGenerator struct {
	mu     sync.Mutex
	lastMS int64
	last   [10]byte
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewEntryNoGenerator returns a generator seeded with a random suffix.
func NewEntryNoGenerator() *EntryNoGenerator {
	g := &EntryNoGenerator{}
	if _, err := rand.Read(g.last[:]); err != nil {
		// A failing system RNG is catastrophic for uniqueness; fall back to a
		// deterministic non-zero suffix rather than silently returning zeros.
		for i := range g.last {
			g.last[i] = byte(i*31 + 17)
		}
	}
	return g
}

// Next returns the next 26-character entry number for instant t.
func (g *EntryNoGenerator) Next(t time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := t.UTC().UnixMilli()
	if ms > g.lastMS {
		g.lastMS = ms
		if _, err := rand.Read(g.last[:]); err != nil {
			for i := range g.last {
				g.last[i] = byte(i*31 + 17)
			}
		}
	} else {
		// Same millisecond, or the clock moved backwards: keep the prefix and
		// increment the 80-bit suffix.
		overflow := true
		for i := len(g.last) - 1; i >= 0; i-- {
			g.last[i]++
			if g.last[i] != 0 {
				overflow = false
				break
			}
		}
		if overflow {
			g.lastMS++
			if _, err := rand.Read(g.last[:]); err != nil {
				for i := range g.last {
					g.last[i] = byte(i*31 + 17)
				}
			}
		}
		ms = g.lastMS
	}
	return encodeULID(ms, g.last)
}

func encodeULID(ms int64, rnd [10]byte) string {
	var out [26]byte
	// 48-bit timestamp, encoded in 10 base32 characters (the top two bits of
	// the first character are zero).
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&0x1f]
		ms >>= 5
	}
	// 80-bit suffix, encoded in 16 base32 characters.
	var buf [10]byte
	copy(buf[:], rnd[:])
	for i := 25; i >= 10; i-- {
		out[i] = crockford[buf[9]&0x1f]
		shiftRight5(&buf)
	}
	return string(out[:])
}

func shiftRight5(b *[10]byte) {
	for i := 9; i > 0; i-- {
		b[i] = (b[i] >> 5) | (b[i-1] << 3)
	}
	b[0] >>= 5
}

// randomDigits returns a digit string with exactly n characters, using
// crypto/rand. It is used for batch_no and arrears_no suffixes.
func randomDigits(n int) string {
	if n <= 0 {
		return ""
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%0*d", n, 0)
	}
	v := binary.BigEndian.Uint64(raw[:])
	mod := uint64(1)
	for i := 0; i < n; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", n, v%mod)
}

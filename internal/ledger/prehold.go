package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"yunshu.dev/core/internal/billing"
)

// TxnTimeout is the hard budget for one prehold transaction (card §4.17).
// It is deliberately short: prehold runs on the gateway's critical path.
const TxnTimeout = 200 * time.Millisecond

// DefaultPreholdTTL covers the non-stream legal request lifetime; the caller
// must pass 600s for streaming requests.
const DefaultPreholdTTL = 125 * time.Second

// PreholdParams carries every number the prehold needs. No threshold is read
// from t_config_item: T05 has not delivered the configuration snapshot yet,
// and the ledger must be independently testable.
type PreholdParams struct {
	RequestID string
	UserID    int64
	APIKeyID  int64
	ModelID   int64
	RouteID   *int64
	NodeID    string

	// RawEstMicro is the admission estimate: tokenizer input + max_tokens
	// output, with no margin. Admission uses this value (G-02).
	RawEstMicro int64
	// EstMicro is the prehold estimate: safety margin applied at the token
	// stage, prehold ratio applied at the amount stage.
	EstMicro int64
	// MaxHoldRatioBP is billing.prehold_max_ratio in basis points (5000 = 50%).
	MaxHoldRatioBP int64
	// ExpiresAt is the hold's recovery deadline.
	ExpiresAt time.Time
}

// PreholdResult describes what happened.
//
// Idempotent is only ever set by readHoldTx, the internal re-read used to
// classify a request_id hit; Prehold never returns a successful result for a
// reused id, so a caller can not receive Idempotent=true from it. The field is
// kept because the re-read needs to report what it found.
type PreholdResult struct {
	RequestID            string
	HoldMicro            int64
	EstimatedMicro       int64
	AvailableBeforeMicro int64
	State                string
	Idempotent           bool
}

// Admit is the 402 admission predicate. It compares the RAW estimate with the
// available balance. It never uses est and never adds the overdraft limit.
func Admit(rawEstMicro, availableMicro int64) bool {
	return rawEstMicro <= availableMicro
}

// HoldMicro computes MIN(est, available × max_ratio), floored. A non-positive
// available balance yields zero: the overdraft limit is not spendable here.
func HoldMicro(estMicro, availableMicro, maxRatioBP int64) int64 {
	if estMicro <= 0 || availableMicro <= 0 || maxRatioBP <= 0 {
		return 0
	}
	if maxRatioBP > billing.BasisPoints {
		maxRatioBP = billing.BasisPoints
	}
	capMicro := availableMicro * maxRatioBP / billing.BasisPoints
	if capMicro < estMicro {
		return capMicro
	}
	return estMicro
}

// Prehold locks the account, admits the request on raw_est, computes the hold
// from est and available×ratio, and writes the prehold ledger row, the hold
// row and the balance CAS in one transaction.
//
// A request_id that already has a hold is refused with ErrDuplicateRequestID,
// in every state: see classifyExistingHold.
func (s *Store) Prehold(ctx context.Context, p PreholdParams) (PreholdResult, error) {
	var result PreholdResult
	err := s.withTx(ctx, s.PreholdTimeout, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		result, err = s.preholdTx(ctx, tx, p)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrPreholdExists) || errors.Is(err, ErrDuplicateIdem) {
			// A concurrent prehold won the unique key. Re-read the winner's
			// row and apply the SAME refusal as the in-transaction hit below:
			// the race must not become a way around it.
			if _, readErr := s.getPrehold(ctx, p.RequestID); readErr != nil {
				return PreholdResult{}, readErr
			}
			return classifyExistingHold(p.RequestID)
		}
		return PreholdResult{}, err
	}
	return result, nil
}

// classifyExistingHold answers a prehold that found an existing row for
// request_id.
//
// request_id is the unique key of t_prehold_hold, so it is the money
// idempotency key: a hit means the id has already been used, and it is never
// reusable. There is no HELD exception. Replaying a live hold would leave the
// window a long stream opens (the hold is HELD until settlement) usable for N
// more real upstream calls that settle exactly once — the free ride this guard
// exists to close. PRD §"X-Request-Id" requires one answer for every state: 409
// duplicate_request_id. A genuine retry is P08's Idempotency-Key, which runs
// before P10 and replays the stored response instead of reaching the ledger.
//
// Ownership is deliberately NOT distinguished. Same-user and other-user hits
// return the same error with the same text, so a caller cannot use the response
// to probe whether a stranger's request_id exists. Do not add an ownership check
// "to look stricter": it would not change the answer, and the comment above
// would start describing a behaviour the code does not have.
func classifyExistingHold(requestID string) (PreholdResult, error) {
	return PreholdResult{}, fmt.Errorf("%w: %s", ErrDuplicateRequestID, requestID)
}

func (s *Store) preholdTx(ctx context.Context, tx *sql.Tx, p PreholdParams) (PreholdResult, error) {
	now := s.now()
	if err := validatePrehold(p); err != nil {
		return PreholdResult{}, err
	}

	// Idempotency guard: the unique key on request_id is what makes a second
	// hold impossible. The read deliberately does NOT take FOR UPDATE. The row
	// does not exist yet, so InnoDB would put a gap lock on uk_ph_request, and
	// gap locks do not conflict with each other: every concurrent prehold would
	// hold the same trailing gap (request_id is UUIDv7, so new keys are always
	// appended) and then deadlock on the INSERT's insert-intention lock. Under
	// a 200 QPS soak this path deadlocked on ~0.7% of requests, each one a
	// user-visible 503. A plain read lets the race resolve on the unique key
	// (1062), which isDuplicate turns into ErrPreholdExists and Prehold answers
	// by re-reading the winner's row outside this transaction and re-applying
	// classifyExistingHold to it.
	if _, err := s.readHoldTx(ctx, tx, p.RequestID); err == nil {
		return classifyExistingHold(p.RequestID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return PreholdResult{}, err
	}

	if !p.ExpiresAt.IsZero() && !p.ExpiresAt.After(now) {
		return PreholdResult{}, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidParam)
	}

	acct, err := LockAccount(ctx, tx, p.UserID)
	if err != nil {
		return PreholdResult{}, err
	}
	available := acct.AvailableMicro()
	if !Admit(p.RawEstMicro, available) {
		return PreholdResult{}, fmt.Errorf("%w: available=%d raw_est=%d",
			ErrInsufficientCredits, available, p.RawEstMicro)
	}

	hold := HoldMicro(p.EstMicro, available, p.MaxHoldRatioBP)
	bonusPart := int64(0)
	if hold > 0 && acct.BalanceBonusMicro > 0 {
		bonusPart = min64(hold, acct.BalanceBonusMicro)
	}
	rechargePart := hold - bonusPart
	if rechargePart > 0 && rechargePart > acct.BalanceRechargeMicro {
		return PreholdResult{}, fmt.Errorf("%w: hold=%d recharge_part=%d balance_recharge=%d",
			ErrInvalidParam, hold, rechargePart, acct.BalanceRechargeMicro)
	}

	expiresAt := p.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = now.Add(DefaultPreholdTTL)
	}

	// The hold row is inserted before the ledger row so a concurrent duplicate
	// is caught before any balance movement is recorded.
	const holdSQL = `INSERT INTO t_prehold_hold (
		request_id, user_id, api_key_id, hold_micro, estimated_micro, model_id,
		route_id, state, expires_at, node_id, created_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	if _, err := tx.ExecContext(ctx, holdSQL,
		p.RequestID, p.UserID, p.APIKeyID, hold, p.EstMicro, p.ModelID,
		p.RouteID, HoldStateHeld, expiresAt.UTC(), p.NodeID, now,
	); err != nil {
		if isDuplicate(err) {
			return PreholdResult{}, fmt.Errorf("%w: %s", ErrPreholdExists, p.RequestID)
		}
		return PreholdResult{}, fmt.Errorf("ledger: insert prehold: %w", err)
	}

	before := acct.TotalMicro()
	entryID, err := AppendEntry(ctx, tx, s.Entries, now, Entry{
		UserID:              p.UserID,
		Direction:           DirectionDebit,
		BizType:             BizTypeConsumePrehold,
		AmountBonusMicro:    -bonusPart,
		AmountRechargeMicro: -rechargePart,
		AmountTotalMicro:    -hold,
		BalanceBeforeMicro:  before,
		BalanceAfterMicro:   before - hold,
		RequestID:           &p.RequestID,
		IdemKey:             "prehold:" + p.RequestID,
		CreatedAt:           now,
	})
	if err != nil {
		return PreholdResult{}, err
	}

	if err := acct.Apply(ctx, tx, BalanceDelta{
		BonusMicro:    -bonusPart,
		RechargeMicro: -rechargePart,
	}, entryID, nil, now); err != nil {
		return PreholdResult{}, err
	}

	return PreholdResult{
		RequestID:            p.RequestID,
		HoldMicro:            hold,
		EstimatedMicro:       p.EstMicro,
		AvailableBeforeMicro: available,
		State:                HoldStateHeld,
	}, nil
}

func validatePrehold(p PreholdParams) error {
	switch {
	case p.RequestID == "":
		return fmt.Errorf("%w: request_id is required", ErrInvalidParam)
	case p.UserID <= 0, p.APIKeyID <= 0, p.ModelID <= 0:
		return fmt.Errorf("%w: user_id/api_key_id/model_id must be positive", ErrInvalidParam)
	case p.NodeID == "":
		return fmt.Errorf("%w: node_id is required", ErrInvalidParam)
	case p.RawEstMicro < 0, p.EstMicro < 0:
		return fmt.Errorf("%w: estimates must not be negative", ErrInvalidParam)
	case p.MaxHoldRatioBP < 0 || p.MaxHoldRatioBP > billing.BasisPoints:
		return fmt.Errorf("%w: max hold ratio %d bp", ErrInvalidParam, p.MaxHoldRatioBP)
	}
	return nil
}

// readHoldTx is the reuse lookup. It is a plain read on purpose: see the call
// site in preholdTx for why FOR UPDATE here is a deadlock generator.
//
// Only the columns the answer needs are selected: the hit is refused whatever
// its owner and state are, so selecting user_id would invite a check that does
// not exist (see classifyExistingHold).
func (s *Store) readHoldTx(ctx context.Context, tx *sql.Tx, requestID string) (PreholdResult, error) {
	const q = `SELECT request_id, hold_micro, estimated_micro, state
	             FROM t_prehold_hold WHERE request_id = ?`
	var r PreholdResult
	err := tx.QueryRowContext(ctx, q, requestID).Scan(
		&r.RequestID, &r.HoldMicro, &r.EstimatedMicro, &r.State,
	)
	if err != nil {
		return PreholdResult{}, err
	}
	r.Idempotent = true
	return r, nil
}

// getPrehold reads a hold after a concurrent-insert race. It is intentionally
// outside the failed transaction so the caller sees the winner's row.
//
// It only reads: the caller must run the row through classifyExistingHold,
// because "a row exists" is a refusal, not an answer.
func (s *Store) getPrehold(ctx context.Context, requestID string) (PreholdResult, error) {
	var r PreholdResult
	err := s.withTx(ctx, s.PreholdTimeout, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		r, err = s.readHoldTx(ctx, tx, requestID)
		return err
	})
	if err != nil {
		return PreholdResult{}, err
	}
	return r, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

package ledger

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// commitTx commits a caller-owned transaction and fails the test on error.
// The deferred Rollback after a successful Commit is a documented no-op.
func commitTx(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestAdjustTxValidationBranches(t *testing.T) {
	s := newTestStore(t)
	now := s.now()

	valid := AdjustParams{
		UserID:      1,
		AdjustID:    2,
		Direction:   AdjustDirectionIncrease,
		AmountMicro: 100,
		OperatorID:  3,
		Reason:      "manual adjustment",
	}

	// nil tx is rejected before any other parameter is inspected.
	if _, err := s.AdjustTx(context.Background(), nil, valid, now); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("nil tx err = %v, want ErrInvalidParam", err)
	}

	// The remaining validation branches never touch the database, so one
	// transaction shared across the subtests is enough.
	tx := mustBegin(t)
	cases := []struct {
		name   string
		mutate func(*AdjustParams)
	}{
		{"missing user", func(p *AdjustParams) { p.UserID = 0 }},
		{"missing adjust", func(p *AdjustParams) { p.AdjustID = 0 }},
		{"missing amount", func(p *AdjustParams) { p.AmountMicro = 0 }},
		{"missing operator", func(p *AdjustParams) { p.OperatorID = 0 }},
		{"blank reason", func(p *AdjustParams) { p.Reason = "   " }},
		{"unknown direction", func(p *AdjustParams) { p.Direction = "SIDEWAYS" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := valid
			c.mutate(&p)
			if _, err := s.AdjustTx(context.Background(), tx, p, now); !errors.Is(err, ErrInvalidParam) {
				t.Fatalf("err = %v, want ErrInvalidParam", err)
			}
		})
	}
}

func TestAdjustTxIncreasePostsCompensationBatch(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 0)
	newAccount(t, userID, 0, 0, 0)
	adjustID := uniqueOrderID()
	now := s.now()

	tx, err := testDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	res, err := s.AdjustTx(context.Background(), tx, AdjustParams{
		UserID:      userID,
		AdjustID:    adjustID,
		Direction:   AdjustDirectionIncrease,
		AmountMicro: 1000000,
		OperatorID:  777,
		Reason:      "manual credit",
	}, now)
	if err != nil {
		t.Fatalf("AdjustTx increase: %v", err)
	}
	commitTx(t, tx)

	if res.BalanceAfterMicro != 1000000 {
		t.Fatalf("balance after = %d, want 1000000", res.BalanceAfterMicro)
	}
	if res.BatchID == 0 || len(res.EntryIDs) != 1 {
		t.Fatalf("increase result = %+v, want one batch and one entry", res)
	}

	st := getAccount(t, userID)
	if st.Bonus != 1000000 || st.Recharge != 0 || st.Total != 1000000 {
		t.Fatalf("account after increase = %+v, want bonus=1000000 recharge=0", st)
	}

	// The manual increase is a non-refundable COMPENSATION batch carrying the
	// t_balance_adjust id in source_id.
	var batchType string
	var refundable int
	var sourceID int64
	if err := testDB.QueryRow(
		`SELECT batch_type, refundable, source_id FROM t_credit_batch WHERE id = ?`,
		res.BatchID,
	).Scan(&batchType, &refundable, &sourceID); err != nil {
		t.Fatalf("read compensation batch: %v", err)
	}
	if batchType != BatchTypeCompensation || refundable != 0 || sourceID != adjustID {
		t.Fatalf("batch = type %s refundable %d source %d, want COMPENSATION/0/%d",
			batchType, refundable, sourceID, adjustID)
	}

	entries := ledgerEntries(t, userID, BizTypeAdjust)
	if len(entries) != 1 {
		t.Fatalf("adjust entries = %d, want 1", len(entries))
	}
	if entries[0].Direction != DirectionCredit || entries[0].AmountBonus != 1000000 {
		t.Fatalf("adjust entry = %+v, want CREDIT +1000000 bonus", entries[0])
	}
}

func TestAdjustTxDecreaseConsumesBatchFIFO(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 0)
	newAccount(t, userID, 0, 10000000, 0)
	batchID := newBatch(t, userID, batchOpts{
		Refundable: true, Face: 10000000, Remaining: 10000000,
		CreatedAt: fixtureBase,
	})
	adjustID := uniqueOrderID()
	now := s.now()

	tx, err := testDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	res, err := s.AdjustTx(context.Background(), tx, AdjustParams{
		UserID:      userID,
		AdjustID:    adjustID,
		Direction:   AdjustDirectionDecrease,
		AmountMicro: 4000000,
		OperatorID:  888,
		Reason:      "manual debit",
	}, now)
	if err != nil {
		t.Fatalf("AdjustTx decrease: %v", err)
	}
	commitTx(t, tx)

	if res.BalanceAfterMicro != 6000000 {
		t.Fatalf("balance after = %d, want 6000000", res.BalanceAfterMicro)
	}
	if len(res.EntryIDs) != 1 {
		t.Fatalf("decrease entry ids = %v, want one", res.EntryIDs)
	}

	st := getAccount(t, userID)
	if st.Recharge != 6000000 || st.Bonus != 0 || st.Total != 6000000 {
		t.Fatalf("account after decrease = %+v, want recharge=6000000", st)
	}
	if got := batchRemaining(t, batchID); got != 6000000 {
		t.Fatalf("batch remaining = %d, want 6000000", got)
	}

	entries := ledgerEntries(t, userID, BizTypeAdjust)
	if len(entries) != 1 {
		t.Fatalf("adjust entries = %d, want 1", len(entries))
	}
	if entries[0].Direction != DirectionDebit || entries[0].AmountRecharge != -4000000 {
		t.Fatalf("adjust entry = %+v, want DEBIT -4000000 recharge", entries[0])
	}
}

func TestAdjustTxDecreaseInsufficientCredits(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 0)
	newAccount(t, userID, 0, 1000000, 0)
	tx := mustBegin(t)

	_, err := s.AdjustTx(context.Background(), tx, AdjustParams{
		UserID:      userID,
		AdjustID:    uniqueOrderID(),
		Direction:   AdjustDirectionDecrease,
		AmountMicro: 5000000,
		OperatorID:  1,
		Reason:      "too much",
	}, s.now())
	if !errors.Is(err, ErrInsufficientCredits) {
		t.Fatalf("err = %v, want ErrInsufficientCredits", err)
	}
}

func TestAdjustTxDecreaseInsufficientBatches(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 0)
	// The account claims 1,000,000 but no batch backs it: the manual decrease
	// must refuse to fabricate an overdraft.
	newAccount(t, userID, 0, 1000000, 0)
	tx := mustBegin(t)

	_, err := s.AdjustTx(context.Background(), tx, AdjustParams{
		UserID:      userID,
		AdjustID:    uniqueOrderID(),
		Direction:   AdjustDirectionDecrease,
		AmountMicro: 1000000,
		OperatorID:  1,
		Reason:      "unbacked",
	}, s.now())
	if !errors.Is(err, ErrInsufficientBatches) {
		t.Fatalf("err = %v, want ErrInsufficientBatches", err)
	}
}

func TestFreezeRefundTxValidationBranches(t *testing.T) {
	s := newTestStore(t)
	if err := s.FreezeRefundTx(context.Background(), nil, 1, 100); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("nil tx freeze err = %v, want ErrInvalidParam", err)
	}
	if err := s.ReleaseRefundTx(context.Background(), nil, 1, 100); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("nil tx release err = %v, want ErrInvalidParam", err)
	}

	tx := mustBegin(t)
	if err := s.FreezeRefundTx(context.Background(), tx, 0, 100); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero user freeze err = %v, want ErrInvalidParam", err)
	}
	if err := s.FreezeRefundTx(context.Background(), tx, 1, 0); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero amount freeze err = %v, want ErrInvalidParam", err)
	}
	if err := s.ReleaseRefundTx(context.Background(), tx, 0, 100); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero user release err = %v, want ErrInvalidParam", err)
	}
	if err := s.ReleaseRefundTx(context.Background(), tx, 1, 0); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero amount release err = %v, want ErrInvalidParam", err)
	}
}

func TestFreezeAndReleaseRefundRoundTrip(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 0)
	newAccount(t, userID, 0, 10000000, 0)

	tx, err := testDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if err := s.FreezeRefundTx(context.Background(), tx, userID, 3000000); err != nil {
		t.Fatalf("FreezeRefundTx: %v", err)
	}
	commitTx(t, tx)

	frozen := getAccount(t, userID)
	if frozen.Frozen != 3000000 || frozen.Total != 10000000 {
		t.Fatalf("after freeze = %+v, want frozen=3000000 total=10000000", frozen)
	}

	tx, err = testDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin release: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if err := s.ReleaseRefundTx(context.Background(), tx, userID, 3000000); err != nil {
		t.Fatalf("ReleaseRefundTx: %v", err)
	}
	commitTx(t, tx)

	released := getAccount(t, userID)
	if released.Frozen != 0 || released.Total != 10000000 {
		t.Fatalf("after release = %+v, want frozen=0 total=10000000", released)
	}
}

func TestFreezeRefundInsufficientBalance(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 0)
	newAccount(t, userID, 0, 1000000, 0)
	tx := mustBegin(t)

	if err := s.FreezeRefundTx(context.Background(), tx, userID, 5000000); !errors.Is(err, ErrInsufficientCredits) {
		t.Fatalf("err = %v, want ErrInsufficientCredits", err)
	}
}

func TestReleaseRefundInsufficientFrozen(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 0)
	newAccount(t, userID, 0, 1000000, 0)
	tx := mustBegin(t)

	if err := s.ReleaseRefundTx(context.Background(), tx, userID, 1); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("err = %v, want ErrInvalidParam", err)
	}
}

func TestQuoteRefundTxValidationAndHappyPath(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.QuoteRefundTx(context.Background(), nil, RefundQuoteParams{UserID: 1}); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("nil tx err = %v, want ErrInvalidParam", err)
	}

	userID := newUser(t, 500000)
	newAccount(t, userID, 0, 79000000, 500000)
	orderID := newRechargeOrder(t, userID, 79000000)
	newBatch(t, userID, batchOpts{
		Refundable: true, Face: 79000000, Remaining: 79000000,
		CreatedAt: fixtureBase, SourceID: &orderID,
	})

	tx := mustBegin(t)
	q, err := s.QuoteRefundTx(context.Background(), tx, RefundQuoteParams{
		UserID:          userID,
		RechargeOrderID: &orderID,
	})
	if err != nil {
		t.Fatalf("QuoteRefundTx: %v", err)
	}
	if !q.Eligible || q.RefundableMicro != 79000000 {
		t.Fatalf("quote = %+v, want eligible/79000000", q)
	}
	if q.RefundFen != 7110 || q.FeeFen != 790 {
		t.Fatalf("quote fen = %d/%d, want 7110/790", q.RefundFen, q.FeeFen)
	}
}

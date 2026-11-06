package recon

import (
	"context"
	"testing"
	"time"

	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/platform/clock"
)

// B4: R-02 must add the in-flight holds back before comparing the batch ledger
// with the two account columns.
//
// A prehold debits t_credit_account and leaves t_credit_batch untouched, so
// every request that is still running puts the batch sum exactly one hold ahead
// of the account. Comparing the two directly reported each of those users as a
// CRITICAL amount mismatch — i.e. every active request, every day.
func TestR02AddsBackHeldPreholds(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 10, 22, 0, 0, 0, 0, CST)
	bizDate := day.Format("2006-01-02")
	dayStart, dayEnd := day, day.AddDate(0, 0, 1)

	d := NewDaily(testDB, nil, nil, clock.Real{}, "b4")
	d.UserLo, d.UserHi = 903000, 903100
	cleanReconScope(t, 903000, 903100, bizDate)
	t.Cleanup(func() { cleanReconScope(t, 903000, 903100, bizDate) })

	const (
		userInFlight = int64(903001) // a HELD prehold covers the whole gap
		userMismatch = int64(903002) // HELD prehold plus a real 500 shortfall
		userFinished = int64(903003) // SETTLED prehold: no add-back
	)
	expires := time.Now().UTC().Add(5 * time.Minute)

	// in-flight: balance was 100000, the hold took 30000 off the account only.
	insertAccount(t, userInFlight, 70000, 0)
	insertBatch(t, userInFlight, 100000, 100000, 0)
	reqInFlight := rid("b4if", 1)
	insertLedgerEntry(t, userInFlight, entryNo("B4A", 1), ledger.BizTypeConsumePrehold,
		ledger.DirectionDebit, 0, -30000, -30000, 100000, 70000, "prehold:"+reqInFlight, &reqInFlight)
	insertHold(t, reqInFlight, userInFlight, 30000, ledger.HoldStateHeld, expires)

	// mismatch: same shape, but the batch ledger is 500 short of the account.
	insertAccount(t, userMismatch, 70000, 0)
	insertBatch(t, userMismatch, 99500, 99500, 0)
	reqMismatch := rid("b4mm", 1)
	insertLedgerEntry(t, userMismatch, entryNo("B4B", 1), ledger.BizTypeConsumePrehold,
		ledger.DirectionDebit, 0, -30000, -30000, 100000, 70000, "prehold:"+reqMismatch, &reqMismatch)
	insertHold(t, reqMismatch, userMismatch, 30000, ledger.HoldStateHeld, expires)

	// finished: the hold is SETTLED, so it protects nothing and must not be
	// added back — the batch should have been debited and was not.
	insertAccount(t, userFinished, 70000, 0)
	insertBatch(t, userFinished, 100000, 100000, 0)
	reqFinished := rid("b4fn", 1)
	insertHold(t, reqFinished, userFinished, 30000, ledger.HoldStateSettled, expires)

	diffs, err := checkR02(ctx, d, bizDate, dayStart, dayEnd)
	if err != nil {
		t.Fatalf("R-02: %v", err)
	}
	if len(diffs) != 2 {
		t.Fatalf("R-02 diffs = %d (%+v); want 2 (in-flight user clean, mismatch + settled reported)",
			len(diffs), diffs)
	}
	for _, diff := range diffs {
		switch diff.Subject {
		case "batch:903002":
			if diff.DiffType != DiffAmountMismatch || diff.Level != LevelCritical {
				t.Fatalf("mismatch diff = %+v", diff)
			}
			// 70000 account + 30000 held = 100000 vs 99500 batch.
			if diff.LocalValue != 100000 || diff.UpstreamValue != 99500 || diff.DeltaMicro != 500 {
				t.Fatalf("mismatch values = %+v; want local=100000 upstream=99500 delta=500", diff)
			}
		case "batch:903003":
			if diff.LocalValue != 70000 || diff.UpstreamValue != 100000 || diff.DeltaMicro != -30000 {
				t.Fatalf("settled values = %+v; want local=70000 upstream=100000 delta=-30000", diff)
			}
		default:
			t.Fatalf("unexpected R-02 diff %+v", diff)
		}
	}

	// The batch-side invariant (§2-3) only changed for users with a live hold:
	// the in-flight row must produce no diff at all.
	for _, diff := range diffs {
		if diff.Subject == "batch:903001" {
			t.Fatalf("in-flight user reported as %+v; the hold must be added back", diff)
		}
	}
	t.Logf("B4: in-flight clean, mismatch delta=%d, settled-hold delta=%d",
		diffs[0].DeltaMicro, diffs[1].DeltaMicro)
}

// insertHold writes one t_prehold_hold row in an arbitrary state.
func insertHold(t *testing.T, requestID string, userID, holdMicro int64, state string, expiresAt time.Time) {
	t.Helper()
	now := time.Now().UTC()
	execFixture(t,
		`INSERT INTO t_prehold_hold
		   (request_id, user_id, api_key_id, hold_micro, estimated_micro, model_id,
		    state, expires_at, node_id, created_at)
		 VALUES (?,?,1,?,?,910001,?,?, 'test-node', ?)`,
		requestID, userID, holdMicro, holdMicro, state, expiresAt.UTC(), now)
}

package ledger

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

// G-02 discriminator: admission compares RAW_EST with available, not est.
func TestG02AdmitUsesRawEstimate(t *testing.T) {
	if !Admit(6000, 6500) {
		t.Fatal("raw_est 6000 <= available 6500 must be admitted")
	}
	if Admit(7200, 6500) {
		t.Fatal("est 7200 > available 6500 must not be the admission criterion")
	}
	if Admit(6001, 6000) {
		t.Fatal("raw_est above available must be rejected")
	}
	if !Admit(6000, 6000) {
		t.Fatal("raw_est equal to available must be admitted")
	}
}

// The prehold is MIN(est, available × 50%), floored. With the G-02 boundary
// values the correct hold is 3,250 (6500 × 50%). The task card's assertion
// says 3,600 and labels it "6500×50%"; 3,600 is actually 7200×50% and would
// violate the 50% cap. The formula is authoritative.
func TestG02HoldIsFiftyPercentOfAvailable(t *testing.T) {
	got := HoldMicro(7200, 6500, 5000)
	if got != 3250 {
		t.Fatalf("HoldMicro(7200,6500,5000)=%d, want 3250 (6500×50%%, floored)", got)
	}
	t.Logf("G-02 hold evidence: available=6500 est=7200 => 6500×50%%=3250, est×50%%=3600; formula MIN(est, available×50%%)=3250")
	if got > 6500*5000/10000 {
		t.Fatalf("hold %d exceeds the 50%% cap %d", got, 6500*5000/10000)
	}
}

func TestHoldNeverExceedsAvailableCap(t *testing.T) {
	cases := []struct {
		est, available, ratio, want int64
	}{
		{7200, 7200, 5000, 3600},  // the only way to get the card's 3,600
		{5000, 10000, 5000, 5000}, // est is the smaller value
		{7200, 10000, 5000, 5000}, // cap is the smaller value
		{7200, 0, 5000, 0},
		{7200, -100, 5000, 0}, // overdraft is not spendable at prehold
		{7200, 6500, 0, 0},
	}
	for _, c := range cases {
		if got := HoldMicro(c.est, c.available, c.ratio); got != c.want {
			t.Fatalf("HoldMicro(%d,%d,%d)=%d, want %d", c.est, c.available, c.ratio, got, c.want)
		}
	}
}

// G-02 integration: balance 6,500, raw_est 6,000, est 7,200 => admit, hold
// 3,250, settle 6,000, final balance 500.
func TestG02PreholdAndSettleBoundary(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 500000)
	newAccount(t, userID, 0, 6500, 500000)
	newBatch(t, userID, batchOpts{Refundable: true, Face: 6500, Remaining: 6500})

	requestID := newRequestID("g02")
	pre, err := s.Prehold(t.Context(), PreholdParams{
		RequestID:      requestID,
		UserID:         userID,
		APIKeyID:       1,
		ModelID:        1,
		NodeID:         "test-node",
		RawEstMicro:    6000,
		EstMicro:       7200,
		MaxHoldRatioBP: 5000,
		ExpiresAt:      s.Clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Prehold: %v", err)
	}
	if pre.HoldMicro != 3250 {
		t.Fatalf("hold = %d, want 3250", pre.HoldMicro)
	}
	if pre.AvailableBeforeMicro != 6500 {
		t.Fatalf("available before = %d, want 6500", pre.AvailableBeforeMicro)
	}
	if st := getAccount(t, userID); st.Total != 3250 {
		t.Fatalf("balance after prehold = %d, want 3250", st.Total)
	}
	t.Logf("G-02 prehold: raw_est=6000 available_before=6500 admitted=%v hold=%d balance_after_prehold=%d",
		Admit(6000, 6500), pre.HoldMicro, getAccount(t, userID).Total)

	settle, err := s.Settle(t.Context(), SettleParams{
		RequestID:     requestID,
		UserID:        userID,
		ConsumedMicro: 6000,
	})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if settle.ConsumedMicro != 6000 || settle.ArrearsMicro != 0 {
		t.Fatalf("settle = %+v, want consumed 6000 / arrears 0", settle)
	}
	if st := getAccount(t, userID); st.Total != 500 {
		t.Fatalf("balance after settle = %d, want 500", st.Total)
	}
	if got := sumEntries(t, userID, BizTypeConsumeSettle); got != -6000 {
		t.Fatalf("settle entry sum = %d, want -6000", got)
	}
	t.Logf("G-02 settle: consumed=%d batch_backed=%d overdraft=%d arrears=%d balance_after=%d entry_sum=%d",
		settle.ConsumedMicro, settle.BatchBackedMicro, settle.OverdraftMicro,
		settle.ArrearsMicro, getAccount(t, userID).Total, sumEntries(t, userID, BizTypeConsumeSettle))
}

// G-02 "截断后放行": est > available×50% does not reject the request; the hold
// is capped at 5,000 and settlement proceeds.
func TestG02HoldTruncatedButRequestAdmitted(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 500000)
	newAccount(t, userID, 0, 10000, 500000)
	newBatch(t, userID, batchOpts{Refundable: true, Face: 10000, Remaining: 10000})

	requestID := newRequestID("g02c")
	pre, err := s.Prehold(t.Context(), PreholdParams{
		RequestID: requestID, UserID: userID, APIKeyID: 1, ModelID: 1, NodeID: "test",
		RawEstMicro: 6000, EstMicro: 7200, MaxHoldRatioBP: 5000,
		ExpiresAt: s.Clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Prehold: %v", err)
	}
	if pre.HoldMicro != 5000 {
		t.Fatalf("hold = %d, want 5000 (capped at 10000×50%%)", pre.HoldMicro)
	}
}

func TestG02InsufficientCreditsDoesNotMoveMoney(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 500000)
	newAccount(t, userID, 0, 10000, 500000)
	newBatch(t, userID, batchOpts{Refundable: true, Face: 10000, Remaining: 10000})

	requestID := newRequestID("g02r")
	_, err := s.Prehold(t.Context(), PreholdParams{
		RequestID: requestID, UserID: userID, APIKeyID: 1, ModelID: 1, NodeID: "test",
		RawEstMicro: 52500, EstMicro: 63000, MaxHoldRatioBP: 5000,
		ExpiresAt: s.Clock.Now().Add(time.Hour),
	})
	if !errors.Is(err, ErrInsufficientCredits) {
		t.Fatalf("err = %v, want ErrInsufficientCredits", err)
	}
	if st := getAccount(t, userID); st.Total != 10000 || st.Version != 0 {
		t.Fatalf("account moved on rejection: %+v", st)
	}
	if entries := ledgerEntries(t, userID, ""); len(entries) != 0 {
		t.Fatalf("ledger entries on rejection = %d, want 0", len(entries))
	}
}

// B1: a request_id that already has a hold is refused in every state, HELD
// included. The HELD case used to be answered idempotently ("the request is
// still in flight, let the retry through"), which is exactly the free-ride
// window: a long stream keeps the hold HELD for minutes, and N replays of the
// same id inside that window all reach the upstream while only the first
// settlement is ever charged. A real retry is P08's Idempotency-Key, which runs
// before P10 and replays the stored response instead of asking the ledger for a
// second hold.
//
// This replaces the old TestPreholdIsIdempotentByRequestID assertion
// "second Prehold succeeds with Idempotent=true": the same fixture now expects
// ErrDuplicateRequestID.
func TestPreholdRefusesReplayWhileHeld(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 500000)
	newAccount(t, userID, 0, 10000, 500000)
	newBatch(t, userID, batchOpts{Refundable: true, Face: 10000, Remaining: 10000})

	requestID := newRequestID("held")
	p := PreholdParams{
		RequestID: requestID, UserID: userID, APIKeyID: 1, ModelID: 1, NodeID: "test",
		RawEstMicro: 6000, EstMicro: 7200, MaxHoldRatioBP: 5000,
		ExpiresAt: s.Clock.Now().Add(time.Hour),
	}
	first, err := s.Prehold(t.Context(), p)
	if err != nil {
		t.Fatalf("first Prehold: %v", err)
	}
	if first.Idempotent {
		t.Fatalf("first Prehold = %+v, want a fresh hold", first)
	}
	second, err := s.Prehold(t.Context(), p)
	if !errors.Is(err, ErrDuplicateRequestID) {
		t.Fatalf("replay while HELD err = %v, want ErrDuplicateRequestID", err)
	}
	if second != (PreholdResult{}) {
		t.Fatalf("refused replay returned a result: %+v", second)
	}
	entries := ledgerEntries(t, userID, BizTypeConsumePrehold)
	if len(entries) != 1 {
		t.Fatalf("prehold entries = %d, want 1", len(entries))
	}
	if st := getAccount(t, userID); st.Total != 10000-first.HoldMicro {
		t.Fatalf("balance after refused replay = %d, want %d", st.Total, 10000-first.HoldMicro)
	}
	if state, hold := getHoldState(t, requestID); state != HoldStateHeld || hold != first.HoldMicro {
		t.Fatalf("hold after refused replay = %s/%d, want HELD/%d", state, hold, first.HoldMicro)
	}
}

// B1 branch 2: a request_id whose request already finished is not a replay, it
// is a reuse. Replaying it would return a hold that protects nothing, and the
// settlement that follows would find SETTLED and answer with the historical
// result without charging — an unlimited free ride.
func TestPreholdRejectsRequestIDAfterSettle(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 500000)
	newAccount(t, userID, 0, 100000, 500000)
	newBatch(t, userID, batchOpts{Refundable: true, Face: 100000, Remaining: 100000})

	requestID := newRequestID("reuse-settled")
	p := PreholdParams{
		RequestID: requestID, UserID: userID, APIKeyID: 1, ModelID: 1, NodeID: "test",
		RawEstMicro: 6000, EstMicro: 7200, MaxHoldRatioBP: 5000,
		ExpiresAt: s.Clock.Now().Add(time.Hour),
	}
	if _, err := s.Prehold(t.Context(), p); err != nil {
		t.Fatalf("Prehold: %v", err)
	}
	if _, err := s.Settle(t.Context(), SettleParams{
		RequestID: requestID, UserID: userID, ConsumedMicro: 5000,
	}); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	state, _ := getHoldState(t, requestID)
	if state != HoldStateSettled {
		t.Fatalf("hold state = %s, want SETTLED", state)
	}
	balanceAfterSettle := getAccount(t, userID).Total
	preholdEntries := len(ledgerEntries(t, userID, BizTypeConsumePrehold))

	replay, err := s.Prehold(t.Context(), p)
	if !errors.Is(err, ErrDuplicateRequestID) {
		t.Fatalf("reuse after settle err = %v, want ErrDuplicateRequestID", err)
	}
	if replay != (PreholdResult{}) {
		t.Fatalf("reuse after settle returned a result: %+v", replay)
	}
	if got := getAccount(t, userID).Total; got != balanceAfterSettle {
		t.Fatalf("balance moved on refused reuse: %d -> %d", balanceAfterSettle, got)
	}
	if got := len(ledgerEntries(t, userID, BizTypeConsumePrehold)); got != preholdEntries {
		t.Fatalf("prehold entries = %d, want %d (no second hold may be written)", got, preholdEntries)
	}
	if got, _ := getHoldState(t, requestID); got != HoldStateSettled {
		t.Fatalf("hold state after refused reuse = %s, want SETTLED", got)
	}
	t.Logf("B1: request_id=%s reused after SETTLED => rejected, balance=%d unchanged, state=%s",
		requestID, balanceAfterSettle, state)
}

// B1 branch 2, RELEASED variant: the failure path is the same free ride (the
// request failed upstream, so the id must not be able to start a new one).
func TestPreholdRejectsRequestIDAfterRelease(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 500000)
	newAccount(t, userID, 0, 100000, 500000)
	newBatch(t, userID, batchOpts{Refundable: true, Face: 100000, Remaining: 100000})

	requestID := newRequestID("reuse-released")
	p := PreholdParams{
		RequestID: requestID, UserID: userID, APIKeyID: 1, ModelID: 1, NodeID: "test",
		RawEstMicro: 6000, EstMicro: 7200, MaxHoldRatioBP: 5000,
		ExpiresAt: s.Clock.Now().Add(time.Hour),
	}
	if _, err := s.Prehold(t.Context(), p); err != nil {
		t.Fatalf("Prehold: %v", err)
	}
	if _, err := s.Settle(t.Context(), SettleParams{
		RequestID: requestID, UserID: userID, ConsumedMicro: 0, ReleaseReason: "UPSTREAM_FAIL",
	}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if state, _ := getHoldState(t, requestID); state != HoldStateReleased {
		t.Fatalf("hold state = %s, want RELEASED", state)
	}
	if _, err := s.Prehold(t.Context(), p); !errors.Is(err, ErrDuplicateRequestID) {
		t.Fatalf("reuse after release err = %v, want ErrDuplicateRequestID", err)
	}
	if st := getAccount(t, userID); st.Total != 100000 {
		t.Fatalf("balance = %d, want 100000 (release then refused reuse)", st.Total)
	}
}

// B1 branch 3: another account's request_id is refused with the same error.
// The two cases are deliberately indistinguishable so the response cannot be
// used to probe whether a stranger's request_id exists.
func TestPreholdRejectsRequestIDOfAnotherUser(t *testing.T) {
	s := newTestStore(t)
	owner := newUser(t, 500000)
	newAccount(t, owner, 0, 100000, 500000)
	newBatch(t, owner, batchOpts{Refundable: true, Face: 100000, Remaining: 100000})
	attacker := newUser(t, 500000)
	newAccount(t, attacker, 0, 100000, 500000)
	newBatch(t, attacker, batchOpts{Refundable: true, Face: 100000, Remaining: 100000})

	requestID := newRequestID("crossuser")
	if _, err := s.Prehold(t.Context(), PreholdParams{
		RequestID: requestID, UserID: owner, APIKeyID: 1, ModelID: 1, NodeID: "test",
		RawEstMicro: 6000, EstMicro: 7200, MaxHoldRatioBP: 5000,
		ExpiresAt: s.Clock.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("owner Prehold: %v", err)
	}
	ownerBalance := getAccount(t, owner).Total

	// Same id, different user, while the owner's hold is still HELD. This is
	// the branch that a plain "row exists" check would have answered.
	if _, err := s.Prehold(t.Context(), PreholdParams{
		RequestID: requestID, UserID: attacker, APIKeyID: 2, ModelID: 1, NodeID: "test",
		RawEstMicro: 1, EstMicro: 1, MaxHoldRatioBP: 5000,
		ExpiresAt: s.Clock.Now().Add(time.Hour),
	}); !errors.Is(err, ErrDuplicateRequestID) {
		t.Fatalf("cross-user reuse err = %v, want ErrDuplicateRequestID", err)
	}
	if got := getAccount(t, owner).Total; got != ownerBalance {
		t.Fatalf("owner balance moved: %d -> %d", ownerBalance, got)
	}
	if got := getAccount(t, attacker).Total; got != 100000 {
		t.Fatalf("attacker balance moved: %d, want 100000", got)
	}
	var holders int
	if err := testDB.QueryRow(
		`SELECT COUNT(DISTINCT user_id) FROM t_prehold_hold WHERE request_id = ?`, requestID,
	).Scan(&holders); err != nil {
		t.Fatalf("count holders: %v", err)
	}
	if holders != 1 {
		t.Fatalf("hold rows belong to %d users, want 1", holders)
	}
}

// B1 concurrency path: the loser of the unique-key race re-reads the winner's
// row outside its failed transaction (getPrehold) and must run it through the
// same refusal. This test drives that exact composition, because a race cannot
// be staged deterministically inside one transaction.
func TestPreholdRaceRereadAppliesReuseRules(t *testing.T) {
	s := newTestStore(t)
	owner := newUser(t, 500000)
	other := newUser(t, 500000)

	for _, tc := range []struct {
		name  string
		state string
	}{
		{"own SETTLED", HoldStateSettled},
		{"own RELEASED", HoldStateReleased},
		{"own SUSPENDED", HoldStateSuspended},
		{"own HELD", HoldStateHeld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestID := newRequestID("race")
			insertHoldDirect(t, requestID, owner, 1234, tc.state)
			// The re-read must succeed (the row is there) and the answer must still
			// be the refusal: getPrehold only reads, classifyExistingHold decides.
			if _, err := s.getPrehold(t.Context(), requestID); err != nil {
				t.Fatalf("getPrehold: %v", err)
			}
			got, err := classifyExistingHold(requestID)
			if !errors.Is(err, ErrDuplicateRequestID) {
				t.Fatalf("race reread of %s err = %v, want ErrDuplicateRequestID", tc.state, err)
			}
			if got != (PreholdResult{}) {
				t.Fatalf("race reread returned a result: %+v", got)
			}
		})
	}
	// The other-account case is covered by TestPreholdRejectsRequestIDOfAnotherUser,
	// which drives the same code through Prehold: the refusal does not look at the
	// owner, so a per-state table here would only duplicate it.
	_ = other
}

// Concurrent preholds for one request_id must still produce exactly one hold and
// one ledger entry. The losers used to be answered idempotently; they are now
// refused, which is the point of B1 — the unique key stops being a replay
// channel while remaining a hard guard against a second hold.
func TestPreholdConcurrentSameRequestID(t *testing.T) {
	s := newTestStore(t)
	userID := newUser(t, 500000)
	newAccount(t, userID, 0, 100000, 500000)
	newBatch(t, userID, batchOpts{Refundable: true, Face: 100000, Remaining: 100000})

	requestID := newRequestID("concat")
	p := PreholdParams{
		RequestID: requestID, UserID: userID, APIKeyID: 1, ModelID: 1, NodeID: "test",
		RawEstMicro: 6000, EstMicro: 7200, MaxHoldRatioBP: 5000,
		ExpiresAt: s.Clock.Now().Add(time.Hour),
	}
	const workers = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		results []PreholdResult
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.Prehold(t.Context(), p)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, res)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		// ErrDuplicateRequestID is the expected loser outcome; sql.ErrNoRows is
		// tolerated because a winner that rolled back leaves nothing to read.
		if !errors.Is(err, ErrDuplicateRequestID) && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("concurrent prehold: %v", err)
		}
	}
	if len(results) != 1 {
		t.Fatalf("successful concurrent preholds = %d, want exactly 1", len(results))
	}
	hold := results[0].HoldMicro
	if results[0].Idempotent {
		t.Fatalf("the winner reported Idempotent=true: %+v", results[0])
	}
	if entries := ledgerEntries(t, userID, BizTypeConsumePrehold); len(entries) != 1 {
		t.Fatalf("prehold entries = %d, want 1", len(entries))
	}
	if st := getAccount(t, userID); st.Total != 100000-hold {
		t.Fatalf("balance = %d, want %d", st.Total, 100000-hold)
	}
	if state, _ := getHoldState(t, requestID); state != HoldStateHeld {
		t.Fatalf("hold state = %s, want HELD", state)
	}
}

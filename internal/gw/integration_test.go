package gw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/billing"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/idemstore"
	"yunshu.dev/core/internal/ledger"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/ratelimit"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/settle"
)

// TestIntegrationAuthReadsK13 uses the real Redis cache: the gateway has no
// database fallback, so a missing HASH must be a 401.
func TestIntegrationAuthReadsK13(t *testing.T) {
	client := newRedisClient(t)
	cache := redisx.NewKeyAuthCache(client)
	key := "sk-integration-" + randomID(t)
	if err := cache.Upsert(t.Context(), KeyHash16(key), redisx.KeyAuthRecord{
		UID: 7, KID: 10, ST: "ENABLED", GRP: "contract", GP: 8500,
	}); err != nil {
		t.Fatalf("upsert K13: %v", err)
	}

	deps := staticDeps(nil)
	deps.Auth = cache
	c := ctxWithAuthHeader(key)
	if err := NewAuthStage(deps).Run(t.Context(), c); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if c.Auth.UserID != 7 || c.Auth.GroupCode != "contract" || c.Auth.GroupRatioBP != 8500 {
		t.Fatalf("auth = %+v", c.Auth)
	}

	miss := ctxWithAuthHeader("sk-not-in-k13-" + randomID(t))
	err := NewAuthStage(deps).Run(t.Context(), miss)
	assertAPIError(t, err, apierr.CodeInvalidAPIKey, http.StatusUnauthorized)
}

// TestA8RateLimit429Headers uses the real Lua limiter.
func TestA8RateLimit429Headers(t *testing.T) {
	client := newRedisClient(t)
	clock := redisx.NewClock(nil, nil)
	limiter := ratelimit.New(client, clock, nil, nil, nil)
	keyID := time.Now().UnixNano() & 0x7fffffff
	userID := keyID + 1

	deps := staticDeps(nil)
	deps.Limiter = limiter
	deps.CounterReader = RedisCounters{Client: client}
	deps.Clock = clock
	deps.Config = nil

	limits := Limits{RPM: 1, TPM: 10_000_000, Concurrency: 100,
		DailyQuotaMicro: 1_000_000, DailyEnabled: true}
	estValue := billingPreestimateForTest()
	est := &estValue

	first := &Ctx{
		RequestID: "rl-first-" + randomID(t),
		Auth:      &AuthContext{UserID: userID, KeyID: keyID, GroupCode: "standard", GroupRatioBP: 10000},
		Limits:    limits,
		Est:       est,
	}
	stage := NewRateLimitStage(deps)
	if err := stage.Run(t.Context(), first); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !first.RateAcquired {
		t.Fatal("first acquire did not set RateAcquired")
	}

	second := &Ctx{
		RequestID: "rl-second-" + randomID(t),
		Auth:      &AuthContext{UserID: userID, KeyID: keyID, GroupCode: "standard", GroupRatioBP: 10000},
		Limits:    limits,
		Est:       est,
	}
	err := stage.Run(t.Context(), second)
	assertAPIError(t, err, apierr.CodeRPMLimitExceeded, http.StatusTooManyRequests)

	var se *StageError
	if errAs(err, &se) {
		e := se.API
		if e.RetryAfterSeconds <= 0 {
			t.Fatalf("retry_after = %d", e.RetryAfterSeconds)
		}
		reset, parseErr := time.Parse(time.RFC3339, e.ResetAt)
		if parseErr != nil {
			t.Fatalf("reset_at %q: %v", e.ResetAt, parseErr)
		}
		nowSec := time.Now().Unix()
		if got, want := e.RetryAfterSeconds, int(reset.Unix()-nowSec); absInt(got-want) > 1 {
			t.Fatalf("retry_after=%d but reset_at implies %d", got, want)
		}

		rec := httptest.NewRecorder()
		if writeErr := apierr.Write(rec, e.WithRequestID("rid-rl")); writeErr != nil {
			t.Fatalf("write: %v", writeErr)
		}
		res := rec.Result()
		defer res.Body.Close()
		if got := res.Header.Get("Retry-After"); got != strconv.Itoa(e.RetryAfterSeconds) {
			t.Fatalf("Retry-After = %q, want %d", got, e.RetryAfterSeconds)
		}
		if res.Header.Get("X-RateLimit-Limit-Requests") == "" ||
			res.Header.Get("X-RateLimit-Remaining-Requests") == "" ||
			res.Header.Get("X-RateLimit-Reset-Requests") == "" {
			t.Fatalf("missing RPM headers: %v", res.Header)
		}
		if res.Header.Get("X-Quota-Daily-Limit") == "" ||
			res.Header.Get("X-Quota-Daily-Remaining") == "" {
			t.Fatalf("missing daily header group: %v", res.Header)
		}
	}

	// Release the first request's lease so the fixture does not leak into a
	// later run of the same binary.
	_, _ = limiter.Release(t.Context(), ratelimit.ReleaseInput{
		KeyID: keyID, UserID: userID, RequestID: first.RequestID,
		AcquireMinEpoch: first.AcquiredMin, CurrentMinEpoch: minEpochOf(clock.NowMS()),
		Day: ratelimit.Day(clock.Now()), TPMDelta: -est.Tokens.InputTokens,
		HadLease: true,
	})
}

// TestIntegrationIdempotencyReplay drives the real Redis + MySQL store.
func TestIntegrationIdempotencyReplay(t *testing.T) {
	client := newRedisClient(t)
	clock := redisx.NewClock(nil, nil)
	store := idemstore.New(client, clock, idemstore.NewMySQL(testDB), idemstore.DefaultOptions(), nil)

	idemKey := "idem-" + randomID(t)
	apiKeyID := time.Now().UnixNano() & 0x7fffffff
	mustExec(t, `DELETE FROM t_idempotency_record WHERE scope = 'gateway' AND api_key_id = ? AND idem_key = ?`,
		apiKeyID, idemKey)
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.WithoutCancel(t.Context()),
			`DELETE FROM t_idempotency_record WHERE scope = 'gateway' AND api_key_id = ? AND idem_key = ?`,
			apiKeyID, idemKey)
	})

	deps := staticDeps(nil)
	deps.Idem = store
	deps.Auth = fakeAuth{records: map[string]redisx.KeyAuthRecord{}}
	stage := NewIdemStage(deps)

	header := http.Header{}
	header.Set("Idempotency-Key", idemKey)
	first := &Ctx{
		RequestID: "idem-first-" + randomID(t),
		Header:    header,
		RawBody:   []byte(`{"model":"m","messages":[]}`),
		Auth:      &AuthContext{UserID: 7, KeyID: apiKeyID},
	}
	if err := stage.Run(t.Context(), first); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if first.Idem == nil {
		t.Fatal("first request did not own the idempotency lease")
	}
	if err := store.Complete(t.Context(), first.Idem.Record, idemstore.StateDone, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	second := &Ctx{
		RequestID: "idem-second-" + randomID(t),
		Header:    header,
		RawBody:   []byte(`{"model":"m","messages":[]}`),
		Auth:      &AuthContext{UserID: 7, KeyID: apiKeyID},
	}
	err := stage.Run(t.Context(), second)
	if err != ErrPipelineStop {
		t.Fatalf("replay err = %v, want ErrPipelineStop", err)
	}
	if second.Replay == nil || second.Replay.Status != 200 {
		t.Fatalf("replay = %+v", second.Replay)
	}
	if strings.TrimSpace(string(second.Replay.Body)) != `{"ok":true}` {
		t.Fatalf("replay body = %s", second.Replay.Body)
	}

	// Same key, different body hash: the key was reused.
	third := &Ctx{
		RequestID: "idem-third-" + randomID(t),
		Header:    header,
		RawBody:   []byte(`{"model":"other","messages":[]}`),
		Auth:      &AuthContext{UserID: 7, KeyID: apiKeyID},
	}
	if err := stage.Run(t.Context(), third); err != ErrPipelineStop {
		t.Fatalf("reuse err = %v, want ErrPipelineStop", err)
	}
	if third.Replay == nil || third.Replay.Code != "idempotency_key_reused" {
		t.Fatalf("reuse decision = %+v", third.Replay)
	}
}

// TestIntegrationPreholdHonoursHoldCap drives the real ledger transaction.
func TestIntegrationPreholdHonoursHoldCap(t *testing.T) {
	userID := seedAccount(t, 6500)
	ledgerStore := ledger.NewStore(testDB, nil)

	deps := staticDeps(nil)
	deps.Ledger = ledgerStore
	deps.NodeID = "gw-test"
	deps.Config = nil

	stage := NewPreholdStage(deps)
	est := billingPreestimateForTest()
	c := &Ctx{
		RequestID: "hold-" + randomID(t),
		Auth:      &AuthContext{UserID: userID, KeyID: 1},
		Model:     &ModelEntry{ID: 1},
		Est:       &est,
	}
	// raw_est 6000 <= 6500, est 7200, hold = min(7200, 6500×50%) = 3250.
	c.Est.RawMicro = 6000
	c.Est.EstMicro = 7200
	c.RawEstMicro = 6000
	if err := stage.Run(t.Context(), c); err != nil {
		t.Fatalf("prehold: %v", err)
	}
	if c.Hold == nil || c.HoldMicro != 3250 {
		t.Fatalf("hold = %+v, want 3250", c.Hold)
	}
	var state string
	if err := testDB.QueryRowContext(t.Context(),
		`SELECT state FROM t_prehold_hold WHERE request_id = ?`, c.RequestID).Scan(&state); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if state != ledger.HoldStateHeld {
		t.Fatalf("state = %q, want HELD", state)
	}

	// A balance below raw_est must be 402 even though est would fit the cap.
	poorUser := seedAccount(t, 5000)
	poorEst := billingPreestimateForTest()
	poor := &Ctx{
		RequestID: "hold-poor-" + randomID(t),
		Auth:      &AuthContext{UserID: poorUser, KeyID: 1},
		Model:     &ModelEntry{ID: 1},
		Est:       &poorEst,
	}
	poor.Est.RawMicro = 6000
	poor.Est.EstMicro = 7200
	err := stage.Run(t.Context(), poor)
	assertAPIError(t, err, apierr.CodeInsufficientCredits, http.StatusPaymentRequired)
}

// TestIntegrationPreholdRejectsReusedRequestID is the B1 regression: X-Request-Id
// is client-controlled and is the prehold's unique key, so reusing the id of a
// finished request used to return a stale hold and let the request through
// without a second hold (the settlement then found SETTLED and answered with the
// historical result). The stage must answer 409 duplicate_request_id instead.
func TestIntegrationPreholdRejectsReusedRequestID(t *testing.T) {
	userID := seedAccount(t, 100000)
	seedRechargeBatch(t, userID, 100000)
	ledgerStore := ledger.NewStore(testDB, nil)

	requestID := "hold-reuse-" + randomID(t)
	// The real usage writer, wired into FailureSink exactly as gatewayd wires it
	// (main.go: FailureSink -> usageWriter.WriteUsage). A-12 lives in the
	// interaction between that writer and the prehold 409, so the test must use
	// the production path rather than a capturing stub.
	usageWriter := settle.NewUsageWriter(testDB, nil, nil)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM t_usage_record WHERE request_id = ?`, requestID)
	})

	deps := staticDeps(nil)
	deps.Ledger = ledgerStore
	deps.NodeID = "gw-test"
	deps.Config = nil
	deps.FailureSink = func(f FailureUsage) {
		usageWriter.WriteUsage(settle.UsageRecord{
			RequestID:    f.RequestID,
			UserID:       f.UserID,
			APIKeyID:     f.APIKeyID,
			APIKeyName:   f.APIKeyName,
			ModelID:      f.ModelID,
			ModelName:    f.ModelName,
			RequestModel: f.RequestModel,
			InboundProto: f.InboundProto,
			PriceRuleID:  f.PriceRuleID,
			GroupCode:    f.GroupCode,
			PriceRatioBP: f.PriceRatioBP,
			BillingTier:  "BASE",
			IsStream:     f.IsStream,
			HTTPStatus:   f.HTTPStatus,
			BizCode:      f.BizCode,
			Result:       "FAILED",
			FailStage:    f.FailStage,
			FailOwner:    f.FailOwner,
			UsageSource:  "NONE",
			TotalMS:      f.TotalMS,
			ClientIP:     f.ClientIP,
			UserAgent:    f.UserAgent,
			CreatedAt:    f.CreatedAt,
		})
	}
	stage := NewPreholdStage(deps)

	est := billingPreestimateForTest()
	est.RawMicro = 6000
	est.EstMicro = 7200
	first := &Ctx{
		RequestID: requestID,
		Auth:      &AuthContext{UserID: userID, KeyID: 1},
		Model:     &ModelEntry{ID: 1},
		Est:       &est,
	}
	if err := stage.Run(t.Context(), first); err != nil {
		t.Fatalf("first prehold: %v", err)
	}
	balanceAfterPrehold := accountTotal(t, userID)
	if balanceAfterPrehold != 100000-first.HoldMicro {
		t.Fatalf("balance after prehold = %d, want %d", balanceAfterPrehold, 100000-first.HoldMicro)
	}

	// --- A-12, incident order: the replay is rejected while the request is
	// still HELD, i.e. before the owner has written its usage row. This is the
	// order that poisons the table: the rejected row lands first, and the
	// owner's settlement row is then swallowed by uk_ur_request.
	reuseEst := billingPreestimateForTest()
	reuseEst.RawMicro = 90000
	reuseEst.EstMicro = 95000
	reuse := &Ctx{
		RequestID: requestID,
		Auth:      &AuthContext{UserID: userID, KeyID: 1},
		Model:     &ModelEntry{ID: 1},
		Est:       &reuseEst,
	}
	err := stage.Run(t.Context(), reuse)
	assertAPIError(t, err, apierr.CodeDuplicateRequestID, http.StatusConflict)
	if reuse.Hold != nil || reuse.HoldMicro != 0 {
		t.Fatalf("refused reuse still populated a hold: %+v", reuse.Hold)
	}
	if got := accountTotal(t, userID); got != balanceAfterPrehold {
		t.Fatalf("balance moved on refused reuse: %d -> %d", balanceAfterPrehold, got)
	}
	if state, _ := holdStateOf(t, requestID); state != ledger.HoldStateHeld {
		t.Fatalf("hold state after refused reuse = %s, want HELD", state)
	}

	// The production failure path: the server loop copies the stage error into
	// Ctx.ErrCode, then the deferred emitFailureUsage runs. The rejected row IS
	// written (it is the only record that the id was replayed); it wins the key
	// for now, and the settlement below must take it back.
	reuse.ErrCode = apierr.CodeDuplicateRequestID
	reuse.FailStage = StageP10
	emitFailureUsage(&Handler{d: deps}, reuse, false)
	if err := usageWriter.Flush(t.Context()); err != nil {
		t.Fatalf("flush failure usage: %v", err)
	}
	if rows, consumed := usageRowStats(t, requestID); rows != 1 || consumed != 0 {
		t.Fatalf("rejected replay row = %d rows / consumed %d, want 1 / 0 (FAILED, no money)", rows, consumed)
	}
	if result, biz := usageRowIdentity(t, requestID); result != "FAILED" || biz != apierr.CodeDuplicateRequestID {
		t.Fatalf("before settle the row = %s/%s, want FAILED/%s",
			result, biz, apierr.CodeDuplicateRequestID)
	}

	// --- The owner settles and writes its own row (P28). A-12: this row must
	// replace the FAILED/0 row the rejection left behind, otherwise the customer
	// is billed while the record says "failed, 0 credits".
	if _, err := ledgerStore.Settle(t.Context(), ledger.SettleParams{
		RequestID: requestID, UserID: userID, ConsumedMicro: 1000,
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	balanceAfterSettle := accountTotal(t, userID)
	if balanceAfterSettle != 99000 {
		t.Fatalf("balance after settle = %d, want 99000 (100000 - 1000 consumed)", balanceAfterSettle)
	}
	usageWriter.WriteUsage(settle.UsageRecord{
		RequestID: requestID, UserID: userID, APIKeyID: 1,
		ModelID: 1, ModelName: "gw-test", RequestModel: "gw-test",
		InboundProto: "OPENAI_COMPAT", GroupCode: "standard", PriceRatioBP: 10000,
		BillingTier: "BASE", HTTPStatus: 200, Result: "SUCCESS",
		UsageSource: "UPSTREAM", TokensIn: 30, TokensOut: 50,
		PreholdMicro: first.HoldMicro, ConsumedMicro: 1000,
		UpstreamAttempts: 1, CreatedAt: time.Now().UTC(),
	})
	if err := usageWriter.Flush(t.Context()); err != nil {
		t.Fatalf("flush owner usage: %v", err)
	}
	ownerRows, ownerConsumed := usageRowStats(t, requestID)
	if ownerRows != 1 || ownerConsumed != 1000 {
		t.Fatalf("owner usage = %d rows / consumed %d, want 1 / 1000 "+
			"(the settlement row must replace the rejected one)", ownerRows, ownerConsumed)
	}
	// Four-conjunct assertion. The first three can be satisfied by a row that is
	// internally consistent but wrong, so the fourth compares against the ledger —
	// the only authority on what was actually charged.
	//   (1) exactly one row for the id   (2) result is a completed state
	//   (3) consumed > 0                 (4) consumed == |CONSUME_SETTLE|
	gotResult, gotBizCode := usageRowIdentity(t, requestID)
	if gotResult != "SUCCESS" && gotResult != "PARTIAL" {
		t.Fatalf("result = %s; want SUCCESS or PARTIAL (an interrupted stream settles as PARTIAL)", gotResult)
	}
	if gotBizCode != "" {
		t.Fatalf("biz_code = %q; a completed row must not carry a rejection code", gotBizCode)
	}
	var settleSum int64
	if err := testDB.QueryRowContext(t.Context(),
		`SELECT COALESCE(SUM(amount_total_micro),0) FROM t_ledger_entry
		  WHERE request_id = ? AND biz_type = ?`,
		requestID, ledger.BizTypeConsumeSettle).Scan(&settleSum); err != nil {
		t.Fatalf("read settle entry: %v", err)
	}
	if settleSum != -1000 {
		t.Fatalf("CONSUME_SETTLE sum = %d, want -1000", settleSum)
	}
	if ownerConsumed != -settleSum {
		t.Fatalf("usage consumed %d != |CONSUME_SETTLE| %d; the record and the books disagree",
			ownerConsumed, -settleSum)
	}

	// Same id again, now that the owner has finished: this ordering must not
	// clobber the good row either (the opposite poisoning direction).
	late := &Ctx{
		RequestID: requestID,
		Auth:      &AuthContext{UserID: userID, KeyID: 1},
		Model:     &ModelEntry{ID: 1},
		Est:       &reuseEst,
	}
	err = stage.Run(t.Context(), late)
	assertAPIError(t, err, apierr.CodeDuplicateRequestID, http.StatusConflict)
	if late.Hold != nil || late.HoldMicro != 0 {
		t.Fatalf("refused post-settlement reuse still populated a hold: %+v", late.Hold)
	}
	if got := accountTotal(t, userID); got != balanceAfterSettle {
		t.Fatalf("balance moved on refused post-settlement reuse: %d -> %d", balanceAfterSettle, got)
	}
	if state, _ := holdStateOf(t, requestID); state != ledger.HoldStateSettled {
		t.Fatalf("hold state after refused reuse = %s, want SETTLED", state)
	}

	// A-12, opposite direction: a rejection that arrives AFTER the settlement
	// must not take the row back. Its INSERT is a no-op on the unique key and it
	// carries consumed_micro = 0, so the overwrite rule (only a row that spent
	// money may replace one that did not) leaves the good row alone.
	late.ErrCode = apierr.CodeDuplicateRequestID
	late.FailStage = StageP10
	emitFailureUsage(&Handler{d: deps}, late, false)
	if err := usageWriter.Flush(t.Context()); err != nil {
		t.Fatalf("flush failure usage: %v", err)
	}
	rowsAfter, consumedAfter := usageRowStats(t, requestID)
	if rowsAfter != 1 || consumedAfter != 1000 {
		t.Fatalf("usage after a post-settlement rejection = %d rows / consumed %d, want 1 / 1000",
			rowsAfter, consumedAfter)
	}
	if result, bizCode := usageRowIdentity(t, requestID); result != "SUCCESS" || bizCode != "" {
		t.Fatalf("usage row = %s/%q, want SUCCESS with no biz_code "+
			"(a rejection must never overwrite a paid row)", result, bizCode)
	}
}

// usageRowIdentity reads the (result, biz_code) of the single usage row for one
// request_id.
func usageRowIdentity(t *testing.T, requestID string) (string, string) {
	t.Helper()
	var result, bizCode string
	if err := testDB.QueryRowContext(t.Context(),
		`SELECT result, COALESCE(biz_code,'') FROM t_usage_record WHERE request_id = ?`,
		requestID).Scan(&result, &bizCode); err != nil {
		t.Fatalf("read usage identity %s: %v", requestID, err)
	}
	return result, bizCode
}

// TestPreholdReuseWarnCarriesTheAccount pins the only line an operator can use
// to answer "which account kept reusing ids": the 409 WARN must name the
// request_id AND the account, or a complaint cannot be attributed to a tenant.
func TestPreholdReuseWarnCarriesTheAccount(t *testing.T) {
	userID := seedAccount(t, 100000)
	seedRechargeBatch(t, userID, 100000)
	buf := &bytes.Buffer{}

	deps := staticDeps(nil)
	deps.Ledger = ledger.NewStore(testDB, nil)
	deps.NodeID = "gw-test"
	deps.Config = nil
	deps.Logger = obslog.NewWithWriter("gatewayd", "test", buf, nil)
	stage := NewPreholdStage(deps)

	requestID := "hold-warn-" + randomID(t)
	est := billingPreestimateForTest()
	est.RawMicro, est.EstMicro = 6000, 7200
	p := &Ctx{
		RequestID: requestID,
		Auth:      &AuthContext{UserID: userID, KeyID: 42},
		Model:     &ModelEntry{ID: 1},
		Est:       &est,
	}
	if err := stage.Run(t.Context(), p); err != nil {
		t.Fatalf("first prehold: %v", err)
	}
	err := stage.Run(t.Context(), p)
	assertAPIError(t, err, apierr.CodeDuplicateRequestID, http.StatusConflict)

	line := ""
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "prehold_request_id_reused") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("no prehold_request_id_reused line in %q", buf.String())
	}
	for _, want := range []string{requestID, `"uid":"` + strconv.FormatInt(userID, 10) + `"`, `"kid":"42"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("WARN line %q does not carry %s", line, want)
		}
	}
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM t_ledger_entry WHERE request_id = ?`, requestID)
		_, _ = testDB.Exec(`DELETE FROM t_prehold_hold WHERE request_id = ?`, requestID)
	})
}

// holdStateOf reads one prehold row's state.
func holdStateOf(t *testing.T, requestID string) (string, int64) {
	t.Helper()
	var state string
	var hold int64
	if err := testDB.QueryRowContext(t.Context(),
		`SELECT state, hold_micro FROM t_prehold_hold WHERE request_id = ?`,
		requestID).Scan(&state, &hold); err != nil {
		t.Fatalf("read hold %s: %v", requestID, err)
	}
	return state, hold
}

// usageRowStats returns how many usage rows exist for one request_id and the sum
// of their consumed_micro.
func usageRowStats(t *testing.T, requestID string) (int64, int64) {
	t.Helper()
	var rows, consumed int64
	if err := testDB.QueryRowContext(t.Context(),
		`SELECT COUNT(*), COALESCE(SUM(consumed_micro),0) FROM t_usage_record WHERE request_id = ?`,
		requestID).Scan(&rows, &consumed); err != nil {
		t.Fatalf("read usage stats: %v", err)
	}
	return rows, consumed
}

// accountTotal reads the materialised balance for one account.
func accountTotal(t *testing.T, userID int64) int64 {
	t.Helper()
	var total int64
	if err := testDB.QueryRowContext(t.Context(),
		`SELECT balance_micro FROM t_credit_account WHERE user_id = ?`, userID).Scan(&total); err != nil {
		t.Fatalf("read account %d: %v", userID, err)
	}
	return total
}

// seedRechargeBatch backs the account with one refundable RECHARGE batch so a
// real settlement can consume it. seedAccount only writes the account row.
func seedRechargeBatch(t *testing.T, userID, amountMicro int64) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := testDB.ExecContext(t.Context(), `INSERT INTO t_credit_batch
		(user_id, batch_no, batch_type, face_value_micro, remaining_micro, consumed_micro,
		 refundable, effective_at, expire_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, 1, ?, NULL, ?, ?)`,
		userID, fmt.Sprintf("B%d%09d", userID, now.UnixNano()%1_000_000_000),
		ledger.BatchTypeRecharge, amountMicro, amountMicro, now, now, now); err != nil {
		t.Fatalf("seed recharge batch for %d: %v", userID, err)
	}
}

func TestIntegrationStartupCheckPassesOnSeededConfig(t *testing.T) {
	if err := config.StartupCheck(t.Context(), testDB); err != nil {
		t.Fatalf("startup check: %v", err)
	}
}

// billingPreestimateForTest returns a small estimate with the token shape the
// rate limiter and prehold need.
func billingPreestimateForTest() billing.Preestimate {
	return billing.Preestimate{
		Tokens: billing.TokenEstimate{
			InputTokens: 100, OutputTokens: 100, SafeInputTokens: 110, SafeOutputTokens: 110, SafetyBP: 11000,
		},
		RawMicro: 0, EstMicro: 0,
	}
}

func errAs(err error, target any) bool {
	return errors.As(err, target)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestRoutingSnapshotDecodesFromRedis proves the cfg:snap:ROUTING decoder runs
// against a payload written through the real T05 loader, including the P06
// whitelist and the four-dimension policy.
func TestRoutingSnapshotDecodesFromRedis(t *testing.T) {
	store := writeRouting(t, map[string]any{
		"models": []map[string]any{{
			"id": 1, "model_name": "snap-model", "region": "DOMESTIC",
			"model_type": "TEXT", "listing_state": "LISTED", "context_length": 4096,
		}},
		"routes": []map[string]any{{
			"id": 1, "model_id": 1, "channel_id": 1, "upstream_model_name": "up",
			"region": "DOMESTIC", "weight": 10, "priority": 100,
			"stream_supported": true, "enabled": 1,
		}},
		"channels": []map[string]any{{
			"id": 1, "region": "DOMESTIC", "enabled": true,
			"group_scope_mode": "ALL", "stream_supported": 1,
		}},
		"groups": map[string]any{
			"standard": map[string]any{"code": "standard", "ratio_bp": 10000,
				"model_scope_mode": "ALL", "channel_scope_mode": "ALL"},
		},
		"keyModels": map[string]any{"10": map[string]any{"mode": "ALL"}},
		"ipRules": map[string]any{
			"10": map[string]any{"version": 3, "cidrs": []string{"10.1.0.0/16"}},
		},
		"rateLimits": map[string]any{
			"KEY:10": map[string]any{"rpm": 120, "tpm": 100000, "concurrency": 7},
			"USER:7": map[string]any{"daily_quota_micro": 5000},
		},
	})

	catalog := NewCatalog(store)
	snap, err := catalog.Current()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	model := snap.ModelByName("snap-model")
	if model == nil || model.ID != 1 {
		t.Fatalf("model = %+v", model)
	}
	if routes := snap.RoutesFor(1); len(routes) != 1 || routes[0].ChannelID != 1 {
		t.Fatalf("routes = %+v", routes)
	}
	if ch, ok := snap.Channel(1); !ok || !ch.Enabled.Bool() {
		t.Fatalf("channel = %+v, %v", ch, ok)
	}
	limits, ok := snap.RateLimitFor(10, 7)
	if !ok || limits.RPM != 120 || limits.Concurrency != 7 ||
		limits.DailyQuotaMicro != 5000 || !limits.DailyEnabled {
		t.Fatalf("limits = %+v, ok=%v", limits, ok)
	}

	deps := staticDeps(catalog)
	allowed := &Ctx{
		ClientIP: netip.MustParseAddr("10.1.2.3"),
		Auth:     &AuthContext{KeyID: 10, IPMRuleVer: 3},
	}
	if err := NewIPStage(deps).Run(t.Context(), allowed); err != nil {
		t.Fatalf("whitelisted IP rejected: %v", err)
	}
	denied := &Ctx{
		ClientIP: netip.MustParseAddr("8.8.8.8"),
		Auth:     &AuthContext{KeyID: 10, IPMRuleVer: 3},
	}
	assertAPIError(t, NewIPStage(deps).Run(t.Context(), denied),
		apierr.CodeIPNotAllowed, http.StatusForbidden)

	stale := &Ctx{
		ClientIP: netip.MustParseAddr("10.1.2.3"),
		Auth:     &AuthContext{KeyID: 10, IPMRuleVer: 2},
	}
	assertAPIError(t, NewIPStage(deps).Run(t.Context(), stale),
		apierr.CodeIPNotAllowed, http.StatusForbidden)
}

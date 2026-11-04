package fund

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/auth"
	"yunshu.dev/core/internal/control/ctltest"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

const testPaySecret = "test-callback-secret"

var testDB *sql.DB

// TestMain serialises this package against internal/acceptance, the other test
// binary that drives the plan-grant path. Both run under `go test ./...` and
// share one yunshu_test schema, and two concurrent applyPlanTx calls deadlock on
// the uk_rlp_scope_active supremum gap lock (see ctltest.PlanGrantLock for the
// InnoDB dump). The lock is a property of the *fixture*, not of one assertion,
// so it is taken once here instead of in every test that happens to grant a
// plan: missing one call site would put the flake straight back.
func TestMain(m *testing.M) {
	db, err := ctltest.SetupDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fund tests: %v\n", err)
		os.Exit(1)
	}
	testDB = db
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	unlockPlanGrant, err := ctltest.LockPlanGrant(ctx, db, 5*time.Minute)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fund tests: %v\n", err)
		os.Exit(1)
	}
	// os.Exit skips deferred calls, so the lock is released explicitly.
	code := redistest.Setup(m)
	unlockPlanGrant()
	_ = db.Close()
	os.Exit(code)
}

func newTestService(t *testing.T) (*Service, *redisx.Client) {
	t.Helper()
	// 2s command budget, not redisx.DefaultCommandTimeout (50ms): the 50ms is a
	// production latency policy, and as a fixture budget it turns a stall of the
	// one shared Redis server into auth_unavailable (503) under `go test ./...`.
	rdb := redisx.New(redisx.Options{
		Addr:           redistest.Addr(),
		DB:             redistest.DB(),
		PoolSize:       10,
		CommandTimeout: 2 * time.Second,
		LuaTimeout:     2 * time.Second,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := New(testDB, rdb, nil, &audit.Writer{DB: testDB, Now: clock.Real{}}, clock.Real{}, nil)
	svc.PaySecrets = map[string]string{
		PayChannelWechat: testPaySecret,
		PayChannelAlipay: testPaySecret,
	}
	svc.FingerprintKey = "test-fp-hmac-key"
	return svc, rdb
}

var userSeq uint64

// newTestUser inserts one account with an empty credit account. planID nil
// means "never bought a plan" (the free default).
func newTestUser(t *testing.T, planID *int64) int64 {
	t.Helper()
	ctx := context.Background()
	userSeq++
	var uid int64
	var lastErr error
	for attempt := 0; attempt < 5 && uid == 0; attempt++ {
		var noise [4]byte
		_, _ = rand.Read(noise[:])
		userNo := fmt.Sprintf("F%s%03d", time.Now().Format("20060102"),
			(int(binary.BigEndian.Uint32(noise[:]))+int(userSeq))%1000)
		email := fmt.Sprintf("t13-fund-%d-%d-%d@test.local", time.Now().UnixNano(), userSeq, attempt)
		res, err := testDB.ExecContext(ctx, `
			INSERT INTO t_user
				(user_no, email, password_hash, password_algo, password_updated_at,
				 nickname, role, status, plan_id, created_at, updated_at)
			VALUES (?,?,'not-a-real-hash','bcrypt',?,'t13','PERSONAL','NORMAL',?,?,?)`,
			userNo, email, time.Now(), planID, time.Now(), time.Now())
		if err != nil {
			lastErr = err
			if !strings.Contains(err.Error(), "uk_user_no") {
				t.Fatalf("insert user: %v", err)
			}
			continue
		}
		uid, err = res.LastInsertId()
		if err != nil {
			t.Fatalf("user id: %v", err)
		}
	}
	if uid == 0 {
		t.Fatalf("insert user after retries: %v", lastErr)
	}
	if _, err := testDB.ExecContext(ctx,
		`INSERT INTO t_credit_account (user_id, created_at, updated_at) VALUES (?,?,?)`,
		uid, time.Now(), time.Now()); err != nil {
		t.Fatalf("insert credit account: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_ledger_entry WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_credit_batch WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_rate_limit_policy WHERE scope_id = ? AND scope_type = 'USER'`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE rt FROM t_review_task rt
			JOIN t_refund_order r ON r.id = rt.biz_id AND rt.biz_type = 'REFUND_APPLY'
			WHERE r.user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE rt FROM t_review_task rt
			JOIN t_invoice_apply i ON i.id = rt.biz_id AND rt.biz_type = 'INVOICE_APPLY'
			WHERE i.user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE rt FROM t_review_task rt
			JOIN t_quota_apply q ON q.id = rt.biz_id AND rt.biz_type = 'QUOTA_APPLY'
			WHERE q.user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_refund_order WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_invoice_apply WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_quota_apply WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_device_fingerprint WHERE user_id = ? OR granted_user_id = ?`, uid, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_notification WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_recharge_order WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_redeem_code WHERE redeemed_by = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_admin_audit_log WHERE operator_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_credit_account WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_phone_claim
			WHERE first_user_id = ? OR last_claim_user_id = ? OR last_trial_user_id = ?`,
			uid, uid, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

func planIDByCode(t *testing.T, code string) int64 {
	t.Helper()
	var id int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT id FROM t_plan WHERE code = ?`, code).Scan(&id); err != nil {
		t.Fatalf("plan %s: %v", code, err)
	}
	return id
}

func planByCode(t *testing.T, code string) planView {
	t.Helper()
	var v planView
	var (
		daily sql.NullInt64
		perm  int64
	)
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT id, code, name, price_fen, grant_recharge_micro, grant_bonus_micro,
		       bonus_valid_days, rpm, tpm, concurrency, daily_quota_micro,
		       permanent, sort_order
		  FROM t_plan WHERE code = ?`, code).Scan(
		&v.ID, &v.Code, &v.Name, &v.PriceFen, &v.GrantRechargeMicro, &v.GrantBonusMicro,
		&v.BonusValidDays, &v.RPM, &v.TPM, &v.Concurrency, &daily, &perm, &v.SortOrder); err != nil {
		t.Fatalf("plan row %s: %v", code, err)
	}
	if daily.Valid {
		d := daily.Int64
		v.DailyQuotaMicro = &d
	}
	v.Permanent = perm == 1
	return v
}

// call invokes a console handler with a synthetic session.
func call(t *testing.T, h http.HandlerFunc, method, target string, body any, uid int64) *httptest.ResponseRecorder {
	t.Helper()
	return callWithHeaders(t, h, method, target, body, uid, nil)
}

// callWithHeaders is call plus request headers (Idempotency-Key and friends).
func callWithHeaders(t *testing.T, h http.HandlerFunc, method, target string, body any, uid int64, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = jsonx.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(payload))
	req = req.WithContext(mw.WithSession(req.Context(), &mw.Session{UID: uid, Role: "PERSONAL"}))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// callCallback invokes the public callback handler for one channel.
func callCallback(t *testing.T, svc *Service, channel string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(body)
	if err != nil {
		t.Fatalf("marshal callback: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pay/callback/"+channel, bytes.NewReader(payload))
	req.SetPathValue("channel", channel)
	rec := httptest.NewRecorder()
	svc.Callback(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := jsonx.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response (%d %s): %v", rec.Code, rec.Body.String(), err)
	}
}

type apiErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	} `json:"error"`
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) apiErrorBody {
	t.Helper()
	var body apiErrorBody
	decodeBody(t, rec, &body)
	return body
}

// createOrder posts a console order and returns the view.
func createOrder(t *testing.T, svc *Service, uid int64, body map[string]any) (orderView, *httptest.ResponseRecorder) {
	t.Helper()
	rec := call(t, svc.CreateOrder, http.MethodPost, "/api/console/orders", body, uid)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create order status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp createOrderResponse
	decodeBody(t, rec, &resp)
	return resp.Order, rec
}

// successfulCallback signs and delivers one SUCCESS notice.
func successfulCallback(t *testing.T, svc *Service, order orderView, tradeNo string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{
		"out_trade_no": order.OrderNo,
		"trade_no":     tradeNo,
		"amount_fen":   order.PayAmountFen,
		"status":       callbackStatusSuccess,
		"sign":         signCallback(testPaySecret, order.OrderNo, tradeNo, order.PayAmountFen, callbackStatusSuccess),
	}
	return callCallback(t, svc, "wechat", body)
}

type balanceRow struct {
	Recharge int64
	Bonus    int64
	Total    int64
}

func balanceOf(t *testing.T, uid int64) balanceRow {
	t.Helper()
	var b balanceRow
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT balance_recharge_micro, balance_bonus_micro, balance_micro
		  FROM t_credit_account WHERE user_id = ?`, uid).
		Scan(&b.Recharge, &b.Bonus, &b.Total); err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b
}

func countLedger(t *testing.T, uid int64) int64 {
	t.Helper()
	var n int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_ledger_entry WHERE user_id = ?`, uid).Scan(&n); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	return n
}

func countBatches(t *testing.T, uid int64) int64 {
	t.Helper()
	var n int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_credit_batch WHERE user_id = ?`, uid).Scan(&n); err != nil {
		t.Fatalf("count batches: %v", err)
	}
	return n
}

func orderState(t *testing.T, orderID int64) string {
	t.Helper()
	var st string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT state FROM t_recharge_order WHERE id = ?`, orderID).Scan(&st); err != nil {
		t.Fatalf("order state: %v", err)
	}
	return st
}

// activePolicy returns the running USER-scope rate-limit row.
func activePolicy(t *testing.T, uid int64) (source string, rpm, tpm, conc int64, daily *int64) {
	t.Helper()
	var (
		d  sql.NullInt64
		to sql.NullTime
	)
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT source, rpm, tpm, concurrency_quota, daily_quota_micro, effective_to
		  FROM t_rate_limit_policy
		 WHERE scope_type = 'USER' AND scope_id = ?
		   AND (effective_to IS NULL OR effective_to > ?)
		 ORDER BY effective_from DESC, id DESC LIMIT 1`, uid, time.Now().UTC()).
		Scan(&source, &rpm, &tpm, &conc, &d, &to); err != nil {
		t.Fatalf("active policy: %v", err)
	}
	if d.Valid {
		v := d.Int64
		daily = &v
	}
	return source, rpm, tpm, conc, daily
}

// seedRedeemBatch inserts one batch and one code, returning the plaintext.
func seedRedeemBatch(t *testing.T, createdBy int64, faceValueFen int64, validTo time.Time) (plain, codeHash string, codeID int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	res, err := testDB.ExecContext(ctx, `
		INSERT INTO t_redeem_batch
			(batch_name, face_value_fen, quantity, generated_count, valid_from, valid_to,
			 credit_target, created_by, created_at)
		VALUES (?,?,10,1,?,?,'BONUS',?,?)`,
		"t13-test-batch", faceValueFen, now.Add(-time.Hour), validTo, createdBy, now)
	if err != nil {
		t.Fatalf("insert redeem batch: %v", err)
	}
	batchID, _ := res.LastInsertId()
	plain = strings.ToUpper("T13CODE" + strconv.FormatInt(now.UnixNano()%1000000, 10))
	sum := sha256.Sum256([]byte(plain))
	codeHash = hex.EncodeToString(sum[:])
	cres, err := testDB.ExecContext(ctx, `
		INSERT INTO t_redeem_code
			(batch_id, code_hash, code_prefix, status, valid_to, created_at)
		VALUES (?,?,?,'UNUSED',?,?)`, batchID, codeHash, plain[:6], validTo, now)
	if err != nil {
		t.Fatalf("insert redeem code: %v", err)
	}
	codeID, _ = cres.LastInsertId()
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_redeem_code WHERE batch_id = ?`, batchID)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_redeem_batch WHERE id = ?`, batchID)
	})
	return plain, codeHash, codeID
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	raw, err := jsonx.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// payPlanOrder creates and pays one PLAN order, returning the order view.
func payPlanOrder(t *testing.T, svc *Service, uid int64, code string) orderView {
	t.Helper()
	plan := planByCode(t, code)
	order, _ := createOrder(t, svc, uid, map[string]any{
		"order_type":  OrderTypePlan,
		"plan_id":     plan.ID,
		"pay_channel": PayChannelWechat,
	})
	rec := successfulCallback(t, svc, order, "WX"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("pay callback status = %d body=%s", rec.Code, rec.Body.String())
	}
	return order
}

// consumeDebits burns credits through the ledger's real FIFO path, which is
// what makes A4's bonus-first split (1,000 / 2,000) verifiable.
func consumeDebits(t *testing.T, svc *Service, uid, credits int64) {
	t.Helper()
	_, err := svc.Ledger.Debit(context.Background(), ledger.DebitParams{
		UserID:      uid,
		AmountMicro: credits * MicroPerCredit,
		RequestID:   fmt.Sprintf("t13c-%d-%012d", uid%100000, time.Now().UnixNano()%1_000_000_000_000),
	})
	if err != nil {
		t.Fatalf("consume %d credits: %v", credits, err)
	}
}

// consumeG04 builds G-04's exact ledger state: 1,000 bonus consumed while it
// was valid, the remaining 1,000 bonus expired (and therefore unusable) before
// the next 2,000 came out of recharge. The expired batch keeps its remaining
// amount until jobsd sweeps it, so the books still show 1,000 bonus after the
// refund — which is exactly errata E-10's corrected expectation.
func consumeG04(t *testing.T, svc *Service, uid int64) {
	t.Helper()
	consumeDebits(t, svc, uid, 1000)
	if _, err := testDB.ExecContext(context.Background(), `
		UPDATE t_credit_batch
		   SET expire_at = UTC_TIMESTAMP(3) - INTERVAL 1 SECOND
		 WHERE user_id = ? AND batch_type = 'BONUS' AND remaining_micro > 0`, uid); err != nil {
		t.Fatalf("expire remaining bonus: %v", err)
	}
	consumeDebits(t, svc, uid, 2000)
}

func frozenOf(t *testing.T, uid int64) int64 {
	t.Helper()
	var frozen int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT frozen_refund_micro FROM t_credit_account WHERE user_id = ?`, uid).
		Scan(&frozen); err != nil {
		t.Fatalf("frozen: %v", err)
	}
	return frozen
}

func orderRefundableMicro(t *testing.T, orderID int64) int64 {
	t.Helper()
	var v int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT refundable_micro FROM t_recharge_order WHERE id = ?`, orderID).Scan(&v); err != nil {
		t.Fatalf("order refundable: %v", err)
	}
	return v
}

// setUserPhone gives uid a phone number and the matching phone_hmac.
//
// The hmac is not optional: t_user.phone_hmac carries uk_user_phone_hmac, and
// trial de-duplication compares hashes. A fixture that wrote only the plaintext
// column would leave the row in the "phone but no hmac" state the production
// code answers 500 for — so every trial test would exercise the fail-closed
// branch instead of the one it names, and a green run would mean nothing.
func setUserPhone(t *testing.T, uid int64, phone string) {
	t.Helper()
	mac, err := phoneHMACForTest(phone)
	if err != nil {
		t.Fatalf("phone hmac for %s: %v", phone, err)
	}
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone = ?, phone_hmac = ? WHERE id = ?`, phone, mac, uid); err != nil {
		t.Fatalf("set phone: %v", err)
	}
	// The ledger row is part of "this account has a number": every write path
	// creates it (registration through insertUserRow, operator provisioning
	// through the same funnel, seedctl), and the trial path fails closed without
	// it. A fixture that only set the two t_user columns would be modelling a
	// state production cannot reach.
	upsertClaimForTest(t, mac, uid)
}

// setUserPhoneWithoutClaim builds the state the trial path must refuse: a phone
// hash on the account and no ledger row. Only a manual edit or a backfill that
// has not run can produce it — which is exactly why the code fails closed
// instead of granting a trial nobody can de-duplicate.
func setUserPhoneWithoutClaim(t *testing.T, uid int64, phone string) {
	t.Helper()
	mac, err := phoneHMACForTest(phone)
	if err != nil {
		t.Fatalf("phone hmac for %s: %v", phone, err)
	}
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone = ?, phone_hmac = ? WHERE id = ?`, phone, mac, uid); err != nil {
		t.Fatalf("set phone: %v", err)
	}
	if _, err := testDB.ExecContext(context.Background(),
		`DELETE FROM t_phone_claim WHERE phone_hmac = ?`, mac); err != nil {
		t.Fatalf("clear claim: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(),
			`DELETE FROM t_phone_claim WHERE phone_hmac = ?`, mac)
	})
}

// upsertClaimForTest mirrors what registration writes, and then heals
// trial_granted_at from the account row the way the backfill does — a fixture
// that says "this account already claimed" must put that fact where the trial
// check now looks for it.
func upsertClaimForTest(t *testing.T, hmac string, uid int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := testDB.ExecContext(ctx, `
		INSERT INTO t_phone_claim
		  (phone_hmac, first_user_id, last_claim_user_id, last_claim_at, cancel_count,
		   created_at, updated_at)
		VALUES (?,?,?,?,0,?,?)
		ON DUPLICATE KEY UPDATE
		  last_claim_user_id = VALUES(last_claim_user_id),
		  last_claim_at      = VALUES(last_claim_at),
		  updated_at         = VALUES(updated_at)`,
		hmac, uid, uid, now, now, now); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	if _, err := testDB.ExecContext(ctx, `
		UPDATE t_phone_claim c JOIN t_user u ON u.phone_hmac = c.phone_hmac
		   SET c.trial_granted_at = COALESCE(c.trial_granted_at, u.trial_claimed_at),
		       c.last_trial_user_id = COALESCE(c.last_trial_user_id,
		           IF(u.trial_claimed_at IS NULL, NULL, u.id)),
		       c.updated_at = ?
		 WHERE c.phone_hmac = ?`, now, hmac); err != nil {
		t.Fatalf("heal claim: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(),
			`DELETE FROM t_phone_claim WHERE phone_hmac = ?`, hmac)
	})
}

// setUserPhoneWithoutHMAC reproduces the transitional state the trial path must
// fail closed on: a phone on the row, no hash. Only a manual UPDATE (or a
// backfill that has not run yet) can produce it, which is exactly why the code
// refuses rather than guessing.
//
// It is also the only way a fixture can give two accounts the same number once
// uk_user_phone_hmac is in force — the unique key lives on the hash, not on the
// plaintext column.
func setUserPhoneWithoutHMAC(t *testing.T, uid int64, phone string) {
	t.Helper()
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone = ?, phone_hmac = NULL WHERE id = ?`, phone, uid); err != nil {
		t.Fatalf("set phone without hmac: %v", err)
	}
}

// testPhoneHMACKey is the fixture key. It is 32+ bytes so the production
// length rule is satisfied by construction.
const testPhoneHMACKey = "t13-phone-hmac-key-for-fixtures-32b"

// phoneHMACForTest hashes with the same implementation production uses.
func phoneHMACForTest(phone string) (string, error) {
	return auth.PhoneHMAC(testPhoneHMACKey, phone)
}

func markVerified(t *testing.T, uid int64) {
	t.Helper()
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET verify_status = 'VERIFIED', verified_at = NOW(3) WHERE id = ?`, uid); err != nil {
		t.Fatalf("mark verified: %v", err)
	}
}

// callWithRemote is callWithHeaders plus a RemoteAddr, for the trial IP-segment
// check and for auditing a realistic operator IP.
func callWithRemote(t *testing.T, h http.HandlerFunc, method, target string, body any, uid int64, remote string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = jsonx.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(payload))
	req.RemoteAddr = remote
	req = req.WithContext(mw.WithSession(req.Context(), &mw.Session{UID: uid, Role: "PERSONAL"}))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func refundStateOf(t *testing.T, refundID int64) string {
	t.Helper()
	var state string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT state FROM t_refund_order WHERE id = ?`, refundID).Scan(&state); err != nil {
		t.Fatalf("refund state: %v", err)
	}
	return state
}

func countNotifications(t *testing.T, uid int64, event string) int64 {
	t.Helper()
	var n int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_notification WHERE user_id = ? AND event_code = ?`, uid, event).
		Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

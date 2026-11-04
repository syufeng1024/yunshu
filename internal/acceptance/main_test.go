// Package acceptance encodes T21's promised behaviour as tests that run on
// every commit. The 14 CI items (ci14_test.go) and the 12 GWT scenarios
// (gwt_test.go) use the real MySQL schema and the real Redis instance through
// either the control-plane mux, the gateway stages, or the exported domain
// services. Nothing in this package mocks the database or Redis.
//
// Test roots:
//   - TEST_DB_DSN        MySQL DSN (scripts/test-env.sh resolves a local one)
//   - TEST_REDIS_ADDR    Redis address
//   - TEST_REDIS_DB      optional override; otherwise redistest hashes the
//     package directory to DB 1..15.
package acceptance

import (
	"context"
	"crypto/hmac"
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
	"sync"
	"testing"
	"time"

	"yunshu.dev/core/internal/boot"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control"
	"yunshu.dev/core/internal/control/auth"
	"yunshu.dev/core/internal/control/ctltest"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

var (
	testDB     *sql.DB
	testRedis  *redisx.Client
	testSess   *mw.SessionManager
	testMux    *http.ServeMux
	testServer *control.Server
	serverErr  error
	serverOnce sync.Once

	testRouteBuilder *boot.RouteSnapshot
	testCacheWriter  *boot.CacheWriter
	testPublisher    *config.Publisher

	seqMu  sync.Mutex
	seqVal uint64
)

const (
	paySecret   = "t21-pay-secret"
	fingerprint = "t21-fingerprint-key"
	// phoneHMACKey is PHONE_HMAC_KEY for this binary. Registration and operator
	// provisioning hash the phone number with it, so it must be ≥32 bytes or
	// those paths answer 503 config_unavailable (which is asserted elsewhere, in
	// internal/control/auth).
	phoneHMACKey = "t21-phone-hmac-key-for-acceptance-32b"
)

func TestMain(m *testing.M) {
	db, err := ctltest.SetupDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "acceptance tests: %v\n", err)
		os.Exit(1)
	}
	testDB = db

	// CHANNEL_KEY_KEK is read by control.New. Use a fixed local key so the
	// channel-write assertions can decode the envelope if they need to.
	_ = os.Setenv("CHANNEL_KEY_KEK", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	_ = os.Setenv("FP_HMAC_KEY", fingerprint)
	// PHONE_HMAC_KEY feeds t_user.phone_hmac, which carries uk_user_phone_hmac.
	// The fixture below writes the hash itself, so this must be the same value
	// the assertions hash with (phoneHMACForTest).
	_ = os.Setenv("PHONE_HMAC_KEY", phoneHMACKey)

	// Serialise this binary against internal/control/fund: both drive the
	// plan-grant path (G-09 claims a trial, fund's trial/confirm tests do the
	// same) and two concurrent grants deadlock on the uk_rlp_scope_active
	// supremum gap lock, which surfaces here as a 500 from /trial/claim. See
	// ctltest.PlanGrantLock for the InnoDB deadlock dump.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	unlockPlanGrant, err := ctltest.LockPlanGrant(ctx, testDB, 5*time.Minute)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "acceptance tests: %v\n", err)
		os.Exit(1)
	}

	// os.Exit skips deferred calls, so the lock is released explicitly.
	code := redistest.Setup(m)
	unlockPlanGrant()
	_ = db.Close()
	os.Exit(code)
}

// newAcceptanceRedis opens this package's Redis client with a command budget
// sized for a machine that runs a dozen package binaries at once.
//
// redisx.DefaultCommandTimeout is 50ms and it is a production latency policy.
// As a fixture budget it turns a momentary stall of the one shared Redis server
// into auth_unavailable (503) from SessionManager.Load — observed repeatedly
// while `go test ./...` was running: G-09's third claim, and the CSRF test in
// internal/control, both answered 503 where the assertion expects 409/204.
// Nothing these tests assert is about Redis latency, so the fixture asks for 2s.
func newAcceptanceRedis() *redisx.Client {
	return redisx.New(redisx.Options{
		Addr:           redistest.Addr(),
		DB:             redistest.DB(),
		PoolSize:       10,
		CommandTimeout: 2 * time.Second,
		LuaTimeout:     2 * time.Second,
	})
}

// server builds the control plane exactly once. The closures mirror
// cmd/controld/main.go: admin writes publish the routing snapshot and refresh
// K19 through the same boot builders production uses.
func server(t *testing.T) (*control.Server, *http.ServeMux) {
	t.Helper()
	serverOnce.Do(func() {
		rdb := newAcceptanceRedis()
		testRedis = rdb
		sess, err := mw.NewSessionManager(testDB, rdb, "t21-acceptance-session-secret")
		if err != nil {
			serverErr = err
			return
		}
		testSess = sess
		sess.Secure = false // httptest requests are plain HTTP

		cfgRedis := config.NewGoRedis(rdb.Raw())
		testPublisher = &config.Publisher{
			DB: testDB, Redis: cfgRedis, Now: clock.Real{}.Now, LockTimeout: 30 * time.Second,
		}
		testRouteBuilder = &boot.RouteSnapshot{
			DB: testDB, Publisher: testPublisher, Now: clock.Real{},
		}
		testCacheWriter = &boot.CacheWriter{
			DB:     testDB,
			Prices: &config.PriceCache{Redis: cfgRedis},
			Keys:   redisx.NewKeyAuthCache(rdb),
			Now:    clock.Real{},
		}

		srv, err := control.New(control.Deps{
			DB:       testDB,
			Redis:    rdb,
			Sessions: sess,
			Now:      clock.Real{},
			PublishRoutes: func(ctx context.Context) error {
				_, _, err := testRouteBuilder.Publish(ctx)
				return err
			},
			PublishPrices: func(ctx context.Context) error {
				_, err := testCacheWriter.RefreshPrices(ctx)
				return err
			},
			PaySecrets:     map[string]string{"WECHAT": paySecret, "ALIPAY": paySecret},
			FingerprintKey: fingerprint,
			// Same value as the PHONE_HMAC_KEY set in TestMain: registration
			// hashes the phone with it and the fixture hashes with it too, so a
			// test that registers a number the fixture also wrote collides on
			// uk_user_phone_hmac the way production would.
			PhoneHMACKey: phoneHMACKey,
		})
		if err != nil {
			serverErr = err
			return
		}
		mux := http.NewServeMux()
		srv.Register(mux)
		testServer, testMux = srv, mux
	})
	if serverErr != nil {
		t.Fatalf("build control server: %v", serverErr)
	}
	return testServer, testMux
}

// ---------------------------------------------------------------------------
// sessions and HTTP
// ---------------------------------------------------------------------------

func issueSession(t *testing.T, uid int64) (string, string) {
	t.Helper()
	server(t)
	sess, cookie, err := testSess.Issue(context.Background(), uid)
	if err != nil {
		t.Fatalf("issue session uid=%d: %v", uid, err)
	}
	return cookie, testSess.CSRFToken(sess.SID)
}

type callOpts struct {
	header     map[string]string
	remoteAddr string
}

func call(t *testing.T, uid int64, method, path string, body any, opts ...callOpts) *httptest.ResponseRecorder {
	t.Helper()
	_, mux := server(t)
	var payload []byte
	if body != nil {
		var err error
		payload, err = jsonx.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	if uid > 0 {
		cookie, csrf := issueSession(t, uid)
		req.AddCookie(&http.Cookie{Name: mw.SessionCookieName, Value: cookie})
		req.AddCookie(&http.Cookie{Name: mw.CSRFCookieName, Value: csrf})
		req.Header.Set(mw.CSRFHeaderName, csrf)
	}
	for _, o := range opts {
		for k, v := range o.header {
			req.Header.Set(k, v)
		}
		if o.remoteAddr != "" {
			req.RemoteAddr = o.remoteAddr
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

type apiErrorBody struct {
	Error struct {
		Type      string   `json:"type"`
		Code      string   `json:"code"`
		Message   string   `json:"message"`
		Hint      string   `json:"hint"`
		RequestID string   `json:"request_id"`
		Limit     *float64 `json:"limit"`
		Remaining *float64 `json:"remaining"`
		ResetAt   string   `json:"reset_at"`
	} `json:"error"`
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := jsonx.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
}

func apiErr(t *testing.T, rec *httptest.ResponseRecorder) apiErrorBody {
	t.Helper()
	var body apiErrorBody
	decodeBody(t, rec, &body)
	return body
}

func requireStatus(t *testing.T, label string, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("%s: status=%d body=%s, want %d", label, rec.Code, rec.Body.String(), want)
	}
}

func requireCode(t *testing.T, label string, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) apiErrorBody {
	t.Helper()
	requireStatus(t, label, rec, wantStatus)
	body := apiErr(t, rec)
	if body.Error.Code != wantCode {
		t.Fatalf("%s: code=%q body=%s, want %q", label, body.Error.Code, rec.Body.String(), wantCode)
	}
	return body
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func nextInt() uint64 {
	seqMu.Lock()
	defer seqMu.Unlock()
	seqVal++
	return seqVal
}

func rid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "t21-" + hex.EncodeToString(b[:])
}

func ticketNo() string { return "T21" + strconv.FormatInt(time.Now().UnixNano()%1e12, 10) }

type userOpts struct {
	role       string
	planID     *int64
	verified   bool
	phone      string
	overdraft  int64
	trialClaim *time.Time
}

func newUser(t *testing.T, o userOpts) int64 {
	t.Helper()
	if o.role == "" {
		o.role = "PERSONAL"
	}
	now := time.Now().UTC()
	overdraft := o.overdraft
	if overdraft == 0 {
		overdraft = 500000
	}
	var uid int64
	for attempt := 0; attempt < 8 && uid == 0; attempt++ {
		var noise [4]byte
		_, _ = rand.Read(noise[:])
		no := fmt.Sprintf("T%011d", (time.Now().UnixNano()+int64(nextInt())+int64(binary.BigEndian.Uint32(noise[:])))%100000000000)
		email := fmt.Sprintf("t21-%d-%d-%d@test.local", time.Now().UnixNano(), nextInt(), attempt)
		var plan any
		if o.planID != nil {
			plan = *o.planID
		}
		var verified any
		if o.verified {
			verified = now
		}
		var phone any
		var phoneMAC any
		if o.phone != "" {
			phone = o.phone
			// t_user.phone_hmac carries uk_user_phone_hmac and is what the trial
			// de-duplication compares, so a fixture that wrote only the plaintext
			// column would produce rows the production code treats as "phone but
			// no hmac" (trial claim answers 500).
			mac, macErr := auth.PhoneHMAC(phoneHMACKey, o.phone)
			if macErr != nil {
				t.Fatalf("phone hmac for fixture: %v", macErr)
			}
			phoneMAC = mac
		}
		var trial any
		if o.trialClaim != nil {
			trial = *o.trialClaim
		}
		res, err := testDB.ExecContext(context.Background(), `
			INSERT INTO t_user
				(user_no, email, phone, phone_hmac, password_hash, password_algo, password_updated_at,
				 nickname, role, status, plan_id, verified_at, trial_claimed_at,
				 overdraft_limit_micro, created_at, updated_at)
			VALUES (?,?,?,?,'not-a-real-hash','bcrypt',?,?,?, 'NORMAL', ?,?,?,?,?,?)`,
			no, email, phone, phoneMAC, now, "t21", o.role, plan, verified, trial, overdraft, now, now)
		if err != nil {
			if strings.Contains(err.Error(), "uk_user_no") || strings.Contains(err.Error(), "uk_user_email") ||
				strings.Contains(err.Error(), "uk_user_phone_hmac") {
				continue
			}
			t.Fatalf("insert user: %v", err)
		}
		uid, err = res.LastInsertId()
		if err != nil {
			t.Fatalf("user id: %v", err)
		}
	}
	if uid == 0 {
		t.Fatal("could not allocate a unique user")
	}
	_, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_credit_account (user_id, overdraft_limit_micro, created_at, updated_at)
		VALUES (?,?,?,?)`, uid, overdraft, now, now)
	if err != nil {
		t.Fatalf("insert credit account: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM t_prehold_hold WHERE user_id = ?`,
			`DELETE FROM t_ledger_entry WHERE user_id = ?`,
			`DELETE FROM t_credit_batch WHERE user_id = ?`,
			`DELETE FROM t_arrears WHERE user_id = ?`,
			`DELETE FROM t_usage_record WHERE user_id = ?`,
			`DELETE FROM t_upstream_attempt WHERE user_id = ?`,
			`DELETE FROM t_ticket_reply WHERE ticket_id IN (SELECT id FROM t_ticket WHERE user_id = ?)`,
			`DELETE FROM t_ticket WHERE user_id = ?`,
			`DELETE FROM t_review_task WHERE applicant_id = ?`,
			`DELETE FROM t_quota_apply WHERE user_id = ?`,
			`DELETE FROM t_refund_order WHERE user_id = ?`,
			`DELETE FROM t_invoice_apply WHERE user_id = ?`,
			`DELETE FROM t_recharge_order WHERE user_id = ?`,
			`DELETE FROM t_notification WHERE user_id = ?`,
			`DELETE FROM t_device_fingerprint WHERE user_id = ? OR granted_user_id = ?`,
			`DELETE FROM t_user_daily_quota WHERE user_id = ?`,
			`DELETE FROM t_rate_limit_policy WHERE scope_type = 'USER' AND scope_id = ?`,
			`DELETE FROM t_idempotency_record WHERE user_id = ?`,
			`DELETE FROM t_admin_audit_log WHERE operator_id = ?`,
			`DELETE FROM t_api_key_audit WHERE api_key_id IN (SELECT id FROM t_api_key WHERE user_id = ?)`,
			`DELETE FROM t_api_key_model WHERE api_key_id IN (SELECT id FROM t_api_key WHERE user_id = ?)`,
			`DELETE FROM t_api_key_ip_rule WHERE api_key_id IN (SELECT id FROM t_api_key WHERE user_id = ?)`,
			`DELETE FROM t_api_key WHERE user_id = ?`,
			`DELETE FROM t_credit_account WHERE user_id = ?`,
			`DELETE FROM t_user WHERE id = ?`,
		} {
			if strings.Contains(q, "granted_user_id") {
				_, _ = testDB.ExecContext(ctx, q, uid, uid)
			} else {
				_, _ = testDB.ExecContext(ctx, q, uid)
			}
		}
	})
	return uid
}

func setAccount(t *testing.T, uid, recharge, bonus int64) {
	t.Helper()
	_, err := testDB.ExecContext(context.Background(), `
		UPDATE t_credit_account
		   SET balance_recharge_micro = ?, balance_bonus_micro = ?,
		       balance_micro = ?, updated_at = UTC_TIMESTAMP(3)
		 WHERE user_id = ?`, recharge, bonus, recharge+bonus, uid)
	if err != nil {
		t.Fatalf("set account: %v", err)
	}
}

type batchOpts struct {
	typ       string
	face      int64
	remaining int64
	expireAt  *time.Time
	sourceID  *int64
}

func addBatch(t *testing.T, uid int64, o batchOpts) int64 {
	t.Helper()
	if o.typ == "" {
		o.typ = ledger.BatchTypeRecharge
	}
	refundable := 0
	if o.typ == ledger.BatchTypeRecharge {
		refundable = 1
	}
	now := time.Now().UTC()
	no := fmt.Sprintf("T%d%06d", time.Now().Unix()%1e7, nextInt()%1000000)
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_credit_batch
			(user_id, batch_no, batch_type, source_id, face_value_micro, remaining_micro,
			 consumed_micro, refundable, effective_at, expire_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		uid, no, o.typ, o.sourceID, o.face, o.remaining, o.face-o.remaining,
		refundable, now, o.expireAt, now, now)
	if err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("batch id: %v", err)
	}
	return id
}

func protocolID(t *testing.T, code string) int64 {
	t.Helper()
	var id int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT id FROM t_protocol WHERE code = ?`, code).Scan(&id); err != nil {
		t.Fatalf("protocol %s: %v", code, err)
	}
	return id
}

func planID(t *testing.T, code string) int64 {
	t.Helper()
	var id int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT id FROM t_plan WHERE code = ?`, code).Scan(&id); err != nil {
		t.Fatalf("plan %s: %v", code, err)
	}
	return id
}

func newModel(t *testing.T, name, listing string) int64 {
	t.Helper()
	if listing == "" {
		listing = "LISTED"
	}
	now := time.Now().UTC()
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_model
			(model_name, display_name, region, model_type, protocol_id, context_length,
			 listing_state, published_at, created_at, updated_at)
		VALUES (?,?,'DOMESTIC','TEXT',?,32768,?,?,?,?)`,
		name, name, protocolID(t, "OPENAI_COMPAT"), listing, now, now, now)
	if err != nil {
		t.Fatalf("insert model: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("model id: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_price_rule WHERE scope_id = ?`, id)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_route WHERE model_id = ?`, id)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_model WHERE id = ?`, id)
	})
	return id
}

func modelName() string {
	return fmt.Sprintf("t21-model-%d-%d", time.Now().UnixNano(), nextInt())
}

func addPrice(t *testing.T, modelID, inPrice, outPrice, cost int64) int64 {
	t.Helper()
	now := time.Now().UTC()
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_price_rule
			(scope_level, scope_id, model_type, in_price_micro_per_k, out_price_micro_per_k,
			 per_call_price_micro, cost_in_micro_per_k, cost_out_micro_per_k,
			 currency_cost, version, effective_from, enabled, created_by, created_at)
		VALUES ('MODEL',?,'TEXT',?,?,0,?,?,'CNY',1,?,1,1,?)`,
		modelID, inPrice, outPrice, cost, cost, now, now)
	if err != nil {
		t.Fatalf("insert price: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("price id: %v", err)
	}
	return id
}

func newUserKey(t *testing.T, uid int64, name string) (int64, string) {
	t.Helper()
	rec := call(t, uid, http.MethodPost, "/api/console/keys", map[string]any{
		"name":             name,
		"model_scope_mode": "ALL",
		"expire_at":        time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
	})
	requireStatus(t, "create key", rec, http.StatusCreated)
	var body struct {
		Items []struct {
			ID   int64  `json:"id"`
			Key  string `json:"key"`
			Mask string `json:"key_masked"`
		} `json:"items"`
	}
	decodeBody(t, rec, &body)
	if len(body.Items) != 1 {
		t.Fatalf("create key items = %d", len(body.Items))
	}
	return body.Items[0].ID, body.Items[0].Key
}

func insertUsage(t *testing.T, requestID string, uid, modelID int64, at time.Time, result string, consumed int64) {
	t.Helper()
	_, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_usage_record
			(request_id, user_id, api_key_id, api_key_name_snap, model_id, model_name_snap,
			 request_model_req, inbound_protocol, http_status, result, consumed_micro,
			 total_ms, created_at)
		VALUES (?,?,1,'t21',?,?,'t21','OPENAI_COMPAT',200,?,?,1,?)`,
		requestID, uid, modelID, "t21", result, consumed, at.UTC())
	if err != nil {
		t.Fatalf("insert usage %s: %v", requestID, err)
	}
}

func publishRoutes(t *testing.T) {
	t.Helper()
	server(t)
	if _, _, err := testRouteBuilder.Publish(context.Background()); err != nil {
		t.Fatalf("publish routing snapshot: %v", err)
	}
}

func publishPrices(t *testing.T) {
	t.Helper()
	server(t)
	if _, err := testCacheWriter.RefreshPrices(context.Background()); err != nil {
		t.Fatalf("refresh K19: %v", err)
	}
}

func futureRFC3339(d time.Duration) string {
	return time.Now().UTC().Add(d).Format(time.RFC3339)
}

// signCallback mirrors internal/control/fund's canonical string. It is
// duplicated on purpose: if the production formula changes, G-03 fails rather
// than silently agreeing with itself.
func signCallback(secret, outTradeNo, tradeNo string, amountFen int64, status string) string {
	canonical := outTradeNo + "|" + tradeNo + "|" + strconv.FormatInt(amountFen, 10) + "|" + status
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

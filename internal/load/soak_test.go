//go:build soak

// Package load holds the T21 soak baseline. It is excluded from the normal
// build and from `go test ./...`; the nightly workflow runs it with -tags soak.
//
// The load is generated against the real gateway handler (auth → routing →
// estimate → rate limit → prehold → terminal) with the real MySQL and Redis
// instances. The terminal settles with ConsumedMicro=0, so the money path is
// exercised (prehold + release) without pretending to produce tokens.
package load

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yunshu.dev/core/internal/apikey"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control/ctltest"
	"yunshu.dev/core/internal/gw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/ratelimit"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

const (
	defaultQPS      = 200
	defaultDuration = 10 * time.Minute
	// PRD §9.4: gateway-added latency P99 ≤ 200ms.
	maxP99 = 200 * time.Millisecond
	// §9.3's 99.5% is a monthly SLO, not a 30-second sample. Two defects had
	// to be fixed before this gate meant anything: the prehold idempotency
	// read took a FOR UPDATE gap lock on a row that did not exist yet (1213 on
	// ~0.7% of requests), and this test never refreshed K19, so everything
	// after the 120s TTL was a 500. The gate is a regression bound, not the
	// SLO. Raise it only with a new measurement in docs/test-matrix.md.
	defaultMaxErrorPct = 2.5
)

func TestSoak(t *testing.T) {
	qps := intEnv(t, "SOAK_QPS", defaultQPS)
	duration := durationEnv(t, "SOAK_DURATION", defaultDuration)
	maxErrorPct := floatEnv(t, "SOAK_MAX_ERROR_PCT", defaultMaxErrorPct)

	db, err := ctltest.SetupDB()
	if err != nil {
		t.Fatalf("soak: db: %v", err)
	}
	// ctltest's 20-connection ceiling is sized for unit tests. A 200 QPS soak
	// needs enough connections that the 200ms prehold budget measures the
	// transaction, not the pool queue.
	db.SetMaxOpenConns(120)
	db.SetMaxIdleConns(120)
	t.Cleanup(func() { _ = db.Close() })

	redisEnv, err := redistest.Open()
	if err != nil {
		t.Fatalf("soak: redis: %v", err)
	}
	t.Cleanup(func() { _ = redisEnv.Close() })
	// Deliberately NOT redistest.NewClient: its 2s fixture budget would hide the
	// latency this soak exists to measure. The prehold budget below is 200ms and
	// the pool sizes are tuned so the measurement is the transaction, not the
	// connection queue — keep the production 50ms command budget here.
	rdb := redisx.New(redisx.Options{Addr: redisEnv.Addr, DB: redisEnv.DB, PoolSize: 64})
	t.Cleanup(func() { _ = rdb.Close() })

	const (
		userBase  = int64(9_900_000)
		userCount = 64
		modelID   = int64(9_900_003)
		modelName = "soak-model"
	)
	// One account serialises its preholds on the account row. 200 QPS against
	// one user would measure lock contention, not the gateway; spread the load
	// across 16 independent accounts, the way real traffic arrives.
	keyModels := map[string]gw.KeyModelGrant{}
	rateLimits := map[string]gw.LimitPolicy{}
	plains := make([]string, 0, userCount)
	for i := 0; i < userCount; i++ {
		userID := userBase + int64(i)*10
		keyID := userID + 1
		seedSoakUser(t, db, userID)
		plain, hash16, err := seedSoakKey(t, rdb, userID, keyID)
		if err != nil {
			t.Fatalf("soak: seed K13 for %d: %v", userID, err)
		}
		hash16Copy := hash16
		t.Cleanup(func() { _ = rdb.Raw().Del(context.Background(), redisx.APIKeyHashKey(hash16Copy)).Err() })
		plains = append(plains, plain)
		keyModels[strconv.FormatInt(keyID, 10)] = gw.KeyModelGrant{Mode: gw.ScopeAll}
		rateLimits["KEY:"+strconv.FormatInt(keyID, 10)] = gw.LimitPolicy{
			RPM: 1_000_000, TPM: 1_000_000_000, Concurrency: 100_000,
		}
		rateLimits["USER:"+strconv.FormatInt(userID, 10)] = gw.LimitPolicy{
			DailyQuotaMicro: 1_000_000_000_000,
		}
	}
	if err := seedPrice(rdb, modelID); err != nil {
		t.Fatalf("soak: seed K19: %v", err)
	}
	// K19 expires after config.PriceCacheTTL (120s); production re-publishes it
	// from t_price_rule on controld's timer, but this test seeds it once. Without
	// a refresher every request after the second minute fails with
	// 500 price_not_configured — measured 48001 of 60000 (80.002%) on a
	// 10-minute run, which reads like a capacity collapse but is the cache
	// expiring. 30s is comfortably inside the TTL.
	const priceRefreshEvery = 30 * time.Second
	stopRefresh := make(chan struct{})
	var refreshWG sync.WaitGroup
	refreshWG.Add(1)
	go func() {
		defer refreshWG.Done()
		tk := time.NewTicker(priceRefreshEvery)
		defer tk.Stop()
		for {
			select {
			case <-stopRefresh:
				return
			case <-tk.C:
				if err := seedPrice(rdb, modelID); err != nil {
					// The load will surface the resulting 500s. Never log from
					// here: t.Logf after the test completes panics.
					return
				}
			}
		}
	}()
	t.Cleanup(func() { close(stopRefresh); refreshWG.Wait() })

	catalog := gw.NewStaticCatalog(&gw.RoutingSnapshot{
		Models: []gw.ModelEntry{{
			ID: modelID, ModelName: modelName, Region: gw.RegionDomestic,
			ModelType: gw.ModelTypeText, ContextLength: 32768, MaxOutputTokens: 1024,
			ListingState: gw.ListingListed,
		}},
		Routes: []gw.RouteEntry{{
			ID: 1, ModelID: modelID, ChannelID: 1, ProtocolID: 1,
			UpstreamModelName: modelName, Region: gw.RegionDomestic,
			Weight: 100, Priority: 100, StreamSupported: true, Enabled: true,
		}},
		Channels: []gw.ChannelEntry{{
			ID: 1, Name: "soak", ProtocolID: 1, Region: gw.RegionDomestic,
			Weight: 100, Priority: 100, GroupScopeMode: gw.ScopeAll,
			Enabled: true, StreamSupported: true, RetryMax: int64Ptr(0),
		}},
		Groups: map[string]gw.GroupEntry{
			"standard": {Code: "standard", RatioBP: 10000, ModelScopeMode: gw.ScopeAll,
				ChannelScopeMode: gw.ScopeAll, Enabled: true},
		},
		KeyModels:  keyModels,
		RateLimits: rateLimits,
	})

	ledgerStore := &soakLedger{Store: ledger.NewStore(db, clock.Real{})}
	deps := &gw.Deps{
		Catalog:      catalog,
		Auth:         redisx.NewKeyAuthCache(rdb),
		Balance:      dbBalance{db: db},
		Prices:       &config.PriceCache{Redis: config.NewGoRedis(rdb.Raw())},
		Ledger:       ledgerStore,
		Limiter:      ratelimit.New(rdb, redisx.NewClock(nil, nil), nil, nil, nil),
		Tokens:       gw.HeuristicCounter{},
		Clock:        redisx.NewClock(nil, nil),
		NodeID:       "soak",
		Backpressure: gw.NewBackpressure(nil),
		ConnGuard:    gw.NewConnGuard(nil),
	}
	if err := deps.Validate(); err != nil {
		t.Fatalf("soak: deps: %v", err)
	}

	terminal := gw.TerminalFunc(func(w http.ResponseWriter, r *http.Request, c *gw.Ctx) {
		if c.Auth != nil && c.RequestID != "" {
			_, _ = ledgerStore.Settle(r.Context(), ledger.SettleParams{
				RequestID: c.RequestID, UserID: c.Auth.UserID,
				ConsumedMicro: 0, ReleaseReason: "SOAK",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"soak","object":"chat.completion"}`))
	})
	handler := gw.NewHandler(deps, terminal)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"soak"}],"max_tokens":1}`, modelName)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 512,
			MaxConnsPerHost:     512,
		},
	}

	workers := qps / 10
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan struct{}, qps)
	var (
		wg         sync.WaitGroup
		latencies  = make([]time.Duration, 0, qps*int(duration.Seconds())+qps)
		latMu      sync.Mutex
		failures   atomic.Int64
		total      atomic.Int64
		errSamples []string
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		// One key per worker: two in-flight requests must not share an account
		// row, or the measurement becomes lock contention on that row.
		workerKey := plains[i%len(plains)]
		go func(workerKey string) {
			defer wg.Done()
			for range jobs {
				start := time.Now()
				req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+workerKey)
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				elapsed := time.Since(start)
				ok := err == nil && resp.StatusCode == http.StatusOK
				snippet := ""
				if resp != nil {
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
					snippet = string(body)
					_ = resp.Body.Close()
				}
				if !ok {
					failures.Add(1)
					latMu.Lock()
					if len(errSamples) < 5 {
						status := 0
						if resp != nil {
							status = resp.StatusCode
						}
						errSamples = append(errSamples, fmt.Sprintf("status=%d err=%v body=%s", status, err, snippet))
					}
					latMu.Unlock()
				}
				total.Add(1)
				latMu.Lock()
				latencies = append(latencies, elapsed)
				latMu.Unlock()
			}
		}(workerKey)
	}

	deadline := time.Now().Add(duration)
	interval := time.Second / time.Duration(qps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		<-ticker.C
		select {
		case jobs <- struct{}{}:
		default:
			// The workers are behind; drop this tick rather than queue without
			// bound. Failures still count, so a saturated handler is visible.
			failures.Add(1)
			total.Add(1)
		}
	}
	close(jobs)
	wg.Wait()

	n := int(total.Load())
	if n == 0 {
		t.Fatal("soak: no requests were issued")
	}
	latMu.Lock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p99 := latencies[(len(latencies)*99)/100-1]
	p999 := latencies[(len(latencies)*999)/1000-1]
	samples := append([]string(nil), errSamples...)
	latMu.Unlock()

	errPct := float64(failures.Load()) * 100 / float64(n)
	if causes := ledgerStore.Causes(); len(causes) > 0 {
		t.Logf("soak: ledger errors: %v", causes)
	}
	t.Logf("soak: qps=%d duration=%s requests=%d errors=%d (%.3f%%) p99=%s p99.9=%s",
		qps, duration, n, failures.Load(), errPct, p99, p999)
	t.Logf("soak: first errors: %v", samples)
	if errPct > maxErrorPct {
		t.Fatalf("soak: error rate %.3f%% > %.1f%% (PRD §9.3)", errPct, maxErrorPct)
	}
	if p99 > maxP99 {
		t.Fatalf("soak: P99 %s > %s (PRD §9.4)", p99, maxP99)
	}
}

// soakLedger records the first prehold/settle errors. The gateway reports every
// one as settlement_unavailable; the cause (deadline, duplicate, SQL) is only
// visible here.
type soakLedger struct {
	*ledger.Store
	mu    sync.Mutex
	cause []string
}

func (l *soakLedger) record(err error) {
	if err == nil {
		return
	}
	l.mu.Lock()
	if len(l.cause) < 8 {
		l.cause = append(l.cause, err.Error())
	}
	l.mu.Unlock()
}

func (l *soakLedger) Prehold(ctx context.Context, p ledger.PreholdParams) (ledger.PreholdResult, error) {
	r, err := l.Store.Prehold(ctx, p)
	l.record(err)
	return r, err
}

func (l *soakLedger) Settle(ctx context.Context, p ledger.SettleParams) (ledger.SettleResult, error) {
	r, err := l.Store.Settle(ctx, p)
	l.record(err)
	return r, err
}

func (l *soakLedger) Causes() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.cause...)
}

type dbBalance struct{ db *sql.DB }

func (b dbBalance) Get(ctx context.Context, userID int64) (int64, error) {
	var bonus, recharge, frozen int64
	err := b.db.QueryRowContext(ctx, `
		SELECT balance_bonus_micro, balance_recharge_micro, frozen_refund_micro
		  FROM t_credit_account WHERE user_id = ?`, userID).Scan(&bonus, &recharge, &frozen)
	if err != nil {
		return 0, err
	}
	return bonus + recharge - frozen, nil
}

func seedSoakUser(t *testing.T, db *sql.DB, uid int64) {
	t.Helper()
	now := time.Now().UTC()
	_, _ = db.Exec(`DELETE FROM t_credit_account WHERE user_id = ?`, uid)
	_, _ = db.Exec(`DELETE FROM t_user WHERE id = ?`, uid)
	if _, err := db.Exec(`
		INSERT INTO t_user
			(id, user_no, email, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at)
		VALUES (?, ?, ?, 'x', 'bcrypt', ?, 'soak', 'PERSONAL', 'NORMAL', ?, ?)`,
		uid, fmt.Sprintf("SOAK%08d", uid%100_000_000), fmt.Sprintf("soak-%d@test.local", uid), now, now, now); err != nil {
		t.Fatalf("soak: insert user: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO t_credit_account
			(user_id, balance_recharge_micro, balance_micro, overdraft_limit_micro, created_at, updated_at)
		VALUES (?, 1000000000000, 1000000000000, 500000, ?, ?)`, uid, now, now); err != nil {
		t.Fatalf("soak: insert account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM t_credit_account WHERE user_id = ?`, uid)
		_, _ = db.Exec(`DELETE FROM t_user WHERE id = ?`, uid)
	})
}

func seedSoakKey(t *testing.T, rdb *redisx.Client, uid, kid int64) (string, string, error) {
	t.Helper()
	plain, hash, err := apikey.Generate()
	if err != nil {
		return "", "", err
	}
	hash16, err := apikey.HashPrefix16(hash)
	if err != nil {
		return "", "", err
	}
	err = redisx.NewKeyAuthCache(rdb).Upsert(context.Background(), hash16, redisx.KeyAuthRecord{
		UID: uid, KID: kid, ST: "ENABLED", GRP: "standard", GP: 10000,
	})
	return plain, hash16, err
}

func seedPrice(rdb *redisx.Client, modelID int64) error {
	payload, err := jsonx.Marshal(gw.PriceRule{
		ID: 1, ScopeLevel: "MODEL", ModelType: "TEXT",
		InPriceMicroPerK: 1500, OutPriceMicroPerK: 4500,
	})
	if err != nil {
		return err
	}
	return (&config.PriceCache{Redis: config.NewGoRedis(rdb.Raw())}).
		SetStandard(context.Background(), modelID, payload, config.PriceCacheTTL)
}

func int64Ptr(v int64) *int64 { return &v }

func intEnv(t *testing.T, key string, def int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		t.Fatalf("%s=%q is not a positive integer", key, raw)
	}
	return v
}

func floatEnv(t *testing.T, key string, def float64) float64 {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		t.Fatalf("%s=%q is not a non-negative number", key, raw)
	}
	return v
}

func durationEnv(t *testing.T, key string, def time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		t.Fatalf("%s=%q is not a positive duration", key, raw)
	}
	return v
}

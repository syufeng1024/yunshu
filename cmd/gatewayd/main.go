// Command gatewayd is the stateless data-plane process.
//
// It owns the public API listener (GATEWAY_HTTP_ADDR) and a separate metrics
// listener. It never opens a general main-database pool; the only main-database
// channel is the restricted gateway write DSN configured by
// GATEWAY_DB_MAX_CONNS (8 connections, 5min lifetime, 200ms prehold txn).
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"yunshu.dev/core/internal/balance"
	"yunshu.dev/core/internal/breaker"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/cryptobox"
	"yunshu.dev/core/internal/dispatch"
	"yunshu.dev/core/internal/gw"
	"yunshu.dev/core/internal/idemstore"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/logq"
	"yunshu.dev/core/internal/obs/health"
	obslog "yunshu.dev/core/internal/obs/log"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/db"
	"yunshu.dev/core/internal/platform/env"
	"yunshu.dev/core/internal/platform/httpsrv"
	"yunshu.dev/core/internal/ratelimit"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/settle"
)

const service = env.ServiceGateway

func main() {
	rootCtx := context.Background()

	cfg, err := env.Load(service)
	if err != nil {
		obslog.NewWithWriter(service, os.Getenv("ENV"), os.Stderr, clock.Real{}).
			Error(rootCtx, "startup failed: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}

	logger := obslog.New(service, cfg.Env)
	logger.Info(rootCtx, "starting", obslog.Fields{Stage: "startup"})

	startedAt := time.Now()
	reg := obsmetrics.NewRegistry()
	rec := obsmetrics.NewRecorder(reg)

	// The restricted gateway channel: 8 connections by default, 5min lifetime.
	gwDB, err := db.OpenGateway(cfg.GWWriteDSN, cfg.GatewayDBMaxConns)
	if err != nil {
		logger.Error(rootCtx, "open gateway db channel failed: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	defer gwDB.Close()

	rdb := redisx.New(redisx.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		PoolSize: 50,
	})
	defer rdb.Close()
	if err := rdb.Ping(rootCtx); err != nil {
		logger.Error(rootCtx, "redis ping failed: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	if err := rdb.CheckPolicy(rootCtx); err != nil {
		logger.Error(rootCtx, "redis policy rejected: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}

	// Configuration gate (T05): an unregistered strategy must stop the process
	// before the listener starts, not surface as a per-request fallback.
	if err := config.StartupCheck(rootCtx, gwDB); err != nil {
		logger.Error(rootCtx, "config startup check failed: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}

	store := config.NewStore()
	cfgRedis := config.NewGoRedis(rdb.Raw())
	cfgLogger := obsConfigLogger{logger: logger}
	loader := &config.Loader{Redis: cfgRedis, Store: store, Logger: cfgLogger}
	if err := loader.RefreshAll(rootCtx); err != nil {
		// Not fatal: the store carries built-in defaults and the watcher keeps
		// retrying. /readyz reports the dependency state to the load balancer.
		logger.Warn(rootCtx, "initial config snapshot unavailable: "+err.Error(), obslog.Fields{Stage: "startup"})
	}
	// TDS §5.3: a zero LoadedAt here means the process is serving built-in
	// defaults. The counter is lazy, so a normal startup produces no series at
	// all; a fabricated zero would disarm the ">0 即 P1" alert.
	if snap := store.Snapshot(config.NsUpstream); snap == nil || snap.LoadedAt().IsZero() {
		rec.ConfigBootstrapUsed()
	}

	ctx, cancel := context.WithCancel(rootCtx)
	defer cancel()

	watcher := &config.Watcher{
		Redis:  cfgRedis,
		Store:  store,
		Loader: loader,
		Logger: cfgLogger,
	}
	go func() {
		if err := watcher.Run(ctx); err != nil {
			logger.Error(ctx, "config watcher stopped: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		}
	}()

	redisClock := redisx.NewClock(logger, nil)
	stopClock := redisClock.StartSync(ctx, rdb, 30*time.Second)
	defer stopClock()

	nodeID := hostname()
	go observeConfig(ctx, store, rec, nodeID, startedAt)
	logStream := logq.New(rdb, 0)
	logStream.Metrics = rec
	go observeLogBacklog(ctx, logStream, rec)

	// Degradation mode comes from the environment (TDS §2.3 gives V1 one value,
	// concurrency_only). A value that is not understood stops startup: silently
	// falling back would leave the process running with a limiter shape nobody
	// asked for, and the matrix in docs/degradation.md would describe something
	// else than the binary.
	degradeMode, modeErr := ratelimit.ModeFromEnv()
	if modeErr != nil {
		logger.Error(rootCtx, "ratelimit degrade mode misconfigured: "+modeErr.Error(),
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	deg := ratelimit.NewDegrader(rdb, redisClock, nodeID, degradeMode, 200_000)
	limiter := ratelimit.New(rdb, redisClock, deg, rec, logger)
	logger.Info(rootCtx, fmt.Sprintf("ratelimit degrade mode=%s", degradeMode),
		obslog.Fields{Stage: "startup"})

	// Seed the cached replica divisor before the first request so a Redis
	// outage right after boot still has a last-known value. Errors are
	// non-fatal: the cache starts at floor 1 and the heartbeat loop retries.
	if err := deg.Heartbeat(ctx); err != nil {
		logger.Warn(ctx, "degrader initial heartbeat failed: "+err.Error(),
			obslog.Fields{Stage: "startup"})
	}
	if err := deg.RefreshReplicas(ctx); err != nil {
		logger.Warn(ctx, "degrader initial replica refresh failed: "+err.Error(),
			obslog.Fields{Stage: "startup"})
	}
	stopHeartbeat := deg.StartHeartbeat(ctx)
	defer stopHeartbeat()

	idemOpts := idemstore.DefaultOptions()
	if v, ok := store.Int(config.KeyIdemLeaseTTLSeconds); ok {
		idemOpts.LeaseTTL = time.Duration(v) * time.Second
	}
	if v, ok := store.Int(config.KeyIdemLeaseRenewSeconds); ok {
		idemOpts.RenewEvery = time.Duration(v) * time.Second
	}
	if v, ok := store.Int(config.KeyIdemDuplicateWindowSeconds); ok {
		idemOpts.DuplicateWindow = time.Duration(v) * time.Second
	}

	mirror := balance.New(rdb, gw.DBAccountSource{DB: gwDB}, 60*time.Second, logger)
	brk := breaker.New(rdb, redisClock, logger)

	// T09: P26 enqueue, GR-03 backlog gate, P28/P29 asynchronous rows and
	// P30 metric accumulation. All of them use the existing restricted
	// gateway channel; none of them is allowed to fail the request.
	enqueuer := settle.NewEnqueuer(gwDB, clock.Real{})
	backlog := settle.NewBacklog(gwDB, clock.Real{})
	usageWriter := settle.NewUsageWriter(gwDB, logger, clock.Real{})
	metricAcc := settle.NewMetrics(rdb, logger, clock.Real{})

	// The writers outlive the HTTP drain: cancel() stops accepting work, and
	// the final flush must still happen after the last request settles.
	bgCtx, bgCancel := context.WithCancel(context.WithoutCancel(ctx))
	writerDone := make(chan struct{})
	metricDone := make(chan struct{})
	go func() { defer close(writerDone); _ = usageWriter.Run(bgCtx) }()
	go func() { defer close(metricDone); _ = metricAcc.Run(bgCtx) }()

	backpressure := gw.NewBackpressure(logger)
	backpressure.Metrics = rec

	// P06.5 content safety (T42). The keyword set always runs. The external
	// adapter is built only when GW_POLICY_ENDPOINT is set; an unset endpoint
	// leaves the keyword checker alone instead of installing an always-allow
	// implementation. A malformed endpoint or timeout stops startup: the
	// operator asked for an external check, and silently ignoring the typo
	// would make the deployment look protected when it is not.
	keywordPolicy := gw.NewKeywordChecker()
	policyChecker := gw.PolicyChecker(keywordPolicy)
	externalPolicy, policyErr := gw.NewHTTPPolicyCheckerFromEnv()
	if policyErr != nil {
		logger.Error(rootCtx, "content policy external checker misconfigured: "+policyErr.Error(),
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	if externalPolicy != nil {
		policyChecker = gw.NewChainPolicyChecker(keywordPolicy, externalPolicy)
		logger.Info(rootCtx, "content policy external checker enabled",
			obslog.Fields{Stage: "startup"})
	}

	deps := &gw.Deps{
		Config:        store,
		Catalog:       gw.NewCatalog(store),
		Auth:          redisx.NewKeyAuthCache(rdb),
		Balance:       mirror,
		Prices:        &config.PriceCache{Redis: cfgRedis},
		Ledger:        ledger.NewStore(gwDB, clock.Real{}),
		Limiter:       limiter,
		Idem:          idemstore.New(rdb, redisClock, idemstore.NewMySQL(gwDB), idemOpts, logger),
		Breaker:       brk,
		Tokens:        gw.HeuristicCounter{},
		CounterReader: gw.RedisCounters{Client: rdb},
		Backpressure:  backpressure,
		ConnGuard:     gw.NewConnGuard(logger),
		SettleBacklog: backlog,
		Logger:        logger,
		Clock:         redisClock,
		Metrics:       rec,
		NodeID:        nodeID,
		// P06.5 content safety (T42). The keyword half is in-process and has no
		// configuration key (D-08 freezes the set); hits are written through the
		// same restricted gateway channel as every other gw write.
		PolicyChecker: policyChecker,
		PolicyHits:    gw.ContentPolicyHitWriter{DB: gwDB},
		// G23: requests that fail before the terminal still produce a usage
		// row. The adapter keeps every money column structurally zero.
		FailureSink: func(f gw.FailureUsage) {
			usageWriter.WriteUsage(rejectedUsageRecord(f))
		},
	}
	if err := deps.Validate(); err != nil {
		logger.Error(rootCtx, "gateway wiring incomplete: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	deps.WarnDisabled(rootCtx)

	// The channel KEK. A missing or malformed KEK does not stop the process:
	// every dispatch then answers 503 config_unavailable, which is visible and
	// never falls back to a plaintext key (T08 hard constraint 21).
	box, err := cryptobox.FromEnv()
	if err != nil {
		logger.Error(rootCtx, "channel key KEK unavailable: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		box = nil
	}

	// The request-body KEK is independent of the channel KEK. A missing key
	// does not stop the process and does not affect requests: bodies are
	// simply not stored and gw_log_body_dropped_total{reason="no_kek"} keeps
	// the condition visible.
	//
	// It is a ring, not one key (G3): LOG_BODY_KEY_ID names the key that seals
	// new rows and LOG_BODY_KEK_PREVIOUS keeps the retired ones readable for
	// the rest of their 180-day window. A malformed rotation config degrades
	// exactly like a missing key — loudly, in this line — instead of silently
	// making stored bodies undecryptable.
	bodyKeys, bodyErr := cryptobox.BuildBodyKeyRing(os.Getenv(config.EnvLogBodyKEK))
	if bodyErr != nil {
		logger.Warn(rootCtx, "log body key ring unavailable; request bodies will not be stored: "+bodyErr.Error(),
			obslog.Fields{Stage: "startup"})
		bodyKeys = nil
	} else {
		logger.Info(rootCtx, fmt.Sprintf("log body keys: current=%s configured=%v",
			bodyKeys.CurrentID(), bodyKeys.KeyIDs()), obslog.Fields{Stage: "startup"})
	}
	usageWriter.BodyKeys = bodyKeys
	usageWriter.Metrics = rec
	usageWriter.BodyMode = func() string {
		if v, ok := store.String(config.KeyLogBodyMode); ok {
			return v
		}
		return config.EnumMetaOnly
	}
	usageWriter.RetentionDays = func() int {
		if v, ok := store.Int(config.KeyLogRetentionDays); ok && v > 0 {
			return int(v)
		}
		return settle.DefaultBodyRetentionDays
	}

	dispatcher := dispatch.New(dispatch.Options{
		Catalog:        deps.Catalog,
		Tokens:         deps.Tokens,
		Ledger:         deps.Ledger,
		Balance:        deps.Balance,
		Config:         deps.Config,
		Logger:         logger,
		Idem:           deps.Idem,
		Metrics:        rec,
		KEK:            box,
		Cooldown:       brk,
		SettleEnqueuer: enqueuer,
		UsageSink:      usageWriter.WriteUsage,
		MetricSink:     metricAcc.Add,
		AttemptSink: func(a dispatch.Attempt) {
			usageWriter.WriteAttempt(a.SettleRecord(time.Now().UTC()))
		},
	})
	handler := gw.NewHandler(deps, dispatcher)

	// T27 internal entry. An unset or short secret means the route does not
	// exist at all: an unconfigured deployment must not expose an endpoint that
	// answers 401 and can be probed. The secret is not a configuration key and
	// is never logged.
	internalSecret := strings.TrimSpace(os.Getenv(config.EnvInternalCallSecret))
	var internalHandler *gw.InternalHandler
	if len(internalSecret) < gw.InternalMinSecretBytes {
		logger.Warn(rootCtx,
			"INTERNAL_CALL_SECRET missing or shorter than 32 bytes: POST "+gw.InternalPath+" is not registered",
			obslog.Fields{Stage: "startup"})
	} else {
		internalHandler, err = gw.NewInternalHandler(deps, dispatcher, internalSecret)
		if err != nil {
			logger.Error(rootCtx, "internal handler: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
			os.Exit(1)
		}
	}

	h := health.New()
	h.Add("redis", func(ctx context.Context) error {
		return rdb.Ping(ctx)
	})
	h.Add("mysql", func(ctx context.Context) error {
		return db.Ping(ctx, gwDB)
	})
	h.Add("config", func(context.Context) error {
		if store.Snapshot(config.NsUpstream) == nil {
			return fmt.Errorf("config snapshot missing")
		}
		return nil
	})

	drain := time.Duration(120_000) * time.Millisecond
	if v, ok := store.Int(config.KeyGwDrainDeadlineMs); ok && v > 0 {
		drain = time.Duration(v) * time.Millisecond
	}

	api := httpsrv.New(httpsrv.Config{
		Addr:              cfg.GatewayAddr,
		Svc:               service,
		Env:               cfg.Env,
		Logger:            logger,
		Clock:             clock.Real{},
		Health:            h,
		DrainTimeout:      drain,
		ReadHeaderTimeout: gw.TLSHandshakeBudget,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE responses must not be cut by a server-level deadline.
		IdleTimeout:       120 * time.Second,
		Register: func(mux *http.ServeMux) {
			mux.Handle("/v1/", handler)
			if internalHandler != nil {
				// Not reachable through nginx (location /internal/ returns 404)
				// and not published on the host; the HMAC is the third gate.
				mux.Handle("POST "+gw.InternalPath, internalHandler)
			}
		},
	})

	metricsSrv := httpsrv.New(httpsrv.Config{
		Addr:              cfg.MetricsAddr,
		Svc:               service,
		Env:               cfg.Env,
		Logger:            logger,
		Clock:             clock.Real{},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		Register: func(mux *http.ServeMux) {
			mux.Handle("GET /metrics", httpsrv.MetricsHandler(cfg.MetricsToken, reg))
		},
	})

	installSignalHandling(rootCtx, logger, cancel)

	errCh := make(chan error, 2)
	go func() { errCh <- api.Run(ctx) }()
	go func() { errCh <- metricsSrv.Run(ctx) }()

	var runErr error
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil && runErr == nil {
			runErr = err
			cancel()
		}
	}

	// Final flush of P28/P29 rows and P30 deltas. A slow database must not
	// hold the process open indefinitely; the rows are logs, not money.
	bgCancel()
	for _, done := range []chan struct{}{writerDone, metricDone} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logger.Warn(rootCtx, "async writer flush timeout", obslog.Fields{Stage: "shutdown"})
		}
	}

	if runErr != nil {
		logger.Error(rootCtx, "server exited with error: "+runErr.Error(), obslog.Fields{Stage: "shutdown", Status: 1})
		os.Exit(1)
	}
	logger.Info(rootCtx, "shutdown complete", obslog.Fields{Stage: "shutdown", Status: 0})
}

// observeConfig exports the two config-plane signals that only gatewayd can
// see: staleness of the oldest namespace snapshot, and rows that were skipped
// while a snapshot was applied. The version check makes the rejection counter
// count each bad row once per published version, not once per tick.
func observeConfig(ctx context.Context, store *config.Store, rec *obsmetrics.Recorder, instance string, startedAt time.Time) {
	if store == nil || rec == nil {
		return
	}
	last := make(map[config.Namespace]int64, len(config.SnapshotNamespaces))
	for _, ns := range config.SnapshotNamespaces {
		last[ns] = -1
	}
	collect := func() {
		var oldest time.Time
		for _, ns := range config.SnapshotNamespaces {
			snap := store.Snapshot(ns)
			if snap == nil {
				continue
			}
			loaded := snap.LoadedAt()
			if loaded.IsZero() {
				// Built-in defaults have no load time; measuring from process
				// start is the honest "configuration has never loaded" value.
				loaded = startedAt
			}
			if oldest.IsZero() || loaded.Before(oldest) {
				oldest = loaded
			}
			if last[ns] != snap.Version() {
				last[ns] = snap.Version()
				recordConfigFallbacks(rec, ns, snap.Fallbacks())
			}
		}
		if !oldest.IsZero() {
			rec.ConfigStale(instance, int64(time.Since(oldest).Seconds()))
		}
	}
	collect()
	ticker := time.NewTicker(obsmetrics.DefaultWindow)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

func recordConfigFallbacks(rec *obsmetrics.Recorder, ns config.Namespace, fallbacks []string) {
	for _, fb := range fallbacks {
		key, reason, ok := strings.Cut(fb, ": ")
		if !ok {
			rec.ConfigItemRejected(string(ns), "invalid_value")
			continue
		}
		switch {
		case reason == "unknown key":
			// The key is not in the metadata table, so there is no namespace
			// to attribute it to; "unrecognized" is one of the bounded values.
			rec.ConfigItemRejected("unrecognized", "unknown_key")
		case reason == "key belongs to another namespace":
			rec.ConfigItemRejected(string(ns), "wrong_namespace")
		case strings.HasPrefix(reason, "invalid value"):
			rec.ConfigItemRejected(string(ns), "invalid_value")
		case strings.HasPrefix(reason, "strategy not registered"):
			rec.ConfigItemRejected(string(ns), "strategy_unregistered")
			if m, ok := config.Meta(config.Key(key)); ok && m.Strategy != "" {
				// The original value is deliberately not a label; it stays in
				// the existing config_field_fallback WARN line.
				rec.StrategyFallback(string(m.Strategy), "unregistered")
			}
		default:
			rec.ConfigItemRejected(string(ns), "invalid_value")
		}
	}
}

// observeLogBacklog exports the Redis request-log stream length. It is a
// monitoring read, not a consumer; a growing value means the consumer is stuck.
func observeLogBacklog(ctx context.Context, producer *logq.Producer, rec *obsmetrics.Recorder) {
	if producer == nil || rec == nil {
		return
	}
	collect := func() {
		if n, err := producer.Backlog(ctx); err == nil {
			rec.LogBacklog(n)
		}
	}
	collect()
	ticker := time.NewTicker(obsmetrics.DefaultWindow)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "gatewayd"
}

// obsConfigLogger adapts the structured logger to config's Warn-only surface.
type obsConfigLogger struct{ logger *obslog.Logger }

func (l obsConfigLogger) Warn(ctx context.Context, msg string) {
	l.logger.Warn(ctx, msg, obslog.Fields{Stage: "config"})
}

// installSignalHandling drains on the first termination signal and exits
// immediately on the second, matching the 120s drain contract (TDS §4.4).
func installSignalHandling(rootCtx context.Context, logger *obslog.Logger, cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Warn(rootCtx, fmt.Sprintf("signal received (%s); drain starts", sig), obslog.Fields{Stage: "drain"})
		cancel()

		sig = <-sigCh
		logger.Warn(rootCtx, fmt.Sprintf("second signal received (%s); forcing exit", sig), obslog.Fields{Stage: "drain", Status: 1})
		os.Exit(1)
	}()
}

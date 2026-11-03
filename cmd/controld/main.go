// Command controld is the stateless control-plane process.
//
// It owns the console/admin listener (:8081) and holds the general main-database
// and Redis pools. Business handlers arrive with later task cards; T01 registers
// only the route prefixes and the health endpoints.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"yunshu.dev/core/internal/boot"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/cryptobox"
	"yunshu.dev/core/internal/notify/mail"
	"yunshu.dev/core/internal/obs/health"
	obslog "yunshu.dev/core/internal/obs/log"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/db"
	"yunshu.dev/core/internal/platform/env"
	"yunshu.dev/core/internal/platform/httpsrv"
	"yunshu.dev/core/internal/redisx"
)

const service = env.ServiceControl

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

	reg := obsmetrics.NewRegistry()
	// The recorder is passed to control for the log-body read counter
	// (gw_log_body_open_failed_total); registering all families also keeps the
	// /metrics endpoint shape consistent with gatewayd.
	rec := obsmetrics.NewRecorder(reg)

	mainDB, err := db.OpenMain(cfg.DBDSN, db.MainPoolMaxOpen, db.MainPoolMaxIdle)
	if err != nil {
		logger.Error(rootCtx, "open main db failed: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	defer mainDB.Close()

	// The log tables may live in their own instance. A configured DSN opens a
	// second pool; an empty one is announced and the control service falls
	// back to the main DB (the local single-database deployment).
	var logDB *sql.DB
	if strings.TrimSpace(cfg.LogSQLDSN) != "" {
		logDB, err = db.OpenMain(cfg.LogSQLDSN, db.MainPoolMaxOpen, db.MainPoolMaxIdle)
		if err != nil {
			logger.Error(rootCtx, "open log db failed: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
			os.Exit(1)
		}
		defer logDB.Close()
	} else {
		logger.Warn(rootCtx,
			"LOG_SQL_DSN is empty: console log queries fall back to DB_DSN",
			obslog.Fields{Stage: "startup"})
	}

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

	sessions, err := mw.NewSessionManager(mainDB, rdb, cfg.SessionSecret)
	if err != nil {
		logger.Error(rootCtx, "session manager: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	// The first-segment producer. T15 replaces the timer with an operator
	// endpoint; T12 also calls it synchronously after an IP-rule change so a
	// new whitelist is visible to P06 before its K13 ipm is published.
	cfgRedis := config.NewGoRedis(rdb.Raw())
	publisher := &config.Publisher{DB: mainDB, Redis: cfgRedis, Now: clock.Real{}.Now}
	routeBuilder := &boot.RouteSnapshot{
		DB: mainDB, Publisher: publisher, Now: clock.Real{}, Logger: logger,
	}
	caches := &boot.CacheWriter{
		DB:     mainDB,
		Prices: &config.PriceCache{Redis: cfgRedis},
		Keys:   redisx.NewKeyAuthCache(rdb),
		Now:    clock.Real{},
	}

	auditWriter := &audit.Writer{DB: mainDB, Now: clock.Real{}}

	// Payment callback signing secrets. They are read from the environment,
	// not from t_config_item: the frozen configuration table carries no payment
	// credential, and a secret in that table would be readable by every process
	// that loads configuration. A missing secret is announced loudly here and
	// turns the channel's callback into a 503 — never a skipped verification.
	paySecrets := payCallbackSecrets()
	for _, channel := range []string{"WECHAT", "ALIPAY"} {
		if paySecrets[channel] == "" {
			logger.Warn(rootCtx,
				"payment callback secret missing for "+channel+": callbacks will answer 503 until YUNSHU_PAY_CALLBACK_SECRET_"+channel+" is set",
				obslog.Fields{Stage: "startup"})
		}
	}

	// T27 playground relay. INTERNAL_CALL_SECRET signs the call to gatewayd's
	// internal entry; it must be its own secret. Reusing the session key or
	// either KEK is treated as unconfigured, so the relay answers 503 instead
	// of letting one leaked value weaken two boundaries.
	internalSecret := strings.TrimSpace(os.Getenv(config.EnvInternalCallSecret))
	internalURL := strings.TrimSpace(os.Getenv(config.EnvGatewayInternalURL))
	if internalSecret != "" && reusesProcessSecret(internalSecret, cfg.SessionSecret,
		os.Getenv(cryptobox.EnvKEK), os.Getenv(config.EnvLogBodyKEK)) {
		logger.Error(rootCtx,
			"INTERNAL_CALL_SECRET must not reuse SESSION_SECRET / CHANNEL_KEY_KEK / LOG_BODY_KEK; playground relay disabled",
			obslog.Fields{Stage: "startup", Status: 1})
		internalSecret = ""
	}

	// Verification codes need a delivery channel. "log" prints the code to this
	// process log and is the development/CI default; "smtp" e-mails it and is
	// the production setting. Asking for smtp without SMTP settings fails at
	// boot rather than at the first registration, which would look like a
	// product outage with no visible cause.
	verificationMode := strings.TrimSpace(os.Getenv("AUTH_VERIFICATION_MODE"))
	if verificationMode == "" {
		verificationMode = "log"
	}
	if verificationMode != "log" && verificationMode != "smtp" && verificationMode != "off" {
		logger.Error(rootCtx, "AUTH_VERIFICATION_MODE must be log, smtp or off, got "+verificationMode,
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	mailer, mailErr := mail.FromEnv()
	switch {
	case mailErr == nil:
		logger.Info(rootCtx, "smtp mailer configured: host="+mailer.Host+" from="+mailer.From,
			obslog.Fields{Stage: "startup"})
	case errors.Is(mailErr, mail.ErrNotConfigured):
		logger.Warn(rootCtx, "SMTP is not configured; verification codes can only be issued in log mode",
			obslog.Fields{Stage: "startup"})
	default:
		logger.Error(rootCtx, "SMTP configuration is invalid: "+mailErr.Error(),
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	if verificationMode == "smtp" && mailer == nil {
		logger.Error(rootCtx, "AUTH_VERIFICATION_MODE=smtp requires SMTP_HOST/SMTP_FROM; refusing to start",
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	if verificationMode == "log" {
		logger.Warn(rootCtx, "AUTH_VERIFICATION_MODE=log: codes are printed to this log, not e-mailed — "+
			"set AUTH_VERIFICATION_MODE=smtp before production", obslog.Fields{Stage: "startup"})
	}

	deps := control.Deps{
		DB:       mainDB,
		LogDB:    logDB,
		Redis:    rdb,
		Sessions: sessions,
		Audit:    auditWriter,
		Logger:   logger,
		Now:      clock.Real{},
		PublishRoutes: func(ctx context.Context) error {
			_, _, err := routeBuilder.Publish(ctx)
			return err
		},
		PublishPrices: func(ctx context.Context) error {
			_, err := caches.RefreshPrices(ctx)
			return err
		},
		PaySecrets:     paySecrets,
		FingerprintKey: fpHMACKey(logger, rootCtx),
		Metrics:        rec,

		GatewayInternalURL: internalURL,
		InternalCallSecret: internalSecret,
		VerificationMode:   verificationMode,
	}
	if mailer != nil {
		deps.Mailer = mailer
	}
	ctl, err := control.New(deps)
	if err != nil {
		logger.Error(rootCtx, "control server: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}

	h := health.New()
	h.Add("redis", func(ctx context.Context) error {
		return rdb.Ping(ctx)
	})
	h.Add("mysql", func(ctx context.Context) error {
		return db.Ping(ctx, mainDB)
	})
	if logDB != nil {
		h.Add("log_mysql", func(ctx context.Context) error {
			return db.Ping(ctx, logDB)
		})
	}
	h.Add("config", func(ctx context.Context) error {
		n, err := rdb.Raw().Exists(ctx, config.SnapshotRedisKey(config.NsRouting)).Result()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("routing snapshot missing")
		}
		return nil
	})

	ctx, cancel := context.WithCancel(rootCtx)
	defer cancel()

	refresher := &boot.Refresher{
		Routes: routeBuilder,
		Caches: caches,
		Logger: logger,
	}
	go refresher.Run(ctx)

	srv := httpsrv.New(httpsrv.Config{
		Addr:              cfg.ControlAddr,
		Svc:               service,
		Env:               cfg.Env,
		Logger:            logger,
		Clock:             clock.Real{},
		Health:            h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		Register:          ctl.Register,
	})

	// /metrics is a separate listener: it has no auth by default, so it must
	// never share the console port.
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
	go func() { errCh <- srv.Run(ctx) }()
	go func() { errCh <- metricsSrv.Run(ctx) }()

	var runErr error
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil && runErr == nil {
			runErr = err
			cancel()
		}
	}
	if runErr != nil {
		logger.Error(rootCtx, "server exited with error: "+runErr.Error(), obslog.Fields{Stage: "shutdown", Status: 1})
		os.Exit(1)
	}
	logger.Info(rootCtx, "shutdown complete", obslog.Fields{Stage: "shutdown", Status: 0})
}

// fpHMACKey reads FP_HMAC_KEY from the environment. It is not a configuration
// key: the 45 frozen keys carry no HMAC secret, and a secret in t_config_item
// would be readable by every process. An empty key disables trial claims with
// a 503; it never degrades to hashing with an empty secret.
func fpHMACKey(logger *obslog.Logger, ctx context.Context) string {
	key := strings.TrimSpace(os.Getenv("FP_HMAC_KEY"))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("YUNSHU_FP_HMAC_KEY"))
	}
	if key == "" {
		logger.Warn(ctx,
			"FP_HMAC_KEY is not set: /api/console/trial/claim will answer 503",
			obslog.Fields{Stage: "startup"})
	}
	return key
}

// reusesProcessSecret reports whether secret equals any other configured
// process secret. Empty candidates are ignored: an unset KEK must not count as
// a match.
func reusesProcessSecret(secret string, others ...string) bool {
	for _, other := range others {
		if other = strings.TrimSpace(other); other != "" && other == secret {
			return true
		}
	}
	return false
}

// payCallbackSecrets reads the per-channel callback signing secrets. An unset
// variable is left out of the map so the service can report "not configured"
// instead of verifying with an empty key.
func payCallbackSecrets() map[string]string {
	out := make(map[string]string, 2)
	for _, channel := range []string{"WECHAT", "ALIPAY"} {
		if secret := strings.TrimSpace(os.Getenv("YUNSHU_PAY_CALLBACK_SECRET_" + channel)); secret != "" {
			out[channel] = secret
		}
	}
	return out
}

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

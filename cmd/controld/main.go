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
	"yunshu.dev/core/internal/control/auth"
	"yunshu.dev/core/internal/control/bootstrap"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/cryptobox"
	"yunshu.dev/core/internal/notify/mail"
	"yunshu.dev/core/internal/notify/sms"
	"yunshu.dev/core/internal/obs/health"
	obslog "yunshu.dev/core/internal/obs/log"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/db"
	"yunshu.dev/core/internal/platform/env"
	"yunshu.dev/core/internal/platform/httpsrv"
	"yunshu.dev/core/internal/platform/migrate"
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

	// 短信通道（自助找回密码的手机渠道）。放在打开任何连接**之前**：一个写错的
	// AUTH_SMS_MODE 是纯配置错误，不该等到数据库、Redis 都连上才被发现——那样运维
	// 会先怀疑依赖，而且「拒绝启动」的退出原因会被更早的失败掩盖。
	//
	// 三种模式（off / log / provider）里，provider 缺凭据同样在这里拒绝启动，与
	// AUTH_VERIFICATION_MODE=smtp 无 SMTP 通道即拒启一致：把缺凭据拖到首次发送，
	// 表现是「用户点了没反应」，看起来像产品故障。错误串只含变量名，绝不含凭据值。
	smsClient, smsErr := sms.FromEnv(logger)
	if smsErr != nil {
		logger.Error(rootCtx, "sms configuration is invalid: "+smsErr.Error(),
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	switch smsClient.Mode {
	case sms.ModeLog:
		logger.Warn(rootCtx, "AUTH_SMS_MODE=log: 验证码会打印到本日志，不会真实下发 —— 生产环境请设为 provider",
			obslog.Fields{Stage: "startup"})
	case sms.ModeProvider:
		// 只打印供应商、签名与模板号（都是供应商侧的可公开标识），不打印任何凭据。
		logger.Info(rootCtx, fmt.Sprintf("sms provider=%s %s", smsClient.Provider, smsClient.String()),
			obslog.Fields{Stage: "startup"})
	default:
		logger.Info(rootCtx, "AUTH_SMS_MODE=off: 手机号找回渠道关闭，找回仅支持邮箱",
			obslog.Fields{Stage: "startup"})
	}

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

	// Startup migration. The binary queries columns (t_user.username,
	// password_is_initial) that a database from the previous release does not
	// have, so "migrate before start" is mandatory — the only question is who
	// runs it. AUTO_MIGRATE answers that:
	//
	//   - unset: on for ENV=dev/local, off otherwise. Local development becomes
	//     one command instead of two, while production keeps its dedicated
	//     migrate job (compose `migrate` service / k8s Job) and an application
	//     account without DDL rights.
	//   - true: explicit opt-in for single-node deployments that would rather
	//     not run a separate job.
	//   - false: explicit opt-out.
	//
	// Concurrency is handled inside AutoUp with a MySQL advisory lock, so two
	// replicas starting together do not race on DDL. A failure here is fatal:
	// serving with an older schema means every login answers 500.
	autoMigrate, autoErr := migrate.AutoMigrateEnabled(cfg.Env, os.Getenv("AUTO_MIGRATE"))
	if autoErr != nil {
		logger.Error(rootCtx, "auto migrate config: "+autoErr.Error(),
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	if autoMigrate {
		results, migrateErr := migrate.AutoUp(rootCtx, cfg.DBDSN, cfg.Env, clock.Real{}.Now)
		if migrateErr != nil {
			logger.Error(rootCtx, "auto migrate failed: "+migrateErr.Error(),
				obslog.Fields{Stage: "startup", Status: 1})
			os.Exit(1)
		}
		if n := migrate.AppliedCount(results); n > 0 {
			logger.Info(rootCtx, fmt.Sprintf("auto migrate: applied %d pending migration(s)", n),
				obslog.Fields{Stage: "startup"})
		}
	}

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

	// Built-in super administrator (INC-admin-bootstrap). Placed before
	// control.New on purpose: there must be no window in which the process
	// accepts logins while username=admin cannot be found.
	//
	// Failure handling is two-tier, not all-or-nothing:
	//   - invalid configuration refuses to start: a username or password that
	//     can never become valid means the operator has to edit the env file, and
	//     starting anyway would just hide the mistake behind a missing account;
	//   - every runtime SQL error — including MySQL 1054 for a schema without
	//     migration 0027 — is logged and startup continues. This is a one-shot
	//     action bolted onto an existing production process: turning a transient
	//     database hiccup, or a migration applied one deploy too late, into "the
	//     whole control plane is down" costs far more than a visible ERROR line
	//     plus the runbook's manual fallback. A missing username column only
	//     affects the username-login path; every existing customer account
	//     signs in with email or phone.
	//
	// gatewayd and jobsd must never run this: the console would be created three
	// times and the data plane would depend on a control-plane table.
	bootOpts, bootErr := bootstrap.OptionsFromEnv()
	if bootErr != nil {
		logger.Error(rootCtx, "admin bootstrap config: "+bootErr.Error(),
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	if res, ensureErr := bootstrap.EnsureSuperAdmin(rootCtx, bootstrap.Deps{
		DB:     mainDB,
		Audit:  auditWriter,
		Logger: logger,
		Now:    clock.Real{},
	}, bootOpts); ensureErr != nil {
		switch {
		case errors.Is(ensureErr, bootstrap.ErrSchemaNotMigrated):
			logger.Error(rootCtx, "admin bootstrap: "+ensureErr.Error()+"；请先执行 make migrate（迁移 0027）后重启 controld",
				obslog.Fields{Stage: "startup", Status: 1})
			// 不退出：缺列只影响用户名登录一条路径，不能让存量客户的控制面陪葬
		case errors.Is(ensureErr, bootstrap.ErrInvalidConfig):
			logger.Error(rootCtx, "admin bootstrap config: "+ensureErr.Error(),
				obslog.Fields{Stage: "startup", Status: 1})
			os.Exit(1)
		default:
			logger.Error(rootCtx, "admin bootstrap failed: "+ensureErr.Error(),
				obslog.Fields{Stage: "startup"})
		}
	} else if res.Outcome == bootstrap.OutcomeCreated {
		// The WARN "created, change the password now" line is written inside
		// EnsureSuperAdmin. This INFO adds the uid, which is what an operator
		// needs to find the t_user row and its audit entry.
		logger.Info(rootCtx, fmt.Sprintf("admin bootstrap: outcome=%s uid=%d username=%s",
			res.Outcome, res.UID, res.Username), obslog.Fields{Stage: "startup"})
	}

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
	// the production setting. Asking for smtp without a usable SMTP channel
	// fails at boot rather than at the first registration, which would look
	// like a product outage with no visible cause.
	//
	// Several relays may coexist (SMTP_QQ_*, SMTP_163_*, plus the unprefixed
	// DEFAULT channel); AUTH_SMTP_CHANNEL says which one sends. Nothing here
	// logs a credential, only channel names and the selected host.
	verificationMode := strings.TrimSpace(os.Getenv("AUTH_VERIFICATION_MODE"))
	if verificationMode == "" {
		verificationMode = "log"
	}
	if verificationMode != "log" && verificationMode != "smtp" && verificationMode != "off" {
		logger.Error(rootCtx, "AUTH_VERIFICATION_MODE must be log, smtp or off, got "+verificationMode,
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	channels, mailErr := mail.ChannelsFromEnv()
	if mailErr != nil {
		logger.Error(rootCtx, "SMTP configuration is invalid: "+mailErr.Error(),
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	mailer := channels.Sender
	if mailer == nil {
		logger.Warn(rootCtx, channels.Summary()+": verification codes can only be issued in log mode",
			obslog.Fields{Stage: "startup"})
	} else {
		logger.Info(rootCtx, channels.Summary()+" host="+mailer.Host+" from="+mailer.From,
			obslog.Fields{Stage: "startup"})
	}
	if verificationMode == "smtp" && mailer == nil {
		logger.Error(rootCtx, "AUTH_VERIFICATION_MODE=smtp requires a configured SMTP channel "+
			"(SMTP_HOST/SMTP_FROM, or SMTP_<NAME>_HOST/_FROM with "+mail.EnvChannel+"); refusing to start",
			obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	if verificationMode == "log" {
		logger.Warn(rootCtx, "AUTH_VERIFICATION_MODE=log: codes are printed to this log, not e-mailed — "+
			"set AUTH_VERIFICATION_MODE=smtp before production", obslog.Fields{Stage: "startup"})
	}

	// Phone-number HMAC key (t_user.phone_hmac). Independent of FP_HMAC_KEY by
	// design, and validated with the same "reuse = misconfigured" rule as
	// INTERNAL_CALL_SECRET: one leaked value must not weaken two boundaries.
	//
	// A missing key is announced, never fatal. Exiting here would take e-mail
	// registration, every login and every password recovery down together with
	// a feature none of them use; the phone *write* paths fail closed instead
	// (503 config_unavailable) and the read paths do not need the key at all.
	// ENV=prod is ERROR because a production deploy without it cannot accept a
	// phone sign-up at all; everywhere else a WARN is enough to notice.
	phoneHMAC := phoneHMACKey(logger, cfg, rootCtx)

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
		PhoneHMACKey:   phoneHMAC,
		Metrics:        rec,

		GatewayInternalURL: internalURL,
		InternalCallSecret: internalSecret,
		VerificationMode:   verificationMode,
		SMS:                smsClient,
		SMSMode:            smsClient.Mode,
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

// phoneHMACKey reads PHONE_HMAC_KEY (accepting the YUNSHU_ prefix as a
// fallback, like fpHMACKey) and returns "" for every unusable value.
//
// "Unusable" is three cases, all of them announced and none of them fatal:
//
//	absent            nothing to hash with
//	shorter than 32B  a typed word, not generated key material
//	reused            equal to SESSION_SECRET, either KEK, FP_HMAC_KEY or
//	                  EXPORT_HMAC_KEY — one leak would then weaken two
//	                  boundaries, and the phone key is the one that reverses
//	                  to PII, so it must be the strictest of the set
//
// Empty is a shape the service understands (503 on the phone write paths), so
// there is no "hash with an empty secret" fallback anywhere downstream.
func phoneHMACKey(logger *obslog.Logger, cfg *env.Config, ctx context.Context) string {
	key := strings.TrimSpace(os.Getenv(config.EnvPhoneHMACKey))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("YUNSHU_" + config.EnvPhoneHMACKey))
	}
	announce := func(msg string) {
		// One call site for the severity rule: a production deployment that
		// cannot accept a phone sign-up is an incident waiting to be reported,
		// while a laptop without the variable is just a laptop.
		if strings.EqualFold(cfg.Env, "prod") {
			logger.Error(ctx, msg, obslog.Fields{Stage: "startup", Status: 1})
			return
		}
		logger.Warn(ctx, msg, obslog.Fields{Stage: "startup"})
	}
	switch {
	case key == "":
		announce(config.EnvPhoneHMACKey + " is not set: phone registration and operator " +
			"user creation will answer 503 config_unavailable; login and recovery are unaffected")
		return ""
	case len(key) < auth.PhoneHMACMinKeyLen:
		announce(config.EnvPhoneHMACKey + " is shorter than 32 bytes: treated as unset; " +
			"phone registration and operator user creation will answer 503 config_unavailable")
		return ""
	case reusesProcessSecret(key, cfg.SessionSecret,
		os.Getenv(cryptobox.EnvKEK), os.Getenv(config.EnvLogBodyKEK),
		os.Getenv("FP_HMAC_KEY"), os.Getenv("EXPORT_HMAC_KEY")):
		announce(config.EnvPhoneHMACKey + " must not reuse SESSION_SECRET / CHANNEL_KEY_KEK / " +
			"LOG_BODY_KEK / FP_HMAC_KEY / EXPORT_HMAC_KEY; treated as unset, phone writes will answer 503")
		return ""
	}
	return key
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

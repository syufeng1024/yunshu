// Command jobsd is the background-task process.
//
// It exposes no HTTP port. It runs the T09 task table: hanging-prehold
// recovery, daily reconciliation, BONUS expiry, unpaid-order expiry, log
// retention, metric rollup and the daily quota snapshot. Every task has a
// side effect and therefore runs only while this process holds its
// t_job_lock lease. The settlement consumer remains on every
// replica (D-18): FOR UPDATE SKIP LOCKED plus request_id idempotency makes that
// safe and keeps settlement available when the leader is down.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/jobs"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/lock"
	"yunshu.dev/core/internal/obs/health"
	obslog "yunshu.dev/core/internal/obs/log"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/db"
	"yunshu.dev/core/internal/platform/env"
	"yunshu.dev/core/internal/platform/httpsrv"
	"yunshu.dev/core/internal/recon"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/settle"
)

const service = env.ServiceJobs

// cst is the business timezone used for job schedules and daily boundaries.
var cst = time.FixedZone("CST", 8*3600)

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
	rec := obsmetrics.NewRecorder(reg)

	mainDB, err := db.OpenMain(cfg.DBDSN, db.JobsPoolMaxOpen, db.MainPoolMaxIdle)
	if err != nil {
		logger.Error(rootCtx, "open main db failed: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	defer mainDB.Close()

	// K25 uses the shared key namespace, so jobsd must use the same client
	// wrapper as the gateway rather than a bare go-redis client.
	rdb := redisx.New(redisx.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, PoolSize: 20})
	defer rdb.Close()

	registry := jobs.NewRegistry()
	for _, task := range jobs.TaskTable() {
		if err := registry.Register(task); err != nil {
			logger.Error(rootCtx, "task registration failed: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
			os.Exit(1)
		}
	}
	logger.Info(rootCtx, fmt.Sprintf("task table registered: count=%d", registry.Len()), obslog.Fields{Stage: "startup"})
	for _, task := range registry.Sorted() {
		schedule := task.Period.String()
		if task.Cron != "" {
			schedule = task.Cron
		}
		logger.Info(rootCtx, fmt.Sprintf("task registered: name=%s schedule=%s leader=%t timeout=%s concurrency=%s",
			task.Name, schedule, task.RequiresLeader, task.Timeout, task.Concurrency), obslog.Fields{Stage: "startup"})
	}

	instanceID := hostname() + "-" + strconv.Itoa(os.Getpid())

	// /healthz and /readyz share the metrics listener (:9092): jobsd has no
	// public HTTP port, so the metrics port is its only probe surface. healthz
	// only reports the process; readyz checks MySQL, Redis and this instance's
	// t_job_lock lease. An expired lease makes the pod not ready but must never
	// exit the process (T22 §14.4).
	h := health.New()
	h.Add("mysql", func(ctx context.Context) error {
		var one int
		return mainDB.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	})
	h.Add("redis", func(ctx context.Context) error {
		return rdb.Ping(ctx)
	})
	h.Add("job_lease", func(ctx context.Context) error {
		var held int
		err := mainDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM t_job_lock
			  WHERE owner_id = ? AND released_at IS NULL AND lease_until > NOW(3)`,
			instanceID,
		).Scan(&held)
		if err != nil {
			return err
		}
		if held == 0 {
			return fmt.Errorf("no live t_job_lock lease for %s", instanceID)
		}
		return nil
	})

	election := jobs.NewElection(mainDB, rdb, instanceID, logger, clock.Real{})
	stopHeartbeat := election.StartHeartbeat(rootCtx)
	defer stopHeartbeat()

	ctx, cancel := context.WithCancel(rootCtx)
	defer cancel()
	installSignalHandling(rootCtx, logger, cancel)

	var wg sync.WaitGroup

	// jobsd has no public HTTP port, so the metrics listener also carries
	// /healthz and /readyz (T22 §14.4). It keeps the same Bearer-token switch
	// on /metrics as the other two processes (T22 §9 errata #2; TDS §5.4's
	// "no HTTP port" predates it).
	metricsSrv := httpsrv.New(httpsrv.Config{
		Addr:              cfg.MetricsAddr,
		Svc:               service,
		Env:               cfg.Env,
		Logger:            logger,
		Clock:             clock.Real{},
		Health:            h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		Register: func(mux *http.ServeMux) {
			mux.Handle("GET /metrics", httpsrv.MetricsHandler(cfg.MetricsToken, reg))
		},
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := metricsSrv.Run(ctx); err != nil {
			logger.Error(rootCtx, "metrics server stopped: "+err.Error(), obslog.Fields{Stage: "shutdown", Status: 1})
		}
	}()

	ledgerStore := ledger.NewStore(mainDB, clock.Real{})

	// P26 consumer. Every jobsd replica runs one worker: the claim uses
	// FOR UPDATE SKIP LOCKED and ledger.Settle is keyed by request_id, so two
	// replicas can never charge twice. Holding a leader lease here would make
	// settlement availability depend on the lease table.
	settleWorker := settle.NewWorker(mainDB, ledgerStore, clock.Real{}, logger, instanceID)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := settleWorker.Run(ctx); err != nil {
			logger.Error(rootCtx, "settle worker stopped: "+err.Error(), obslog.Fields{Stage: "P26", Status: 1})
		}
	}()

	locker := lock.New(rdb, nil)
	hanging := recon.NewHanging(mainDB, rdb, ledgerStore,
		settle.NewEnqueuer(mainDB, clock.Real{}), logger, clock.Real{})
	hanging.Metrics = rec
	daily := recon.NewDaily(mainDB, locker, logger, clock.Real{}, instanceID)
	daily.Metrics = rec
	expire := jobs.NewExpireBatch(mainDB, locker, logger, clock.Real{}, instanceID)
	purge := jobs.NewPurgeLog(mainDB, logger, clock.Real{})
	rollup := jobs.NewRollup(mainDB, logger, clock.Real{})
	rollup.Location = cst
	dailyQuota := jobs.NewDailyQuota(mainDB, logger, clock.Real{})
	dailyQuota.Location = cst
	closeExpired := jobs.NewCloseExpiredOrder(mainDB,
		&audit.Writer{DB: mainDB, Now: clock.Real{}}, logger, clock.Real{})
	closeExpired.Metrics = rec

	runner := &jobs.Runner{
		Registry: registry,
		Election: election,
		Logger:   logger,
		Clock:    clock.Real{},
		Location: cst,
		Executors: map[string]jobs.TaskFunc{
			"prehold_release": func(ctx context.Context, _ time.Time) error {
				res, err := hanging.Round(ctx)
				if err != nil {
					return err
				}
				logger.Info(ctx, fmt.Sprintf(
					"prehold_release scanned=%d processed=%d A=%d B=%d C=%d skipped=%d backlog=%t",
					res.Scanned, res.Processed, res.TypeA, res.TypeB, res.TypeC,
					res.Skipped, res.Backlog), obslog.Fields{Stage: "P29"})
				return nil
			},
			"daily_reconcile": func(ctx context.Context, scheduled time.Time) error {
				res, err := daily.Run(ctx, previousDay(scheduled))
				if err != nil {
					return err
				}
				logger.Info(ctx, fmt.Sprintf(
					"daily_reconcile date=%s skipped=%t internal=%d external=%d p0=%t changed=%d",
					res.BizDate, res.Skipped, res.Internal, res.External, res.P0, res.Changed),
					obslog.Fields{Stage: "reconcile"})
				return nil
			},
			"expire_batch": func(ctx context.Context, _ time.Time) error {
				res, err := expire.Run(ctx)
				if err != nil {
					return err
				}
				logger.Info(ctx, fmt.Sprintf("expire_batch cleared=%d micro=%d",
					res.Cleared, res.ClearedMicro), obslog.Fields{Stage: "expire_batch"})
				return nil
			},
			"purge_log": func(ctx context.Context, _ time.Time) error {
				_, err := purge.Run(ctx)
				return err
			},
			"rollup": func(ctx context.Context, scheduled time.Time) error {
				_, err := rollup.Run(ctx, previousDay(scheduled))
				return err
			},
			// RunRecent always rebuilds yesterday and repairs up to
			// DefaultQuotaBackfillDays earlier days with missing rows. A single
			// date can be repaired by calling DailyQuota.Run with that day.
			"daily_quota": func(ctx context.Context, _ time.Time) error {
				_, err := dailyQuota.RunRecent(ctx)
				return err
			},
			// The automatic half of order expiry: fund.CloseOrder is the
			// operator void, this is the same transition with actor SYSTEM.
			"close_expired_order": func(ctx context.Context, _ time.Time) error {
				res, err := closeExpired.Run(ctx)
				if err != nil {
					return err
				}
				logger.Info(ctx, fmt.Sprintf("close_expired_order closed=%d scanned=%d skipped=%d",
					res.Closed, res.Scanned, res.Skipped), obslog.Fields{Stage: "close_expired_order"})
				return nil
			},
		},
	}
	if err := runner.Validate(); err != nil {
		logger.Error(rootCtx, "job runner invalid: "+err.Error(), obslog.Fields{Stage: "startup", Status: 1})
		os.Exit(1)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := runner.Start(ctx); err != nil {
			logger.Error(rootCtx, "job runner stopped: "+err.Error(), obslog.Fields{Stage: "jobs", Status: 1})
		}
	}()

	<-ctx.Done()
	logger.Warn(rootCtx, "drain started, timeout=120s", obslog.Fields{Stage: "drain"})

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logger.Info(rootCtx, "drain complete", obslog.Fields{Stage: "drain", Status: 0})
	case <-time.After(120 * time.Second):
		logger.Error(rootCtx, "drain timeout; forcing exit", obslog.Fields{Stage: "drain", Status: 1})
		os.Exit(1)
	}
}

// previousDay returns the CST calendar day before the scheduled fire time.
// A 00:20 rollup therefore covers the day that just ended, not the new one.
func previousDay(scheduled time.Time) time.Time {
	if scheduled.IsZero() {
		scheduled = time.Now()
	}
	return scheduled.In(cst).AddDate(0, 0, -1)
}

func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "jobsd"
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

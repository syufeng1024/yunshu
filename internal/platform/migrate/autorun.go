package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	// sql.Open("mysql") below needs the driver registered in this package too:
	// relying on another package's blank import would break the day a caller
	// stops importing internal/platform/db.
	_ "github.com/go-sql-driver/mysql"
)

// AutoMigrateLockName is the MySQL advisory lock every process takes before it
// applies migrations at startup.
//
// It is a GET_LOCK (session scoped) on purpose: two replicas rolling out
// together must not run DDL concurrently — `ADD UNIQUE KEY` rebuilds the index
// and the loser of that race sees a metadata-lock timeout that looks like a
// random startup crash. The second replica waits here and then finds nothing
// pending.
const AutoMigrateLockName = "yunshu_auto_migrate"

// autoMigrateLockTimeout bounds how long a replica waits for the migrating
// replica. It is generous because a large-table index build can take minutes;
// giving up earlier would turn "another replica is migrating" into an outage.
const autoMigrateLockTimeout = 300 * time.Second

// AutoMigrateEnabled decides whether a process applies pending migrations at
// startup. It exists so that "one command to run locally" does not also mean
// "every production replica runs DDL".
//
// Rules (env is ENV, raw is AUTO_MIGRATE):
//
//	raw unset          -> true only for dev/local
//	raw true/1/yes/on  -> true  (explicit opt-in, including production)
//	raw false/0/no/off -> false (explicit opt-out, including dev)
//	anything else      -> error, and the caller must refuse to start:
//	                      a typo here silently changes who runs DDL.
//
// The production default stays false because the deployment already runs a
// dedicated migrate job (deploy/docker-compose.stack.yml and the k8s Job) and
// the application account is not supposed to hold DDL rights.
func AutoMigrateEnabled(env, raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		e := strings.ToLower(strings.TrimSpace(env))
		return e == "dev" || e == "local", nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("AUTO_MIGRATE 必须是布尔值（true/false/1/0），当前值无法解析")
	}
}

// AutoUp applies every pending migration while holding AutoMigrateLockName.
//
// It is the startup counterpart of `migrate up`: the same embedded SQL, the
// same checksum verification (Runner.Up calls Verify), the same 0019 seeding
// rule. What differs is only the lock and the fact that the caller is the
// application process, so the failure policy belongs to the caller (see
// cmd/controld: a failed auto-migration refuses to start, because running with
// a schema older than the binary is a 500 on every login).
//
// The returned slice is empty when nothing was pending, which is the common
// case on every restart after the first.
func AutoUp(ctx context.Context, dsn, env string, now func() time.Time) ([]Result, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("auto migrate: DB_DSN is empty")
	}
	// A dedicated, short-lived pool: the migration SQL is multi-statement, so it
	// needs NormalizeDSN's multiStatements=true, which the application pool
	// deliberately does not carry. Reusing the app pool here fails on the first
	// file with a syntax error at the second statement (MySQL 1064).
	pool, err := sql.Open("mysql", NormalizeDSN(dsn))
	if err != nil {
		return nil, fmt.Errorf("auto migrate: open: %w", err)
	}
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(1)
	defer func() { _ = pool.Close() }()
	if err := pool.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("auto migrate: ping: %w", err)
	}

	// GET_LOCK is connection scoped, so the lock must be held on one pinned
	// connection while the Runner uses the pool for its own statements.
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("auto migrate: pin connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT GET_LOCK(?, ?)`, AutoMigrateLockName, int(autoMigrateLockTimeout.Seconds()),
	).Scan(&got); err != nil {
		return nil, fmt.Errorf("auto migrate: acquire lock: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		return nil, fmt.Errorf("auto migrate: 等待迁移锁超时（另一个实例正在迁移）")
	}
	defer func() {
		// Release even when ctx is already cancelled: otherwise the lock lives
		// until this connection is closed, and a retry waits for the full
		// timeout for no reason.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT RELEASE_LOCK(?)`, AutoMigrateLockName)
	}()

	migrations, err := LoadEmbedded()
	if err != nil {
		return nil, fmt.Errorf("auto migrate: load migrations: %w", err)
	}
	runner := NewRunner(pool, migrations, now)
	// Same rule as cmd/migrate: the local seed (0019) must never run outside
	// ENV=local, and it must not abort a production upgrade either.
	if e := strings.TrimSpace(env); !strings.EqualFold(e, "local") {
		runner.Skip("0019", "本地联调种子只在 ENV=local 执行，当前 ENV="+e)
	}
	results, err := runner.Up(ctx)
	if err != nil {
		return results, fmt.Errorf("auto migrate: %w", err)
	}
	return results, nil
}

// AppliedCount counts how many migrations this call actually executed (skips
// excluded). Used for the one startup log line.
func AppliedCount(results []Result) int {
	n := 0
	for _, r := range results {
		if r.Direction == "up" {
			n++
		}
	}
	return n
}

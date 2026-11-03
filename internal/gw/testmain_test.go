package gw

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	randv2 "math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	goredis "github.com/redis/go-redis/v9"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/migrate"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

// The gateway tests use the same real services as T04/T05/T06:
// MySQL 127.0.0.1:13306 / yunshu_test and Redis DB 15. Redis is owned by
// redistest.Setup, which takes a cross-binary lock so the T06 packages and this
// one never flush each other mid-test.
const defaultTestDSN = "root:root@tcp(127.0.0.1:13306)/yunshu_test?parseTime=true&loc=UTC&charset=utf8mb4&multiStatements=true"

var (
	testDB         *sql.DB
	safeDBName     = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	routingVersion atomic.Int64
)

func TestMain(m *testing.M) {
	if err := setupDatabase(); err != nil {
		fmt.Fprintf(os.Stderr, "gw tests: %v\n", err)
		os.Exit(1)
	}
	code := redistest.Setup(m)
	if testDB != nil {
		_ = testDB.Close()
	}
	os.Exit(code)
}

func setupDatabase() error {
	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("parse TEST_DB_DSN: %w", err)
	}
	if cfg.DBName == "" {
		cfg.DBName = "yunshu_test"
	}
	if !safeDBName.MatchString(cfg.DBName) {
		return fmt.Errorf("unsafe database name %q", cfg.DBName)
	}
	cfg.ParseTime = true
	if cfg.Loc == nil {
		cfg.Loc = time.UTC
	}
	cfg.MultiStatements = true

	dbName := cfg.DBName
	adminCfg := *cfg
	adminCfg.DBName = ""
	admin, err := sql.Open("mysql", adminCfg.FormatDSN())
	if err != nil {
		return fmt.Errorf("open admin: %w", err)
	}
	defer admin.Close()

	ctx, cancel := context.WithTimeout(context.TODO(), 2*time.Minute)
	defer cancel()

	createSQL := fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_as_cs",
		dbName,
	)
	if _, err := admin.ExecContext(ctx, createSQL); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}

	// Recover from an interrupted previous run (tables but no bookkeeping).
	var tables, bookkeeping int
	if err := admin.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ?`, dbName,
	).Scan(&tables); err != nil {
		return fmt.Errorf("count tables: %w", err)
	}
	if err := admin.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = ? AND table_name = 'schema_migrations'`, dbName,
	).Scan(&bookkeeping); err != nil {
		return fmt.Errorf("count bookkeeping: %w", err)
	}
	if tables > 0 && bookkeeping == 0 {
		if _, err := admin.ExecContext(ctx, fmt.Sprintf("DROP DATABASE `%s`", dbName)); err != nil {
			return fmt.Errorf("drop incomplete schema: %w", err)
		}
		if _, err := admin.ExecContext(ctx, createSQL); err != nil {
			return fmt.Errorf("recreate schema: %w", err)
		}
	}

	testDB, err = sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return fmt.Errorf("open schema: %w", err)
	}
	testDB.SetMaxOpenConns(20)
	testDB.SetMaxIdleConns(10)
	testDB.SetConnMaxLifetime(5 * time.Minute)
	if err := testDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping schema: %w", err)
	}

	// Every test package migrates this schema and `go test ./...` runs them in
	// parallel. GET_LOCK serialises the runners; without it two runners can
	// both apply a pending file and one fails on the duplicate CREATE TABLE.
	lockConn, err := testDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open migration lock conn: %w", err)
	}
	defer lockConn.Close()
	var locked sql.NullInt64
	if err := lockConn.QueryRowContext(ctx, `SELECT GET_LOCK('yunshu_test_migrate', 120)`).Scan(&locked); err != nil || !locked.Valid || locked.Int64 != 1 {
		return fmt.Errorf("migration lock failed: %v %v", err, locked)
	}
	defer func() { _, _ = lockConn.ExecContext(ctx, `SELECT RELEASE_LOCK('yunshu_test_migrate')`) }()

	migrations, err := migrate.LoadEmbedded()
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	runner := migrate.NewRunner(testDB, migrations, clock.Real{}.Now)
	runner.Skip("0019", "gateway tests use their own routing snapshot fixtures")
	if _, err := runner.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// newRedisClient opens the shared test Redis through redisx.
func newRedisClient(t *testing.T) *redisx.Client {
	t.Helper()
	client := redistest.NewClient(0)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// newGoRedisClient returns the raw client for fixture writes.
func newGoRedisClient(t *testing.T) *goredis.Client {
	t.Helper()
	client := goredis.NewClient(&goredis.Options{Addr: redistest.Addr(), DB: redistest.DB()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// writeRouting publishes one ROUTING snapshot through the real T05 loader and
// returns a store that serves it. This exercises decodeRouting, not a test-only
// shortcut.
func writeRouting(t *testing.T, members map[string]any) *config.Store {
	t.Helper()
	version := routingVersion.Add(1) + time.Now().Unix()
	payload := map[string]any{
		"namespace": string(config.NsRouting),
		"version":   version,
		"values":    members,
	}
	raw, err := jsonx.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal routing snapshot: %v", err)
	}
	cfgRedis := config.NewGoRedis(newGoRedisClient(t))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := cfgRedis.Set(ctx, config.SnapshotRedisKey(config.NsRouting), string(raw), 0); err != nil {
		t.Fatalf("set routing snapshot: %v", err)
	}
	if err := cfgRedis.Set(ctx, config.RedisVersionKey, fmt.Sprintf("%d", version), 0); err != nil {
		t.Fatalf("set cfg:ver: %v", err)
	}
	store := config.NewStore()
	loader := &config.Loader{Redis: cfgRedis, Store: store}
	if err := loader.RefreshNamespace(ctx, config.NsRouting); err != nil {
		t.Fatalf("refresh routing namespace: %v", err)
	}
	return store
}

// staticDeps is the shared fixture for stage tests that do not need I/O.
// noopTerminal satisfies NewHandler for tests whose subject is a pre-P14
// error. T08 removed the production temporary terminal; tests must supply one
// explicitly.
var noopTerminal = TerminalFunc(func(w http.ResponseWriter, _ *http.Request, _ *Ctx) {
	w.WriteHeader(http.StatusOK)
})

func staticDeps(catalog *Catalog) *Deps {
	if catalog == nil {
		catalog = NewStaticCatalog(&RoutingSnapshot{})
	}
	return &Deps{
		Catalog:      catalog,
		Tokens:       HeuristicCounter{},
		Backpressure: NewBackpressure(nil),
		ConnGuard:    NewConnGuard(nil),
	}
}

func staticCatalog(models []ModelEntry, routes []RouteEntry, channels []ChannelEntry,
	groups map[string]GroupEntry, keyModels map[string]KeyModelGrant) *Catalog {
	return NewStaticCatalog(&RoutingSnapshot{
		Models:    models,
		Routes:    routes,
		Channels:  channels,
		Groups:    groups,
		KeyModels: keyModels,
	})
}

func basicCatalog() *Catalog {
	return staticCatalog(
		[]ModelEntry{{
			ID: 1, ModelName: "qwen-plus", Region: RegionDomestic, ModelType: ModelTypeText,
			ContextLength: 32768, MaxOutputTokens: 32768, ListingState: ListingListed,
			DefaultPriceRuleID: 11,
		}},
		[]RouteEntry{{
			ID: 1, ModelID: 1, ChannelID: 1, ProtocolID: 1,
			UpstreamModelName: "qwen-plus", Region: RegionDomestic,
			Weight: 100, Priority: 100, StreamSupported: true, Enabled: true,
		}},
		[]ChannelEntry{{
			ID: 1, Name: "ch-1", ProtocolID: 1, Region: RegionDomestic,
			Weight: 100, Priority: 100, GroupScopeMode: ScopeAll, Enabled: true,
			StreamSupported: true,
		}},
		map[string]GroupEntry{
			"standard": {Code: "standard", RatioBP: 10000, ModelScopeMode: ScopeAll,
				ChannelScopeMode: ScopeAll, Enabled: true},
		},
		map[string]KeyModelGrant{
			"10": {Mode: ScopeAll, ModelIDs: []int64{1}},
		},
	)
}

func newPCGRand(seed uint64) *randv2.Rand { return randv2.New(randv2.NewPCG(seed, seed+1)) }

func randomID(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func mustExec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := testDB.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// seedAccount creates a user and a credit account with the given available
// micro-credits. It returns the user id.
func seedAccount(t *testing.T, availableMicro int64) int64 {
	t.Helper()
	// The ledger package shares yunshu_test and allocates ids through
	// AUTO_INCREMENT. A MAX(id)+1 read is a cross-package race: the ledger can
	// insert between the read and this insert. A random id plus a duplicate
	// retry removes the collision without serialising the whole suite.
	for attempt := 0; attempt < 20; attempt++ {
		userID := int64(100_000 + randv2.Int64N(800_000))
		now := time.Now().UTC()
		_, err := testDB.ExecContext(t.Context(), `INSERT INTO t_user
			(id, user_no, password_hash, password_updated_at, created_at, updated_at)
			VALUES (?, ?, 'x', ?, ?, ?)`,
			userID, fmt.Sprintf("U%011d", userID), now, now, now)
		if err != nil {
			if isDuplicateKey(err) {
				continue
			}
			t.Fatalf("insert user: %v", err)
		}
		if _, err := testDB.ExecContext(t.Context(), `INSERT INTO t_credit_account
			(user_id, balance_recharge_micro, balance_bonus_micro, balance_micro,
			 frozen_refund_micro, overdraft_limit_micro, version, created_at, updated_at)
			VALUES (?, ?, 0, ?, 0, 500000, 0, ?, ?)`,
			userID, availableMicro, availableMicro, now, now); err != nil {
			_, _ = testDB.ExecContext(context.WithoutCancel(t.Context()),
				`DELETE FROM t_user WHERE id = ?`, userID)
			t.Fatalf("insert account: %v", err)
		}
		t.Cleanup(func() {
			cleanupCtx := context.WithoutCancel(t.Context())
			_, _ = testDB.ExecContext(cleanupCtx, `DELETE FROM t_credit_account WHERE user_id = ?`, userID)
			_, _ = testDB.ExecContext(cleanupCtx, `DELETE FROM t_prehold_hold WHERE user_id = ?`, userID)
			_, _ = testDB.ExecContext(cleanupCtx, `DELETE FROM t_ledger_entry WHERE user_id = ?`, userID)
			_, _ = testDB.ExecContext(cleanupCtx, `DELETE FROM t_user WHERE id = ?`, userID)
		})
		return userID
	}
	t.Fatal("could not allocate a unique test user id")
	return 0
}

func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

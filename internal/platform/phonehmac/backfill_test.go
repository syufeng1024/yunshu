package phonehmac

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/migrate"
)

// The backfill is pure SQL against the real schema: a fake database would not
// prove the unique key, the ORDER BY id batching or the 1062 mapping.
//
// It runs in its own schema (yunshu_hmac_test) rather than yunshu_test. The
// operations are whole-table by design — Detect scans every row with a phone,
// Apply fills every row without a hash — so sharing the schema with the other
// package fixtures would make this binary rewrite their rows (and read their
// half-built fixtures as anomalies). Each schema is created and migrated once
// here, then dropped by the operator with `DROP DATABASE yunshu_hmac_test` after
// a run; the rows this file seeds are deleted by their own t.Cleanup.
const (
	defaultTestDSN = "root:root@tcp(127.0.0.1:13306)/yunshu_test?parseTime=true&loc=UTC&charset=utf8mb4&multiStatements=true"
	testSchemaName = "yunshu_hmac_test"
)

const testKey = "phonehmac-backfill-test-key-32bytes"

var testDB *sql.DB

func TestMain(m *testing.M) {
	dsn := strings.TrimSpace(os.Getenv("TEST_DB_DSN"))
	if dsn == "" {
		dsn = defaultTestDSN
	}
	conn, err := openPrivateSchema(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phonehmac tests: %v\n", err)
		os.Exit(1)
	}
	testDB = conn
	code := m.Run()
	_ = conn.Close()
	os.Exit(code)
}

// openPrivateSchema connects to testSchemaName, creating and migrating it when it
// does not exist yet.
func openPrivateSchema(dsn string) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse TEST_DB_DSN: %w", err)
	}
	cfg.ParseTime = true
	if cfg.Loc == nil {
		cfg.Loc = time.UTC
	}
	adminCfg := *cfg
	adminCfg.DBName = ""
	admin, err := sql.Open("mysql", adminCfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open admin: %w", err)
	}
	defer admin.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_as_cs",
		testSchemaName)); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}

	cfg.DBName = testSchemaName
	conn, err := sql.Open("mysql", migrate.NormalizeDSN(cfg.FormatDSN()))
	if err != nil {
		return nil, fmt.Errorf("open private schema: %w", err)
	}
	conn.SetMaxOpenConns(4)

	var exists int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = 't_user'`,
		testSchemaName).Scan(&exists); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("inspect schema: %w", err)
	}
	if exists == 0 {
		migrations, loadErr := migrate.LoadEmbedded()
		if loadErr != nil {
			_ = conn.Close()
			return nil, loadErr
		}
		runner := migrate.NewRunner(conn, migrations, clock.Real{}.Now)
		// The local demo seed is irrelevant here and inserts rows this file would
		// then have to exclude from every count.
		runner.Skip("0019", "phonehmac tests create their own rows")
		if _, upErr := runner.Up(ctx); upErr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("migrate private schema: %w", upErr)
		}
	}
	// A previous failed run can leave rows behind; this schema belongs to this
	// test binary alone, so clearing t_user is safe and keeps Detect's global
	// counts meaningful.
	if _, err := conn.ExecContext(ctx, `DELETE FROM t_user`); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clear t_user: %w", err)
	}
	return conn, nil
}

// seedUser inserts a bare t_user row with an optional phone (and hash) and
// registers its own cleanup.
func seedUser(t *testing.T, phone string, withHMAC bool, deleted bool) int64 {
	t.Helper()
	now := time.Now().UTC()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random: %v", err)
	}
	// t_user.user_no is char(12): "H" + yyyymmdd + 3 digits.
	userNo := fmt.Sprintf("H%s%03d", now.Format("20060102"), (int(b[0])<<8|int(b[1]))%1000)
	var phoneVal, hmacVal any
	if phone != "" {
		phoneVal = phone
	}
	if withHMAC {
		mac, err := Hash(testKey, phone)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		hmacVal = mac
	}
	var deletedAt any
	if deleted {
		deletedAt = now
	}
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, phone, phone_hmac, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at, deleted_at)
		VALUES (?,?,?,'not-a-real-hash','bcrypt',?,'hmac','PERSONAL','NORMAL',?,?,?)`,
		userNo, phoneVal, hmacVal, now, now, now, deletedAt)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed user id: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

func hmacOf(t *testing.T, uid int64) (string, bool) {
	t.Helper()
	var v sql.NullString
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT phone_hmac FROM t_user WHERE id = ?`, uid).Scan(&v); err != nil {
		t.Fatalf("read hmac: %v", err)
	}
	return v.String, v.Valid
}

// T1: two rows with the same phone and no hash → Detect exits 2 and names both
// ids, and the report never contains the plaintext number.
func TestDetectFindsDuplicatesAndMasks(t *testing.T) {
	phone := uniqueTestPhone(t)
	a := seedUser(t, phone, false, false)
	b := seedUser(t, phone, false, false)

	report, err := Detect(context.Background(), testDB, testKey)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if report.ExitCode() != ExitDuplicate {
		t.Fatalf("exit code = %d, want %d (duplicates block apply)", report.ExitCode(), ExitDuplicate)
	}
	var group *DuplicateGroup
	for i := range report.Duplicates {
		if report.Duplicates[i].Masked == Mask(phone) {
			group = &report.Duplicates[i]
		}
	}
	if group == nil {
		t.Fatalf("no duplicate group for the seeded number; groups=%v", report.Duplicates)
	}
	if len(group.Rows) != 2 {
		t.Fatalf("group rows = %d, want 2", len(group.Rows))
	}
	ids := map[int64]bool{group.Rows[0].ID: true, group.Rows[1].ID: true}
	if !ids[a] || !ids[b] {
		t.Fatalf("group ids = %v, want %d and %d", group.Rows, a, b)
	}
	// Every phone-shaped value the report carries is masked; Row keeps the
	// plaintext column unexported so this cannot regress by accident.
	if !strings.Contains(group.Masked, "****") {
		t.Fatalf("masked value %q has no mask", group.Masked)
	}
	if group.Masked == phone {
		t.Fatalf("group carries the plaintext phone %q", phone)
	}
	encoded, err := jsonx.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if strings.Contains(string(encoded), phone) {
		t.Fatalf("the serialised report leaks the plaintext phone: %s", encoded)
	}
}

// T2: empty phone, non-canonical phone and a soft-deleted row holding a live
// hash → Detect exits 3 and classifies each one.
func TestDetectFindsAnomalies(t *testing.T) {
	empty := seedUser(t, "", false, false)
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone = '' WHERE id = ?`, empty); err != nil {
		t.Fatalf("set empty phone: %v", err)
	}
	nonCanonical := seedUser(t, "+8613800138111", false, false)
	ghost := seedUser(t, uniqueTestPhone(t), true, true) // deleted, hash not rewritten

	report, err := Detect(context.Background(), testDB, testKey)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if report.ExitCode() != ExitAnomaly {
		t.Fatalf("exit code = %d, want %d (anomalies block apply)", report.ExitCode(), ExitAnomaly)
	}
	kinds := map[int64]string{}
	for _, a := range report.Anomalies {
		kinds[a.ID] = a.Kind
	}
	if kinds[empty] != AnomalyEmptyPhone {
		t.Fatalf("empty phone classified %q, want %q", kinds[empty], AnomalyEmptyPhone)
	}
	if kinds[nonCanonical] != AnomalyNonCanonical {
		t.Fatalf("non-canonical phone classified %q, want %q", kinds[nonCanonical], AnomalyNonCanonical)
	}
	if kinds[ghost] != AnomalyGhostHMAC {
		t.Fatalf("soft-deleted row classified %q, want %q", kinds[ghost], AnomalyGhostHMAC)
	}
}

// T3/T4/T5: apply is a dry run unless --commit, writes exactly HMAC(phone) when
// it does, and a second run fills nothing.
func TestApplyDryRunThenCommitThenIdempotent(t *testing.T) {
	ids := make([]int64, 3)
	for i := range ids {
		ids[i] = seedUser(t, uniqueTestPhone(t), false, false)
	}

	// T3: no commit, no writes.
	dry, err := Apply(context.Background(), testDB, testKey, Options{Commit: false})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dry.DryRun {
		t.Fatal("Commit=false did not report a dry run")
	}
	if dry.WouldFill < len(ids) {
		t.Fatalf("dry run would fill %d rows, want at least %d", dry.WouldFill, len(ids))
	}
	for _, uid := range ids {
		if _, ok := hmacOf(t, uid); ok {
			t.Fatalf("dry run wrote phone_hmac for id=%d", uid)
		}
	}

	// T4: commit writes HMAC(key, phone) for every row.
	if _, err := Apply(context.Background(), testDB, testKey, Options{Commit: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, uid := range ids {
		stored, ok := hmacOf(t, uid)
		if !ok {
			t.Fatalf("id=%d still has no phone_hmac after apply", uid)
		}
		var phone string
		if err := testDB.QueryRowContext(context.Background(),
			`SELECT phone FROM t_user WHERE id = ?`, uid).Scan(&phone); err != nil {
			t.Fatalf("read phone: %v", err)
		}
		want, err := Hash(testKey, phone)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		if stored != want {
			t.Fatalf("id=%d phone_hmac = %q, want %q", uid, stored, want)
		}
	}

	// T5: the second run is a no-op — that is what makes the command resumable
	// without a state file.
	second, err := Apply(context.Background(), testDB, testKey, Options{Commit: true})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if second.Filled != 0 {
		t.Fatalf("second apply filled %d rows, want 0", second.Filled)
	}
}

// T6: interrupting a run and starting again fills the remaining rows exactly
// once. The `AND phone_hmac IS NULL` gate is what provides this.
func TestApplyResumesAfterInterruption(t *testing.T) {
	ids := make([]int64, 4)
	for i := range ids {
		ids[i] = seedUser(t, uniqueTestPhone(t), false, false)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// max-rows=2 with batch=1 stops after two rows.
	partial, err := Apply(ctx, testDB, testKey, Options{Commit: true, Batch: 1, MaxRows: 2})
	cancel()
	if err != nil {
		t.Fatalf("partial apply: %v", err)
	}
	if partial.Filled != 2 {
		t.Fatalf("partial apply filled %d, want 2", partial.Filled)
	}
	if _, err := Apply(context.Background(), testDB, testKey, Options{Commit: true, Batch: 2}); err != nil {
		t.Fatalf("resume apply: %v", err)
	}
	for _, uid := range ids {
		if _, ok := hmacOf(t, uid); !ok {
			t.Fatalf("id=%d was not filled after resume", uid)
		}
	}
}

// T7: a row that wants a hash another row already owns stops the run with the
// conflict, and the conflicting row is not written. This is the state that must
// never be "skipped over": two rows claiming one number is the bug.
func TestApplyStopsOnConflict(t *testing.T) {
	phone := uniqueTestPhone(t)
	holder := seedUser(t, phone, true, false) // already owns the hash
	victim := seedUser(t, phone, false, false)

	// Detect already sees this as a duplicate, so the gate must refuse first.
	report, err := Detect(context.Background(), testDB, testKey)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if report.ExitCode() != ExitDuplicate {
		t.Fatalf("detect exit = %d, want %d", report.ExitCode(), ExitDuplicate)
	}
	_, err = Apply(context.Background(), testDB, testKey, Options{Commit: true})
	var blocked *GateBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("apply err = %v, want *GateBlocked", err)
	}
	if blocked.ExitCode() != ExitDuplicate {
		t.Fatalf("gate exit = %d, want %d", blocked.ExitCode(), ExitDuplicate)
	}
	if _, ok := hmacOf(t, victim); ok {
		t.Fatal("apply wrote the conflicting row despite the gate")
	}

	// Remove the duplicate the way an operator would (the victim gives up the
	// number) and the run completes: the gate no longer blocks, and the holder
	// keeps the hash it already had.
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone = NULL WHERE id = ?`, victim); err != nil {
		t.Fatalf("clear victim phone: %v", err)
	}
	reported, err := Apply(context.Background(), testDB, testKey, Options{Commit: true})
	if err != nil {
		t.Fatalf("apply after dedup: %v", err)
	}
	if reported.Conflict != nil {
		t.Fatalf("unexpected conflict after the duplicate was removed: %v", reported.Conflict)
	}
	if _, ok := hmacOf(t, holder); !ok {
		t.Fatal("the holder lost its hash")
	}
	if _, ok := hmacOf(t, victim); ok {
		t.Fatal("the released row was given a hash; the number would stay claimed")
	}
}

// T17: verify is clean once every row carries the right hash, and names the row
// that does not.
func TestVerifyReportsDrift(t *testing.T) {
	ok := seedUser(t, uniqueTestPhone(t), false, false)
	drift := seedUser(t, uniqueTestPhone(t), false, false)
	if _, err := Apply(context.Background(), testDB, testKey, Options{Commit: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if report, err := Verify(context.Background(), testDB, testKey); err != nil {
		t.Fatalf("verify: %v", err)
	} else if report.Blocked() {
		t.Fatalf("verify found %d anomalies right after apply: %v", len(report.Anomalies), report.Anomalies)
	}
	_ = ok

	// A hash computed with a different key is exactly what a half-done rotation
	// or a manual edit leaves behind.
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone_hmac = ? WHERE id = ?`,
		strings.Repeat("a", 64), drift); err != nil {
		t.Fatalf("plant drift: %v", err)
	}
	report, err := Verify(context.Background(), testDB, testKey)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.ExitCode() != ExitAnomaly {
		t.Fatalf("verify exit = %d, want %d", report.ExitCode(), ExitAnomaly)
	}
	found := false
	for _, a := range report.Anomalies {
		if a.ID == drift && a.Kind == AnomalyStaleHMAC {
			found = true
		}
	}
	if !found {
		t.Fatalf("verify did not report STALE_HMAC for id=%d: %v", drift, report.Anomalies)
	}
}

// A missing key must stop the command before anything happens, and the error is
// distinguishable from "clean" and from "duplicates".
func TestDetectRequiresKey(t *testing.T) {
	if _, err := Detect(context.Background(), testDB, ""); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("err = %v, want ErrKeyMissing", err)
	}
	if _, err := Hash("short", "8613800138000"); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("short key err = %v, want ErrKeyMissing", err)
	}
}

// canonical + mask are the pieces the report and the hash depend on.
func TestCanonicalAndMask(t *testing.T) {
	cases := map[string]string{
		"8613800138000":      "8613800138000",
		"+86 138-0013-8000":  "8613800138000",
		"(86) 138 0013 8000": "8613800138000",
		"138.0013.8000":      "13800138000",
	}
	for raw, want := range cases {
		got, ok := Canonical(raw)
		if !ok || got != want {
			t.Fatalf("Canonical(%q) = %q,%v want %q", raw, got, ok, want)
		}
	}
	for _, bad := range []string{"", "12345", "abc", "8613800138000x"} {
		if _, ok := Canonical(bad); ok {
			t.Fatalf("Canonical(%q) accepted", bad)
		}
	}
	if got := Mask("8613800138000"); got != "861****00" {
		t.Fatalf("Mask = %q, want 861****00", got)
	}
	if got := Mask("1234"); got != "***" {
		t.Fatalf("Mask(short) = %q, want ***", got)
	}
}

var testPhoneSeq int64

// uniqueTestPhone builds a distinct number per call so a rerun never collides on
// uk_user_phone_hmac or manufactures a duplicate Detect would then report.
func uniqueTestPhone(t *testing.T) string {
	t.Helper()
	testPhoneSeq++
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random: %v", err)
	}
	return fmt.Sprintf("8619%02d%08d", time.Now().UnixNano()%100, int(b[0])<<16|int(b[1])<<8|int(b[2]))
}

// A soft-deleted row whose hash is missing (or is not the release marker) is a
// GHOST_HMAC in verify too — the same class detect reports, with the same remedy
// in the detail. Reporting it as PHONE_WITHOUT_HMAC ("the backfill has not
// covered this row") sent operators to rerun apply for a row apply skips on
// purpose, and the actual fix (rewrite #del#{id}{unix_ms}) never surfaced.
func TestVerifyTreatsSoftDeletedRowsAsGhosts(t *testing.T) {
	withLiveHash := seedUser(t, uniqueTestPhone(t), true, true) // deleted, hash not rewritten
	withNoHash := seedUser(t, uniqueTestPhone(t), false, true)  // deleted, hash NULL
	released := seedUser(t, uniqueTestPhone(t), false, true)    // deleted, correct marker
	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone_hmac = CONCAT('#del#', id, '#1717171717171') WHERE id = ?`,
		released); err != nil {
		t.Fatalf("write release marker: %v", err)
	}

	report, err := Verify(context.Background(), testDB, testKey)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	byID := map[int64]Anomaly{}
	for _, a := range report.Anomalies {
		byID[a.ID] = a
	}
	for _, id := range []int64{withLiveHash, withNoHash} {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("id=%d (soft-deleted) was not reported at all", id)
		}
		if got.Kind != AnomalyGhostHMAC {
			t.Fatalf("id=%d classified %q, want %q", id, got.Kind, AnomalyGhostHMAC)
		}
		if !strings.Contains(got.Detail, "#del#{id}{unix_ms}") {
			t.Fatalf("id=%d detail does not name the remedy: %q", id, got.Detail)
		}
	}
	if _, reported := byID[released]; reported {
		t.Fatalf("the row that already holds a #del# marker was reported: %v", byID[released])
	}
	// Nothing may be filed as "the backfill missed it": apply skips these rows by
	// design, so that label would be a wild goose chase.
	for _, a := range report.Anomalies {
		if a.ID == withNoHash && a.Kind == AnomalyPhoneWithoutHMAC {
			t.Fatal("a soft-deleted row was reported as PHONE_WITHOUT_HMAC")
		}
	}
}

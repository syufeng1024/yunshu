package main

// QA-level tests for the backfill command's *process contract*: the exit codes
// and the "no plaintext phone on stdout" rule are what docs/ops-runbook.md
// quotes, so they are asserted through run() rather than through the
// phonehmac package's API.
//
// The schema is private to this binary (yunshu_qa_hmac_cli) and dropped by
// TestMain after m.Run(), because Detect/Apply/Verify are whole-table
// operations: sharing yunshu_test would make them rewrite other packages'
// fixtures and read their half-built rows as anomalies.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/migrate"
	"yunshu.dev/core/internal/platform/phonehmac"
)

const (
	qaSchema = "yunshu_qa_hmac_cli"
	// qaKey is ≥32 bytes, so it satisfies the production length rule.
	qaKey = "qa-backfill-cli-phone-hmac-key-0001"
)

var (
	qaDB  *sql.DB
	qaDSN string
)

func TestMain(m *testing.M) {
	base := strings.TrimSpace(os.Getenv("TEST_DB_DSN"))
	if base == "" {
		// Same default the rest of the suite uses (compose overlay port).
		base = "root:root@tcp(127.0.0.1:13306)/yunshu_test?parseTime=true&loc=UTC&charset=utf8mb4&multiStatements=true"
	}
	conn, dsn, err := openQASchema(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill-phone-hmac tests: %v\n", err)
		os.Exit(1)
	}
	qaDB, qaDSN = conn, dsn
	code := m.Run()

	// Leave nothing behind: the schema exists only for this run.
	_, _ = conn.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+qaSchema+"`")
	_ = conn.Close()
	os.Exit(code)
}

// openQASchema creates, migrates and returns a schema nobody else touches.
func openQASchema(dsn string) (*sql.DB, string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, "", fmt.Errorf("parse TEST_DB_DSN: %w", err)
	}
	cfg.ParseTime = true
	if cfg.Loc == nil {
		cfg.Loc = time.UTC
	}
	adminCfg := *cfg
	adminCfg.DBName = ""
	admin, err := sql.Open("mysql", adminCfg.FormatDSN())
	if err != nil {
		return nil, "", fmt.Errorf("open admin: %w", err)
	}
	defer admin.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+qaSchema+"`"); err != nil {
		return nil, "", fmt.Errorf("drop stale schema: %w", err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(
		"CREATE DATABASE `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_as_cs", qaSchema)); err != nil {
		return nil, "", fmt.Errorf("create schema: %w", err)
	}

	cfg.DBName = qaSchema
	conn, err := sql.Open("mysql", migrate.NormalizeDSN(cfg.FormatDSN()))
	if err != nil {
		return nil, "", fmt.Errorf("open private schema: %w", err)
	}
	conn.SetMaxOpenConns(2)
	migrations, err := migrate.LoadEmbedded()
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	runner := migrate.NewRunner(conn, migrations, clock.Real{}.Now)
	// The demo seed inserts accounts this file would have to exclude from every
	// count, and it needs no phone handling of its own.
	runner.Skip("0019", "backfill CLI tests create their own rows")
	if _, err := runner.Up(ctx); err != nil {
		_ = conn.Close()
		return nil, "", fmt.Errorf("migrate private schema: %w", err)
	}
	return conn, cfg.FormatDSN(), nil
}

// getenv returns qaDSN for DB_DSN and a fixed key, so run() can be driven
// without the process environment.
func getenvWithKey(key string) func(string) string {
	return func(k string) string {
		switch k {
		case "DB_DSN":
			return qaDSN
		case "PHONE_HMAC_KEY":
			return key
		default:
			return ""
		}
	}
}

// runCLI invokes the command the way a shell would and returns its exit code
// with both streams.
func runCLI(t *testing.T, getenv func(string) string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, getenv, &out, &errb)
	return code, out.String(), errb.String()
}

// uniquePhone returns a distinct, canonical 13-digit number per call. t_phone
// carries no unique key, but uk_user_phone_hmac does: reusing a number across
// tests would make the next run's Detect see a duplicate it did not seed.
func uniquePhone(t *testing.T) string {
	t.Helper()
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random: %v", err)
	}
	var digits strings.Builder
	digits.WriteString("861")
	for _, v := range b {
		digits.WriteByte('0' + v%10)
	}
	return digits.String()
}

// seedPhone inserts one t_user row. phoneMAC "" writes NULL; deletedAt non-nil
// writes deleted_at (the soft-delete state).
func seedPhone(t *testing.T, phone, phoneMAC string, deleted bool) int64 {
	t.Helper()
	now := time.Now().UTC()
	var b [4]byte
	_, _ = rand.Read(b[:])
	userNo := fmt.Sprintf("Q%s%03d", now.Format("20060102"), (int(b[0])<<8|int(b[1]))%1000)
	var phoneVal, macVal, delVal any
	if phone != "" {
		phoneVal = phone
	}
	if phoneMAC != "" {
		macVal = phoneMAC
	}
	if deleted {
		delVal = now
	}
	res, err := qaDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, phone, phone_hmac, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at, deleted_at)
		VALUES (?,?,?,'not-a-real-hash','bcrypt',?,'qa','PERSONAL','NORMAL',?,?,?)`,
		userNo, phoneVal, macVal, now, now, now, delVal)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed id: %v", err)
	}
	t.Cleanup(func() {
		_, _ = qaDB.ExecContext(context.Background(), `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

func hmacOf(t *testing.T, uid int64) (string, bool) {
	t.Helper()
	var v sql.NullString
	if err := qaDB.QueryRowContext(context.Background(),
		`SELECT phone_hmac FROM t_user WHERE id = ?`, uid).Scan(&v); err != nil {
		t.Fatalf("read hmac: %v", err)
	}
	return v.String, v.Valid
}

// filledCount is the whole-table count of rows that already carry a hash; the
// dry-run assertion compares it before and after.
func filledCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := qaDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE phone_hmac IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count filled: %v", err)
	}
	return n
}

// C7: three phones, one duplicate pair → detect exits 2, names both ids, masks
// every number and never prints a plaintext one.
func TestQADetectDuplicateExitsTwoAndMasks(t *testing.T) {
	phone := uniquePhone(t)
	first := seedPhone(t, phone, "", false)
	second := seedPhone(t, phone, "", false)
	third := seedPhone(t, uniquePhone(t), "", false)
	_ = third

	code, out, errb := runCLI(t, getenvWithKey(qaKey), "detect")
	if code != phonehmac.ExitDuplicate {
		t.Fatalf("detect exit = %d, want %d\nstdout=%s\nstderr=%s",
			code, phonehmac.ExitDuplicate, out, errb)
	}
	for _, want := range []string{
		fmt.Sprintf("id=%d", first), fmt.Sprintf("id=%d", second), "DUP",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("detect output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "****") {
		t.Fatalf("detect output carries no masked number:\n%s", out)
	}
	// The masking rule must actually remove the tail: no 13-digit plaintext.
	if strings.Contains(out, phone) {
		t.Fatalf("detect printed the plaintext phone %s:\n%s", phone, out)
	}

	// Removing the duplicate (the operator's decision) clears the blocker.
	if _, err := qaDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone = NULL WHERE id = ?`, second); err != nil {
		t.Fatalf("release duplicate: %v", err)
	}
	code, out, errb = runCLI(t, getenvWithKey(qaKey), "detect")
	if code != phonehmac.ExitOK {
		t.Fatalf("detect after dedup exit = %d, want 0\nstdout=%s\nstderr=%s", code, out, errb)
	}
}

// C7 (JSON form): the machine-readable report must not carry plaintext either.
func TestQADetectJSONHasNoPlaintext(t *testing.T) {
	phone := uniquePhone(t)
	seedPhone(t, phone, "", false)
	code, out, errb := runCLI(t, getenvWithKey(qaKey), "detect", "--json")
	if code != phonehmac.ExitOK {
		t.Fatalf("detect --json exit = %d, want 0\nstdout=%s\nstderr=%s", code, out, errb)
	}
	if strings.Contains(out, phone) {
		t.Fatalf("detect --json printed the plaintext phone:\n%s", out)
	}
	if !strings.Contains(out, `"scanned"`) {
		t.Fatalf("detect --json is not JSON:\n%s", out)
	}
}

// C8: apply is a dry run by default, --commit writes, a second --commit fills 0
// rows, and verify is clean afterwards.
func TestQAApplyDryRunCommitIdempotentVerify(t *testing.T) {
	ids := make([]int64, 2)
	for i := range ids {
		ids[i] = seedPhone(t, uniquePhone(t), "", false)
	}
	before := filledCount(t)

	code, out, errb := runCLI(t, getenvWithKey(qaKey), "apply")
	if code != phonehmac.ExitOK {
		t.Fatalf("apply dry-run exit = %d, want 0\nstdout=%s\nstderr=%s", code, out, errb)
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("apply without --commit did not report a dry run:\n%s", out)
	}
	if after := filledCount(t); after != before {
		t.Fatalf("dry run changed the phone_hmac count %d -> %d", before, after)
	}

	code, out, errb = runCLI(t, getenvWithKey(qaKey), "apply", "--commit")
	if code != phonehmac.ExitOK {
		t.Fatalf("apply --commit exit = %d, want 0\nstdout=%s\nstderr=%s", code, out, errb)
	}
	for _, uid := range ids {
		stored, ok := hmacOf(t, uid)
		if !ok || len(stored) != 64 {
			t.Fatalf("id=%d phone_hmac = %q (ok=%v), want 64 hex chars", uid, stored, ok)
		}
	}

	code, out, errb = runCLI(t, getenvWithKey(qaKey), "verify")
	if code != phonehmac.ExitOK || !strings.Contains(out, "0 差异") {
		t.Fatalf("verify exit = %d, want 0 with 0 differences\nstdout=%s\nstderr=%s", code, out, errb)
	}

	// Idempotence: the AND phone_hmac IS NULL gate makes the rerun a no-op.
	code, out, errb = runCLI(t, getenvWithKey(qaKey), "apply", "--commit")
	if code != phonehmac.ExitOK {
		t.Fatalf("second apply --commit exit = %d, want 0\nstdout=%s\nstderr=%s", code, out, errb)
	}
	if !strings.Contains(out, "已回填 0 行") {
		t.Fatalf("second apply --commit filled rows again:\n%s", out)
	}
}

// C9: a row whose number is already owned by a *different* row (only reachable
// by hand: phone=NULL + hash kept, i.e. the post-PII-clearing state) makes
// apply exit 4 without writing that row. Rows already filled stay filled —
// per-row UPDATE, no batch transaction — which is the documented atomicity.
func TestQAApplyConflictExitsFour(t *testing.T) {
	phone := uniquePhone(t)
	owner, err := phonehmac.Hash(qaKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	// A second, unrelated pending row, seeded first so it has the lowest id: it
	// is filled before the conflict is reached and must stay filled afterwards
	// (per-row UPDATE, no batch transaction).
	other := seedPhone(t, uniquePhone(t), "", false)
	// The "claim" row: number released (phone NULL) but hash retained, exactly
	// what a cancelled / PII-cleared account looks like. Detect only scans rows
	// with a phone, so the gate is clean and Apply reaches the UPDATE.
	holder := seedPhone(t, "", owner, false)
	victim := seedPhone(t, phone, "", false)

	code, out, errb := runCLI(t, getenvWithKey(qaKey), "detect")
	if code != phonehmac.ExitOK {
		t.Fatalf("precondition: detect exit = %d, want 0 (the gate must be clean)\nstdout=%s\nstderr=%s",
			code, out, errb)
	}

	code, out, errb = runCLI(t, getenvWithKey(qaKey), "apply", "--commit")
	if code != phonehmac.ExitConflict {
		t.Fatalf("apply exit = %d, want %d (1062 must not be swallowed\nstdout=%s\nstderr=%s",
			code, phonehmac.ExitConflict, out, errb)
	}
	if !strings.Contains(out, "CONFLICT") {
		t.Fatalf("apply did not name the conflict:\n%s", out)
	}
	if strings.Contains(out, phone) {
		t.Fatalf("the conflict report printed the plaintext phone:\n%s", out)
	}
	if _, ok := hmacOf(t, victim); ok {
		t.Fatal("apply wrote the conflicting row; two rows would claim one number")
	}
	if _, ok := hmacOf(t, holder); !ok {
		t.Fatal("apply cleared the holder's hash")
	}
	if _, ok := hmacOf(t, other); !ok {
		t.Fatal("apply lost the row it had already filled before the conflict: " +
			"the conflict must stop the run, not roll back completed single-row writes")
	}
}

// C10: a soft-deleted row whose hash was never rewritten is a ghost that still
// occupies the number. Detect blocks (exit 3, GHOST_HMAC) and apply writes
// nothing — the repair (#del#{id}{unix_ms}) is an operator decision, and apply
// must never fill a live hash into a deleted row.
func TestQASoftDeletedRowBlocksApply(t *testing.T) {
	ghost := seedPhone(t, uniquePhone(t), "", true)

	code, out, errb := runCLI(t, getenvWithKey(qaKey), "detect")
	if code != phonehmac.ExitAnomaly {
		t.Fatalf("detect exit = %d, want %d\nstdout=%s\nstderr=%s",
			code, phonehmac.ExitAnomaly, out, errb)
	}
	if !strings.Contains(out, phonehmac.AnomalyGhostHMAC) || !strings.Contains(out, fmt.Sprintf("id=%d", ghost)) {
		t.Fatalf("detect did not flag the ghost row as %s:\n%s", phonehmac.AnomalyGhostHMAC, out)
	}

	code, out, errb = runCLI(t, getenvWithKey(qaKey), "apply", "--commit")
	if code != phonehmac.ExitAnomaly {
		t.Fatalf("apply over a ghost exit = %d, want %d\nstdout=%s\nstderr=%s",
			code, phonehmac.ExitAnomaly, out, errb)
	}
	if _, ok := hmacOf(t, ghost); ok {
		t.Fatal("apply filled a soft-deleted row with a live hash; the number stays occupied")
	}

	// Once an operator rewrites the marker the number is released and the
	// command is clean, even though the row is still there.
	if _, err := qaDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone_hmac = ? WHERE id = ?`,
		fmt.Sprintf("%s%d#%d", phonehmac.DuplicateMergePrefix, ghost, time.Now().UnixMilli()), ghost); err != nil {
		t.Fatalf("write release marker: %v", err)
	}
	code, out, errb = runCLI(t, getenvWithKey(qaKey), "detect")
	if code != phonehmac.ExitOK {
		t.Fatalf("detect after #del# rewrite exit = %d, want 0\nstdout=%s\nstderr=%s", code, out, errb)
	}
}

// C10b (targeted re-check): verify judges a soft-deleted row by one rule only —
// did it release the number? Three shapes, and only two are reported:
//
//	软删 + 正常 hmac(HMAC(phone))  -> GHOST_HMAC, detail names #del#{id}{unix_ms}
//	软删 + NULL                    -> GHOST_HMAC, same remedy (was
//	                                  PHONE_WITHOUT_HMAC, which sent the
//	                                  operator to rerun apply on a row apply
//	                                  skips on purpose)
//	软删 + #del#{id}#{ms}          -> clean, not reported
//
// Nothing in the output may say PHONE_WITHOUT_HMAC: that class now means "a
// live row the backfill has not reached" and nothing else.
func TestQAVerifyClassifiesSoftDeletedRowsAsGhost(t *testing.T) {
	liveHash := seedPhone(t, uniquePhone(t), "", true) // deleted, still holds a live hash
	if mac, err := phonehmac.Hash(qaKey, mustPhone(t, liveHash)); err != nil {
		t.Fatalf("hash: %v", err)
	} else if _, err := qaDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone_hmac = ? WHERE id = ?`, mac, liveHash); err != nil {
		t.Fatalf("plant live hash: %v", err)
	}
	nullHash := seedPhone(t, uniquePhone(t), "", true) // deleted, hash never written
	released := seedPhone(t, uniquePhone(t), "", true) // deleted, marker written
	if _, err := qaDB.ExecContext(context.Background(),
		`UPDATE t_user SET phone_hmac = ? WHERE id = ?`,
		fmt.Sprintf("%s%d#%d", phonehmac.DuplicateMergePrefix, released, time.Now().UnixMilli()),
		released); err != nil {
		t.Fatalf("write release marker: %v", err)
	}

	code, out, errb := runCLI(t, getenvWithKey(qaKey), "verify")
	if code != phonehmac.ExitAnomaly {
		t.Fatalf("verify exit = %d, want %d (two ghosts must block)\nstdout=%s\nstderr=%s",
			code, phonehmac.ExitAnomaly, out, errb)
	}
	if strings.Contains(out, phonehmac.AnomalyPhoneWithoutHMAC) {
		t.Fatalf("verify still classifies a soft-deleted row as %s:\n%s",
			phonehmac.AnomalyPhoneWithoutHMAC, out)
	}
	for _, uid := range []int64{liveHash, nullHash} {
		line := ghostLine(out, uid)
		if line == "" {
			t.Fatalf("id=%d is not reported at all:\n%s", uid, out)
		}
		if !strings.Contains(line, phonehmac.AnomalyGhostHMAC) {
			t.Fatalf("id=%d classified as something other than %s: %s",
				uid, phonehmac.AnomalyGhostHMAC, line)
		}
		if !strings.Contains(line, "#del#{id}") {
			t.Fatalf("id=%d detail does not name the remedy #del#{id}: %s", uid, line)
		}
	}
	if line := ghostLine(out, released); line != "" {
		t.Fatalf("a row carrying the #del# marker was reported: %s", line)
	}
}

// mustPhone reads back the phone a seeded row holds.
func mustPhone(t *testing.T, uid int64) string {
	t.Helper()
	var phone string
	if err := qaDB.QueryRowContext(context.Background(),
		`SELECT phone FROM t_user WHERE id = ?`, uid).Scan(&phone); err != nil {
		t.Fatalf("read phone: %v", err)
	}
	return phone
}

// ghostLine returns the report line that mentions id, or "" when there is none.
func ghostLine(out string, uid int64) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, fmt.Sprintf("id=%d ", uid)) {
			return line
		}
	}
	return ""
}

// B5/C: without a key the command refuses before touching the database — exit
// 1, and the message carries no key material.
func TestQAMissingKeyExitsOne(t *testing.T) {
	getenv := func(k string) string {
		if k == "DB_DSN" {
			return "root:root@tcp(127.0.0.1:1)/none?parseTime=true" // would fail if opened
		}
		return ""
	}
	code, out, errb := runCLI(t, getenv, "detect")
	if code != phonehmac.ExitRuntime {
		t.Fatalf("detect without key exit = %d, want %d\nstdout=%s\nstderr=%s",
			code, phonehmac.ExitRuntime, out, errb)
	}
	if !strings.Contains(errb, "PHONE_HMAC_KEY") {
		t.Fatalf("the refusal does not name the missing variable:\n%s", errb)
	}

	// A too-short key is the same class of refusal.
	code, _, errb = runCLI(t, getenvWithKey("short-key"), "verify")
	if code != phonehmac.ExitRuntime {
		t.Fatalf("verify with a short key exit = %d, want %d\nstderr=%s",
			code, phonehmac.ExitRuntime, errb)
	}
}

// Unknown/missing subcommand is a usage error, not a panic.
func TestQAUsageErrors(t *testing.T) {
	code, _, errb := runCLI(t, getenvWithKey(qaKey))
	if code != phonehmac.ExitRuntime {
		t.Fatalf("no args exit = %d, want %d (stderr=%s)", code, phonehmac.ExitRuntime, errb)
	}
	code, _, errb = runCLI(t, getenvWithKey(qaKey), "nonsense")
	if code != phonehmac.ExitRuntime {
		t.Fatalf("unknown command exit = %d, want %d (stderr=%s)", code, phonehmac.ExitRuntime, errb)
	}
}

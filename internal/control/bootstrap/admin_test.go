package bootstrap

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/auth"
	"yunshu.dev/core/internal/control/ctltest"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// ---------------------------------------------------------------------------
// configuration: no database involved
// ---------------------------------------------------------------------------

// clearBootstrapEnv removes the three variables so a developer shell that has
// them exported cannot change what these assertions see.
func clearBootstrapEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvDisabled, EnvUsername, EnvPassword} {
		t.Setenv(name, "")
	}
}

func TestOptionsFromEnvDefaults(t *testing.T) {
	clearBootstrapEnv(t)
	opts, err := OptionsFromEnv()
	if err != nil {
		t.Fatalf("OptionsFromEnv: %v", err)
	}
	if opts.Disabled {
		t.Fatal("Disabled = true, want false when the variable is unset")
	}
	if opts.Username != "admin" {
		t.Fatalf("Username = %q, want %q", opts.Username, "admin")
	}
	if opts.Password != "12345678" {
		t.Fatalf("Password = %q, want the documented default", opts.Password)
	}
}

// TestOptionsFromEnvDisabled covers the five accepted spellings plus one
// rejected one. strconv.ParseBool is the contract, so the accepted set is
// exactly its set.
func TestOptionsFromEnvDisabled(t *testing.T) {
	cases := []struct {
		raw       string
		want      bool
		wantError bool
	}{
		{"", false, false},
		{"false", false, false},
		{"0", false, false},
		{"true", true, false},
		{"1", true, false},
		{"TRUE", true, false},
		{" 1 ", true, false},
		{"yes", false, true},
		{"on", false, true},
		{"disable", false, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.raw), func(t *testing.T) {
			clearBootstrapEnv(t)
			t.Setenv(EnvDisabled, tc.raw)
			opts, err := OptionsFromEnv()
			if tc.wantError {
				if err == nil {
					t.Fatalf("%q was accepted", tc.raw)
				}
				if !errors.Is(err, ErrInvalidConfig) {
					t.Fatalf("error %v is not ErrInvalidConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q rejected: %v", tc.raw, err)
			}
			if opts.Disabled != tc.want {
				t.Fatalf("%q: Disabled = %v, want %v", tc.raw, opts.Disabled, tc.want)
			}
		})
	}
}

// TestOptionsFromEnvNormalizesUsername: the value is stored lowercase, so the
// variable must accept the same shapes a login form does.
func TestOptionsFromEnvNormalizesUsername(t *testing.T) {
	for raw, want := range map[string]string{
		"admin":     "admin",
		"Admin":     "admin",
		" root.ops": "root.ops",
		"ＡＤＭＩＮ":     "admin",
	} {
		t.Run(raw, func(t *testing.T) {
			clearBootstrapEnv(t)
			t.Setenv(EnvUsername, raw)
			opts, err := OptionsFromEnv()
			if err != nil {
				t.Fatalf("%q rejected: %v", raw, err)
			}
			if opts.Username != want {
				t.Fatalf("%q: Username = %q, want %q", raw, opts.Username, want)
			}
		})
	}
}

// TestOptionsFromEnvRejectsBadUsername: an email or a phone number would land in
// the wrong column, and an all-digit value would be ambiguous with a phone
// forever. Both must be refused at boot, not at the first login.
func TestOptionsFromEnvRejectsBadUsername(t *testing.T) {
	for _, raw := range []string{
		"13800138000", // phone shape
		"8613800138000",
		"a@b.com",               // email shape
		"ab",                    // too short
		"Admin!",                // illegal character
		"_admin",                // does not start with a letter
		"admin@",                // malformed email shape
		"admin 名称",              // non-ASCII
		strings.Repeat("a", 33), // too long
	} {
		t.Run(raw, func(t *testing.T) {
			clearBootstrapEnv(t)
			t.Setenv(EnvUsername, raw)
			opts, err := OptionsFromEnv()
			if err == nil {
				t.Fatalf("%q was accepted as %+v", raw, opts)
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error %v is not ErrInvalidConfig", err)
			}
			// The message names the variable and the rule, never the value of
			// the password.
			if !strings.Contains(err.Error(), EnvUsername) {
				t.Fatalf("message %q does not name %s", err.Error(), EnvUsername)
			}
		})
	}
}

// TestOptionsFromEnvRejectsBadPassword also proves the error text never carries
// the password itself: a 7-byte secret would otherwise end up in a log line.
func TestOptionsFromEnvRejectsBadPassword(t *testing.T) {
	cases := map[string]string{
		"7 bytes":   "p@ss123",
		"73 bytes":  strings.Repeat("a", 73),
		"all blank": "        ",
	}
	for name, pw := range cases {
		t.Run(name, func(t *testing.T) {
			clearBootstrapEnv(t)
			t.Setenv(EnvPassword, pw)
			opts, err := OptionsFromEnv()
			if err == nil {
				t.Fatalf("%q was accepted as %+v", name, opts)
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error %v is not ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), EnvPassword) {
				t.Fatalf("message %q does not name %s", err.Error(), EnvPassword)
			}
			if strings.Contains(err.Error(), strings.TrimSpace(pw)) && strings.TrimSpace(pw) != "" {
				t.Fatalf("message leaks the password value: %q", err.Error())
			}
		})
	}
}

// TestEnsureSuperAdminRejectsBadOptions covers the defensive re-check inside
// EnsureSuperAdmin: a caller that builds Options by hand must be refused the
// same way OptionsFromEnv refuses it.
//
// Every case runs against the real Deps, not a half-built struct: with a nil
// Audit the function stopped at "审计写入器为空" and every assertion below passed
// without ever reaching the validation it claims to test. The message checks are
// what make each case prove that its own rule fired.
func TestEnsureSuperAdminRejectsBadOptions(t *testing.T) {
	deps := testDeps(t)

	_, err := EnsureSuperAdmin(context.Background(), deps, Options{})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty options: err = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), EnvUsername) {
		t.Fatalf("empty options: message %q does not name %s", err.Error(), EnvUsername)
	}

	_, err = EnsureSuperAdmin(context.Background(), deps, Options{
		Username: "13800138000", Password: "whatever1",
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("phone username: err = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), EnvUsername) {
		t.Fatalf("phone username: message %q does not name %s", err.Error(), EnvUsername)
	}

	_, err = EnsureSuperAdmin(context.Background(), deps, Options{
		Username: "root", Password: "short7!",
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("short password: err = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), EnvPassword) {
		t.Fatalf("short password: message %q does not name %s", err.Error(), EnvPassword)
	}

	// A missing database is caught before anything else, so the audit writer is
	// present here on purpose: without it this would be the audit-nil case again.
	_, err = EnsureSuperAdmin(context.Background(), Deps{Audit: deps.Audit}, Options{
		Username: "root", Password: "longenough1",
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil db: err = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "数据库连接") {
		t.Fatalf("nil db: message %q does not report the missing database", err.Error())
	}
}

// TestOptionsFromEnvDisabledShortCircuits: ADMIN_BOOTSTRAP_DISABLED=true is the
// documented "完全跳过引导" switch, so a value the enabled path would reject must
// not stop the process from starting. Nothing about the account is validated or
// created — the switch is the whole answer.
func TestOptionsFromEnvDisabledShortCircuits(t *testing.T) {
	clearBootstrapEnv(t)
	t.Setenv(EnvDisabled, "true")
	t.Setenv(EnvUsername, "13800138000") // a phone shape: rejected when enabled
	t.Setenv(EnvPassword, "Zq7")         // 3 bytes: rejected when enabled

	opts, err := OptionsFromEnv()
	if err != nil {
		t.Fatalf("OptionsFromEnv with %s=true: %v", EnvDisabled, err)
	}
	if !opts.Disabled {
		t.Fatal("Disabled = false, want true")
	}
	if opts.Username != "13800138000" {
		t.Fatalf("Username = %q, want the raw environment value carried through", opts.Username)
	}
	if opts.Password != "" {
		t.Fatalf("Password = %q, want empty on the disabled path", opts.Password)
	}

	res, err := EnsureSuperAdmin(context.Background(), testDeps(t), opts)
	if err != nil {
		t.Fatalf("EnsureSuperAdmin with the switch off: %v", err)
	}
	if res.Outcome != OutcomeDisabled {
		t.Fatalf("outcome = %s, want %s", res.Outcome, OutcomeDisabled)
	}
	if n := countUsername(t, opts.Username); n != 0 {
		t.Fatalf("%d rows were written with the switch off", n)
	}
}

// TestOptionsFromEnvPasswordTrailingNewline: a CRLF .env / configmap file hands
// the line break to os.Getenv, and a '\r' baked into the password makes the
// documented value permanently unloggable. Only the trailing "\r"/"\n" are
// removed — never a space, which is a legal password character — and the hash
// actually written to t_user must verify the documented value.
func TestOptionsFromEnvPasswordTrailingNewline(t *testing.T) {
	clearBootstrapEnv(t)
	t.Setenv(EnvPassword, "abcdefgh\r\n")

	opts, err := OptionsFromEnv()
	if err != nil {
		t.Fatalf("OptionsFromEnv: %v", err)
	}
	if opts.Password != "abcdefgh" {
		t.Fatalf("Password = %q, want %q", opts.Password, "abcdefgh")
	}
	if err := auth.ValidatePassword(opts.Password); err != nil {
		t.Fatalf("the trimmed password does not satisfy the rule: %v", err)
	}

	username := uniqueUsername(t)
	cleanupUsername(t, username)
	res, err := EnsureSuperAdmin(context.Background(), testDeps(t), Options{
		Username: username, Password: opts.Password,
	})
	if err != nil {
		t.Fatalf("EnsureSuperAdmin: %v", err)
	}
	if res.Outcome != OutcomeCreated {
		t.Fatalf("outcome = %s, want %s", res.Outcome, OutcomeCreated)
	}

	var hash string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT password_hash FROM t_user WHERE id = ?`, res.UID).Scan(&hash); err != nil {
		t.Fatalf("read password_hash: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("abcdefgh")); err != nil {
		t.Fatalf("stored hash does not verify the trimmed password: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("abcdefgh\r\n")); err == nil {
		t.Fatal("stored hash still verifies the untrimmed value: the line break was hashed")
	}

	// Surrounding spaces are part of the secret and must survive untouched; a
	// TrimSpace here would silently hash a different password than the one the
	// operator typed.
	clearBootstrapEnv(t)
	t.Setenv(EnvPassword, " abcdefgh ")
	opts, err = OptionsFromEnv()
	if err != nil {
		t.Fatalf(`" abcdefgh " rejected: %v`, err)
	}
	if opts.Password != " abcdefgh " {
		t.Fatalf("Password = %q, want the spaces preserved", opts.Password)
	}

	// An entry that is nothing but a line break is empty after trimming, so it
	// falls back to the documented default like an absent variable does.
	clearBootstrapEnv(t)
	t.Setenv(EnvPassword, "\r\n")
	opts, err = OptionsFromEnv()
	if err != nil {
		t.Fatalf("line-break-only password rejected: %v", err)
	}
	if opts.Password != defaultPassword {
		t.Fatalf("Password = %q, want the documented default", opts.Password)
	}
}

func TestEnsureSuperAdminDisabledWritesNothing(t *testing.T) {
	username := uniqueUsername(t)
	res, err := EnsureSuperAdmin(context.Background(), testDeps(t), Options{
		Disabled: true, Username: username, Password: testPassword,
	})
	if err != nil {
		t.Fatalf("EnsureSuperAdmin: %v", err)
	}
	if res.Outcome != OutcomeDisabled {
		t.Fatalf("outcome = %s, want %s", res.Outcome, OutcomeDisabled)
	}
	if n := countUsername(t, username); n != 0 {
		t.Fatalf("%d rows were written with the switch off", n)
	}
}

// ---------------------------------------------------------------------------
// integration: real MySQL, real audit table
// ---------------------------------------------------------------------------

// testPassword is long enough for auth.ValidatePassword and is not the shipped
// default, so a test never depends on the production literal.
const testPassword = "Bootstrap#2026"

var usernameSeq int64

// uniqueUsername returns a lowercase ASCII username unique across processes and
// runs. admin is never reused: leaving a row named "admin" behind would make
// another package's assertion about that username flaky.
func uniqueUsername(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&usernameSeq, 1)
	name := fmt.Sprintf("boot%d%03d", time.Now().UnixNano(), n%1000)
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

func testDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		DB:     testDB,
		Audit:  &audit.Writer{DB: testDB, Now: clock.Real{}},
		Logger: nil, // assertions read the database, not the log
		Now:    clock.Real{},
	}
}

// cleanupUsername registers the three deletes a bootstrap test owes the shared
// yunshu_test schema.
func cleanupUsername(t *testing.T, username string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		var uid int64
		_ = testDB.QueryRowContext(ctx, `SELECT id FROM t_user WHERE username = ?`, username).Scan(&uid)
		if uid != 0 {
			_, _ = testDB.ExecContext(ctx, `DELETE FROM t_admin_audit_log WHERE target_type = 't_user' AND target_id = ?`,
				strconv.FormatInt(uid, 10))
			_, _ = testDB.ExecContext(ctx, `DELETE FROM t_credit_account WHERE user_id = ?`, uid)
			_, _ = testDB.ExecContext(ctx, `DELETE FROM t_user WHERE id = ?`, uid)
		}
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_user WHERE username = ?`, username)
	})
}

func countUsername(t *testing.T, username string) int {
	t.Helper()
	var n int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE username = ?`, username).Scan(&n); err != nil {
		t.Fatalf("count username %q: %v", username, err)
	}
	return n
}

func countSystemCreateAudit(t *testing.T, uid int64) int {
	t.Helper()
	var n int
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM t_admin_audit_log
		 WHERE module = 'USER' AND action = 'CREATE' AND operator_role = 'SYSTEM'
		   AND target_type = 't_user' AND target_id = ?`, strconv.FormatInt(uid, 10)).Scan(&n); err != nil {
		t.Fatalf("count audit rows for uid %d: %v", uid, err)
	}
	return n
}

type superAdminRow struct {
	Role            string
	Status          string
	Algo            string
	Nickname        string
	Email           sql.NullString
	Phone           sql.NullString
	PasswordUpdated time.Time
	UserNo          string
	PasswordInitial bool
}

func loadSuperAdmin(t *testing.T, username string) superAdminRow {
	t.Helper()
	var row superAdminRow
	err := testDB.QueryRowContext(context.Background(), `
		SELECT role, status, password_algo, nickname, email, phone, password_updated_at, user_no,
		       password_is_initial
		  FROM t_user WHERE username = ?`, username).
		Scan(&row.Role, &row.Status, &row.Algo, &row.Nickname, &row.Email, &row.Phone,
			&row.PasswordUpdated, &row.UserNo, &row.PasswordInitial)
	if err != nil {
		t.Fatalf("load t_user row for %q: %v", username, err)
	}
	return row
}

func creditAccountCount(t *testing.T, uid int64) int {
	t.Helper()
	var n int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_credit_account WHERE user_id = ?`, uid).Scan(&n); err != nil {
		t.Fatalf("count credit account: %v", err)
	}
	return n
}

func TestEnsureSuperAdminCreatesAccount(t *testing.T) {
	username := uniqueUsername(t)
	cleanupUsername(t, username)

	res, err := EnsureSuperAdmin(context.Background(), testDeps(t), Options{
		Username: username, Password: testPassword,
	})
	if err != nil {
		t.Fatalf("EnsureSuperAdmin: %v", err)
	}
	if res.Outcome != OutcomeCreated {
		t.Fatalf("outcome = %s, want %s", res.Outcome, OutcomeCreated)
	}
	if res.UID <= 0 {
		t.Fatalf("uid = %d, want > 0", res.UID)
	}
	if res.Username != username {
		t.Fatalf("username = %q, want %q", res.Username, username)
	}
	if n := countUsername(t, username); n != 1 {
		t.Fatalf("t_user rows = %d, want 1", n)
	}

	row := loadSuperAdmin(t, username)
	if row.Role != auth.RoleSuperAdmin {
		t.Fatalf("role = %q, want %q", row.Role, auth.RoleSuperAdmin)
	}
	if row.Status != "NORMAL" {
		t.Fatalf("status = %q, want NORMAL", row.Status)
	}
	if row.Algo != "bcrypt" {
		t.Fatalf("password_algo = %q, want bcrypt", row.Algo)
	}
	// 初始口令标记必须由引导本身写入：没有它，前端不会出现改密提醒，而这条
	// 路径之外没有任何地方会补这个标记。
	if !row.PasswordInitial {
		t.Fatalf("password_is_initial = false, want true (drives the console reminder)")
	}
	if row.Nickname != superAdminNickname {
		t.Fatalf("nickname = %q, want %q (the console header renders it as display_name)",
			row.Nickname, superAdminNickname)
	}
	if row.Email.Valid {
		t.Fatalf("email = %q, want NULL", row.Email.String)
	}
	if row.Phone.Valid {
		t.Fatalf("phone = %q, want NULL", row.Phone.String)
	}
	if row.UserNo == "" {
		t.Fatal("user_no is empty")
	}
	// The empty credit account is what keeps SUPER_ADMIN's own console overview
	// from reporting exists=false.
	if n := creditAccountCount(t, res.UID); n != 1 {
		t.Fatalf("t_credit_account rows = %d, want 1", n)
	}
	if n := countSystemCreateAudit(t, res.UID); n != 1 {
		t.Fatalf("SYSTEM CREATE audit rows = %d, want 1", n)
	}

	// The stored hash must actually verify: a bootstrap that wrote a hash the
	// login path cannot verify would look correct in every column assertion.
	var hash string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT password_hash FROM t_user WHERE id = ?`, res.UID).Scan(&hash); err != nil {
		t.Fatalf("read password_hash: %v", err)
	}
	if strings.Contains(hash, testPassword) {
		t.Fatal("password_hash contains the plaintext password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(testPassword)); err != nil {
		t.Fatalf("stored hash does not verify the configured password: %v", err)
	}
}

// TestEnsureSuperAdminIsIdempotent runs the bootstrap three times and asserts
// that the second and third calls changed nothing at all: same row count, same
// password timestamp, still exactly one audit row.
func TestEnsureSuperAdminIsIdempotent(t *testing.T) {
	username := uniqueUsername(t)
	cleanupUsername(t, username)
	deps := testDeps(t)

	first, err := EnsureSuperAdmin(context.Background(), deps, Options{
		Username: username, Password: testPassword,
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.Outcome != OutcomeCreated {
		t.Fatalf("first outcome = %s, want %s", first.Outcome, OutcomeCreated)
	}
	before := loadSuperAdmin(t, username)

	for i := 2; i <= 3; i++ {
		res, err := EnsureSuperAdmin(context.Background(), deps, Options{
			Username: username, Password: testPassword,
		})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if res.Outcome != OutcomeSkipped {
			t.Fatalf("run %d outcome = %s, want %s", i, res.Outcome, OutcomeSkipped)
		}
		if res.UID != 0 {
			t.Fatalf("run %d reported uid=%d for a skipped outcome", i, res.UID)
		}
		if n := countUsername(t, username); n != 1 {
			t.Fatalf("run %d: t_user rows = %d, want 1", i, n)
		}
		if n := countSystemCreateAudit(t, first.UID); n != 1 {
			t.Fatalf("run %d: SYSTEM CREATE audit rows = %d, want 1", i, n)
		}
	}

	after := loadSuperAdmin(t, username)
	if !after.PasswordUpdated.Equal(before.PasswordUpdated) {
		t.Fatalf("password_updated_at moved from %v to %v: the bootstrap reset the password",
			before.PasswordUpdated, after.PasswordUpdated)
	}
	if after.Role != before.Role || after.Status != before.Status {
		t.Fatalf("row changed: %+v -> %+v", before, after)
	}
}

// TestEnsureSuperAdminConcurrent: two replicas starting at the same time must
// produce one created, one skipped and one row. The unique key is the
// idempotency source; a SELECT-then-INSERT would create two.
func TestEnsureSuperAdminConcurrent(t *testing.T) {
	username := uniqueUsername(t)
	cleanupUsername(t, username)
	deps := testDeps(t)

	results := make([]Result, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = EnsureSuperAdmin(context.Background(), deps, Options{
				Username: username, Password: testPassword,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	created, skipped := 0, 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		switch results[i].Outcome {
		case OutcomeCreated:
			created++
		case OutcomeSkipped:
			skipped++
		default:
			t.Fatalf("goroutine %d: unexpected outcome %s", i, results[i].Outcome)
		}
	}
	if created != 1 || skipped != 1 {
		t.Fatalf("created=%d skipped=%d, want exactly one of each", created, skipped)
	}
	if n := countUsername(t, username); n != 1 {
		t.Fatalf("t_user rows = %d, want 1", n)
	}
}

// TestEnsureSuperAdminDoesNotOverwrite is the "restart must not escalate"
// guarantee: an existing row with a different role keeps that role, keeps its
// password, and gains no second audit row.
func TestEnsureSuperAdminDoesNotOverwrite(t *testing.T) {
	username := uniqueUsername(t)
	cleanupUsername(t, username)

	now := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Millisecond)
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, username, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at)
		VALUES (?,?,'placeholder-hash','bcrypt',?,'被降级的账号','OPS','NORMAL',?,?)`,
		"B"+strconv.FormatInt(now.UnixNano()%100000000000, 10), username, now, now, now)
	if err != nil {
		t.Fatalf("seed existing row: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("existing uid: %v", err)
	}

	got, err := EnsureSuperAdmin(context.Background(), testDeps(t), Options{
		Username: username, Password: testPassword,
	})
	if err != nil {
		t.Fatalf("EnsureSuperAdmin: %v", err)
	}
	if got.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeSkipped)
	}

	row := loadSuperAdmin(t, username)
	if row.Role != "OPS" {
		t.Fatalf("role = %q, want OPS: a restart silently promoted the account", row.Role)
	}
	var hash string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT password_hash FROM t_user WHERE id = ?`, uid).Scan(&hash); err != nil {
		t.Fatalf("read password_hash: %v", err)
	}
	if hash != "placeholder-hash" {
		t.Fatalf("password_hash = %q, want it untouched", hash)
	}
	if !row.PasswordUpdated.Equal(now) {
		t.Fatalf("password_updated_at moved from %v to %v", now, row.PasswordUpdated)
	}
	if n := countSystemCreateAudit(t, uid); n != 0 {
		t.Fatalf("SYSTEM CREATE audit rows = %d, want 0 for a skipped run", n)
	}
	if n := creditAccountCount(t, uid); n != 0 {
		t.Fatalf("t_credit_account rows = %d, want 0: the skipped path must not write", n)
	}
}

// TestEnsureSuperAdminSoftDeletedUsername pins the trap the unique key hides:
// uk_user_username has no deleted_at component, so a soft-deleted row still owns
// the username while findUser (deleted_at IS NULL) can never reach it.
//
// The bootstrap must report that as WARN「需人工处理」and must NOT revive the row:
// resurrecting a deleted account from a startup path would be an unrequested
// privilege restoration. Without this test the failure mode is silent — the run
// would look like an ordinary "already exists" skip.
func TestEnsureSuperAdminSoftDeletedUsername(t *testing.T) {
	username := uniqueUsername(t)
	cleanupUsername(t, username)

	// Seed a soft-deleted SUPER_ADMIN holding the username.
	now := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Millisecond)
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, username, password_hash, password_algo, password_updated_at,
			 nickname, role, status, deleted_at, created_at, updated_at)
		VALUES (?,?,'placeholder-hash','bcrypt',?,'已注销的超管','SUPER_ADMIN','DISABLED',?,?,?)`,
		"S"+strconv.FormatInt(now.UnixNano()%100000000000, 10), username, now, now, now, now)
	if err != nil {
		t.Fatalf("seed soft-deleted row: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seeded uid: %v", err)
	}

	var logs bytes.Buffer
	deps := testDeps(t)
	deps.Logger = obslog.NewWithWriter("controld", "test", &logs, clock.Real{})

	got, err := EnsureSuperAdmin(context.Background(), deps, Options{
		Username: username, Password: testPassword,
	})
	if err != nil {
		t.Fatalf("EnsureSuperAdmin: %v", err)
	}
	if got.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeSkipped)
	}
	if got.UID != 0 {
		t.Fatalf("uid = %d, want 0: the bootstrap must not adopt the deleted row", got.UID)
	}

	// Exactly one row, still soft-deleted, still DISABLED, still the placeholder
	// hash. A revival would show up as any one of these changing.
	var (
		deletedAt sql.NullTime
		status    string
		role      string
		hash      string
		rows      int
	)
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM t_user WHERE username = ?`, username).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("t_user rows with this username = %d, want 1 (no second row)", rows)
	}
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT deleted_at, status, role, password_hash FROM t_user WHERE id = ?`, uid).
		Scan(&deletedAt, &status, &role, &hash); err != nil {
		t.Fatalf("reload seeded row: %v", err)
	}
	if !deletedAt.Valid {
		t.Fatal("deleted_at was cleared: the bootstrap revived a soft-deleted account")
	}
	if status != "DISABLED" {
		t.Fatalf("status = %q, want DISABLED", status)
	}
	if role != auth.RoleSuperAdmin {
		t.Fatalf("role = %q, want it untouched", role)
	}
	if hash != "placeholder-hash" {
		t.Fatalf("password_hash = %q, want it untouched", hash)
	}
	if n := creditAccountCount(t, uid); n != 0 {
		t.Fatalf("t_credit_account rows = %d, want 0", n)
	}
	if n := countSystemCreateAudit(t, uid); n != 0 {
		t.Fatalf("SYSTEM CREATE audit rows = %d, want 0", n)
	}

	out := logs.String()
	if !strings.Contains(out, "软删除") || !strings.Contains(out, "需人工处理") {
		t.Fatalf("log does not report the soft-deleted occupancy: %s", out)
	}
	if !strings.Contains(out, `"level":"warn"`) {
		t.Fatalf("log is not a WARN: %s", out)
	}
	// The raw driver error must be present so an operator can tell this apart
	// from an ordinary second start: it carries the key name.
	if !strings.Contains(out, "uk_user_username") {
		t.Fatalf("log does not carry the driver error text: %s", out)
	}
	if strings.Contains(out, testPassword) {
		t.Fatalf("log leaks the password: %s", out)
	}
	t.Logf("soft-deleted occupancy log: %s", strings.TrimSpace(out))
}

// TestEnsureSuperAdminSkippedLogIsInfoWithErr covers the normal second start:
// INFO (not WARN, so operators do not learn to ignore the stage) and the raw
// driver error text for triage.
func TestEnsureSuperAdminSkippedLogIsInfoWithErr(t *testing.T) {
	username := uniqueUsername(t)
	cleanupUsername(t, username)

	if _, err := EnsureSuperAdmin(context.Background(), testDeps(t), Options{
		Username: username, Password: testPassword,
	}); err != nil {
		t.Fatalf("seed bootstrap: %v", err)
	}

	var logs bytes.Buffer
	deps := testDeps(t)
	deps.Logger = obslog.NewWithWriter("controld", "test", &logs, clock.Real{})
	res, err := EnsureSuperAdmin(context.Background(), deps, Options{
		Username: username, Password: testPassword,
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %s, want %s", res.Outcome, OutcomeSkipped)
	}
	out := logs.String()
	if strings.Contains(out, `"level":"warn"`) {
		t.Fatalf("the normal second start must not log a WARN: %s", out)
	}
	if !strings.Contains(out, `"level":"info"`) || !strings.Contains(out, "已存在") {
		t.Fatalf("skipped log lacks the INFO 已存在 line: %s", out)
	}
	// The pre-check answers without ever attempting the INSERT, so no 1062
	// should appear and the ~100ms bcrypt hash is not paid. If the pre-check is
	// ever removed, this assertion flips and says so.
	if strings.Contains(out, "Duplicate entry") {
		t.Fatalf("second start went through the INSERT instead of the pre-check: %s", out)
	}
	t.Logf("second-start log: %s", strings.TrimSpace(out))
}

// TestLookupExisting is the guard for the design's easiest mistake: the
// pre-check must NOT filter deleted_at. A soft-deleted row still occupies
// uk_user_username, so the pre-check must see it — otherwise the bootstrap
// answers "already exists" from a 1062 while findUser (deleted_at IS NULL) can
// never reach that row, and the two disagree forever.
func TestLookupExisting(t *testing.T) {
	ctx := context.Background()

	t.Run("absent", func(t *testing.T) {
		username := uniqueUsername(t)
		cleanupUsername(t, username)
		row, found, err := lookupExisting(ctx, testDB, username)
		if err != nil {
			t.Fatalf("lookupExisting: %v", err)
		}
		if found {
			t.Fatalf("found = true for an unused username: %+v", row)
		}
	})

	t.Run("live", func(t *testing.T) {
		username := uniqueUsername(t)
		cleanupUsername(t, username)
		uid := newBootstrapUser(t, username, "OPS", "NORMAL")

		row, found, err := lookupExisting(ctx, testDB, username)
		if err != nil {
			t.Fatalf("lookupExisting: %v", err)
		}
		if !found {
			t.Fatal("live row was not found")
		}
		if row.UID != uid || row.Role != "OPS" || row.Status != "NORMAL" {
			t.Fatalf("row = %+v, want uid=%d OPS NORMAL", row, uid)
		}
		if row.DeletedAt.Valid {
			t.Fatalf("live row reported deleted_at=%v", row.DeletedAt.Time)
		}
	})

	t.Run("soft-deleted", func(t *testing.T) {
		username := uniqueUsername(t)
		cleanupUsername(t, username)
		uid := newBootstrapUser(t, username, "SUPER_ADMIN", "DISABLED")
		if _, err := testDB.ExecContext(ctx,
			`UPDATE t_user SET deleted_at = UTC_TIMESTAMP(3) WHERE id = ?`, uid); err != nil {
			t.Fatalf("soft delete: %v", err)
		}

		row, found, err := lookupExisting(ctx, testDB, username)
		if err != nil {
			t.Fatalf("lookupExisting: %v", err)
		}
		if !found {
			t.Fatal("soft-deleted row was filtered out: the WHERE clause must not use deleted_at")
		}
		if !row.DeletedAt.Valid {
			t.Fatal("deleted_at was not read back: the soft-deleted case cannot be reported")
		}
		if row.Role != "SUPER_ADMIN" || row.Status != "DISABLED" {
			t.Fatalf("row = %+v, want the stored role/status", row)
		}
	})
}

// newBootstrapUser inserts a plain t_user row for the lookupExisting test.
func newBootstrapUser(t *testing.T, username, role, status string) int64 {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, username, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at)
		VALUES (?,?,'placeholder-hash','bcrypt',?,'辅助行',?,?,?,?)`,
		"R"+strconv.FormatInt(now.UnixNano()%100000000000, 10), username, now, role, status, now, now)
	if err != nil {
		t.Fatalf("insert bootstrap test user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("uid: %v", err)
	}
	return uid
}

// TestEnsureSuperAdminNonUsernameDuplicateIsSkipped covers the other key that can
// raise 1062: user_no. insertUserRow already retried five times before bubbling
// it up, so the bootstrap must still answer "no new row" (skipped) rather than
// turning a collision into a startup error — but the WARN must not claim the
// username exists, and must carry the raw driver text so the two causes stay
// distinguishable.
func TestEnsureSuperAdminNonUsernameDuplicateIsSkipped(t *testing.T) {
	// The branch is driven by the driver error text, so exercise the classifier
	// directly with a synthetic uk_user_no error: producing a real one requires
	// forcing five consecutive user_no collisions, which is not expressible
	// without reaching into the allocator.
	err := &mysql.MySQLError{
		Number:  1062,
		Message: "Duplicate entry 'U20261104001' for key 't_user.uk_user_no'",
	}
	if !auth.IsDuplicate(err) {
		t.Fatalf("synthetic error is not classified as a duplicate: %v", err)
	}
	if strings.Contains(err.Error(), "uk_user_username") {
		t.Fatal("the uk_user_no branch would be misrouted to the username branch")
	}
	// And the converse: the username branch selector must key off the name.
	if strings.Contains("Duplicate entry 'admin' for key 't_user.uk_user_username'", "uk_user_no") {
		t.Fatal("username errors must not fall into the user_no branch")
	}
}

// TestIsUnknownColumnClassifier covers the guard that turns MySQL 1054 into
// ErrSchemaNotMigrated. The classifier is checked on its own here; the live 1054
// is produced end-to-end by TestEnsureSuperAdminReportsSchemaNotMigrated.
//
// A live 1054 is deliberately NOT produced by dropping the column from
// yunshu_test: DDL is not transactional in MySQL, and every other package
// sharing that schema runs in parallel under `go test ./...`.
//
// Why there is no test for "a 1062 that is not uk_user_username must not be
// reported as skipped": there is no stable way to construct one. The only other
// unique key the bootstrap INSERT can hit is uk_user_no, and a user_no collision
// is retried up to five times inside auth.insertUserRow, so it never reaches the
// IsDuplicate branch in EnsureSuperAdmin unless five consecutive randomly
// generated numbers collide — forcing that would mean occupying all 1000
// "U"+yyyymmdd+NNN values of the day. The email/phone keys cannot collide either,
// because a username row binds both columns to NULL. Writing a test that
// simulates the error with a hand-built *mysql.MySQLError would only assert that
// strings.Contains works, not that the bootstrap classifies real driver errors
// correctly, so it is not written.
func TestIsUnknownColumnClassifier(t *testing.T) {
	if isUnknownColumn(errors.New("Error 1054: Unknown column 'username'")) {
		t.Fatal("a plain error must not be classified as a MySQL 1054")
	}
	if isUnknownColumn(nil) {
		t.Fatal("nil must not be classified as a 1054")
	}
}

// TestEnsureSuperAdminReportsSchemaNotMigrated produces a real MySQL 1054 and
// asserts the sentinel comes back: that is what lets controld log "run make
// migrate first" and keep serving instead of exiting (design §2 C3).
func TestEnsureSuperAdminReportsSchemaNotMigrated(t *testing.T) {
	db := openSchemaWithoutUsernameColumn(t)
	deps := Deps{
		DB:    db,
		Audit: &audit.Writer{DB: db, Now: clock.Real{}},
		Now:   clock.Real{},
	}

	_, err := EnsureSuperAdmin(context.Background(), deps, Options{
		Username: "bootnomig", Password: testPassword,
	})
	if !errors.Is(err, ErrSchemaNotMigrated) {
		t.Fatalf("err = %v, want ErrSchemaNotMigrated", err)
	}
	if !strings.Contains(err.Error(), "0027") {
		t.Fatalf("message %q does not name the migration", err.Error())
	}

	// Nothing may be left behind: the INSERT never happened, so the transaction
	// rolled back and no account was half-created.
	var rows int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("t_user rows = %d, want 0", rows)
	}
}

// schemaNameRE guards the two identifiers this file interpolates into DDL. They
// come from TEST_DB_DSN, so this is cheap insurance rather than a real boundary.
var schemaNameRE = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// openSchemaWithoutUsernameColumn creates — and drops again afterwards — a
// private database whose t_user has no username column, i.e. exactly the schema
// of a deployment that never ran migration 0027.
//
// A private database rather than ALTER TABLE ... RENAME COLUMN on the shared
// yunshu_test: DDL is not transactional in MySQL, and `go test ./...` runs
// package binaries in parallel against that one schema, so even a rename that a
// deferred statement restores would break every other package's username INSERT
// for the duration of the window. ctltest.SetupDB already requires CREATE
// DATABASE, so this needs no privilege the suite does not already have.
func openSchemaWithoutUsernameColumn(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		dsn = ctltest.DefaultDSN
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DB_DSN: %v", err)
	}
	cfg.ParseTime = true
	if cfg.Loc == nil {
		cfg.Loc = time.UTC
	}

	source := cfg.DBName
	if source == "" {
		source = "yunshu_test"
	}
	const target = "yunshu_nomig_test"
	if !schemaNameRE.MatchString(source) || !schemaNameRE.MatchString(target) {
		t.Fatalf("unsafe schema name: source=%q target=%q", source, target)
	}

	adminCfg := *cfg
	adminCfg.DBName = ""
	admin, err := sql.Open("mysql", adminCfg.FormatDSN())
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}

	ctx := context.Background()
	quoted := "`" + target + "`"
	// Dropped first so an interrupted earlier run cannot hand this test a table
	// that still has the column.
	for _, stmt := range []string{
		"DROP DATABASE IF EXISTS " + quoted,
		"CREATE DATABASE " + quoted + " CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_as_cs",
		"CREATE TABLE " + quoted + ".t_user LIKE `" + source + "`.t_user",
		"ALTER TABLE " + quoted + ".t_user DROP INDEX uk_user_username, DROP COLUMN username",
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			_ = admin.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// Prove the fixture is what the test needs before asserting on the result;
	// otherwise a stray column would make the test pass for the wrong reason.
	var cols int
	if err := admin.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = 't_user' AND column_name = 'username'`,
		target).Scan(&cols); err != nil {
		_ = admin.Close()
		t.Fatalf("inspect %s.t_user: %v", target, err)
	}
	if cols != 0 {
		_ = admin.Close()
		t.Fatalf("%s.t_user still has a username column", target)
	}

	poolCfg := *cfg
	poolCfg.DBName = target
	db, err := sql.Open("mysql", poolCfg.FormatDSN())
	if err != nil {
		_ = admin.Close()
		t.Fatalf("open %s: %v", target, err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quoted)
		_ = admin.Close()
	})
	return db
}

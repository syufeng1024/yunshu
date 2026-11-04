package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"yunshu.dev/core/internal/control/ctltest"
	"yunshu.dev/core/internal/platform/phoneclaim"
)

var testDB *sql.DB

const testKey = "backfill-phone-claim-test-key-32b!!"

func TestMain(m *testing.M) {
	// This package gets its OWN schema.
	//
	// `release` refuses to run while ANY number is missing from the ledger, and
	// that gate is a whole-schema aggregate — by design, because the command
	// operates on the deployment, not on a fixture. In a shared test schema that
	// makes the assertion depend on what every other package happens to have
	// inserted at that instant (auth/fund/phoneclaim fixtures create phone rows
	// before their ledger rows), which is a flake waiting to be reported as a
	// product bug. A private schema keeps the gate global and the test local.
	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		dsn = ctltest.DefaultDSN
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill-phone-claim tests: parse TEST_DB_DSN: %v\n", err)
		os.Exit(1)
	}
	cfg.DBName = "yunshu_test_backfill_claim"
	_ = os.Setenv("TEST_DB_DSN", cfg.FormatDSN())

	db, err := ctltest.SetupDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill-phone-claim tests: %v\n", err)
		os.Exit(1)
	}
	testDB = db
	code := m.Run()
	_ = db.Close()
	os.Exit(code)
}

// env answers the two variables run() reads. TEST_DB_DSN is the same schema the
// rest of the suite uses, so the command is exercised against real SQL.
func env(extra map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := extra[k]; ok {
			return v
		}
		if k == "DB_DSN" {
			if dsn := os.Getenv("TEST_DB_DSN"); dsn != "" {
				return dsn
			}
			return ctltest.DefaultDSN
		}
		return ""
	}
}

type cmdUser struct {
	uid  int64
	hmac string
}

func insertCancelUser(t *testing.T, phone string, withClaim bool) cmdUser {
	t.Helper()
	ctx := context.Background()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random hmac: %v", err)
	}
	hmac := hex.EncodeToString(b[:])
	now := time.Now().UTC()
	userNo := fmt.Sprintf("B%011d", now.UnixNano()%100000000000)
	res, err := testDB.ExecContext(ctx, `
		INSERT INTO t_user
			(user_no, phone, phone_hmac, password_hash, password_algo, password_updated_at,
			 nickname, role, status, disable_source, disable_reason, cancel_apply_at,
			 disabled_at, created_at, updated_at)
		VALUES (?,?,?,'x','bcrypt',?,'cli','PERSONAL','DISABLED','SELF_CANCEL','用户主动注销',?,?,?,?)`,
		userNo, phone, hmac, now, now, now, now, now)
	if err != nil {
		t.Fatalf("insert cancel user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("id: %v", err)
	}
	if withClaim {
		if _, err := testDB.ExecContext(ctx, `
			INSERT INTO t_phone_claim
			  (phone_hmac, first_user_id, last_claim_user_id, last_claim_at, cancel_count, created_at, updated_at)
			VALUES (?,?,?,?,0,?,?)`, hmac, uid, uid, now, now, now); err != nil {
			t.Fatalf("seed claim: %v", err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = testDB.ExecContext(c, `DELETE FROM t_phone_claim WHERE phone_hmac = ?`, hmac)
		_, _ = testDB.ExecContext(c, `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return cmdUser{uid: uid, hmac: hmac}
}

func mustRun(t *testing.T, args []string, getenv func(string) string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, getenv, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunRejectsBadInvocations(t *testing.T) {
	cases := [][]string{
		{},
		{"nonsense"},
		{"backfill", "--commit", "--dry-run"},
	}
	for _, args := range cases {
		if code, _, _ := mustRun(t, args, env(nil)); code != phoneclaim.ExitRuntime {
			t.Fatalf("args %v exit=%d, want %d", args, code, phoneclaim.ExitRuntime)
		}
	}
	// A missing DSN is a runtime error, not a silent no-op.
	if code, _, _ := mustRun(t, []string{"detect"}, func(string) string { return "" }); code != phoneclaim.ExitRuntime {
		t.Fatalf("detect without DB_DSN exit=%d, want %d", code, phoneclaim.ExitRuntime)
	}
}

func TestDetectJSONIsParseable(t *testing.T) {
	insertCancelUser(t, "8613800800001", false)
	code, stdout, stderr := mustRun(t, []string{"detect", "--json"}, env(nil))
	if code != phoneclaim.ExitOK && code != phoneclaim.ExitBlocking {
		t.Fatalf("detect --json exit=%d stderr=%s", code, stderr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &report); err != nil {
		t.Fatalf("detect --json is not JSON: %v (%q)", err, stdout)
	}
	if _, ok := report["PendingBackfill"]; !ok {
		t.Fatalf("JSON report has no PendingBackfill field: %v", report)
	}
}

// AC-11 / AC-12 / AC-13 through the CLI: the gate, the release, and idempotency.
func TestReleaseGateBackfillThenRelease(t *testing.T) {
	u := insertCancelUser(t, "8613800800002", false)

	// 1. release before backfill is a scheduling mistake: exit 3, nothing written.
	code, _, stderr := mustRun(t, []string{"release", "--commit"}, env(nil))
	if code != phoneclaim.ExitOrderViolation {
		t.Fatalf("release before backfill exit=%d stderr=%s, want %d",
			code, stderr, phoneclaim.ExitOrderViolation)
	}
	if !strings.Contains(stderr, "backfill") {
		t.Fatalf("stderr=%q, want it to name the missing step", stderr)
	}

	// 2. dry run writes nothing.
	if code, stdout, _ := mustRun(t, []string{"backfill"}, env(nil)); code != phoneclaim.ExitOK {
		t.Fatalf("backfill dry-run exit=%d stdout=%s", code, stdout)
	}
	if _, err := phoneclaim.Get(context.Background(), testDB, u.hmac); err == nil {
		t.Fatal("backfill dry-run wrote a ledger row")
	}

	// 3. commit, then release.
	if code, stdout, stderr := mustRun(t, []string{"backfill", "--commit"}, env(nil)); code != phoneclaim.ExitOK {
		t.Fatalf("backfill --commit exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	code, stdout, stderr := mustRun(t, []string{"release", "--commit"}, env(nil))
	if code != phoneclaim.ExitOK {
		t.Fatalf("release --commit exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "已释放 1") && !strings.Contains(stdout, "已释放") {
		t.Fatalf("release output=%q, want a released count", stdout)
	}
	var phone, hmac sql.NullString
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT phone, phone_hmac FROM t_user WHERE id = ?`, u.uid).Scan(&phone, &hmac); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if phone.Valid || hmac.Valid {
		t.Fatalf("uid %d still holds phone=%v hmac=%v after release", u.uid, phone, hmac)
	}
	claim, err := phoneclaim.Get(context.Background(), testDB, u.hmac)
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if claim.CancelCount != 1 {
		t.Fatalf("cancel_count = %d, want 1", claim.CancelCount)
	}

	// 4. idempotent: the second run has nothing to do.
	code, stdout, _ = mustRun(t, []string{"release", "--commit"}, env(nil))
	if code != phoneclaim.ExitOK {
		t.Fatalf("second release exit=%d stdout=%s", code, stdout)
	}
	if !strings.Contains(stdout, "已释放 0") {
		t.Fatalf("second release output=%q, want 已释放 0 (idempotent)", stdout)
	}
}

func TestLookupNeedsKeyAndMasksValue(t *testing.T) {
	phone := "8613800800003"
	if code, _, stderr := mustRun(t, []string{"lookup", "--phone=" + phone}, env(nil)); code != phoneclaim.ExitRuntime {
		t.Fatalf("lookup without key exit=%d stderr=%s, want %d", code, stderr, phoneclaim.ExitRuntime)
	}
	code, stdout, stderr := mustRun(t, []string{"lookup", "--phone=" + phone},
		env(map[string]string{"PHONE_HMAC_KEY": testKey}))
	if code != phoneclaim.ExitOK {
		t.Fatalf("lookup exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, phone) {
		t.Fatalf("lookup printed the plaintext number: %q", stdout)
	}
	if !strings.Contains(stdout, "台账") {
		t.Fatalf("lookup output=%q, want the ledger line", stdout)
	}
}

func TestVerifyReportsZeroOnCleanLedger(t *testing.T) {
	code, stdout, stderr := mustRun(t, []string{"verify"}, env(nil))
	if code != phoneclaim.ExitOK && code != phoneclaim.ExitBlocking {
		t.Fatalf("verify exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "四条断言") {
		t.Fatalf("verify output=%q, want the four assertions", stdout)
	}
}

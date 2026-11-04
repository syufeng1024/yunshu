package ops

// QA additions for POST /api/admin/users with a *phone* identifier.
//
// The unique key on a phone number is uk_user_phone_hmac (t_user.phone has no
// unique key since migration 0011), so operator provisioning is a write path
// that must fill phone_hmac or it will mint duplicate accounts that no unique
// key can see. None of the pre-existing tests in this package used a phone
// identifier, so these three cases were the gap.

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"

	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/cryptobox"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/env"
)

// opsPhoneKey is ≥32 bytes and distinct from every other fixture key, so a hash
// collision with another package's rows is impossible.
const opsPhoneKey = "qa-ops-operator-phone-hmac-key-0001"

// qaOperatorPhone is fixed, because phone_hmac is asserted against a value
// computed outside Go (python hmac / openssl dgst -sha256 -hmac):
//
//	printf '8613800138000' | openssl dgst -sha256 -hmac 'qa-ops-operator-phone-hmac-key-0001'
//
// An independent implementation is the point: comparing the column with
// auth.PhoneHMAC would only prove the process is self-consistent, not that the
// stored bytes are HMAC-SHA256 of the canonical number with this key.
const (
	qaOperatorPhone = "8613800138000"
	// qaOperatorPhoneHMAC is the golden value from the command above.
	qaOperatorPhoneHMAC = "12d29d3e50e407fcbcee3bb9d3923957bfd1477f8661f5d9db66401d751ec461"
)

func uniqueOperatorPhone(t *testing.T) string {
	t.Helper()
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random: %v", err)
	}
	var digits strings.Builder
	digits.WriteString("862")
	for _, v := range b {
		digits.WriteByte('0' + v%10)
	}
	return digits.String()
}

// A2: a phone account provisioned by an operator gets phone_hmac, and a second
// account on the same number answers 409 account_exists — not 500, and not a
// second row.
func TestQAOperatorPhoneWritesHMACAndRejectsDuplicate(t *testing.T) {
	svc := newTestService(t)
	svc.PhoneHMACKey = opsPhoneKey
	adminUID := newTestUser(t, "SUPER_ADMIN")
	phone := uniqueOperatorPhone(t)

	first := createOperator(t, svc, adminUID, map[string]any{
		"identifier": phone,
		"password":   operatorTestPassword,
		"nickname":   "手机号运营",
		"role":       "OPS",
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("create phone operator status=%d body=%s, want 201",
			first.Code, first.Body.String())
	}
	var detail userDetailView
	decodeBody(t, first, &detail)
	if detail.User.Phone != phone {
		t.Fatalf("response phone = %q, want %q (plaintext must still be written and shown)",
			detail.User.Phone, phone)
	}
	var stored string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone_hmac, '') FROM t_user WHERE id = ?`, detail.User.ID).Scan(&stored); err != nil {
		t.Fatalf("read phone_hmac: %v", err)
	}
	if len(stored) != 64 {
		t.Fatalf("phone_hmac = %q (len %d), want 64 hex chars", stored, len(stored))
	}

	// Same number written with the formatting a human pastes in ("+86 138-0013-
	// 8000" shape): the normalizer canonicalises it, so it hashes to the same
	// value and the unique key refuses it. This is what proves the hash is taken
	// over the canonical form and not over the raw input.
	pasted := "+" + phone
	dup := createOperator(t, svc, adminUID, map[string]any{
		"identifier": pasted,
		"password":   operatorTestPassword,
		"nickname":   "重复手机号",
		"role":       "OPS",
	})
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate phone (as %q) status=%d body=%s, want 409",
			pasted, dup.Code, dup.Body.String())
	}
	if got := errorBody(t, dup).Error.Code; got != "account_exists" {
		t.Fatalf("duplicate phone code = %q, want account_exists", got)
	}
	var rows int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE phone = ?`, phone).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows for the number = %d, want 1", rows)
	}
}

// The stored hash must be HMAC-SHA256(key, canonical number) — verified against
// a value computed outside this codebase.
func TestQAOperatorPhoneHMACMatchesExternalReference(t *testing.T) {
	if qaOperatorPhoneHMAC == "TODO" {
		t.Skip("golden value not filled in")
	}
	svc := newTestService(t)
	svc.PhoneHMACKey = opsPhoneKey
	adminUID := newTestUser(t, "SUPER_ADMIN")

	// Take the number out of circulation first: another run may have left it.
	if _, err := testDB.ExecContext(context.Background(),
		`DELETE FROM t_user WHERE phone = ?`, qaOperatorPhone); err != nil {
		t.Fatalf("clear number: %v", err)
	}
	rec := createOperator(t, svc, adminUID, map[string]any{
		"identifier": qaOperatorPhone,
		"password":   operatorTestPassword,
		"nickname":   "golden",
		"role":       "OPS",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var detail userDetailView
	decodeBody(t, rec, &detail)
	var stored string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone_hmac, '') FROM t_user WHERE id = ?`, detail.User.ID).Scan(&stored); err != nil {
		t.Fatalf("read phone_hmac: %v", err)
	}
	if stored != qaOperatorPhoneHMAC {
		t.Fatalf("phone_hmac = %s, want %s (openssl/python reference)", stored, qaOperatorPhoneHMAC)
	}
}

// B5, admin half: with no PHONE_HMAC_KEY a phone account cannot be created
// (503 config_unavailable, nothing written), while an e-mail account still can.
func TestQAOperatorPhoneWithoutKeyAnswers503(t *testing.T) {
	svc := newTestService(t)
	svc.PhoneHMACKey = ""
	adminUID := newTestUser(t, "SUPER_ADMIN")
	phone := uniqueOperatorPhone(t)

	rec := createOperator(t, svc, adminUID, map[string]any{
		"identifier": phone,
		"password":   operatorTestPassword,
		"nickname":   "无 key",
		"role":       "OPS",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create phone operator without key status=%d body=%s, want 503",
			rec.Code, rec.Body.String())
	}
	if got := errorBody(t, rec).Error.Code; got != "config_unavailable" {
		t.Fatalf("code = %q, want config_unavailable", got)
	}
	var rows int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE phone = ?`, phone).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a 503 provisioning wrote %d rows", rows)
	}

	email := uniqueOperatorIdentifier()
	ok := createOperator(t, svc, adminUID, map[string]any{
		"identifier": email,
		"password":   operatorTestPassword,
		"nickname":   "无 key 邮箱",
		"role":       "OPS",
	})
	if ok.Code != http.StatusCreated {
		t.Fatalf("e-mail provisioning without the phone key status=%d body=%s, want 201",
			ok.Code, ok.Body.String())
	}
}

// P3-1 regression (targeted re-check): a duplicate phone in POST /api/admin/users
// answers 409, and the log record for it must be
//
//	level=warn   — a 409 is a normal operator mistake, not a server fault
//	status=0     — NOT 500: logError stamps 500 on every call, which turned each
//	               duplicate into a phantom 5xx on the dashboard
//	key name only — the driver message carries the duplicated *value*, and for
//	               uk_user_phone_hmac that value is the hmac
//
// A real insert failure on the same endpoint must still be level=error/status=500,
// otherwise the fix would have silenced the noise and the signal together.
func TestQAOperatorDuplicateLogsWarnNotError(t *testing.T) {
	svc, logs := newTestServiceWithLogCapture(t)
	svc.PhoneHMACKey = opsPhoneKey
	adminUID := newTestUser(t, "SUPER_ADMIN")
	phone := uniqueOperatorPhone(t)

	first := createOperator(t, svc, adminUID, map[string]any{
		"identifier": phone, "password": operatorTestPassword, "nickname": "log", "role": "OPS",
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status=%d body=%s, want 201", first.Code, first.Body.String())
	}
	var detail userDetailView
	decodeBody(t, first, &detail)
	var stored string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone_hmac, '') FROM t_user WHERE id = ?`, detail.User.ID).Scan(&stored); err != nil {
		t.Fatalf("read phone_hmac: %v", err)
	}

	dup := createOperator(t, svc, adminUID, map[string]any{
		"identifier": phone, "password": operatorTestPassword, "nickname": "log", "role": "OPS",
	})
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s, want 409", dup.Code, dup.Body.String())
	}

	line := logs.lineFor(t, "ops_create_operator_dup_identifier")
	if line == "" {
		t.Fatalf("the duplicate was not logged at all:/n%s", logs.String())
	}
	for _, bad := range []string{`"level":"error"`, `"status":500`, stored, "Duplicate entry"} {
		if strings.Contains(line, bad) {
			t.Fatalf("the 409 log line contains %q: %s", bad, line)
		}
	}
	if !strings.Contains(line, `"level":"warn"`) {
		t.Fatalf("the 409 was not logged at warn: %s", line)
	}
	if !strings.Contains(line, "uk_user_phone_hmac") {
		t.Fatalf("the 409 log line does not name the unique key: %s", line)
	}

	// The missing-key branch is the same class: 503, logged at warn, no value.
	svc.PhoneHMACKey = ""
	if rec := createOperator(t, svc, adminUID, map[string]any{
		"identifier": uniqueOperatorPhone(t), "password": operatorTestPassword,
		"nickname": "log", "role": "OPS",
	}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-key create status=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
	line = logs.lineFor(t, "ops_create_operator_phone_hmac_unavailable")
	if line == "" {
		t.Fatalf("the 503 was not logged:/n%s", logs.String())
	}
	for _, bad := range []string{`"level":"error"`, `"status":500`} {
		if strings.Contains(line, bad) {
			t.Fatalf("the 503 log line contains %q: %s", bad, line)
		}
	}
	if !strings.Contains(line, `"level":"warn"`) || !strings.Contains(line, "status=503") {
		t.Fatalf("the 503 log line is not a warn that names 503: %s", line)
	}
}

// logCapture holds a Service's log output so a test can assert on one record.
type logCapture struct{ buf *bytes.Buffer }

func (l logCapture) String() string { return l.buf.String() }

// lineFor returns the first log line whose msg contains marker, or "".
func (l logCapture) lineFor(t *testing.T, marker string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	return ""
}

// newTestServiceWithLogCapture is newTestService plus a logger writing into a
// buffer, so the *severity* of a record can be asserted rather than inferred
// from the response code.
func newTestServiceWithLogCapture(t *testing.T) (*Service, logCapture) {
	t.Helper()
	box, err := cryptobox.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("cryptobox: %v", err)
	}
	buf := &bytes.Buffer{}
	logger := obslog.NewWithWriter(env.ServiceControl, "dev", buf, clock.Real{})
	svc := New(testDB, &audit.Writer{DB: testDB, Now: clock.Real{}}, clock.Real{}, logger, nil, box)
	return svc, logCapture{buf: buf}
}

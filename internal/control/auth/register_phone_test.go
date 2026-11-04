package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/apierr"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// phoneFixtureKey is the PHONE_HMAC_KEY the registration tests run with. 32+
// bytes, so the production length rule is satisfied by construction.
const phoneFixtureKey = "t11-register-phone-hmac-key-32byte"

// uniquePhone builds a distinct E.164 number per call. The unique key
// uk_user_phone_hmac makes a fixed number unusable across runs: a row left
// behind by a failed earlier run would make the next run answer 409 instead of
// testing what it names.
func uniqueRegisterPhone(t *testing.T) string {
	t.Helper()
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random phone: %v", err)
	}
	// Digits only: the phone predicate rejects anything else, and a hex string
	// would contain letters. "86" + "1" + ten digits is a 13-digit E.164 shape.
	var digits strings.Builder
	for _, v := range b {
		digits.WriteByte('0' + v%10)
	}
	return "861" + digits.String()
}

// captureSMSSender records the last text message so a test can read the code the
// way a user would.
type captureSMSSender struct {
	to, body string
	sends    int
}

func (s *captureSMSSender) Send(_ context.Context, to, body string) error {
	s.sends++
	s.to, s.body = to, body
	return nil
}

// lastCode pulls the six digits out of the message body.
func (s *captureSMSSender) lastCode(t *testing.T) string {
	t.Helper()
	if s.body == "" {
		t.Fatal("no SMS was sent")
	}
	found := sixDigits.FindStringSubmatch(s.body)
	if found == nil {
		t.Fatalf("SMS body carries no six-digit code: %q", s.body)
	}
	return found[1]
}

// phoneService returns a Service wired for phone registration: PHONE_HMAC_KEY
// present, codes issued and captured instead of e-mailed/texted.
func phoneService(t *testing.T) (*Service, *captureSMSSender) {
	t.Helper()
	svc, _, _ := newAuthService(t)
	svc.PhoneHMACKey = phoneFixtureKey
	svc.VerificationMode = VerificationLog
	svc.SMSMode = SMSLog
	sms := &captureSMSSender{}
	svc.SMSSender = sms
	return svc, sms
}

func cleanupPhone(t *testing.T, phone string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_user WHERE phone = ?`, phone)
	})
}

// T8 (red line): registering a phone number writes phone_hmac, and registering
// the same number a second time answers 409 account_exists with exactly one row
// in the table. Before this increment the second call succeeded and produced a
// duplicate account, because uk_user_phone_hmac never saw a value.
func TestRegisterPhoneWritesHMACAndRejectsDuplicate(t *testing.T) {
	svc, _ := phoneService(t)
	// Verification off: this test is about the INSERT funnel and the unique key,
	// not about codes. With codes on, the second attempt cannot even obtain one
	// (POST /verification-code answers 409 for an existing address), so the
	// duplicate would be refused one step earlier for a different reason.
	svc.VerificationMode = VerificationOff
	phone := uniqueRegisterPhone(t)
	cleanupPhone(t, phone)

	body := map[string]string{"identifier": phone, "password": "Str0ng-Passw0rd!"}
	rec := callRegister(t, svc, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register phone status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}

	var stored string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone_hmac, '') FROM t_user WHERE phone = ?`, phone).Scan(&stored); err != nil {
		t.Fatalf("read phone_hmac: %v", err)
	}
	want, err := PhoneHMAC(phoneFixtureKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if stored != want {
		t.Fatalf("phone_hmac = %q, want %q", stored, want)
	}
	if len(stored) != 64 {
		t.Fatalf("phone_hmac length = %d, want 64", len(stored))
	}

	// Same number, same form: a state conflict, not a new account.
	dup := callRegister(t, svc, body)
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate phone status=%d body=%s, want 409", dup.Code, dup.Body.String())
	}
	if eb := decodeError(t, dup); eb.Error.Code != apierr.CodeAccountExists {
		t.Fatalf("duplicate phone code=%q, want %s", eb.Error.Code, apierr.CodeAccountExists)
	}
	var rows int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE phone = ?`, phone).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows for %s = %d, want 1 (the duplicate must not be written)", phone, rows)
	}
}

// T10 (red line): with no PHONE_HMAC_KEY the process keeps working —
//
//   - phone registration answers 503 config_unavailable and writes nothing,
//   - the verification code is NOT consumed, because the refusal happens before
//     consumeVerificationCode (the same code registers once the key is back),
//   - e-mail registration and phone login are untouched.
func TestRegisterPhoneWithoutKeyAnswers503AndKeepsCode(t *testing.T) {
	svc, sms := phoneService(t)
	phone := uniqueRegisterPhone(t)
	cleanupPhone(t, phone)

	if rec := callSendCode(t, svc, phone, PurposeRegister); rec.Code != http.StatusOK {
		t.Fatalf("send code status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := sms.lastCode(t)

	svc.PhoneHMACKey = "" // a deployment without PHONE_HMAC_KEY
	rec := callRegister(t, svc, map[string]string{
		"identifier": phone, "password": "Str0ng-Passw0rd!", "code": code,
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("register without key status=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
	if eb := decodeError(t, rec); eb.Error.Code != apierr.CodeConfigUnavailable {
		t.Fatalf("code=%q, want %s", eb.Error.Code, apierr.CodeConfigUnavailable)
	}
	var rows int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE phone = ?`, phone).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a 503 registration wrote %d rows", rows)
	}

	// The code survived: restore the key and reuse the very same code.
	svc.PhoneHMACKey = phoneFixtureKey
	ok := callRegister(t, svc, map[string]string{
		"identifier": phone, "password": "Str0ng-Passw0rd!", "code": code,
	})
	if ok.Code != http.StatusCreated {
		t.Fatalf("the 503 burned the code: status=%d body=%s", ok.Code, ok.Body.String())
	}
}

// T10, second half: a missing phone key must not touch the paths that do not
// write a phone number — e-mail registration keeps answering 201, and phone
// login (which reads the plaintext column) keeps answering 200.
func TestMissingPhoneKeyLeavesEmailAndLoginWorking(t *testing.T) {
	svc, mailer := verifierService(t)
	svc.PhoneHMACKey = ""
	email := uniqueVerifyEmail(t, "nokey-email")

	if rec := callSendCode(t, svc, email, PurposeRegister); rec.Code != http.StatusOK {
		t.Fatalf("send code status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := mailer.lastCode(t)
	rec := callRegister(t, svc, map[string]string{
		"identifier": email, "password": "Str0ng-Passw0rd!", "code": code,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("e-mail registration without phone key status=%d body=%s, want 201",
			rec.Code, rec.Body.String())
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_user WHERE email = ?`, email)
	})

	// An existing phone account logs in while the key is absent: login never
	// hashes, so it cannot depend on the key.
	phone := uniqueRegisterPhone(t)
	mac, err := PhoneHMAC(phoneFixtureKey, phone)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	insertPhoneUser(t, phone, mac)
	login := callLogin(t, svc, map[string]string{
		"identifier": phone, "password": testPassword,
	}, uniqueIP())
	if login.Code != http.StatusOK {
		t.Fatalf("phone login without phone key status=%d body=%s, want 200",
			login.Code, login.Body.String())
	}
}

// insertPhoneUser seeds a NORMAL account with a real bcrypt hash and a phone
// number (plus its hmac).
func insertPhoneUser(t *testing.T, phone, phoneMAC string) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcryptCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UTC()
	userNo := fmt.Sprintf("Q%011d", now.UnixNano()%100000000000)
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, phone, phone_hmac, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at)
		VALUES (?,?,?,?,'bcrypt',?,'t11-phone','PERSONAL','NORMAL',?,?)`,
		userNo, phone, phoneMAC, string(hash), now, now, now)
	if err != nil {
		t.Fatalf("insert phone user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("phone user id: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_admin_audit_log WHERE operator_id = ? OR target_id = ?`,
			uid, fmt.Sprint(uid))
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_credit_account WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

// T16: the single INSERT funnel refuses a phone row without a hmac, so a future
// sixth write path that forgets it fails loudly instead of writing a row
// uk_user_phone_hmac cannot see.
func TestInsertUserRowRequiresPhoneHMAC(t *testing.T) {
	tx, err := testDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // the transaction is never committed

	ident := Identifier{Kind: KindPhone, Value: uniqueRegisterPhone(t)}
	if _, err := insertUserRow(context.Background(), tx, ident, "", "nick", "",
		"$2a$12$notarealhash", RolePersonal, time.Now().UTC()); err != ErrPhoneHMACRequired {
		t.Fatalf("err = %v, want ErrPhoneHMACRequired", err)
	}

	// An e-mail row with a stray hmac must not occupy the unique key.
	emailIdent := Identifier{Kind: KindEmail, Value: uniqueVerifyEmail(t, "hmacstray")}
	uid, err := insertUserRow(context.Background(), tx, emailIdent, "stray-hmac", "nick", "",
		"$2a$12$notarealhash", RolePersonal, time.Now().UTC())
	if err != nil {
		t.Fatalf("insert e-mail row: %v", err)
	}
	var stored sql.NullString
	if err := tx.QueryRowContext(context.Background(),
		`SELECT phone_hmac FROM t_user WHERE id = ?`, uid).Scan(&stored); err != nil {
		t.Fatalf("read hmac: %v", err)
	}
	if stored.Valid {
		t.Fatalf("an e-mail row stored phone_hmac = %q, want NULL", stored.String)
	}
}

// logCapture wires a real logger into the service and returns the buffer, so a
// test can assert on the *record* an operator's dashboard would see — level and
// status included. Nil Logger is the default in these fixtures, and "nobody logs"
// is exactly the state that let a 409 carry status=500 unnoticed.
func logCapture(t *testing.T, svc *Service) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	svc.Logger = obslog.NewWithWriter("controld", "dev", buf, clock.Real{})
	return buf
}

// A duplicate registration is a normal client state answered with 409. The log
// record must therefore be a WARN with no 5xx status on it: `errorLog` stamps
// status=500 unconditionally, which turns every double-tap into a phantom server
// error on the dashboard (and, with a 5xx alert, into a page).
func TestRegisterDuplicatePhoneLogsWarnWithout5xxStatus(t *testing.T) {
	svc, _ := phoneService(t)
	svc.VerificationMode = VerificationOff
	logs := logCapture(t, svc)
	phone := uniqueRegisterPhone(t)
	cleanupPhone(t, phone)

	body := map[string]string{"identifier": phone, "password": "Str0ng-Passw0rd!"}
	if rec := callRegister(t, svc, body); rec.Code != http.StatusCreated {
		t.Fatalf("first register status=%d body=%s", rec.Code, rec.Body.String())
	}
	dup := callRegister(t, svc, body)
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s, want 409", dup.Code, dup.Body.String())
	}

	out := logs.String()
	if strings.Contains(out, `"status":500`) {
		t.Fatalf("409 logged with status 500: %s", out)
	}
	if strings.Contains(out, `"level":"error"`) {
		t.Fatalf("409 logged at error level: %s", out)
	}
	if !strings.Contains(out, "register_dup_phone_hmac") {
		t.Fatalf("log does not name the phone unique key: %s", out)
	}
	// The hmac itself must not be in the log — the driver message carries it and
	// is deliberately not logged.
	if strings.Contains(out, "Duplicate entry") {
		t.Fatalf("log carries the driver message (and the hmac): %s", out)
	}
	t.Logf("409 log line: %s", strings.TrimSpace(out))
}

// The same rule for the other unique key: same 409, different token, so an
// operator can tell "the e-mail is taken" from "the phone is taken".
func TestRegisterDuplicateEmailLogsItsOwnToken(t *testing.T) {
	svc, _ := phoneService(t)
	svc.VerificationMode = VerificationOff
	logs := logCapture(t, svc)
	email := uniqueVerifyEmail(t, "dux-email")
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_user WHERE email = ?`, email)
	})

	body := map[string]string{"identifier": email, "password": "Str0ng-Passw0rd!"}
	if rec := callRegister(t, svc, body); rec.Code != http.StatusCreated {
		t.Fatalf("first register status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := callRegister(t, svc, body); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	out := logs.String()
	if strings.Contains(out, `"status":500`) {
		t.Fatalf("409 logged with status 500: %s", out)
	}
	if !strings.Contains(out, "register_dup_email") {
		t.Fatalf("log does not name the e-mail unique key: %s", out)
	}
}

// A 503 config_unavailable is also not a 5xx in the "our code broke" sense: the
// record must say warn, not error, or every phone sign-up attempt during a
// misconfigured deploy would page somebody.
func TestRegisterPhoneMissingKeyLogsWarnWithout5xxStatus(t *testing.T) {
	svc, sms := phoneService(t)
	logs := logCapture(t, svc)
	phone := uniqueRegisterPhone(t)
	cleanupPhone(t, phone)

	if rec := callSendCode(t, svc, phone, PurposeRegister); rec.Code != http.StatusOK {
		t.Fatalf("send code status=%d body=%s", rec.Code, rec.Body.String())
	}
	code := sms.lastCode(t)
	svc.PhoneHMACKey = ""
	logs.Reset()
	rec := callRegister(t, svc, map[string]string{
		"identifier": phone, "password": "Str0ng-Passw0rd!", "code": code,
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
	out := logs.String()
	if strings.Contains(out, `"status":500`) {
		t.Fatalf("503 logged with status 500: %s", out)
	}
	if strings.Contains(out, `"level":"error"`) {
		t.Fatalf("503 logged at error level: %s", out)
	}
	if !strings.Contains(out, "register_phone_hmac_unavailable") {
		t.Fatalf("log does not name the missing key path: %s", out)
	}
	t.Logf("503 log line: %s", strings.TrimSpace(out))
}

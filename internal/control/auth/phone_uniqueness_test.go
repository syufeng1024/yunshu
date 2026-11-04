package auth

// QA additions for the phone-HMAC increment. Two things are asserted here that
// no other test in this package does:
//
//  1. the stored bytes are HMAC-SHA256(key, canonical number) according to an
//     implementation *outside* this codebase (openssl / python hmac), not merely
//     self-consistent with auth.PhoneHMAC;
//  2. login and password recovery behave identically for a row whose phone_hmac
//     is NULL (a row the backfill has not reached) and one whose phone_hmac is
//     set (a row it has). That is the "read paths keep using the plaintext
//     column" red line: if either path ever starts filtering on phone_hmac, the
//     un-backfilled half of production stops being able to log in.

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// qaGoldenPhone / qaGoldenKey are fixed so the expected hash can be computed by
// hand:
//
//	printf '8613800138000' | openssl dgst -sha256 -hmac 'qa-golden-phone-hmac-key-0000001'
//
// Both this value and phoneFixtureKey are ≥32 bytes.
const (
	qaGoldenKey   = "qa-golden-phone-hmac-key-0000001"
	qaGoldenPhone = "8613800138000"
	// qaGoldenHMAC is the openssl output for the line above.
	qaGoldenHMAC = "82624d638565956aae46d4e4b6967fef3b6266c1dd66275d9e4dd276426bdb09"
)

// qaRandomPhone returns a fresh 13-digit canonical number.
func qaRandomPhone(t *testing.T) string {
	t.Helper()
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random: %v", err)
	}
	var digits strings.Builder
	digits.WriteString("863")
	for _, v := range b {
		digits.WriteByte('0' + v%10)
	}
	return digits.String()
}

// cleanupPhoneNumber removes every row holding phone (a previous failed run may
// have left one, and uk_user_phone_hmac would then answer 409 to this run).
func cleanupPhoneNumber(t *testing.T, phone string) {
	t.Helper()
	if _, err := testDB.ExecContext(context.Background(),
		`DELETE FROM t_user WHERE phone = ?`, phone); err != nil {
		t.Fatalf("clear phone: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_user WHERE phone = ?`, phone)
	})
}

// A3, independently: the column must contain the hex digest openssl produces.
func TestQARegisterPhoneHMACMatchesExternalReference(t *testing.T) {
	svc, _ := phoneService(t)
	svc.VerificationMode = VerificationOff
	svc.PhoneHMACKey = qaGoldenKey
	cleanupPhoneNumber(t, qaGoldenPhone)

	rec := callRegister(t, svc, map[string]string{
		"identifier": qaGoldenPhone, "password": "Str0ng-Passw0rd!",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var stored, plain string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone_hmac, ''), COALESCE(phone, '') FROM t_user WHERE phone = ?`,
		qaGoldenPhone).Scan(&stored, &plain); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if stored != qaGoldenHMAC {
		t.Fatalf("phone_hmac = %s, want %s (openssl reference: printf %q | openssl dgst -sha256 -hmac %q)",
			stored, qaGoldenHMAC, qaGoldenPhone, qaGoldenKey)
	}
	if plain != qaGoldenPhone {
		t.Fatalf("phone = %q, want the canonical plaintext %q", plain, qaGoldenPhone)
	}
}

// A3, the other half: the hash input is the *canonical* number, so a submitted
// form with a '+', spaces and dashes must produce exactly the same stored bytes
// as the bare digits.
func TestQARegisterPhoneHashesCanonicalForm(t *testing.T) {
	svc, _ := phoneService(t)
	svc.VerificationMode = VerificationOff
	svc.PhoneHMACKey = qaGoldenKey
	cleanupPhoneNumber(t, qaGoldenPhone)

	rec := callRegister(t, svc, map[string]string{
		"identifier": "+86 138-0013-8000", "password": "Str0ng-Passw0rd!",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var stored, plain string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COALESCE(phone_hmac, ''), COALESCE(phone, '') FROM t_user WHERE phone = ?`,
		qaGoldenPhone).Scan(&stored, &plain); err != nil {
		t.Fatalf("read row (the row must be keyed on the canonical form): %v", err)
	}
	if stored != qaGoldenHMAC {
		t.Fatalf("phone_hmac for the formatted input = %s, want %s: the hash input is not canonical",
			stored, qaGoldenHMAC)
	}
}

// insertQAPhoneUser seeds one account with a phone number and either a real
// hmac (backfilled) or NULL (not yet backfilled / legacy).
func insertQAPhoneUser(t *testing.T, phone string, withHMAC bool) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcryptCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UTC()
	userNo := fmt.Sprintf("X%011d", now.UnixNano()%100000000000)
	var mac any
	if withHMAC {
		m, macErr := PhoneHMAC(phoneFixtureKey, phone)
		if macErr != nil {
			t.Fatalf("hmac: %v", macErr)
		}
		mac = m
	}
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, phone, phone_hmac, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at)
		VALUES (?,?,?,?,'bcrypt',?,'qa-phone','PERSONAL','NORMAL',?,?)`,
		userNo, phone, mac, string(hash), now, now, now)
	if err != nil {
		t.Fatalf("seed phone user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed id: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_admin_audit_log WHERE operator_id = ? OR target_id = ?`,
			uid, fmt.Sprint(uid))
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

// D12: phone login and phone-based password recovery must work the same before
// and after the backfill. The two rows differ only in phone_hmac.
func TestQAPhoneLoginAndRecoveryIgnorePhoneHMAC(t *testing.T) {
	for _, backfilled := range []bool{false, true} {
		name := "hmac_null_legacy_row"
		if backfilled {
			name = "hmac_set_backfilled_row"
		}
		t.Run(name, func(t *testing.T) {
			svc, _, sms, _, _ := recoveryService(t)
			phone := qaRandomPhone(t)
			cleanupPhoneNumber(t, phone)
			insertQAPhoneUser(t, phone, backfilled)

			// 1. login by plaintext phone.
			login := callLogin(t, svc, map[string]string{
				"identifier": phone, "password": testPassword,
			}, uniqueIP())
			if login.Code != http.StatusOK {
				t.Fatalf("phone login status=%d body=%s, want 200 (login must not read phone_hmac)",
					login.Code, login.Body.String())
			}

			// 2. recovery: issue a reset code and use it.
			if rec := callResetSendCode(t, svc, phone); rec.Code != http.StatusOK {
				t.Fatalf("reset send code status=%d body=%s, want 200", rec.Code, rec.Body.String())
			}
			code := sms.lastCode(t)
			newPassword := "Fresh-Passw0rd!2026"
			reset := callResetPassword(t, svc, map[string]string{
				"identifier": phone, "code": code, "new_password": newPassword,
			})
			if reset.Code != http.StatusNoContent {
				t.Fatalf("reset password status=%d body=%s, want 204",
					reset.Code, reset.Body.String())
			}
			// 3. the new password works.
			again := callLogin(t, svc, map[string]string{
				"identifier": phone, "password": newPassword,
			}, uniqueIP())
			if again.Code != http.StatusOK {
				t.Fatalf("login with the reset password status=%d body=%s, want 200",
					again.Code, again.Body.String())
			}
		})
	}
}

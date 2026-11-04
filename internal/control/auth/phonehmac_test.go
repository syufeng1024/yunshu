package auth

import (
	"errors"
	"strings"
	"testing"

	"yunshu.dev/core/internal/platform/phonehmac"
)

// The fixture key is 32+ bytes so it satisfies the production length rule.
const testPHKey = "t11-phone-hmac-key-for-unit-tests!!"

// T15: the hash shape is the contract the column was sized for (char(64)) and
// the value must be reproducible with an independent implementation.
func TestPhoneHMACShape(t *testing.T) {
	got, err := PhoneHMAC(testPHKey, "8613800138000")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if len(got) != 64 {
		t.Fatalf("hash length = %d, want 64 (char(64) column)", len(got))
	}
	if got != strings.ToLower(got) {
		t.Fatalf("hash %q is not lowercase; hex.EncodeToString must stay in use", got)
	}
	for _, r := range got {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("hash %q is not lowercase hex", got)
		}
	}
	// Same input, same key: identical. Different key: different. This is the
	// property that makes a key rotation a full recomputation.
	again, err := PhoneHMAC(testPHKey, "8613800138000")
	if err != nil {
		t.Fatalf("hash again: %v", err)
	}
	if again != got {
		t.Fatalf("hash is not deterministic: %q vs %q", got, again)
	}
	other, err := PhoneHMAC("another-phone-hmac-key-32-bytes!!", "8613800138000")
	if err != nil {
		t.Fatalf("hash other key: %v", err)
	}
	if other == got {
		t.Fatal("two different keys produced the same hash")
	}
	// Known-answer vector, computed independently:
	//   printf '8613800138000' | openssl dgst -sha256 -hmac 't11-phone-hmac-key-for-unit-tests!!'
	// Pinned so a future "optimisation" that changes the message (a prefix, a
	// trim, a different encoding) fails here instead of silently producing a
	// second family of hashes that never collides with the stored ones.
	const known = "20e1296614eebca2d52074adbb9fc40c9dcb5a7c5ae11ddec14a9eacf1f73e91"
	if got != known {
		t.Fatalf("hmac(8613800138000) = %s, want %s (openssl vector)", got, known)
	}
	t.Logf("hmac(8613800138000) = %s (matches openssl dgst -sha256 -hmac)", got)
}

// A short key is "not configured", never "hash with it anyway". An empty-key
// hash would give every phone number one shared value, and every row written
// afterwards would collide with that one row.
func TestPhoneHMACRejectsShortOrEmptyKey(t *testing.T) {
	for _, key := range []string{"", "   ", "short", strings.Repeat("k", 31)} {
		if _, err := PhoneHMAC(key, "8613800138000"); !errors.Is(err, ErrPhoneHMACKeyMissing) {
			t.Fatalf("key %q: err = %v, want ErrPhoneHMACKeyMissing", key, err)
		}
		if PhoneHMACKeyUsable(key) {
			t.Fatalf("key %q reported usable", key)
		}
	}
	if !PhoneHMACKeyUsable(strings.Repeat("k", 32)) {
		t.Fatal("a 32-byte key must be usable")
	}
}

// An empty phone is not "no phone number" — that state is a NULL column — so it
// must not hash into the value every empty row would share.
func TestPhoneHMACRejectsEmptyPhone(t *testing.T) {
	if _, err := PhoneHMAC(testPHKey, ""); err == nil {
		t.Fatal("hashing an empty phone must fail, not produce a shared value")
	}
}

// T15: CanonicalPhone and NormalizeIdentifier must agree, because the value the
// hash is computed over is the normalized one. If these two ever diverge, the
// row is written with a hash that its own login cannot reproduce.
func TestCanonicalPhoneMatchesNormalizeIdentifier(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"8613800138000", "8613800138000"},
		{"+86 138-0013-8000", "8613800138000"},
		{"8613800138000 ", "8613800138000"},
		// Full-width digits, spelled with escapes so the case cannot be
		// corrupted by an editor: ８６１３８００１３８０００.
		{"\uFF18\uFF16\uFF11\uFF13\uFF18\uFF10\uFF10\uFF11\uFF13\uFF18\uFF10\uFF10\uFF10", "8613800138000"},
		{"(86) 138 0013 8000", "8613800138000"},
		{"138.0013.8000", "13800138000"},
	}
	for _, c := range cases {
		got, ok := CanonicalPhone(c.raw)
		if !ok {
			t.Fatalf("CanonicalPhone(%q) rejected", c.raw)
		}
		if got != c.want {
			t.Fatalf("CanonicalPhone(%q) = %q, want %q", c.raw, got, c.want)
		}
		ident, err := NormalizeIdentifier(c.raw)
		if err != nil {
			t.Fatalf("NormalizeIdentifier(%q): %v", c.raw, err)
		}
		if ident.Kind != KindPhone {
			t.Fatalf("NormalizeIdentifier(%q) kind = %q, want phone", c.raw, ident.Kind)
		}
		if ident.Value != got {
			t.Fatalf("NormalizeIdentifier(%q) = %q but CanonicalPhone = %q", c.raw, ident.Value, got)
		}
	}
	// A non-phone is rejected by both.
	if _, ok := CanonicalPhone("alice@example.com"); ok {
		t.Fatal("CanonicalPhone accepted an e-mail address")
	}
	if _, ok := CanonicalPhone("12345"); ok {
		t.Fatal("CanonicalPhone accepted a 5-digit value (below the 6-byte floor)")
	}
}

// The masking rule the backfill prints into tickets must be the one the audit
// trail already writes. This pins the delegation in maskIdentifier.
func TestMaskIdentifierPhoneMatchesPhonehmacMask(t *testing.T) {
	if got, want := maskIdentifier(KindPhone, "8613800138000"), phonehmac.Mask("8613800138000"); got != want {
		t.Fatalf("maskIdentifier(phone) = %q, phonehmac.Mask = %q", got, want)
	}
	if got := maskIdentifier(KindPhone, "8613800138000"); got != "861****00" {
		t.Fatalf("mask = %q, want 861****00 (existing audit shape)", got)
	}
	if got := maskIdentifier(KindPhone, "1234"); got != "***" {
		t.Fatalf("mask of a short value = %q, want ***", got)
	}
}

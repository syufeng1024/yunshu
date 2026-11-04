// Package phonehmac owns the one implementation of the phone-number HMAC that
// t_user.phone_hmac stores.
//
// It lives under internal/platform rather than next to the auth handlers
// because three very different callers need the exact same bytes:
//
//	internal/control/auth      registration, operator provisioning, canon
//	internal/control/fund      reads the stored value only (no key required)
//	cmd/backfill-phone-hmac    recomputes every historical row
//
// A second implementation anywhere — "just this once, for the backfill" — is
// how a value that is supposed to be a uniqueness key quietly splits into two
// families of hashes that never collide. Everything here is therefore
// deterministic and dependency-free.
//
// Non-goals: this package never logs, never reads the environment and never
// touches a database. It is arithmetic plus one predicate.
package phonehmac

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// MinKeyLen is the minimum acceptable length of PHONE_HMAC_KEY, in bytes.
//
// 32 bytes is the block-independent floor for HMAC-SHA256 key material: below
// it the key is hashed internally anyway, so a short key buys nothing and only
// signals that somebody typed a word instead of generating one.
const MinKeyLen = 32

var (
	// ErrKeyMissing means the key is absent or shorter than MinKeyLen. Callers
	// answer 503 config_unavailable for a write and never hash with whatever
	// they were given: an empty-key hash would produce one shared value for
	// every phone number not yet written, and every future row would then
	// collide with it.
	ErrKeyMissing = errors.New("phonehmac: key is not configured or shorter than 32 bytes")

	// ErrPhoneEmpty means the caller handed in an empty canonical phone. The
	// empty string is not "no phone number" — that state is a NULL column — so
	// hashing it would create a value every empty row shares.
	ErrPhoneEmpty = errors.New("phonehmac: phone is empty")
)

// Hash returns lowercase hex HMAC-SHA256(key, canonicalPhone), 64 characters —
// exactly the width of t_user.phone_hmac char(64) and of
// t_device_fingerprint.fp_hash, whose hash this mirrors.
//
// canonicalPhone MUST be the output of Canonical (or of auth.CanonicalPhone,
// which delegates here). Hashing the raw user input would give "+86 138-0013-
// 8000" and "8613800138000" two different values, which is precisely the
// uniqueness the column exists to provide.
func Hash(key, canonicalPhone string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) < MinKeyLen {
		return "", ErrKeyMissing
	}
	if canonicalPhone == "" {
		return "", ErrPhoneEmpty
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(canonicalPhone))
	// hex.EncodeToString is lowercase by construction, so no extra lowering.
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Canonical reports whether raw is a phone number and returns its stored form.
//
// The rule is the pre-username one from the identifier normalizer, unchanged:
// strip the characters humans insert (space, tab, CR, LF, '-', '(', ')', '+',
// '.'), then require 6..20 bytes that are all ASCII digits. A leading '+' is
// dropped because t_user.phone stores E.164 without it.
//
// Full-width to half-width folding is deliberately NOT done here: that is part
// of the input-cleaning step in auth.NormalizeIdentifier, and the backfill must
// be able to say "this stored value is not what the normalizer would produce"
// instead of silently repairing PII in a read-only pass.
func Canonical(raw string) (string, bool) {
	phone := stripSeparators(raw)
	if len(phone) < 6 || len(phone) > 20 {
		return "", false
	}
	for i := 0; i < len(phone); i++ {
		if phone[i] < '0' || phone[i] > '9' {
			return "", false
		}
	}
	return phone, true
}

// stripSeparators removes the characters humans insert in phone numbers.
func stripSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\r', '\n', '-', '(', ')', '+', '.':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Mask keeps enough of an opaque identifier for an operator to recognise the
// account while keeping logs, tickets and CLI output from becoming a PII dump:
// the first three bytes and the last two, or "***" when that would leak most of
// a short value.
//
// It is exported because the backfill CLI writes its findings into tickets and
// must reduce a phone number exactly the way the audit trail does
// (auth.MaskIdentifier delegates its non-e-mail branch here) instead of
// inventing a second masking rule that a reviewer has to compare by eye.
func Mask(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 4 {
		return "***"
	}
	return value[:3] + "****" + value[len(value)-2:]
}

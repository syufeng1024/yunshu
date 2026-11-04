package auth

import (
	"errors"

	"yunshu.dev/core/internal/platform/phonehmac"
)

// PhoneHMACMinKeyLen is the minimum accepted length of PHONE_HMAC_KEY.
//
// It is re-exported so cmd/controld (which validates the variable before the
// service exists) and this package cannot disagree about "too short".
const PhoneHMACMinKeyLen = phonehmac.MinKeyLen

var (
	// ErrPhoneHMACKeyMissing means PHONE_HMAC_KEY is absent or shorter than
	// PhoneHMACMinKeyLen. The handlers turn it into 503 config_unavailable on
	// the phone *write* paths only; every read path (login, recovery, operator
	// listings) keeps working without the key.
	ErrPhoneHMACKeyMissing = phonehmac.ErrKeyMissing

	// ErrPhoneHMACRequired means a phone identifier reached the INSERT with no
	// hmac. It is a programming error, not a client one: the unique key
	// uk_user_phone_hmac can only be enforced if every phone row carries the
	// hash, so insertUserRow refuses to write a phone row without one.
	ErrPhoneHMACRequired = errors.New("auth: a phone identifier requires a non-empty phone hmac")
)

// PhoneHMAC returns lowercase hex HMAC-SHA256(key, canonicalPhone).
//
// Single implementation lives in internal/platform/phonehmac so the backfill
// command cannot drift from the request path. canonicalPhone must already be
// canonical (see CanonicalPhone); passing the raw user input would make
// "+86 138-0013-8000" and "8613800138000" hash differently and silently
// disable the uniqueness this column is for.
func PhoneHMAC(key, canonicalPhone string) (string, error) {
	return phonehmac.Hash(key, canonicalPhone)
}

// PhoneHMACKeyUsable reports whether key can be used for hashing.
//
// It exists so a caller can decide between "answer 503 and touch nothing" and
// "carry on" without attempting a hash it would then have to discard.
func PhoneHMACKeyUsable(key string) bool {
	_, err := phonehmac.Hash(key, "probe")
	return err == nil
}

package auth

import (
	"errors"
	"regexp"
	"strings"

	"yunshu.dev/core/internal/platform/phonehmac"
)

// Identifier kinds. The column that stores the value is chosen from this, not
// from a client-supplied field. The three constants are the only values
// findUser will ever turn into a column name.
const (
	KindEmail    = "email"
	KindPhone    = "phone"
	KindUsername = "username"
)

// usernameRE is the single implementation of the operator-username rule:
// first byte an ASCII letter, 3..32 bytes total, only a-z 0-9 . _ - after that.
//
// The charset is pure ASCII, so the byte count equals the character count and
// the 32-byte ceiling lines up exactly with t_user.username varchar(32).
// Requiring a leading letter is what makes an all-digit username impossible,
// which in turn makes a username never ambiguous with a phone number; it also
// rejects a leading '.', '-' or '_'.
//
// Interior runs of separators ("a..b", "a--b") are accepted on purpose: they
// are neither exploitable nor ambiguous, and every extra rule is one more
// statement a frontend hint and a runbook have to keep in sync.
var usernameRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,31}$`)

// Identifier is a normalized email, phone or operator username.
type Identifier struct {
	Kind  string
	Value string
}

// NormalizeIdentifier applies the input-cleanup rule before any lookup:
// trim, full-width to half-width, then classify by shape.
//
// The order is fixed and is not configurable by the client, which submits one
// `identifier` field and nothing else:
//
//	contains '@'                              -> email
//	strip phone separators, all digits 6..20  -> phone   (byte-identical to the
//	                                                       pre-username rule)
//	otherwise                                 -> username
//
// Lowercasing matters for both branches: utf8mb4_0900_as_cs is case-sensitive,
// so without it Alice@example.com and alice@example.com could register as two
// accounts, and Admin and admin could be two operator rows.
//
// The username candidate is the trimmed/half-width string, NOT the
// phone-stripped one: the phone rule removes '.' and '-', which are legal
// username characters. The stripped form is only ever used to test the phone
// predicate. Using it as the value would silently rewrite "my.name" into
// "myname" and store an account nobody asked for.
//
// For the same reason the whitespace the phone rule would strip is never
// applied to the candidate: "my name" must be rejected, not folded into a
// valid-looking account called "myname" that could collide with — or log in
// as — somebody else's row. Only the surrounding whitespace removed by TrimSpace
// above is invisible to the rule.
func NormalizeIdentifier(raw string) (Identifier, error) {
	s := strings.TrimSpace(toHalfWidth(raw))
	if s == "" {
		return Identifier{}, errors.New("empty identifier")
	}
	if strings.Contains(s, "@") {
		email := strings.ToLower(s)
		if len(email) > 254 {
			return Identifier{}, errors.New("email exceeds 254 characters")
		}
		at := strings.IndexByte(email, '@')
		if at <= 0 || at == len(email)-1 {
			return Identifier{}, errors.New("email is missing a local part or domain")
		}
		if strings.Count(email, "@") != 1 {
			return Identifier{}, errors.New("email contains more than one @")
		}
		domain := email[at+1:]
		if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
			return Identifier{}, errors.New("email domain is malformed")
		}
		for _, r := range email {
			if r <= 0x20 || r == 0x7f {
				return Identifier{}, errors.New("email contains whitespace or a control character")
			}
		}
		return Identifier{Kind: KindEmail, Value: email}, nil
	}

	if phone, ok := asPhone(s); ok {
		return Identifier{Kind: KindPhone, Value: phone}, nil
	}

	username := strings.ToLower(s)
	if !usernameRE.MatchString(username) {
		return Identifier{}, errors.New("identifier is not an email, a phone number or a valid username")
	}
	return Identifier{Kind: KindUsername, Value: username}, nil
}

// asPhone reports whether s is a phone number and returns its stored form.
//
// The predicate (all digits, 6..20 after removing formatting characters) is the
// pre-username one, unchanged: an existing all-digit identifier must keep
// resolving to the phone column or those accounts lose their login.
//
// The implementation lives in internal/platform/phonehmac.Canonical because the
// same predicate decides whether a stored t_user.phone value may be hashed by
// the backfill command; one implementation means "loginable" and "hashable" can
// never disagree.
func asPhone(s string) (string, bool) {
	return phonehmac.Canonical(s)
}

// CanonicalPhone is the exported form of the phone rule: it applies the same
// input cleaning NormalizeIdentifier applies (trim, full-width to half-width)
// and then the phone predicate, returning the value t_user.phone stores.
//
// It exists for the two callers that see a phone number outside a login or
// registration body:
//
//   - cmd/backfill-phone-hmac must decide whether a stored phone value is
//     already canonical before hashing it. Hashing a non-canonical value would
//     produce a hash nobody's login can reproduce — uniqueness silently gone —
//     so the backfill reports it instead of guessing.
//   - cmd/seedctl validates the -phone seed with the same rule the control
//     plane will apply when that account logs in.
//
// It is a wrapper, never a second rule: the predicate itself is
// phonehmac.Canonical, shared with asPhone.
func CanonicalPhone(raw string) (string, bool) {
	return asPhone(strings.TrimSpace(toHalfWidth(raw)))
}

// ParseUsername validates one free-standing username (the account settings
// field). It applies the same normalization the login path applies — trim,
// full-width to half-width, lowercase — and the same regex, so a name saved
// here is exactly a name login can resolve.
//
// It is deliberately separate from NormalizeIdentifier: that function
// classifies an identifier into email/phone/username, and the profile form must
// not accept an email in the username field simply because it could classify it.
func ParseUsername(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(toHalfWidth(raw)))
	if !usernameRE.MatchString(s) {
		return "", errors.New("username must be 3-32 chars, start with a letter, and use only a-z 0-9 . _ -")
	}
	return s, nil
}

// RequireEmailOrPhone rejects a username identifier on the paths that cannot
// handle one.
//
// Registration, verification-code issuance, operator provisioning and seedctl
// all need an address the account owner can read: a username account has no
// mailbox to send a code to, and an operator console that could mint a
// username account would create one with no recovery path at all. Centralising
// the refusal keeps four call sites from each writing their own comparison and
// drifting into "one of them quietly treats a username as an email".
//
// The returned error is only for the caller's decision; the message never
// echoes the value (these call sites answer a fixed invalid_request_body).
func RequireEmailOrPhone(ident Identifier) error {
	if ident.Kind == KindEmail || ident.Kind == KindPhone {
		return nil
	}
	return errors.New("this endpoint accepts an email or phone identifier only")
}

// toHalfWidth converts full-width ASCII and the ideographic space, so a
// pasted identifier from an IME matches the ASCII row.
func toHalfWidth(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == 0x3000:
			b.WriteRune(' ')
		case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteRune(r - 0xFEE0)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// maskIdentifier keeps enough of the identifier for an operator to recognise
// the account while keeping the audit trail from becoming a PII dump.
//
// The e-mail branch is local (it has a shape worth preserving: the domain tells
// an operator where the reset mail went). The phone/username branch is the
// shared phonehmac.Mask rule, because the backfill CLI prints the same
// reductions into tickets and two masking implementations would drift.
func maskIdentifier(kind, value string) string {
	if value == "" {
		return ""
	}
	if kind == KindEmail {
		at := strings.IndexByte(value, '@')
		if at <= 0 {
			return "***"
		}
		local := value[:at]
		if len(local) <= 1 {
			return "*" + value[at:]
		}
		return local[:1] + "***" + value[at:]
	}
	return phonehmac.Mask(value)
}

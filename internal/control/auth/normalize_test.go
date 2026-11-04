package auth

import (
	"strings"
	"testing"
)

func TestNormalizeIdentifierEmail(t *testing.T) {
	got, err := NormalizeIdentifier("  Local＠Yunshu.Dev  ")
	if err != nil {
		t.Fatalf("NormalizeIdentifier: %v", err)
	}
	if got.Kind != KindEmail || got.Value != "local@yunshu.dev" {
		t.Fatalf("got %+v, want lower-case email", got)
	}
}

func TestNormalizeIdentifierPhone(t *testing.T) {
	got, err := NormalizeIdentifier(" +86 (138) 0013-8000 ")
	if err != nil {
		t.Fatalf("NormalizeIdentifier: %v", err)
	}
	if got.Kind != KindPhone || got.Value != "8613800138000" {
		t.Fatalf("got %+v", got)
	}
}

func TestNormalizeIdentifierRejects(t *testing.T) {
	for _, raw := range []string{"", "   ", "@example.com", "a@b", "a@@b.com", "12345", "13800138000x"} {
		if _, err := NormalizeIdentifier(raw); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
}

// TestNormalizeIdentifierUsername covers the rule that a username is the
// fallback branch: not an email (no '@'), not a phone (not all digits in 6..20
// after stripping formatting characters), so it must satisfy
// ^[a-z][a-z0-9._-]{2,31}$ after lowercasing.
func TestNormalizeIdentifierUsername(t *testing.T) {
	accept := []struct {
		raw  string
		want string
		why  string
	}{
		{"admin", "admin", "canonical built-in username"},
		{"Admin", "admin", "uppercase is normalized, not rejected"},
		{" admin ", "admin", "surrounding spaces are trimmed"},
		{"ＡＤＭＩＮ", "admin", "full-width ASCII is converted, then lowercased"},
		{"ａｄｍｉｎ", "admin", "full-width lowercase"},
		{"ops.zhang", "ops.zhang", "'.' is a legal username character and must survive"},
		{"ops-zhang", "ops-zhang", "'-' is a legal username character and must survive"},
		{"ops_zhang", "ops_zhang", "'_' is a legal username character"},
		{"a..b", "a..b", "interior separator runs are allowed"},
		{"a--b", "a--b", "interior separator runs are allowed"},
		{"abc", "abc", "minimum length is 3 bytes"},
		{"a" + strings.Repeat("b", 31), "a" + strings.Repeat("b", 31), "maximum length is 32 bytes"},
		{"abc123", "abc123", "digits after the leading letter are fine"},
	}
	for _, tc := range accept {
		got, err := NormalizeIdentifier(tc.raw)
		if err != nil {
			t.Fatalf("%q (%s): unexpected error %v", tc.raw, tc.why, err)
		}
		if got.Kind != KindUsername || got.Value != tc.want {
			t.Fatalf("%q (%s): got %+v, want {%s %q}", tc.raw, tc.why, got, KindUsername, tc.want)
		}
	}

	reject := []struct {
		raw string
		why string
	}{
		{"ab", "2 bytes is below the 3-byte floor"},
		{"a" + strings.Repeat("b", 32), "33 bytes exceeds varchar(32)"},
		{"_admin", "must start with a letter"},
		{"-admin", "must start with a letter"},
		{".admin", "must start with a letter"},
		{"1admin", "must start with a letter"},
		{"13800138000x", "starts with a digit and is not all digits"},
		{"admin$", "'$' is outside the charset"},
		{"admin 名称", "non-ASCII is outside the charset"},
		{"admin名称", "non-ASCII is outside the charset"},
		{"admin@", "contains '@' so it is treated as a malformed email"},
		{"12345", "too short for a phone and all digits so the username rule fails"},
		{"123456789012345678901", "21 digits exceeds the phone range and starts with a digit"},
	}
	for _, tc := range reject {
		if got, err := NormalizeIdentifier(tc.raw); err == nil {
			t.Fatalf("%q (%s) was accepted as %+v", tc.raw, tc.why, got)
		}
	}
}

// TestNormalizeIdentifierRegressions pins the behaviour that existed before
// usernames were added. These inputs are in production databases; a change that
// reclassifies one of them would route a live account to the wrong column.
func TestNormalizeIdentifierRegressions(t *testing.T) {
	for _, raw := range []string{
		"alice@example.com",
		"  ALICE@Example.COM  ",
		"a@b.co",
	} {
		got, err := NormalizeIdentifier(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got.Kind != KindEmail {
			t.Fatalf("%q: kind = %s, want email", raw, got.Kind)
		}
	}

	for raw, want := range map[string]string{
		"+86 (138) 0013-8000":  "8613800138000",
		"13800138000":          "13800138000",
		"138-0013-8000":        "13800138000",
		"123456":               "123456",
		"12345678901234567890": "12345678901234567890",
	} {
		got, err := NormalizeIdentifier(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got.Kind != KindPhone || got.Value != want {
			t.Fatalf("%q: got %+v, want {%s %q}", raw, got, KindPhone, want)
		}
	}
}

// TestNormalizeIdentifierNeverStripsInteriorSeparators pins the regression that
// the removed "strip the separators before classifying" branch caused: it
// rewrote "my name" into the perfectly legal "myname", so an input that must be
// rejected turned into a username — potentially somebody else's account.
//
// The phone predicate is untouched by that removal: asPhone strips the
// formatting characters itself, which is exactly why a formatted number still
// classifies as a phone. Both directions are asserted here so a future
// "helpful" re-introduction of the branch fails immediately.
func TestNormalizeIdentifierNeverStripsInteriorSeparators(t *testing.T) {
	// Rejected, and rejected as a whole: nothing is rewritten into a candidate.
	//
	// "a b" is only 3 bytes, so stripping it would leave "ab" — a different
	// error, but still an error; "my name" is the discriminating case, because
	// the stripped form "myname" is a valid username.
	//
	// Every ASCII whitespace byte the old branch listed is covered, not just the
	// space: the branch tested strings.ContainsAny(s, " \t\r\n"), so a
	// regression that only handled ' ' (e.g. ReplaceAll(s, " ", "")) would sail
	// through a space-only table. "ad min" is the case QA reproduced: it used to
	// normalize to "admin" and log in as somebody else. The full-width space is
	// here because toHalfWidth turns it into a plain space before this check.
	for _, raw := range []string{
		"my name", "a b", "ops zhang",
		"ad min", "admin name", "admin\tname", "admin\rname", "admin\nname",
		"admin\u3000name", "admin  name",
	} {
		got, err := NormalizeIdentifier(raw)
		if err == nil {
			t.Fatalf("%q was accepted as %+v; interior whitespace must be rejected, not stripped", raw, got)
		}
		if got.Kind == KindUsername && got.Value != raw {
			t.Fatalf("%q was rewritten into the username %q", raw, got.Value)
		}
	}

	// Only surrounding whitespace is invisible to the rule: " admin" is admin.
	got, err := NormalizeIdentifier(" admin")
	if err != nil {
		t.Fatalf(`" admin" was rejected: %v`, err)
	}
	if got.Kind != KindUsername || got.Value != "admin" {
		t.Fatalf(`" admin": got %+v, want {%s "admin"}`, got, KindUsername)
	}

	// Deleting the branch must not change phone classification: the stripping
	// happens inside asPhone.
	got, err = NormalizeIdentifier("+86 (138) 0013-8000")
	if err != nil {
		t.Fatalf(`"+86 (138) 0013-8000" was rejected: %v`, err)
	}
	if got.Kind != KindPhone || got.Value != "8613800138000" {
		t.Fatalf(`"+86 (138) 0013-8000": got %+v, want {%s "8613800138000"}`, got, KindPhone)
	}
}

func TestRequireEmailOrPhone(t *testing.T) {
	if err := RequireEmailOrPhone(Identifier{Kind: KindEmail, Value: "a@b.co"}); err != nil {
		t.Fatalf("email rejected: %v", err)
	}
	if err := RequireEmailOrPhone(Identifier{Kind: KindPhone, Value: "13800138000"}); err != nil {
		t.Fatalf("phone rejected: %v", err)
	}
	err := RequireEmailOrPhone(Identifier{Kind: KindUsername, Value: "admin"})
	if err == nil {
		t.Fatal("username was accepted by RequireEmailOrPhone")
	}
	// The message must not echo the value: it ends up in a log line for the
	// registration path.
	if strings.Contains(err.Error(), "admin") {
		t.Fatalf("error message leaks the identifier: %v", err)
	}
}

func TestMaskIdentifier(t *testing.T) {
	if got := maskIdentifier(KindEmail, "alice@example.com"); got != "a***@example.com" {
		t.Fatalf("email mask = %q", got)
	}
	if got := maskIdentifier(KindPhone, "8613800138000"); got != "861****00" {
		t.Fatalf("phone mask = %q", got)
	}
	// document behaviour: the non-email branch is reused unchanged, so a
	// 5-byte username keeps 3 + 2 characters and stays recognisable in the
	// audit trail without becoming a full PII copy.
	if got := maskIdentifier(KindUsername, "admin"); got != "adm****in" {
		t.Fatalf("username mask = %q", got)
	}
	if got := maskIdentifier(KindUsername, "abc"); got != "***" {
		t.Fatalf("short username mask = %q", got)
	}
}

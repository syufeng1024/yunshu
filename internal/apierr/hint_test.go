package apierr

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/ratelimit"
)

// WithHint exists for codes shared by several resources: the call site knows
// which page the operator has to open, the spec table cannot. The override has
// to survive write-time rendering, including the {retry} token, otherwise the
// hint would contradict retry_after_seconds.
func TestWithHintOverridesSpecHint(t *testing.T) {
	e := Newf(CodeRPMLimitExceeded, 10, 60).WithHint("打开控制台 → 用量页（{retry} 秒后可重试）")
	rec := httptest.NewRecorder()
	if err := Write(rec, e); err != nil {
		t.Fatal(err)
	}
	body := decodeBody(t, rec.Body.Bytes())
	const want = "打开控制台 → 用量页（60 秒后可重试）"
	if body["hint"] != want {
		t.Fatalf("hint = %q, want %q", body["hint"], want)
	}
	spec, ok := Lookup(CodeRPMLimitExceeded)
	if !ok {
		t.Fatal("rpm_limit_exceeded must be registered")
	}
	if body["hint"] == renderHint(spec.Hint, 60) {
		t.Fatal("hint still carries the spec wording; the override was ignored")
	}
}

// An override goes through the same sanitizer as a spec hint: a call site that
// pastes in an upstream error must not leak a credential to the client.
func TestWithHintIsSanitized(t *testing.T) {
	e := Newf(CodeRPMLimitExceeded, 10, 60).WithHint("先用 Authorization: Bearer live-token-abc 重试")
	rec := httptest.NewRecorder()
	if err := Write(rec, e); err != nil {
		t.Fatal(err)
	}
	hint, _ := decodeBody(t, rec.Body.Bytes())["hint"].(string)
	if strings.Contains(hint, "live-token-abc") {
		t.Fatalf("hint leaked the bearer token: %q", hint)
	}
	if !strings.Contains(hint, "[redacted") {
		t.Fatalf("hint = %q, want a redaction marker", hint)
	}
}

// An override that sanitizes down to nothing must fall back to the default
// hint rather than emitting an empty field.
func TestWithHintEmptyFallsBackToDefault(t *testing.T) {
	e := Newf(CodeRPMLimitExceeded, 10, 60).WithHint("   ")
	rec := httptest.NewRecorder()
	if err := Write(rec, e); err != nil {
		t.Fatal(err)
	}
	if got := decodeBody(t, rec.Body.Bytes())["hint"]; got != "携 request_id 联系客服" {
		t.Fatalf("hint = %v, want the default hint", got)
	}
}

// LogFields feeds the request logger. The cause stays out of it: it may hold
// SQL, a stack trace or an upstream credential.
func TestLogFieldsOmitsCause(t *testing.T) {
	e := Newf(CodeInternalError).WithCause(errors.New("sql: SELECT * FROM t_secret_probe"))
	fields := e.LogFields()
	if _, ok := fields["cause"]; ok {
		t.Fatal("LogFields must not expose the cause")
	}
	if rid, _ := fields["request_id"].(string); rid == "" {
		t.Fatal("LogFields must carry a request_id")
	}
	if msg, _ := fields["message"].(string); strings.Contains(msg, "t_secret_probe") {
		t.Fatalf("message leaked the cause: %q", msg)
	}
}

// Every accessor tolerates a nil receiver: call sites build an error lazily and
// may chain With* on the result.
func TestNilErrorAccessors(t *testing.T) {
	var e *APIError
	if got := e.WithHint("x"); got != nil {
		t.Fatal("WithHint on nil must return nil")
	}
	if e.Cause() != nil {
		t.Fatal("Cause on nil must be nil")
	}
	if got := e.EnsureRequestID(); got != "" {
		t.Fatalf("EnsureRequestID on nil = %q, want empty", got)
	}
	if got := e.String(); got != "apierr:<nil>" {
		t.Fatalf("String on nil = %q", got)
	}
	if got := e.LogFields()["code"]; got != CodeInternalError {
		t.Fatalf("LogFields[code] = %v, want %s", got, CodeInternalError)
	}
}

// A daily quota reset further out than 24h is clamped, and reset_at must move
// with retry_after_seconds (E.6 boundary ①).
func TestDailyQuotaRetryIsClampedWithResetAt(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	v := ratelimit.Verdict{
		Allowed: false, Code: ratelimit.CodeDailyQuota,
		AllHit: []string{ratelimit.CodeDailyQuota},
		Limit:  100, Remaining: 0,
		ResetAt: now.Add(100 * 24 * time.Hour).Unix(),
	}
	e := FromVerdictAt(v, now)
	if e.RetryAfterSeconds != 86400 {
		t.Fatalf("retry_after_seconds = %d, want 86400", e.RetryAfterSeconds)
	}
	// now is 2026-10-27T12:00:00Z == 20:00 in UTC+8; +86400s lands on the same
	// wall clock the next day.
	if want := "2026-10-28T20:00:00+08:00"; e.ResetAt != want {
		t.Fatalf("reset_at = %q, want %q", e.ResetAt, want)
	}
}

// Dimension labels fall back to the raw code so a dimension added to the
// limiter without a translation still names itself in the message.
func TestOtherHitsLabelUnknownDimension(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	v := ratelimit.Verdict{
		Allowed: false, Code: ratelimit.CodeRPM,
		AllHit: []string{ratelimit.CodeRPM, "mystery_dimension", "mystery_dimension"},
		Limit:  10, Remaining: 0,
		ResetAt: now.Add(30 * time.Second).Unix(),
	}
	e := FromVerdictAt(v, now)
	if !strings.Contains(e.Message, "mystery_dimension") {
		t.Fatalf("message = %q, want the unknown dimension named", e.Message)
	}
	if n := strings.Count(e.Message, "mystery_dimension"); n != 1 {
		t.Fatalf("message names the dimension %d times, want 1 (dedup)", n)
	}
}

// WithMessage overrides the wording without changing the code or status: a
// shared code applied in a narrower context can be actively misleading (G36 —
// invalid_credentials shouting about a password when only a code was sent).
func TestWithMessageOverridesWordingButKeepsCode(t *testing.T) {
	e := New(CodeInvalidCredentials).WithMessage("验证码不正确或已过期")
	body := e.object()
	if body.Message != "验证码不正确或已过期" {
		t.Fatalf("message=%q, want the override", body.Message)
	}
	if body.Code != CodeInvalidCredentials {
		t.Fatalf("code=%q, must stay %s", body.Code, CodeInvalidCredentials)
	}
	if e.HTTPStatus != 401 {
		t.Fatalf("status=%d, must stay 401", e.HTTPStatus)
	}
	// nil receiver must not panic: the builder is chained on values that may be
	// nil in error paths.
	var nilErr *APIError
	if nilErr.WithMessage("x") != nil {
		t.Fatalf("nil receiver returned non-nil")
	}
}

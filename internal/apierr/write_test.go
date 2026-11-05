package apierr

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/ratelimit"
)

var uuidv7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func fixedNow() time.Time {
	return time.Date(2026, 10, 23, 10, 30, 40, 0, shanghai)
}

func timeAtShanghai(now time.Time, hour, minute, second int) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), hour, minute, second, 0, shanghai)
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var envelope map[string]map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode body: %v (%s)", err, raw)
	}
	body, ok := envelope["error"]
	if !ok {
		t.Fatalf("no top-level error object: %s", raw)
	}
	return body
}

func writeError(t *testing.T, e *APIError) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := Write(rec, e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return rec, decodeBody(t, rec.Body.Bytes())
}

// A8: no supplied id -> generated UUIDv7, header == body, time-ordered.
func TestRequestIDFallbackIsUUIDv7AndConsistent(t *testing.T) {
	e := New(CodeInvalidAPIKey)
	rec, body := writeError(t, e)

	rid, _ := body["request_id"].(string)
	if rid == "" {
		t.Fatal("body request_id is empty")
	}
	if !uuidv7Pattern.MatchString(rid) {
		t.Fatalf("request_id %q is not UUIDv7-shaped", rid)
	}
	if got := rec.Header().Get("X-Request-Id"); got != rid {
		t.Fatalf("header X-Request-Id = %q, body request_id = %q", got, rid)
	}
	if e.RequestID != rid {
		t.Fatalf("write-back failed: struct request_id = %q", e.RequestID)
	}

	a := newRequestIDAt(fixedNow())
	b := newRequestIDAt(fixedNow().Add(time.Second))
	ms := func(id string) uint64 {
		v, err := strconv.ParseUint(strings.ReplaceAll(id, "-", "")[:12], 16, 64)
		if err != nil {
			t.Fatalf("parse timestamp from %q: %v", id, err)
		}
		return v
	}
	if got, want := ms(b), ms(a)+1000; got != want {
		t.Fatalf("UUIDv7 timestamp not ordered: %d vs %d", got, want)
	}
}

func TestRequestIDProvidedIsPreserved(t *testing.T) {
	e := New(CodeInvalidAPIKey).WithRequestID("01J8X2K4M6P8Q0R2S4T6V8W0X2")
	rec, body := writeError(t, e)
	if body["request_id"] != "01J8X2K4M6P8Q0R2S4T6V8W0X2" {
		t.Fatalf("body request_id = %v", body["request_id"])
	}
	if rec.Header().Get("X-Request-Id") != "01J8X2K4M6P8Q0R2S4T6V8W0X2" {
		t.Fatalf("header lost the supplied id")
	}
}

func TestRequestIDHeaderInjectionIsRejected(t *testing.T) {
	e := New(CodeInvalidAPIKey).WithRequestID("abc\r\nX-Evil: 1")
	rec, body := writeError(t, e)
	rid, _ := body["request_id"].(string)
	if !uuidv7Pattern.MatchString(rid) {
		t.Fatalf("unsafe id was not replaced: %q", rid)
	}
	if got := rec.Header().Get("X-Evil"); got != "" {
		t.Fatalf("header injection succeeded: %q", got)
	}
}

// A5: 402 remaining may be negative, limit is the overdraft ceiling, no
// reset_at, and the header and body come from the same variable.
func Test402NegativeRemainingAndHeaderParity(t *testing.T) {
	e := NewInsufficientCredits(1860, -12.34, 50)
	rec, body := writeError(t, e)

	if got := body["remaining"].(float64); got != -12.34 {
		t.Fatalf("remaining = %v, want -12.34", got)
	}
	if got := body["limit"].(float64); got != 50 {
		t.Fatalf("limit = %v, want 50", got)
	}
	if _, ok := body["reset_at"]; ok {
		t.Fatalf("402 must not carry reset_at: %v", body)
	}
	if got := rec.Header().Get("X-Credits-Remaining"); got != "-12.34" {
		t.Fatalf("X-Credits-Remaining = %q, want -12.34", got)
	}
	if !strings.Contains(e.Message, "1,860") || !strings.Contains(e.Message, "-12.34") {
		t.Fatalf("message must contain raw_est and available: %q", e.Message)
	}
	if rec.Code != 402 {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("402 must not send Retry-After, got %q", got)
	}
}

func Test402FrozenIncludesArrearsInTopUp(t *testing.T) {
	e := NewFrozenDueToOverdraft(37, 50, 5)
	_, body := writeError(t, e)
	if got := body["remaining"].(float64); got != -37 {
		t.Fatalf("remaining = %v, want -37", got)
	}
	if got := body["limit"].(float64); got != 50 {
		t.Fatalf("limit = %v, want 50", got)
	}
	// 5 arrears + (50 - 37) = 18
	if !strings.Contains(e.Message, "18") {
		t.Fatalf("top-up must include arrears: %q", e.Message)
	}
}

func Test402IgnoresResetAtEvenIfCallerSetsIt(t *testing.T) {
	e := NewInsufficientCredits(100, 10, 50).WithResetAt(fixedNow())
	_, body := writeError(t, e)
	if _, ok := body["reset_at"]; ok {
		t.Fatalf("402 must drop reset_at: %v", body)
	}
}

// A6: all four dimensions saturated -> daily wins; the whole optional trio
// describes daily; the other three only appear in message.
func Test429FourDimensionsPicksLatestRecovery(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, shanghai)
	nextMidnight := time.Date(2026, 10, 24, 0, 0, 0, 0, shanghai)
	v := ratelimit.Verdict{
		Allowed: false,
		Code:    ratelimit.CodeConcurrency, // priority-first, not the outward code
		AllHit: []string{
			ratelimit.CodeConcurrency, ratelimit.CodeRPM,
			ratelimit.CodeTPM, ratelimit.CodeDailyQuota,
		},
		Limit:      5000,
		Total:      5000,
		Remaining:  0,
		ResetAt:    nextMidnight.Unix(),
		ResetAtMax: nextMidnight.Unix(),
	}
	e := FromVerdictAt(v, now)
	if e.Code != CodeDailyQuotaExceeded {
		t.Fatalf("code = %q, want daily_quota_exceeded", e.Code)
	}
	if e.RetryAfterSeconds != 86400 {
		t.Fatalf("retry = %d, want 86400", e.RetryAfterSeconds)
	}
	if *e.Limit != 5000 || *e.Remaining != 0 {
		t.Fatalf("limit/remaining = %v/%v, want daily 5000/0", *e.Limit, *e.Remaining)
	}
	if e.ResetAt != "2026-10-24T00:00:00+08:00" {
		t.Fatalf("reset_at = %q, want next 00:00 UTC+8", e.ResetAt)
	}
	for _, want := range []string{"同时触顶：并发、RPM、TPM"} {
		if !strings.Contains(e.Message, want) {
			t.Fatalf("message = %q, want it to contain %q", e.Message, want)
		}
	}
	rec, body := writeError(t, e)
	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if _, ok := body["reset_at"]; !ok {
		t.Fatalf("429 body lacks reset_at")
	}
}

func Test429SingleDimensionMatchesVerdict(t *testing.T) {
	now := fixedNow()
	reset := now.Add(42 * time.Second)
	v := ratelimit.Verdict{
		Allowed: false, Code: ratelimit.CodeRPM,
		AllHit: []string{ratelimit.CodeRPM},
		Limit:  300, Total: 300, Remaining: 0,
		ResetAt: reset.Unix(), ResetAtMax: reset.Unix(),
	}
	e := FromVerdictAt(v, now)
	if e.Code != CodeRPMLimitExceeded || e.RetryAfterSeconds != 42 {
		t.Fatalf("code/retry = %s/%d, want rpm/42", e.Code, e.RetryAfterSeconds)
	}
	if *e.Limit != 300 || *e.Remaining != 0 || e.ResetAt != "2026-10-23T10:31:22+08:00" {
		t.Fatalf("optional trio = %v/%v/%s", *e.Limit, *e.Remaining, e.ResetAt)
	}
	if strings.Contains(e.Message, "同时触顶") {
		t.Fatalf("single-dimension message must not mention other hits: %q", e.Message)
	}
}

func Test429ConcurrencyFixedThreeSeconds(t *testing.T) {
	now := fixedNow()
	v := ratelimit.Verdict{
		Allowed: false, Code: ratelimit.CodeConcurrency,
		AllHit: []string{ratelimit.CodeConcurrency},
		Limit:  10, Remaining: 0,
		ResetAt: now.Add(time.Minute).Unix(),
	}
	e := FromVerdictAt(v, now)
	if e.RetryAfterSeconds != 3 {
		t.Fatalf("concurrency retry = %d, want 3", e.RetryAfterSeconds)
	}
	if e.ResetAt != "2026-10-23T10:30:43+08:00" {
		t.Fatalf("concurrency reset_at = %q, want now+3s", e.ResetAt)
	}
}

func Test429Headers(t *testing.T) {
	now := fixedNow()
	v := ratelimit.Verdict{
		Allowed: false, Code: ratelimit.CodeRPM,
		AllHit: []string{ratelimit.CodeRPM},
		Limit:  300, Total: 300, Remaining: 0,
		ResetAt: now.Add(42 * time.Second).Unix(),
	}
	e := FromVerdictAt(v, now).WithRateLimitSnapshot(RateLimitSnapshot{
		RPMLimit: 300, RPMRemaining: 0, RPMResetUnix: now.Add(42 * time.Second).Unix(),
		DailyLimit: 5000, DailyRemaining: 1200,
	})
	rec, _ := writeError(t, e)
	want := map[string]string{
		"Retry-After":                    "42",
		"X-RateLimit-Limit-Requests":     "300",
		"X-RateLimit-Remaining-Requests": "0",
		"X-RateLimit-Reset-Requests":     strconv.FormatInt(now.Add(42*time.Second).Unix(), 10),
		"X-Quota-Daily-Limit":            "5000",
		"X-Quota-Daily-Remaining":        "1200",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// A7: non-rate-limit bodies do not contain the three optional keys at all.
func TestNonRateLimitBodyOmitsOptionalKeys(t *testing.T) {
	for _, code := range []string{
		CodeInvalidAPIKey, CodeModelNotAvailable, CodeRequestInFlight,
		CodeUpstreamTimeout, CodeBackpressureRejected, CodeInternalError,
	} {
		rec, _ := writeError(t, New(code))
		body := rec.Body.String()
		for _, key := range []string{`"limit"`, `"remaining"`, `"reset_at"`} {
			if strings.Contains(body, key) {
				t.Errorf("%s: body contains %s: %s", code, key, body)
			}
		}
	}
}

func TestInternalErrorReplacesRequestIDPlaceholder(t *testing.T) {
	e := New(CodeInternalError)
	_, body := writeError(t, e)
	rid := body["request_id"].(string)
	if !strings.Contains(body["message"].(string), rid) {
		t.Fatalf("message %q lacks request id %q", body["message"], rid)
	}
}

func TestUnknownCodeFallsBackWithoutEcho(t *testing.T) {
	rec, body := writeError(t, Newf("not_a_real_code", "anything"))
	if body["code"] != CodeInternalError {
		t.Fatalf("code = %v, want internal_error", body["code"])
	}
	if strings.Contains(rec.Body.String(), "not_a_real_code") {
		t.Fatalf("unknown code leaked into the body: %s", rec.Body.String())
	}
}

func TestNilErrorStillWritesInternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Write(rec, nil); err != nil {
		t.Fatal(err)
	}
	body := decodeBody(t, rec.Body.Bytes())
	if body["code"] != CodeInternalError {
		t.Fatalf("code = %v", body["code"])
	}
}

func TestAuthAndRetryHeaders(t *testing.T) {
	rec, _ := writeError(t, New(CodeInvalidAPIKey))
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="api-gateway"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}

	rec, _ = writeError(t, New(CodeRateLimiterUnavailable))
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("503 Retry-After = %q, want 5", got)
	}
	if rec.Code != 503 {
		t.Fatalf("rate_limiter_unavailable status = %d, want 503", rec.Code)
	}
}

// A11: neither the body nor LogFields may contain the upstream base URL, key,
// SQL fragment or stack line, including text carried only by the cause.
func TestUpstreamLeakIsScrubbed(t *testing.T) {
	const (
		baseURL = "https://upstream.internal.example/v1/chat"
		key     = "sk-live-ABCDEF1234567890"
	)
	cause := errors.New("SELECT * FROM t_upstream_key WHERE k='" + key + "'; goroutine 1 [running]:\n\tmain.handler()")
	e := Newf(CodeUpstreamInvalidRequest,
		baseURL+" rejected credential "+key+"; SELECT * FROM t_upstream_key; goroutine 1 [running]:").
		WithCause(cause).
		WithRedactions(baseURL, key)

	rec, _ := writeError(t, e)
	body := rec.Body.String()
	for _, secret := range []string{baseURL, key, "upstream.internal.example", "SELECT * FROM", "goroutine 1"} {
		if strings.Contains(body, secret) {
			t.Fatalf("body leaked %q: %s", secret, body)
		}
	}

	logged := ""
	for _, v := range e.LogFields() {
		logged += " " + toPlainString(v)
	}
	for _, secret := range []string{baseURL, key, "upstream.internal.example", "SELECT * FROM", "goroutine 1"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("LogFields leaked %q: %s", secret, logged)
		}
	}
	if strings.Contains(e.Error(), key) || strings.Contains(e.Error(), baseURL) {
		t.Fatalf("Error() leaked upstream details: %q", e.Error())
	}
}

func TestUpstreamMessageIsTruncatedTo200Runes(t *testing.T) {
	long := strings.Repeat("错", 500)
	e := Newf(CodeUpstreamInvalidRequest, long)
	_, body := writeError(t, e)
	msg := body["message"].(string)
	if got := len([]rune(msg)); got > maxMessageRunes+1 {
		t.Fatalf("message runes = %d, want <= %d", got, maxMessageRunes+1)
	}
}

func toPlainString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	default:
		return ""
	}
}

func TestWriteIsDeterministicForSameInput(t *testing.T) {
	e := New(CodeInvalidAPIKey).WithRequestID("rid-1")
	rec1, body1 := writeError(t, e)
	rec2, body2 := writeError(t, e)
	if rec1.Body.String() != rec2.Body.String() {
		t.Fatalf("same input produced different bodies:\n%s\n%s", body1, body2)
	}
}

// Retry-After is derived from retry_after_seconds, not from a status-code
// allowlist. A 502 upstream_all_failed used to carry retry_after_seconds=30 and
// hint「按 30 秒退避重试」while sending no header at all — SDKs back off on the
// header, so they saw nothing (found 2026-11-05 during failure injection).
func TestRetryAfterHeaderMatchesBodyOnEveryRetryableStatus(t *testing.T) {
	cases := []struct {
		code   string
		status int
		secs   int
	}{
		{CodeUpstreamAllFailed, 502, 30},
		{CodeUpstreamInvalidResponse, 502, 10},
		{CodeUpstreamCapacityExceeded, 503, 30},
		{CodeUpstreamTimeout, 504, 30},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			rec, body := writeError(t, New(tc.code))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if got := int(body["retry_after_seconds"].(float64)); got != tc.secs {
				t.Fatalf("body retry_after_seconds = %d, want %d", got, tc.secs)
			}
			if got := rec.Header().Get("Retry-After"); got != strconv.Itoa(tc.secs) {
				t.Fatalf("Retry-After = %q, want %d", got, tc.secs)
			}
		})
	}
}

// 402 is the documented exception (E.1.2: top-up, not a wait). Even a caller
// that sets a value by mistake must not advertise it.
func Test402NeverSendsRetryAfter(t *testing.T) {
	e := NewInsufficientCredits(100, -1, 0).WithRetryAfter(30)
	rec, _ := writeError(t, e)
	if rec.Code != 402 {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("402 must not send Retry-After, got %q", got)
	}
}

// A caller-set backoff on a status that has no spec value still reaches the
// header (409 request_in_flight relies on this: TDS §4.5).
func TestCallerSetRetryAfterReachesHeaderOnConflict(t *testing.T) {
	rec, body := writeError(t, New(CodeRequestInFlight).WithRetryAfter(2))
	if rec.Code != 409 {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
	if got := int(body["retry_after_seconds"].(float64)); got != 2 {
		t.Fatalf("body retry_after_seconds = %d, want 2", got)
	}
}

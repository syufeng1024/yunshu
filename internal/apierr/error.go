package apierr

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"yunshu.dev/core/internal/jsonx"
)

// maxMessageRunes bounds upstream-derived text (PRD E.1.7: 脱敏截断 200 字).
const maxMessageRunes = 200

// maxRequestIDLen matches the middleware's 128-byte cap.
const maxRequestIDLen = 128

// APIError is the single error representation for every inbound protocol.
// OpenAI, Anthropic and Bailian share this body; only HTTP status and headers
// differ.
//
// Only the fields below are serialized. Cause and redactions stay in-process.
type APIError struct {
	Code              string
	Type              string
	Message           string
	RetryAfterSeconds int
	Hint              string
	RequestID         string
	ResetAt           string
	Limit             *float64
	Remaining         *float64

	// SSE-only fields. They are never serialized in a normal HTTP body.
	Partial         bool
	CreditsConsumed *int64

	HTTPStatus int
	Owner      Owner

	spec         Spec
	sse          bool
	hintOverride bool
	rateLimit    *RateLimitSnapshot
	redactions   []string
	cause        error
}

// Newf builds an error from its code and message interpolation arguments.
// An unknown code degrades to internal_error without echoing the unknown name.
func Newf(code string, args ...any) *APIError {
	spec, ok := Lookup(code)
	if !ok {
		spec, _ = Lookup(CodeInternalError)
	}
	msg := spec.Message
	if len(args) > 0 {
		msg = fmt.Sprintf(spec.Message, args...)
	}
	e := &APIError{
		Code:              spec.Code,
		Type:              spec.Type,
		Message:           msg,
		RetryAfterSeconds: spec.RetryAfterSeconds,
		Hint:              renderHint(spec.Hint, spec.RetryAfterSeconds),
		HTTPStatus:        spec.HTTPStatus,
		Owner:             spec.Owner,
		spec:              spec,
	}
	// Keep the body shape determined by type even before a caller fills the
	// values. 402 and 429 always carry their optional numeric fields; 429
	// also always carries reset_at.
	if spec.Optional != OptionalNone {
		zero := 0.0
		e.Limit = &zero
		e.Remaining = &zero
	}
	if spec.Optional == OptionalRateLimit {
		e.setResetAt(time.Now().Add(time.Duration(spec.RetryAfterSeconds) * time.Second))
	}
	return e
}

// New is Newf without interpolation arguments.
func New(code string) *APIError { return Newf(code) }

// WithRequestID attaches a caller-supplied request id. Invalid values (CR/LF,
// non-printable or over 128 bytes) are ignored so Write generates a fresh one.
func (e *APIError) WithRequestID(id string) *APIError {
	if e == nil {
		return e
	}
	if clean := sanitizeRequestID(id); clean != "" {
		e.RequestID = clean
	}
	return e
}

// WithLimit sets the optional limit field (402: overdraft ceiling; 429:
// main-dimension ceiling). It is ignored at write time for codes whose spec
// does not allow the field.
func (e *APIError) WithLimit(limit float64) *APIError {
	if e != nil {
		e.Limit = &limit
	}
	return e
}

// WithRemaining sets the optional remaining field. For 402 it may be negative
// (欠费); Write uses this same pointer for X-Credits-Remaining.
func (e *APIError) WithRemaining(remaining float64) *APIError {
	if e != nil {
		e.Remaining = &remaining
	}
	return e
}

// WithResetAt sets reset_at from a time. 402 ignores it by spec.
func (e *APIError) WithResetAt(t time.Time) *APIError {
	if e != nil {
		e.setResetAt(t)
	}
	return e
}

// WithResetAtUnix sets reset_at from unix seconds (rate limiter output).
func (e *APIError) WithResetAtUnix(sec int64) *APIError {
	if e != nil {
		e.setResetAt(time.Unix(sec, 0))
	}
	return e
}

// WithRetryAfter overrides retry_after_seconds and re-renders hint so the
// numeric text and the field cannot drift apart.
func (e *APIError) WithRetryAfter(seconds int) *APIError {
	if e != nil {
		if seconds < 0 {
			seconds = 0
		}
		e.RetryAfterSeconds = seconds
		e.Hint = renderHint(e.spec.Hint, seconds)
		// 429's reset_at and retry_after_seconds must name the same instant
		// (E.6 boundary ①). FromVerdict overrides this immediately with the
		// limiter's exact reset; direct callers still get a coherent pair.
		if e.spec.Optional == OptionalRateLimit {
			e.setResetAt(time.Now().Add(time.Duration(seconds) * time.Second))
		}
	}
	return e
}

// WithHint replaces the spec hint for this instance. It is reserved for codes
// shared by several resources where the correct next step depends on which
// resource was refused — resource_state_conflict covers orders, refunds,
// quota applications and model listing transitions, and only the call site
// knows which page the operator must open. The text is sanitized and rendered
// at write time exactly like a spec hint.
func (e *APIError) WithHint(hint string) *APIError {
	if e != nil {
		e.Hint = hint
		e.hintOverride = true
	}
	return e
}

// WithMessage replaces the code's default message for this instance. It exists
// for the same reason WithHint does: a code shared by several resources can be
// applied in a narrower context where the table's wording is simply wrong.
//
// invalid_credentials says "账号或密码不正确". When the caller submitted only a
// verification code there is no password involved, and the user goes hunting
// for a password problem that does not exist (reported as a bug). The code and
// the HTTP status are unchanged, so clients that branch on `code` are not
// affected — only the sentence they show.
func (e *APIError) WithMessage(msg string) *APIError {
	if e != nil {
		e.Message = msg
	}
	return e
}

// WithRateLimitSnapshot supplies the five 429/200 rate-limit headers. The
// limiter's Verdict only describes the main dimension, so callers that know
// the full snapshot must pass it explicitly.
func (e *APIError) WithRateLimitSnapshot(s RateLimitSnapshot) *APIError {
	if e != nil {
		e.rateLimit = &s
	}
	return e
}

// WithCreditsConsumed sets the SSE-only credits_consumed field. It has no
// effect on a normal HTTP body.
func (e *APIError) WithCreditsConsumed(credits int64) *APIError {
	if e != nil {
		e.CreditsConsumed = &credits
	}
	return e
}

// WithRedactions registers literal secrets (upstream base URLs, keys) that
// must not appear in the response body or log fields.
func (e *APIError) WithRedactions(secrets ...string) *APIError {
	if e == nil {
		return e
	}
	for _, s := range secrets {
		if s != "" {
			e.redactions = append(e.redactions, s)
		}
	}
	return e
}

// WithCause records an internal cause for logs. It is never serialized.
func (e *APIError) WithCause(err error) *APIError {
	if e != nil {
		e.cause = err
	}
	return e
}

// Cause returns the internal cause (never sent to clients).
func (e *APIError) Cause() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Unwrap exposes the cause to errors.Is/As without exposing it on the wire.
func (e *APIError) Unwrap() error { return e.Cause() }

// EnsureRequestID fills an empty/invalid id with a UUIDv7-shaped value and
// writes it back, so X-Request-Id and body request_id are always equal.
func (e *APIError) EnsureRequestID() string {
	if e == nil {
		return ""
	}
	e.RequestID = sanitizeRequestID(e.RequestID)
	if e.RequestID == "" {
		e.RequestID = NewRequestID()
	}
	return e.RequestID
}

func (e *APIError) setResetAt(t time.Time) {
	e.ResetAt = t.In(shanghai).Format("2006-01-02T15:04:05-07:00")
}

// normalize turns a zero-value or partially built APIError into the
// documented internal_error shape, preserving a request id if one exists.
// Business code should never construct APIError literals, but the unique
// write path must still not emit status 0 or an empty code if it happens.
func normalize(e *APIError) *APIError {
	if e != nil && e.Code != "" && e.Type != "" && e.HTTPStatus != 0 {
		return e
	}
	rid := ""
	if e != nil {
		rid = e.RequestID
	}
	return New(CodeInternalError).WithRequestID(rid)
}

// MarshalJSON emits {"error":{...}} and, like the write path, guarantees a
// non-empty request_id.
func (e *APIError) MarshalJSON() ([]byte, error) {
	return jsonx.Marshal(errorEnvelope{Error: normalize(e).object()})
}

type errorEnvelope struct {
	Error errorObject `json:"error"`
}

type errorObject struct {
	Type              string   `json:"type"`
	Code              string   `json:"code"`
	Message           string   `json:"message"`
	RetryAfterSeconds int      `json:"retry_after_seconds"`
	ResetAt           string   `json:"reset_at,omitempty"`
	Limit             *float64 `json:"limit,omitempty"`
	Remaining         *float64 `json:"remaining,omitempty"`
	Hint              string   `json:"hint"`
	RequestID         string   `json:"request_id"`
	Partial           *bool    `json:"partial,omitempty"`
	CreditsConsumed   *int64   `json:"credits_consumed,omitempty"`
}

// object builds the wire form. Optional fields are gated by the spec, not by
// whether the caller happened to set them: a non-rate-limit code can never
// accidentally emit limit/remaining/reset_at.
func (e *APIError) object() errorObject {
	if e == nil {
		return New(CodeInternalError).object()
	}
	rid := e.EnsureRequestID()
	settings := e.spec
	if settings.Code == "" {
		if s, ok := Lookup(e.Code); ok {
			settings = s
		} else {
			settings, _ = Lookup(CodeInternalError)
		}
	}
	msg := sanitizeText(e.Message, e.redactions, true)
	msg = strings.ReplaceAll(msg, "{request_id}", rid)
	// A fallback message keeps the required field non-empty even if a spec is
	// ever added without one.
	if msg == "" {
		msg = "服务内部错误，请求 ID " + rid
	}
	// Render from the spec at write time so a direct change to
	// RetryAfterSeconds cannot leave "{retry}" unresolved in hint.
	hint := renderHint(settings.Hint, e.RetryAfterSeconds)
	if e.hintOverride {
		hint = renderHint(e.Hint, e.RetryAfterSeconds)
	} else if settings.Hint == "" {
		hint = e.Hint
	}
	hint = sanitizeText(hint, e.redactions, false)
	if hint == "" {
		hint = "携 request_id 联系客服"
	}
	obj := errorObject{
		Type:              e.Type,
		Code:              e.Code,
		Message:           msg,
		RetryAfterSeconds: e.RetryAfterSeconds,
		Hint:              hint,
		RequestID:         rid,
	}
	switch settings.Optional {
	case OptionalRateLimit:
		obj.ResetAt = e.ResetAt
		obj.Limit = e.Limit
		obj.Remaining = e.Remaining
	case OptionalBilling:
		// 402 deliberately has no reset_at: charging is a human action.
		obj.Limit = e.Limit
		obj.Remaining = e.Remaining
	}
	if e.sse {
		partial := true
		obj.Partial = &partial
		credits := int64(0)
		if e.CreditsConsumed != nil {
			credits = *e.CreditsConsumed
		}
		obj.CreditsConsumed = &credits
	}
	return obj
}

// LogFields returns a sanitized, structured record for the request logger.
// The cause is intentionally absent: it may contain SQL, a stack trace or an
// upstream credential.
func (e *APIError) LogFields() map[string]any {
	if e == nil {
		return map[string]any{"code": CodeInternalError}
	}
	rid := e.EnsureRequestID()
	return map[string]any{
		"code":                e.Code,
		"type":                e.Type,
		"owner":               string(e.Owner),
		"status":              e.HTTPStatus,
		"request_id":          rid,
		"retry_after_seconds": e.RetryAfterSeconds,
		"message":             sanitizeText(e.Message, e.redactions, true),
		"hint":                sanitizeText(e.Hint, e.redactions, false),
	}
}

// String makes accidental fmt/error logging safe: it never includes cause.
func (e *APIError) String() string {
	if e == nil {
		return "apierr:<nil>"
	}
	return e.Code + ": " + sanitizeText(e.Message, e.redactions, true)
}

// Error lets APIError satisfy error without leaking internals.
func (e *APIError) Error() string { return e.String() }

// Is lets callers compare codes without string matching at each site.
func (e *APIError) Is(target error) bool {
	var other *APIError
	return errors.As(target, &other) && other != nil && e != nil && other.Code == e.Code
}

var (
	// URLs with a host are always withheld; the literal "https://" followed by
	// a space (as in the image_url hint) is not a URL and is left alone.
	urlPattern    = regexp.MustCompile(`https?://[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]+`)
	bearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{8,}=*`)
	secretPattern = regexp.MustCompile(`(?i)\b(?:sk|ak|pk|key)[-_][A-Za-z0-9_-]{8,}`)
	stackPattern  = regexp.MustCompile(`(?m)^\s*(goroutine \d+|panic:|fatal error:|\tat |at [\w.$/-]+\(|File ".*", line \d+).*$`)
	// Only statement-shaped fragments are redacted; a bare English "update"
	// in an upstream message must survive.
	sqlPattern = regexp.MustCompile(`(?i)\b(?:SELECT\b[^\n]{0,200}\bFROM|UPDATE\b[^\n]{0,200}\bSET|DELETE\s+FROM|INSERT\s+INTO|CREATE\s+TABLE|ALTER\s+TABLE)\b[^\n]{0,200}`)
)

// SanitizeText removes URLs, credentials, stack lines and SQL fragments, then
// truncates to 200 runes. Exported so upstream adapters can prepare a message
// before passing it to Newf.
func SanitizeText(s string) string { return sanitizeText(s, nil, true) }

func sanitizeText(s string, redactions []string, truncate bool) string {
	if s == "" {
		return ""
	}
	for _, secret := range redactions {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	s = urlPattern.ReplaceAllString(s, "[redacted-url]")
	s = bearerPattern.ReplaceAllString(s, "Bearer [redacted]")
	s = secretPattern.ReplaceAllString(s, "[redacted-key]")
	s = stackPattern.ReplaceAllString(s, "")
	s = sqlPattern.ReplaceAllString(s, "[redacted-sql]")
	if truncate {
		s = truncateRunes(s, maxMessageRunes)
	}
	return strings.TrimSpace(s)
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

// sanitizeRequestID keeps visible ASCII only and caps the length. An empty or
// unsafe value signals "generate a fresh one".
func sanitizeRequestID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > maxRequestIDLen {
		return ""
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return ""
		}
	}
	return id
}

var shanghai = time.FixedZone("CST", 8*3600)

// NewRequestID returns a UUIDv7-shaped, time-ordered identifier.
func NewRequestID() string { return newRequestIDAt(time.Now()) }

func newRequestIDAt(now time.Time) string {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := crand.Read(b[6:]); err != nil {
		ns := uint64(now.UnixNano())
		for i := 6; i < 16; i++ {
			b[i] = byte(ns >> (8 * uint(i-6)))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x70 // UUID version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// formatNumber matches encoding/json's float64 rendering so a header and a
// body field sourced from the same variable are byte-identical.
func formatNumber(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

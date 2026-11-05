package apierr

import (
	"net/http"
	"strconv"
	"yunshu.dev/core/internal/jsonx"
)

// Write is the only function allowed to put an error response on the wire.
// Business packages must call it instead of http.Error, WriteHeader or
// json.NewEncoder(w) so request_id, body shape and headers cannot drift.
//
// It fills a missing request_id, serializes first (so a marshal failure does
// not leave a half-written response), then sets headers, status and body.
func Write(w http.ResponseWriter, e *APIError) error {
	e = normalize(e)
	rid := e.EnsureRequestID()

	payload, err := jsonx.Marshal(e)
	if err != nil {
		// Keep the contract even if a caller supplied a non-finite float:
		// fall back to internal_error with the same request id.
		e = New(CodeInternalError).WithRequestID(rid)
		payload, err = jsonx.Marshal(e)
		if err != nil {
			return err
		}
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Request-Id", rid)
	applyErrorHeaders(h, e)

	w.WriteHeader(e.HTTPStatus)
	_, err = w.Write(payload)
	return err
}

// applyErrorHeaders is shared by Write and the SSE path only for the id
// (SSE headers are frozen before the first byte, so it does not call this).
func applyErrorHeaders(h http.Header, e *APIError) {
	switch e.HTTPStatus {
	case http.StatusUnauthorized:
		h.Set("WWW-Authenticate", `Bearer realm="api-gateway"`)
	case http.StatusPaymentRequired:
		// Same variable as the body's remaining; negative is a real debt.
		if e.Remaining != nil {
			h.Set("X-Credits-Remaining", formatNumber(*e.Remaining))
		}
	case http.StatusTooManyRequests:
		if e.rateLimit != nil {
			applyRateLimitHeaders(h, *e.rateLimit)
		}
	}
	// Retry-After follows retry_after_seconds, not the status code. Any code
	// whose spec carries a positive backoff must advertise it in the header:
	// SDKs back off on the header, never on the body field, so a 502
	// upstream_all_failed used to say "retry in 30s" in the body while sending
	// no header at all. 402 is the one documented exception (E.1.2: the fix is
	// a top-up, not a wait), and it never carries a value anyway.
	if e.RetryAfterSeconds > 0 && e.HTTPStatus != http.StatusPaymentRequired {
		h.Set("Retry-After", strconv.Itoa(e.RetryAfterSeconds))
	}
}

package dispatch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/stream"
)

// E-09's templates for upstream_timeout and upstream_all_failed both contain
// "%d 次". Two ways to get it wrong, both covered here:
//
//  1. Building them with New instead of Newf ships the literal placeholder to
//     the user (found 2026-10-29 in a T39 counter-proof).
//  2. Passing the 1-based attempt number instead of the retry count tells the
//     user "已重试 2 次" after a single retry (found 2026-11-05 in a 500-once
//     injection run: two attempts, message claimed two retries).
func TestClassifyTransportErrorFormatsAttemptCount(t *testing.T) {
	t.Run("timeout after two retries", func(t *testing.T) {
		class, api := classifyTransportError(timeoutErr{}, 3)
		if class != ClassTimeout || api.Code != apierr.CodeUpstreamTimeout {
			t.Fatalf("class/code = %s/%s", class, api.Code)
		}
		assertInterpolated(t, api.Message, "已重试 2 次")
	})

	t.Run("connection refused after one retry", func(t *testing.T) {
		class, api := classifyTransportError(errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), 2)
		if class != ClassConnectRefused || api.Code != apierr.CodeUpstreamAllFailed {
			t.Fatalf("class/code = %s/%s", class, api.Code)
		}
		assertInterpolated(t, api.Message, "已重试 1 次")
	})

	t.Run("first attempt has no retry to report", func(t *testing.T) {
		_, api := classifyTransportError(context.DeadlineExceeded, 1)
		// "已重试 0 次" would read like a defect; the message says plainly that
		// nothing was retried instead.
		assertInterpolated(t, api.Message, "本次未重试")
		if strings.Contains(api.Message, "已重试") {
			t.Fatalf("first attempt must not claim a retry: %q", api.Message)
		}
	})

	t.Run("upstream 5xx uses the same rule", func(t *testing.T) {
		api := upstreamStatusError(http.StatusBadGateway, 2)
		assertInterpolated(t, api.Message, "已重试 1 次")

		api = upstreamStatusError(http.StatusGatewayTimeout, 1)
		assertInterpolated(t, api.Message, "本次未重试")
	})
}

// assertInterpolated fails when a verb survived formatting or the expected
// count is missing.
func assertInterpolated(t *testing.T, message, want string) {
	t.Helper()
	if strings.Contains(message, "%!") || strings.Contains(message, "%d") {
		t.Fatalf("message still carries an unformatted verb: %q", message)
	}
	if !strings.Contains(message, want) {
		t.Fatalf("message = %q, want it to contain %q", message, want)
	}
}

// TestConnectionRefusedBodyIsUserReadable proves the whole path, not just the
// classifier: a closed loopback port must yield a 502 whose body has real
// numbers and no placeholder.
func TestConnectionRefusedBodyIsUserReadable(t *testing.T) {
	_, envelope := testBox(t)
	cat := testCatalog(t, "http://127.0.0.1:1/v1", envelope, nil)
	d := newDispatcher(t, cat, &fakeLedger{}, &attemptRecorder{}, &fakeIdem{})

	rec := serve(t, d, testCtx(false))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "%d") || strings.Contains(body, "%!") {
		t.Fatalf("body carries an unformatted verb: %s", body)
	}
	// This dispatcher makes a single attempt (connection refused with no retry
	// budget), so the message must not claim a retry happened.
	if !strings.Contains(body, "本次未重试") {
		t.Fatalf("body = %s, want 本次未重试", body)
	}
	if got := errorCode(t, rec)["code"]; got != apierr.CodeUpstreamAllFailed {
		t.Fatalf("code = %v, want %s", got, apierr.CodeUpstreamAllFailed)
	}
	// The body advertises a 30s backoff; SDKs only read the header, so the two
	// must agree (502 used to ship the field without the header).
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30 (body says %v)",
			got, errorCode(t, rec)["retry_after_seconds"])
	}
}

// timeoutErr is a net.Error whose Timeout() is true.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

// The SSE error frame uses the same templates as the unary body, so it must
// follow the same retry-count rule: a bare New shipped a literal "%d 次" inside
// the frame whenever the stream died after the first byte (no retry possible).
func TestStreamFailureAPIReportsRetryCount(t *testing.T) {
	t.Run("mid-stream read error after one retry", func(t *testing.T) {
		api := streamFailureAPI(stream.Result{LastError: errors.New("read: connection reset")}, 2)
		assertInterpolated(t, api.Message, "已重试 1 次")
		if api.RetryAfterSeconds != 30 {
			t.Fatalf("retry_after = %d, want 30", api.RetryAfterSeconds)
		}
	})

	t.Run("idle before any retry", func(t *testing.T) {
		api := streamFailureAPI(stream.Result{LastError: stream.ErrUpstreamIdle}, 1)
		assertInterpolated(t, api.Message, "本次未重试")
	})

	t.Run("start failure uses the caller's attempt number", func(t *testing.T) {
		_, api := classifyStreamStartError(stream.ErrBeforeFirstByte, 2)
		assertInterpolated(t, api.Message, "已重试 1 次")
	})
}

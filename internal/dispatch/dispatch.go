// Package dispatch implements P15–P17 and drives P18–P25: upstream connection,
// first-byte wait, unary body read, retry/degrade, and the terminal that
// replaces T07's temporary not-wired handler.
//
// Every attempt produces one in-memory Attempt. T09 persists it; this package
// never writes t_upstream_attempt and never stores an upstream body except the
// sanitized upstream_msg prefix and the in-flight unary response it must
// forward.
package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/billing"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/cryptobox"
	"yunshu.dev/core/internal/gw"
	obslog "yunshu.dev/core/internal/obs/log"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/settle"
	"yunshu.dev/core/internal/stream"
	"yunshu.dev/relaykit/anthropic"
	"yunshu.dev/relaykit/bailian"
	"yunshu.dev/relaykit/ir"
	"yunshu.dev/relaykit/openai"
)

const (
	// maxUnaryBytes bounds a non-streaming upstream body. A larger body is a
	// malformed response, not something to buffer without bound.
	maxUnaryBytes = 16 * 1024 * 1024
	// maxErrorBodyBytes bounds the body read for an error message.
	maxErrorBodyBytes = 8 * 1024
	// maxReplayBody caps the response body stored for idempotent replay. Larger
	// bodies are marked DONE without a body; a stream replay is already 409.
	maxReplayBody = 64 * 1024
	// defaultMaxConnsPerChannel is the per-channel MaxConnsPerHost.
	defaultMaxConnsPerChannel = 64
)

// StreamUsageModeWriter is the only write-back path for the E-14 automatic
// downgrade. A nil writer must WARN and never touch the database.
type StreamUsageModeWriter interface {
	SetStreamUsageMode(ctx context.Context, channelID int64, mode string) error
}

// Options assembles the dispatcher. The KEK is optional: without it every
// request is rejected with 503 upstream_key_unavailable (the credential, not the
// routing table, is what is missing), never served with plaintext.
type Options struct {
	Catalog *gw.Catalog
	Tokens  billing.InputCounter
	Ledger  gw.LedgerStore
	Balance gw.BalanceReader
	Config  *config.Store
	Logger  *obslog.Logger
	Idem    gw.IdemStore

	// Metrics is optional. Recording never changes an attempt decision.
	Metrics *obsmetrics.Recorder

	KEK *cryptobox.Box

	Cooldown    CooldownSetter
	AttemptSink func(Attempt)
	// UsageSink receives exactly one request-level row per terminal call
	// (P28). It must be non-blocking; the writer drops rows rather than block.
	UsageSink func(settle.UsageRecord)
	// MetricSink receives the K14/K15 contribution (P30). Two deltas are
	// emitted: USER (carrying the channel id) and KEY (without it, so the
	// channel counter is not incremented twice).
	MetricSink func(settle.MetricDelta)
	// SettleEnqueuer is P26. When nil, settlement stays synchronous (T08
	// behaviour). When non-nil, an enqueue failure falls back to the
	// synchronous ledger call rather than failing the request.
	SettleEnqueuer  SettleEnqueuer
	UsageModeWriter StreamUsageModeWriter
	// AutoProbeStreamUsage defaults to true (TDS §4.2 / errata E-14).
	AutoProbeStreamUsage *bool
	MaxConnsPerChannel   int
}

// SettleEnqueuer is the P26 surface. settle.Enqueuer implements it.
type SettleEnqueuer interface {
	Enqueue(ctx context.Context, p settle.EnqueueParams) error
}

// Dispatcher is the gw.Terminal implementation.
type Dispatcher struct {
	opts Options

	mu         sync.Mutex
	transports map[transportKey]*http.Transport
	probes     map[int64]*probeState
}

type transportKey struct {
	channelID   int64
	connectMS   int64
	firstByteMS int64
	maxConns    int
}

type probeState struct {
	misses     int
	downgraded bool
}

// New builds a dispatcher. It deliberately does not validate the KEK: a missing
// KEK is a per-request 503 (and an ERROR log), not a boot failure that hides
// other configuration problems.
func New(opts Options) *Dispatcher {
	if opts.MaxConnsPerChannel <= 0 {
		opts.MaxConnsPerChannel = defaultMaxConnsPerChannel
	}
	return &Dispatcher{
		opts:       opts,
		transports: map[transportKey]*http.Transport{},
		probes:     map[int64]*probeState{},
	}
}

type target struct {
	route gw.RouteCandidate
	ch    *gw.ChannelEntry
}

// Serve implements gw.Terminal.
//
// It writes exactly one t_usage_record row per terminal call (P28), including
// the paths that never reach an upstream. The row is emitted after the response
// is written, so http_status and consumed_micro are the values the client saw.
func (d *Dispatcher) Serve(w http.ResponseWriter, r *http.Request, c *gw.Ctx) {
	var out outcome
	started := time.Now()
	defer func() { d.emitUsage(c, out, started) }()

	if c == nil || c.IR == nil || c.Price == nil || c.Auth == nil {
		d.settle(r.Context(), c, 0, "UPSTREAM_FAIL")
		api := apierr.New(apierr.CodeInternalError)
		out.apiErr = api
		out.result = "FAILED"
		d.fail(w, r, c, api, ClassNone, 0)
		return
	}
	targets, configMissing, problems := d.targets(c)
	if len(targets) == 0 {
		if d.opts.Logger != nil {
			d.opts.Logger.Error(r.Context(), "upstream_targets_unavailable: "+strings.Join(problems, ","),
				obslog.Fields{Stage: "P12", RequestID: c.RequestID})
		}
		code := apierr.CodeNoAvailableUpstream
		if configMissing {
			code = apierr.CodeConfigUnavailable
		}
		d.settle(r.Context(), c, 0, "UPSTREAM_FAIL")
		api := apierr.New(code)
		out.apiErr = api
		out.result = "FAILED"
		d.fail(w, r, c, api, ClassNone, 0)
		return
	}
	out = d.dispatch(w, r, c, targets)
}

func (d *Dispatcher) targets(c *gw.Ctx) ([]target, bool, []string) {
	if d.opts.Catalog == nil {
		return nil, true, []string{"catalog"}
	}
	snap, err := d.opts.Catalog.Current()
	if err != nil {
		return nil, true, []string{"routing_snapshot"}
	}
	routes := c.Routes
	if len(routes) == 0 && c.Route != nil {
		routes = []gw.RouteCandidate{*c.Route}
	}
	var (
		out      []target
		missing  bool
		problems []string
	)
	for _, rt := range routes {
		ch, ok := snap.Channel(rt.ChannelID)
		if !ok {
			missing = true
			problems = append(problems, fmt.Sprintf("channel_%d_missing", rt.ChannelID))
			continue
		}
		if reason := channelProblem(ch, rt, c.Stream); reason != "" {
			missing = true
			problems = append(problems, reason)
			continue
		}
		out = append(out, target{route: rt, ch: ch})
	}
	return out, missing, problems
}

// channelProblem applies D-14's "missing field => refuse, never guess" rule.
// The returned text is a field name list; values (and especially the key) are
// never included.
func channelProblem(ch *gw.ChannelEntry, rt gw.RouteCandidate, stream bool) string {
	var missing []string
	if strings.TrimSpace(ch.BaseURL) == "" {
		missing = append(missing, "base_url")
	}
	if strings.TrimSpace(ch.UpstreamKeyEnc) == "" {
		missing = append(missing, "upstream_key_enc")
	} else if !strings.HasPrefix(ch.UpstreamKeyEnc, cryptobox.EnvelopePrefix) {
		missing = append(missing, "upstream_key_enc_not_v1")
	}
	if ch.TimeoutConnectMS <= 0 {
		missing = append(missing, "timeout_connect_ms")
	}
	if ch.TimeoutFirstByteMS <= 0 {
		missing = append(missing, "timeout_first_byte_ms")
	}
	if ch.TimeoutTotalMS <= 0 {
		missing = append(missing, "timeout_total_ms")
	}
	if ch.RetryMax == nil {
		missing = append(missing, "retry_max")
	}
	if ch.UpstreamModelNameMap == nil {
		missing = append(missing, "upstream_model_name_map")
	}
	if _, ok := protocolName(ch.ProtocolID); !ok {
		missing = append(missing, "protocol_id")
	}
	if strings.TrimSpace(rt.UpstreamModelName) == "" {
		// The route must name a model. upstream_model_name_map is a mapping keyed
		// by the client model name, not a substitute for an empty route name.
		missing = append(missing, "upstream_model")
	}
	if stream && !validUsageMode(ch.StreamUsageMode) {
		missing = append(missing, "stream_usage_mode")
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("channel_%d:%s", ch.ID, strings.Join(missing, "|"))
}

func validUsageMode(mode string) bool {
	switch mode {
	case "NATIVE", "STREAM_OPTIONS", "ESTIMATE", "UNKNOWN":
		return true
	default:
		return false
	}
}

func protocolName(id int64) (string, bool) {
	switch id {
	case 1:
		return stream.ProtoBailian, true
	case 2:
		return stream.ProtoOpenAI, true
	case 3:
		return stream.ProtoAnthropic, true
	default:
		return "", false
	}
}

type outcome struct {
	attempt Attempt
	class   string

	// finished means the response (or its terminal error frame) was written and
	// settlement already ran; dispatch must stop without touching the writer.
	finished bool
	// clientAborted means the caller is gone; nothing can be written.
	clientAborted bool

	apiErr *apierr.APIError

	// tokens are filled for a started stream so the attempt row is complete.
	tokenIn  int64
	tokenOut int64
	result   string

	// usageSource is UPSTREAM_API / UPSTREAM_STREAM / LOCAL_ESTIMATE / NONE.
	usageSource string
	// consumedMicro is the amount settlement charged (0 on failure). It is
	// written to t_usage_record.consumed_micro.
	consumedMicro int64
	// attempts and upstreamMS aggregate every attempt made for this request.
	attempts   int
	upstreamMS int64
}

func (d *Dispatcher) dispatch(w http.ResponseWriter, r *http.Request, c *gw.Ctx, targets []target) outcome {
	idx := 0
	attempts := 0
	var upstreamMS int64
	for {
		if r.Context().Err() != nil {
			if d.opts.Metrics != nil {
				d.opts.Metrics.SettleFallback("disconnect")
			}
			c.ErrCode = apierr.CodeClientAborted
			d.settle(r.Context(), c, 0, "CLIENT_ABORT")
			return outcome{clientAborted: true, class: ClassClientAbort, result: "FAILED", attempts: attempts}
		}
		out := d.try(r.Context(), w, c, targets[idx], attempts+1)
		attempts++
		upstreamMS += out.attempt.TotalMS
		out.attempt.AttemptIndex = attempts
		out.attempt.IsRetryable = Retryable(out.class)
		if out.result != "" {
			out.attempt.Result = out.result
		}
		out.attempts = attempts
		out.upstreamMS = upstreamMS
		remaining := d.remaining(c)
		hasNext := idx+1 < len(targets)
		retry, decision := DecideRetry(RetryInput{
			Attempts:         attempts,
			MaxAttempts:      d.maxAttempts(targets[idx].ch),
			FlushedBytes:     0,
			Class:            out.class,
			Remaining:        remaining,
			Threshold:        d.retryThreshold(c.Stream),
			HasNextCandidate: hasNext,
		})
		out.attempt.RetryDecision = decision
		d.record(out.attempt)

		if out.clientAborted {
			if d.opts.Metrics != nil {
				d.opts.Metrics.SettleFallback("disconnect")
			}
			c.ErrCode = apierr.CodeClientAborted
			d.settle(r.Context(), c, 0, "CLIENT_ABORT")
			out.attempts = attempts
			out.upstreamMS = upstreamMS
			return out
		}
		if out.finished {
			return out
		}
		if retry {
			if decision == DecisionNextCandidate {
				idx++
			}
			if !backoff(r.Context()) {
				d.failFrom(w, r, c, out, attempts)
				out.attempts = attempts
				out.upstreamMS = upstreamMS
				return out
			}
			continue
		}
		d.failFrom(w, r, c, out, attempts)
		out.attempts = attempts
		out.upstreamMS = upstreamMS
		return out
	}
}

func (d *Dispatcher) failFrom(w http.ResponseWriter, r *http.Request, c *gw.Ctx, out outcome, attempts int) {
	api := out.apiErr
	if api == nil {
		// Same template as the classified paths: a bare New would ship the
		// literal "%d" to the client.
		api = retriedError(apierr.CodeUpstreamAllFailed, attempts)
	}
	reason := "UPSTREAM_FAIL"
	switch out.class {
	case ClassTimeout:
		reason = "TIMEOUT"
	case ClassClientAbort:
		reason = "CLIENT_ABORT"
	}
	d.settle(r.Context(), c, 0, reason)
	if r.Context().Err() != nil {
		// The client is gone; writing would only fail. The hold is released.
		c.ErrCode = apierr.CodeClientAborted
		return
	}
	d.fail(w, r, c, api, out.class, attempts)
}

func (d *Dispatcher) fail(w http.ResponseWriter, r *http.Request, c *gw.Ctx, api *apierr.APIError, class string, attempts int) {
	if c == nil {
		_ = apierr.Write(w, api)
		return
	}
	c.ErrCode = api.Code
	api = api.WithRequestID(c.RequestID)
	if d.opts.Logger != nil {
		d.opts.Logger.Warn(r.Context(), "upstream_dispatch_failed: "+api.Code, obslog.Fields{
			Stage: "P15", RequestID: c.RequestID, Status: api.HTTPStatus,
		})
	}
	_ = apierr.Write(w, api)
}

func (d *Dispatcher) try(ctx context.Context, w http.ResponseWriter, c *gw.Ctx, t target, attemptNo int) outcome {
	start := time.Now()
	protoName, _ := protocolName(t.ch.ProtocolID)
	out := outcome{attempt: Attempt{
		AttemptIndex:    attemptNo,
		RequestID:       c.RequestID,
		UserID:          c.Auth.UserID,
		ModelID:         c.Model.ID,
		RouteID:         t.route.RouteID,
		ChannelID:       t.ch.ID,
		ChannelNameSnap: t.ch.Name,
		ProtocolCode:    protoName,
		UpstreamModel:   t.route.UpstreamModelName,
		Phase:           "P15",
	}}

	plain, err := d.decryptKey(t.ch)
	if err != nil {
		if d.opts.Logger != nil {
			// The ciphertext and the plaintext are both absent from this line.
			d.opts.Logger.Error(ctx, fmt.Sprintf("channel_key_decrypt_failed channel=%d: %s", t.ch.ID, err),
				obslog.Fields{Stage: "P15", RequestID: c.RequestID, Status: 503})
		}
		out.apiErr = apierr.New(apierr.CodeUpstreamKeyUnavailable)
		out.attempt.ErrorClass = ClassNone
		out.attempt.Phase = "P14"
		out.attempt.UpstreamMsg = "channel key unavailable"
		return out
	}
	// The plaintext lives only in this function. It is cleared before return and
	// is never copied into Ctx, logs, attempt rows or error bodies.
	defer func() { plain = "" }()

	body, upstreamModel, err := d.buildBody(c, t)
	if err != nil {
		out.apiErr = apierr.New(apierr.CodeConfigUnavailable).WithCause(err)
		out.attempt.ErrorClass = ClassNone
		out.attempt.Phase = "P14"
		return out
	}
	out.attempt.UpstreamModel = upstreamModel
	out.attempt.BytesIn = int64(len(body))

	req, times, err := d.buildHTTP(ctx, c, t, body, plain)
	if err != nil {
		out.apiErr = apierr.New(apierr.CodeConvertFailed).WithCause(err)
		out.attempt.ErrorClass = ClassNone
		out.attempt.Phase = "P14"
		return out
	}

	attemptCtx, cancel := d.attemptContext(ctx, c, t.ch, attemptNo)
	defer cancel()
	req = req.WithContext(attemptCtx)

	resp, err := d.transportFor(t.ch).RoundTrip(req)
	out.attempt.ConnectMS = times.connect.Load()
	out.attempt.TotalMS = time.Since(start).Milliseconds()
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			out.clientAborted = true
			out.class = ClassClientAbort
			out.attempt.ErrorClass = ClassClientAbort
			return out
		}
		out.class, out.apiErr = classifyTransportError(err, attemptNo)
		out.attempt.ErrorClass = out.class
		out.attempt.Phase = "P15"
		out.attempt.UpstreamMsg = sanitizeUpstreamMsg(err.Error(), plain)
		return out
	}
	defer resp.Body.Close()
	out.attempt.HTTPStatus = resp.StatusCode
	out.attempt.UpstreamCode = resp.StatusCode
	out.attempt.FirstByteMS = times.firstByte.Load()
	if out.attempt.FirstByteMS == 0 {
		out.attempt.FirstByteMS = out.attempt.TotalMS
	}
	if c.Stream && out.attempt.FirstByteMS > 0 && d.opts.Metrics != nil {
		d.opts.Metrics.TTFT(modelLabel(c), t.ch.ID,
			time.Duration(out.attempt.FirstByteMS)*time.Millisecond)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		raw := readErrorBody(resp.Body)
		out.class = ClassHTTP4XX
		out.attempt.Phase = "P16"
		out.attempt.ErrorClass = ClassHTTP4XX
		out.attempt.UpstreamMsg = sanitizeUpstreamMsg(raw, plain)
		out.attempt.BytesOut = int64(len(raw))
		out.attempt.TotalMS = time.Since(start).Milliseconds()
		out.apiErr = capacityError(resp.Header)
		if d.opts.Cooldown != nil {
			// K23 is best-effort and must not extend the request. A failed
			// cooldown write leaves the outward 503 unchanged.
			coolCtx, cancelCool := context.WithTimeout(context.WithoutCancel(ctx), 200*time.Millisecond)
			_, coolErr := d.opts.Cooldown.SetCooldown(coolCtx, t.ch.ID, cooldownDuration(resp.Header))
			cancelCool()
			if coolErr != nil && d.opts.Logger != nil {
				d.opts.Logger.Warn(ctx, "upstream_cooldown_write_failed: "+coolErr.Error(),
					obslog.Fields{Stage: "P16", RequestID: c.RequestID})
			}
		}
		out.result = "FAILED"
		return out
	}
	if resp.StatusCode >= 500 {
		raw := readErrorBody(resp.Body)
		out.class = ClassHTTP5XX
		out.attempt.Phase = "P16"
		out.attempt.ErrorClass = ClassHTTP5XX
		out.attempt.UpstreamMsg = sanitizeUpstreamMsg(raw, plain)
		out.attempt.BytesOut = int64(len(raw))
		out.attempt.TotalMS = time.Since(start).Milliseconds()
		out.apiErr = upstreamStatusError(resp.StatusCode, attemptNo)
		out.result = "FAILED"
		return out
	}
	if resp.StatusCode >= 400 {
		raw := readErrorBody(resp.Body)
		out.class = ClassHTTP4XX
		out.attempt.Phase = "P16"
		out.attempt.ErrorClass = ClassHTTP4XX
		out.attempt.UpstreamMsg = sanitizeUpstreamMsg(raw, plain)
		out.attempt.BytesOut = int64(len(raw))
		out.attempt.TotalMS = time.Since(start).Milliseconds()
		out.apiErr = clientStatusError(resp.StatusCode, out.attempt.UpstreamMsg)
		out.result = "FAILED"
		return out
	}

	if c.Stream {
		return d.streamAttempt(ctx, w, c, t, resp, out, start, attemptNo, plain)
	}
	return d.unaryAttempt(ctx, w, c, t, resp, out, start, plain)
}

func (d *Dispatcher) decryptKey(ch *gw.ChannelEntry) (string, error) {
	if d.opts.KEK == nil {
		return "", cryptobox.ErrNoKEK
	}
	plain, err := d.opts.KEK.Open(ch.UpstreamKeyEnc)
	if err != nil {
		return "", err
	}
	if len(plain) == 0 {
		return "", cryptobox.ErrBadEnvelope
	}
	return string(plain), nil
}

func (d *Dispatcher) buildBody(c *gw.Ctx, t target) ([]byte, string, error) {
	upstreamModel := strings.TrimSpace(t.route.UpstreamModelName)
	// 映射按「平台模型名」索引，因此这里用 P07 解析出的规范名，而不是客户端原始
	// 请求名：别名调用和规范名调用必须命中同一条映射。RequestModel 仍保留客户端
	// 原始写法，用于日志与用量归因。
	mapKey := c.IR.Model
	if c.Model != nil && strings.TrimSpace(c.Model.ModelName) != "" {
		mapKey = c.Model.ModelName
	}
	if mapped := strings.TrimSpace(t.ch.UpstreamModelNameMap[mapKey]); mapped != "" {
		upstreamModel = mapped
	}
	if upstreamModel == "" {
		return nil, "", errors.New("upstream model name is empty")
	}
	clone := c.IR.Clone()
	clone.Model = upstreamModel
	if c.Stream {
		switch t.ch.StreamUsageMode {
		case "UNKNOWN", "STREAM_OPTIONS":
			clone.StreamOptions = &ir.StreamOptions{IncludeUsage: true}
		}
	}

	proto, _ := protocolName(t.ch.ProtocolID)
	var (
		body []byte
		err  error
	)
	switch c.Endpoint {
	case gw.EndpointEmbeddings:
		body, err = openai.EncodeEmbeddingsRequest(clone)
	case gw.EndpointImages:
		body, err = openai.EncodeImageRequest(clone)
	default:
		switch proto {
		case stream.ProtoAnthropic:
			body, err = anthropic.EncodeRequest(clone)
		case stream.ProtoBailian:
			body, err = bailian.EncodeRequest(clone)
		default:
			body, err = openai.EncodeChatRequest(clone)
		}
	}
	if err != nil {
		return nil, upstreamModel, err
	}
	return body, upstreamModel, nil
}

func (d *Dispatcher) buildHTTP(ctx context.Context, c *gw.Ctx, t target, body []byte, plain string) (*http.Request, *traceTimes, error) {
	endpoint, err := upstreamURL(t.ch.BaseURL, c.Endpoint, t.ch.ProtocolID)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "yunshu-gateway")
	if c.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if t.ch.ProtocolID == 3 {
		req.Header.Set("x-api-key", plain)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+plain)
	}
	times := &traceTimes{start: time.Now()}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), times.trace()))
	return req, times, nil
}

func upstreamURL(base string, endpoint gw.Endpoint, protocolID int64) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return "", errors.New("empty base_url")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid base_url")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("base_url must be http or https")
	}
	var path string
	switch endpoint {
	case gw.EndpointEmbeddings:
		path = "/embeddings"
	case gw.EndpointImages:
		path = "/images/generations"
	default:
		if protocolID == 3 {
			path = "/messages"
		} else {
			path = "/chat/completions"
		}
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base + path, nil
}

func (d *Dispatcher) attemptContext(ctx context.Context, c *gw.Ctx, ch *gw.ChannelEntry, attemptNo int) (context.Context, context.CancelFunc) {
	remaining := d.remaining(c)
	var timeout time.Duration
	if c.Stream {
		// The stream's total budget is 600s. The channel's timeout_total_ms is
		// the non-stream total and must not cut a healthy long stream at 120s.
		timeout = d.streamTotalTimeout()
	} else {
		timeout = d.totalTimeout(ch)
	}
	if remaining > 0 && remaining < timeout {
		timeout = remaining
	}
	if attemptNo > 1 && remaining > 0 {
		reserve := d.settleReserve()
		if remaining > reserve {
			if budget := remaining - reserve; budget < timeout {
				timeout = budget
			}
		}
	}
	if timeout <= 0 {
		timeout = d.streamTotalTimeout()
	}
	return context.WithTimeout(ctx, timeout)
}

func (d *Dispatcher) remaining(c *gw.Ctx) time.Duration {
	if c == nil || c.Deadline.IsZero() {
		return 0
	}
	return time.Until(c.Deadline)
}

// transportFor reuses one Transport per channel configuration. MaxConnsPerHost
// is per channel, so a noisy channel cannot consume another channel's sockets.
func (d *Dispatcher) transportFor(ch *gw.ChannelEntry) *http.Transport {
	key := transportKey{
		channelID:   ch.ID,
		connectMS:   ch.TimeoutConnectMS,
		firstByteMS: ch.TimeoutFirstByteMS,
		maxConns:    d.opts.MaxConnsPerChannel,
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if tr, ok := d.transports[key]; ok {
		return tr
	}
	connect := d.connectTimeout(ch)
	firstByte := d.firstByteTimeout(ch)
	tr := &http.Transport{
		// Deliberately no ProxyFromEnvironment. Inheriting an ambient
		// HTTP_PROXY would route every customer payload through whatever
		// proxy operator happens to have exported: extra hop, added
		// latency, per-channel timeout budgets become meaningless, and a
		// third party sees the request body. V1 upstreams are all
		// region=DOMESTIC, so there is nothing a proxy is needed for.
		// A per-channel proxy, if ever required, must be explicit config.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext,
		MaxConnsPerHost:       d.opts.MaxConnsPerChannel,
		MaxIdleConns:          d.opts.MaxConnsPerChannel,
		MaxIdleConnsPerHost:   d.opts.MaxConnsPerChannel,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   connect,
		ResponseHeaderTimeout: firstByte,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	d.transports[key] = tr
	return tr
}

type traceTimes struct {
	start     time.Time
	connect   atomic.Int64
	firstByte atomic.Int64
}

func (t *traceTimes) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			t.connect.CompareAndSwap(0, time.Since(t.start).Milliseconds())
		},
		GotFirstResponseByte: func() {
			t.firstByte.CompareAndSwap(0, time.Since(t.start).Milliseconds())
		},
	}
}

// retriedError builds an error whose message names how many retries already
// happened. Both templates read "已重试 %d 次" (apierr spec E-09), so the
// argument is the retry count — not the 1-based attempt number. Passing
// attemptNo verbatim told users "已重试 2 次" after a single retry (observed
// 2026-11-05 in a 500-once injection run).
//
// A first attempt has nothing to report and "已重试 0 次" reads like a defect,
// so that case falls back to the same sentence without a count.
func retriedError(code string, attemptNo int) *apierr.APIError {
	retries := attemptNo - 1
	if retries < 0 {
		retries = 0
	}
	e := apierr.Newf(code, retries)
	if retries == 0 {
		e = e.WithMessage(firstAttemptMessage(code))
	}
	return e
}

// firstAttemptMessage is retriedError's no-retry variant: same claim, without
// the count. Kept next to the templates so the two cannot drift apart.
func firstAttemptMessage(code string) string {
	if code == apierr.CodeUpstreamTimeout {
		return "上游响应超时，本次未重试"
	}
	return "上游服务当前不可用，本次未重试"
}

// classifyTransportError maps a transport failure to its class and outward code.
//
// attemptNo is required, not cosmetic: both upstream_timeout and
// upstream_all_failed carry "%d 次" in their message template (apierr spec
// E-09). Building them with New instead of Newf leaves a literal "%d" in the
// user-facing body — observed 2026-10-29 in a T39 counter-proof. The count
// itself must be the retry count, which is why the templates go through
// retriedError rather than Newf directly.
func classifyTransportError(err error, attemptNo int) (string, *apierr.APIError) {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ClassDNS, apierr.New(apierr.CodeUpstreamDNSFailure)
	}
	if strings.Contains(strings.ToLower(err.Error()), "tls:") {
		return ClassTLS, apierr.New(apierr.CodeUpstreamTLSFailure)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ClassTimeout, retriedError(apierr.CodeUpstreamTimeout, attemptNo)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassTimeout, retriedError(apierr.CodeUpstreamTimeout, attemptNo)
	}
	if errors.Is(err, context.Canceled) {
		return ClassClientAbort, apierr.New(apierr.CodeClientAborted)
	}
	return ClassConnectRefused, retriedError(apierr.CodeUpstreamAllFailed, attemptNo)
}

func upstreamStatusError(status, attemptNo int) *apierr.APIError {
	if status == http.StatusGatewayTimeout {
		return retriedError(apierr.CodeUpstreamTimeout, attemptNo)
	}
	return retriedError(apierr.CodeUpstreamAllFailed, attemptNo)
}

func clientStatusError(status int, msg string) *apierr.APIError {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return apierr.New(apierr.CodeUpstreamAuthFailed)
	case http.StatusNotFound:
		return apierr.New(apierr.CodeUpstreamModelNotFound)
	default:
		return apierr.Newf(apierr.CodeUpstreamInvalidRequest, msg)
	}
}

func readErrorBody(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, maxErrorBodyBytes))
	return string(data)
}

func decodeResponse(proto string, body []byte) (*ir.Response, error) {
	switch proto {
	case stream.ProtoAnthropic:
		return anthropic.DecodeResponse(body)
	case stream.ProtoBailian:
		return bailian.DecodeResponse(body)
	default:
		return openai.DecodeResponse(body)
	}
}

func encodeResponse(clientProto string, resp *ir.Response) ([]byte, error) {
	if clientProto == string(gw.ProtocolAnthropic) {
		if raw := resp.Extra["_raw_response"]; len(raw) > 0 {
			// An embeddings/images body has no Anthropic representation. Do not
			// hand an OpenAI JSON body to an Anthropic client.
			return nil, errors.New("response has no Anthropic representation")
		}
		return anthropic.EncodeResponse(resp)
	}
	return openai.EncodeResponse(resp)
}

func (d *Dispatcher) unaryAttempt(ctx context.Context, w http.ResponseWriter, c *gw.Ctx, t target, resp *http.Response, out outcome, start time.Time, secret string) outcome {
	out.attempt.Phase = "P17"
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUnaryBytes+1))
	out.attempt.BytesOut = int64(len(body))
	out.attempt.TotalMS = time.Since(start).Milliseconds()
	if err != nil {
		out.class, out.apiErr = classifyTransportError(err, out.attempt.AttemptIndex)
		out.attempt.ErrorClass = out.class
		return out
	}
	if len(body) > maxUnaryBytes {
		out.class = ClassMalformedBody
		out.attempt.ErrorClass = out.class
		out.apiErr = apierr.New(apierr.CodeUpstreamInvalidResponse)
		out.result = "FAILED"
		return out
	}
	proto, _ := protocolName(t.ch.ProtocolID)
	parsed, err := decodeResponse(proto, body)
	if err != nil {
		out.class = ClassMalformedBody
		out.attempt.ErrorClass = out.class
		out.attempt.UpstreamMsg = sanitizeUpstreamMsg(err.Error(), secret)
		out.apiErr = apierr.New(apierr.CodeUpstreamInvalidResponse)
		out.result = "FAILED"
		return out
	}
	if isChatEndpoint(c.Endpoint) && len(parsed.Choices) == 0 {
		out.class = ClassEmptyChoices
		out.attempt.ErrorClass = out.class
		out.apiErr = apierr.New(apierr.CodeUpstreamEmptyResponse)
		out.result = "FAILED"
		return out
	}

	usage := d.unaryUsage(c, parsed)
	out.tokenIn = usage.TokensIn
	out.tokenOut = usage.TokensOut
	out.usageSource = usage.Source
	out.attempt.TokensIn = usage.TokensIn
	out.attempt.TokensOut = usage.TokensOut

	clientProto := string(c.Protocol)
	encoded, err := encodeResponse(clientProto, parsed)
	if err != nil {
		out.class = ClassMalformedBody
		out.attempt.ErrorClass = out.class
		out.apiErr = apierr.New(apierr.CodeConvertFailed).WithCause(err)
		out.result = "FAILED"
		return out
	}

	consumed, err := d.cost(c, usage)
	if err != nil {
		out.class = ClassNone
		out.apiErr = apierr.New(apierr.CodePriceNotConfigured).WithCause(err)
		out.result = "FAILED"
		return out
	}
	if _, err := d.settle(ctx, c, consumed, ""); err != nil {
		if d.opts.Logger != nil {
			d.opts.Logger.Error(ctx, "settlement_failed: "+err.Error(), obslog.Fields{
				Stage: "P27", RequestID: c.RequestID, Status: 503,
			})
		}
		out.class = ClassNone
		out.apiErr = apierr.New(apierr.CodeSettlementUnavailable).WithCause(err)
		out.result = "FAILED"
		return out
	}
	out.consumedMicro = consumed
	d.completeIdem(ctx, c, encoded)

	preheld := c.HoldMicro
	remaining := float64(d.availableBase(c) - consumed)
	rid := apierr.SetResponseHeaders(w, apierr.ResponseMeta{
		RequestID:        c.RequestID,
		Stream:           false,
		CreditsConsumed:  &consumed,
		CreditsPreheld:   &preheld,
		CreditsRemaining: &remaining,
	})
	c.RequestID = rid
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(encoded); err != nil && d.opts.Logger != nil {
		d.opts.Logger.Warn(ctx, "client_write_failed: "+err.Error(), obslog.Fields{
			Stage: "P20", RequestID: c.RequestID,
		})
	}
	out.result = "SUCCESS"
	out.finished = true
	return out
}

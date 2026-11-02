package gw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/config"
	obslog "yunshu.dev/core/internal/obs/log"
)

// InternalHandler is the control-plane entry point. It exists because
// controld owns the API-key plaintext never and gatewayd has no main-database
// pool: a console call cannot authenticate with a token, so controld signs the
// verified key attributes and this handler injects them.
//
// Stage coverage: P06.5, P06, P07, P08, P08.5, P12 (P13 folded into
// RouterStage), P09, P11, P10 and P14 (the injected terminal). P05 is
// deliberately absent:
// its only job here would be to overwrite the verified claims with a K13
// lookup, which gatewayd cannot perform for a browser session.
//
// P01's budget and the per-key connection slot still run (the slot is P05's
// tail in the public path), so a console request consumes exactly the same
// capacity and quota as the equivalent /v1 call.
type InternalHandler struct {
	h        *Handler
	pipeline *Pipeline
	secret   []byte
}

// NewInternalHandler wires the internal entry. It refuses a short secret or a
// nil terminal at boot; cmd/gatewayd additionally refuses to register the
// route when the secret is unset.
func NewInternalHandler(d *Deps, terminal Terminal, secret string) (*InternalHandler, error) {
	if d == nil || d.Catalog == nil {
		return nil, errors.New("gw: internal handler requires deps")
	}
	if terminal == nil {
		return nil, errors.New("gw: internal handler requires a terminal (upstream dispatcher)")
	}
	if len(secret) < InternalMinSecretBytes {
		return nil, fmt.Errorf("gw: INTERNAL_CALL_SECRET must be at least %d bytes", InternalMinSecretBytes)
	}
	return &InternalHandler{
		h:        NewHandler(d, terminal),
		pipeline: NewInternalPipeline(d),
		secret:   []byte(secret),
	}, nil
}

// NewInternalPipeline is the public pipeline minus the P05 slot. P06.5 runs
// first, then P06/P07/P08 in numeric order, all ahead of backpressure; P12
// precedes P09 for the same reason as NewDefaultPipeline (the estimate needs
// the route price), and P11 still precedes P10 so no money is written before
// the rate gate. P06.5 runs first because the internal entry injects the signed
// identity before P04, so the hit row has the same owner fields as the public
// path.
func NewInternalPipeline(d *Deps) *Pipeline {
	return NewPipeline(d.Logger,
		NewContentPolicyStage(d), // P06.5
		NewIPStage(d),            // P06
		NewModelStage(d),         // P07
		NewIdemStage(d),          // P08
		NewBackpressureStage(d),  // P08.5
		NewRouterStage(d),        // P12; P13 breaker state is read inside
		NewEstimateStage(d),      // P09
		NewRateLimitStage(d),     // P11
		NewPreholdStage(d),       // P10
	)
}

// ServeHTTP implements http.Handler.
func (h *InternalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var sw *statusRecorder
	if h.h.d != nil && h.h.d.Metrics != nil {
		sw = &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		w = sw
	}

	rid := obslog.MetaFrom(r.Context()).RequestID
	if rid == "" {
		rid = apierr.ResolveRequestID(r.Header.Get("X-Request-Id"), "")
	}
	c := &Ctx{
		RequestID: rid,
		StartedAt: time.Now(),
		ConnID:    r.RemoteAddr,
		ClientIP:  clientIP(r.RemoteAddr),
		Header:    r.Header,
	}
	c.ConfigVersion = h.h.d.Catalog.Version()
	w.Header().Set("X-Request-Id", rid)

	if sw != nil {
		defer func() { h.h.observeRequest(c, sw.Status()) }()
	}

	// Same G23 backfill as the public handler. terminalRan suppresses the row
	// once the dispatcher takes over and writes its own usage record.
	terminalRan := false
	defer func() { emitFailureUsage(h.h, c, terminalRan) }()

	if r.Method != http.MethodPost || r.URL.Path != InternalPath {
		c.FailStage = "P03"
		h.h.writeError(w, r.Context(), c, apierr.New(apierr.CodeUnknownEndpoint))
		return
	}

	base := r.Context()
	ctx, cancel := context.WithTimeout(base, h.h.budget(false))
	defer cancel()

	// P02.
	body, apiErr := ReadBody(w, r, MaxBodyBytes, BodyReadTimeout)
	if apiErr != nil {
		h.h.writeError(w, ctx, c, apiErr)
		return
	}
	c.RawBody = body
	c.BodyHash = BodyHashOf(body)

	// P03 is fixed: this listener only ever accepts the OpenAI chat endpoint.
	c.Endpoint = EndpointChat
	c.Protocol = ProtocolOpenAI

	// P05 replacement, before P04: verify the control plane's signature over the
	// body and the seven identity fields. Verification needs only the raw bytes,
	// so an unauthenticated peer must not be able to reach body parsing and see
	// 400/413 diagnostics. The clock is the Redis-corrected one so the window is
	// stable across hosts; the signer's window is ±60s.
	nowMS := h.h.d.ClockNowMS()
	claims, sigErr := VerifyInternal(h.secret, nowMS/1000, body, r.Header)
	if sigErr != nil {
		// One code for a bad MAC, a stale timestamp, a malformed header and a
		// missing claim: the response must not be a probing oracle.
		c.FailStage = "P05"
		if h.h.d.Logger != nil {
			h.h.d.Logger.Warn(ctx, "gateway_internal_sig_rejected: "+sigErr.Error(),
				obslog.Fields{Stage: "P05", RequestID: rid})
		}
		h.h.writeError(w, ctx, c, apierr.New(apierr.CodeInvalidAPIKey))
		return
	}
	if claims.KeyExpiryMS > 0 && claims.KeyExpiryMS <= nowMS {
		c.FailStage = "P05"
		expired := time.UnixMilli(claims.KeyExpiryMS).UTC().Format(time.RFC3339)
		h.h.writeError(w, ctx, c, apierr.Newf(apierr.CodeKeyExpired, expired))
		return
	}

	// P04.
	ir, apiErr := ParseRequest(EndpointChat, body)
	if apiErr != nil {
		h.h.writeError(w, ctx, c, apiErr)
		return
	}
	c.IR = ir
	c.Stream = ir.Stream
	if c.Stream {
		cancel()
		streamCtx, streamCancel := context.WithTimeout(base, h.h.budget(true))
		ctx = streamCtx
		defer streamCancel()
	}
	if deadline, ok := ctx.Deadline(); ok {
		c.Deadline = deadline
	}
	c.UpstreamTimeout = h.h.configMS(config.KeyGwUpstreamTimeoutMs, 50_000)
	c.FirstByteTimeout = h.h.configMS(config.KeyGwStreamFirstByteMs, 30_000)

	// Inject the signed identity. KeyID is the real t_api_key.id; there is no
	// sentinel path. Group and ratio are normalised exactly as P05 does for a
	// K13 record whose grp/gp are empty, so the console cannot be cheaper than
	// the same key calling /v1 directly.
	c.Auth = &AuthContext{
		UserID:       claims.UserID,
		KeyID:        claims.KeyID,
		KeyName:      claims.KeyName,
		GroupCode:    claims.GroupCode,
		GroupRatioBP: claims.GroupRatioBP,
		IPMRuleVer:   claims.IPMRuleVer,
		KeyExpiryMS:  claims.KeyExpiryMS,
	}
	c.Limits = h.limits(ctx, claims)

	// P05's tail in the public path: the per-key connection slot. The internal
	// caller is a cooperative process, so an exhausted slot is answered with
	// the P11 concurrency code rather than a TCP reset; resetting would surface
	// here as a generic 502 and hide the real cause from the console.
	if !h.h.d.ConnGuard.Acquire(ctx, claims.KeyID, c.Limits.Concurrency) {
		c.FailStage = "P01"
		h.h.writeError(w, ctx, c, apierr.New(apierr.CodeConcurrencyLimitExceeded))
		return
	}
	c.ConnHeld = true
	defer h.h.releaseRequest(ctx, c)

	runErr := h.pipeline.Run(ctx, c)
	if runErr == nil {
		if c.Replay != nil {
			h.h.writeReplay(w, ctx, c)
			return
		}
		// P14: the same dispatcher the public path uses. Settlement, usage
		// records and request logs are written there, not in controld.
		terminalRan = true
		h.h.terminal.Serve(w, r, c)
		return
	}
	h.h.finishError(w, ctx, c, runErr)
}

// limits mirrors P05's snapshot lookup and FREE_DEFAULT fallback.
func (h *InternalHandler) limits(ctx context.Context, claims InternalClaims) Limits {
	if h.h.d.Catalog != nil {
		if snap, err := h.h.d.Catalog.Current(); err == nil {
			if limits, ok := snap.RateLimitFor(claims.KeyID, claims.UserID); ok {
				return limits
			}
		} else if h.h.d.Logger != nil {
			h.h.d.Logger.Warn(ctx, "internal_rate_limit_snapshot_failed: using FREE_DEFAULT",
				obslog.Fields{Stage: "P05"})
		}
	}
	if h.h.d.Logger != nil {
		h.h.d.Logger.Warn(ctx, "rate_limit_policy_missing: using FREE_DEFAULT", obslog.Fields{Stage: "P05"})
	}
	return FreeDefaultLimits()
}

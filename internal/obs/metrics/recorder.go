package metrics

import (
	"strconv"
	"time"
)

// Recorder is the typed facade over the TDS §5.3 metric set. Every method is
// nil-safe and non-blocking: a sampler failure must never change a request.
//
// Label values are closed sets. Model names and channel ids come from the
// routing catalog (business-bounded), while dimensions, outcomes and reasons
// are mapped to a fixed enum; anything outside the set becomes "other". The
// recorder deliberately has no method that accepts a request id, a user id, a
// configuration value or an upstream message.
type Recorder struct {
	reg *Registry

	requests             *CounterVec
	requestDuration      *HistogramVec
	ttft                 *HistogramVec
	upstreamAttempts     *CounterVec
	circuitState         *GaugeVec
	ratelimitRejected    *CounterVec
	ratelimitDegraded    *CounterVec
	ratelimitUnavailable *CounterVec

	inflightVec *GaugeVec
	inflight    *Gauge

	preholdOrphan       *CounterVec
	orderAutoClosed     *CounterVec
	orderAutoCloseFail  *CounterVec
	settleFallback      *CounterVec
	ledgerReconcileDiff *GaugeVec
	balanceCASRetry     *CounterVec
	configStale         *GaugeVec
	configBootstrapUsed *CounterVec
	strategyFallback    *CounterVec
	regionGuard         *CounterVec
	backpressure        *CounterVec
	upstreamMalformed   *CounterVec
	dedicatedFallback   *CounterVec

	settleBacklogTasks *GaugeVec
	ticketSLAOverdue   *GaugeVec

	logWrittenVec *CounterVec
	logWritten    *Counter
	logDropped    *CounterVec
	logBacklogVec *GaugeVec
	logBacklog    *Gauge

	logBodyWritten *Counter
	logBodyDropped *CounterVec
	logBodyOpen    *Counter

	configItemRejected *CounterVec
	policyFailOpen     *CounterVec
}

// NewRecorder registers every TDS §5.3 family on reg. Registering a family
// does not create a series: a label combination appears on its first record,
// so a quiet process does not fabricate zeroes. The three exceptions are
// label-less gauges/counters where zero is a true value at startup (in-flight,
// log written, log backlog); those handles are cached to keep their calls off
// the map lookup path.
func NewRecorder(reg *Registry) *Recorder {
	if reg == nil {
		reg = NewRegistry()
	}
	r := &Recorder{
		reg: reg,

		requests: reg.Counter("gw_requests_total",
			"Total data-plane requests.",
			"model", "protocol_in", "protocol_out", "http_status", "err_code"),
		requestDuration: reg.Histogram("gw_request_duration_seconds",
			"End-to-end data-plane request latency in seconds.",
			DurationBuckets, "model", "stream"),
		ttft: reg.Histogram("gw_ttft_seconds",
			"Time to the first upstream response byte for streaming requests, in seconds.",
			DurationBuckets, "model", "channel_id"),
		upstreamAttempts: reg.Counter("gw_upstream_attempts_total",
			"Upstream attempts by channel and outcome.",
			"channel_id", "outcome"),
		circuitState: reg.Gauge("gw_circuit_state",
			"Circuit breaker state for this process: 0=CLOSED, 1=OPEN, 2=HALF_OPEN.",
			"channel_id"),
		ratelimitRejected: reg.Counter("gw_ratelimit_rejected_total",
			"Rate-limit rejections by dimension.",
			"dim"),
		ratelimitDegraded: reg.Counter("gw_ratelimit_degraded_total",
			"Rate-limit decisions taken by the node-local concurrency counter because Redis was unavailable.",
			"dim"),
		ratelimitUnavailable: reg.Counter("gw_ratelimit_unavailable_total",
			"Rate-limit decisions that failed closed because Redis was unavailable.",
			"dim"),

		inflightVec: reg.Gauge("gw_inflight",
			"Current in-flight data-plane requests."),

		preholdOrphan: reg.Counter("gw_prehold_orphan_total",
			"Expired preholds recovered by the reconciliation job.",
			"reason"),
		orderAutoClosed: reg.Counter("gw_order_auto_closed_total",
			"Unpaid recharge orders closed by the expiry job.",
			"reason"),
		orderAutoCloseFail: reg.Counter("gw_order_auto_close_failed_total",
			"Unpaid orders the expiry job could not close, by failure stage.",
			"stage"),
		settleFallback: reg.Counter("gw_settle_fallback_total",
			"Settlements that fell back because usage or the client was unavailable.",
			"reason"),
		ledgerReconcileDiff: reg.Gauge("gw_ledger_reconcile_diff",
			"Differences found by the latest successful daily reconciliation run."),
		balanceCASRetry: reg.Counter("gw_balance_cas_retry_total",
			"Balance CAS contention observed by the gateway.",
			"result"),
		configStale: reg.Gauge("gw_config_stale_seconds",
			"Seconds since the oldest configuration snapshot was loaded.",
			"instance"),
		configBootstrapUsed: reg.Counter("gw_config_bootstrap_used",
			"Configuration bootstrap (built-in defaults) used at startup."),
		strategyFallback: reg.Counter("gw_strategy_fallback_total",
			"Configured strategy values that were not registered.",
			"kind", "configured"),
		regionGuard: reg.Counter("gw_region_guard_violation_total",
			"Overseas channels excluded from routing by the region guard.",
			"channel_id"),
		backpressure: reg.Counter("gw_backpressure_rejected_total",
			"Requests rejected by process backpressure.",
			"tier"),
		upstreamMalformed: reg.Counter("gw_upstream_frame_malformed_total",
			"Malformed upstream stream frames.",
			"channel_id"),
		dedicatedFallback: reg.Counter("gw_dedicated_fallback_total",
			"Requests served from the shared pool after every dedicated channel was unavailable and fallback was allowed."),

		settleBacklogTasks: reg.Gauge("gw_settle_backlog_tasks",
			"PENDING rows in t_settle_task, sampled by jobsd (settle.PendingCountSQL)."),
		ticketSLAOverdue: reg.Gauge("gw_ticket_sla_overdue",
			"OPEN tickets whose first-response deadline has passed, sampled by jobsd."),

		logWrittenVec: reg.Counter("gw_log_written_total",
			"Request-log entries written to the Redis stream."),
		logDropped: reg.Counter("gw_log_dropped_total",
			"Request-log entries dropped.",
			"reason"),
		logBacklogVec: reg.Gauge("gw_log_backlog",
			"Current request-log stream length."),

		logBodyWritten: reg.Counter("gw_log_body_written_total",
			"Request bodies sealed and inserted into t_request_body.").With(),
		logBodyDropped: reg.Counter("gw_log_body_dropped_total",
			"Request bodies dropped before persistence.",
			"reason"),
		logBodyOpen: reg.Counter("gw_log_body_open_failed_total",
			"Stored request bodies that could not be decrypted.").With(),

		configItemRejected: reg.Counter("gw_config_item_rejected_total",
			"Configuration rows rejected while applying a snapshot.",
			"namespace", "reason"),

		policyFailOpen: reg.Counter("gw_content_policy_fail_open_total",
			"External content-policy checks that failed open because the service was unavailable.",
			"reason"),
	}
	r.inflight = r.inflightVec.With()
	r.logWritten = r.logWrittenVec.With()
	r.logBacklog = r.logBacklogVec.With()
	return r
}

// Registry returns the registry this recorder writes to.
func (r *Recorder) Registry() *Registry {
	if r == nil {
		return nil
	}
	return r.reg
}

// Request records one completed data-plane request. status is the HTTP status
// written to the client; errCode is the API error code, empty on success.
func (r *Recorder) Request(model, protocolIn, protocolOut, errCode string, status int) {
	if r == nil || r.requests == nil {
		return
	}
	r.requests.With(
		modelValue(model),
		protocolValue(protocolIn),
		protocolValue(protocolOut),
		strconv.Itoa(status),
		sanitizeLabelValue(errCode),
	).Inc()
}

// RequestDuration records the end-to-end latency. stream=true covers SSE
// requests, whose budget is the 600s stream budget rather than the 120s unary
// budget.
func (r *Recorder) RequestDuration(model string, stream bool, d time.Duration) {
	if r == nil || r.requestDuration == nil {
		return
	}
	r.requestDuration.With(modelValue(model), strconv.FormatBool(stream)).ObserveDuration(d)
}

// TTFT records the first upstream byte for a streaming attempt.
func (r *Recorder) TTFT(model string, channelID int64, d time.Duration) {
	if r == nil || r.ttft == nil {
		return
	}
	r.ttft.With(modelValue(model), channelValue(channelID)).ObserveDuration(d)
}

// UpstreamAttempt records one attempt outcome.
func (r *Recorder) UpstreamAttempt(channelID int64, outcome string) {
	if r == nil || r.upstreamAttempts == nil {
		return
	}
	r.upstreamAttempts.With(channelValue(channelID), enumValue(attemptOutcomes, outcome)).Inc()
}

// CircuitState records the breaker state read during routing. It is refreshed
// when this process reads K16; a channel with no traffic keeps its last value
// (TDS §5.3 deviation note).
func (r *Recorder) CircuitState(channelID int64, state int) {
	if r == nil || r.circuitState == nil {
		return
	}
	if state < 0 || state > 2 {
		state = 0
	}
	r.circuitState.With(channelValue(channelID)).Set(int64(state))
}

// RatelimitRejected is the ratelimit.Metrics surface.
func (r *Recorder) RatelimitRejected(dim string) {
	if r == nil || r.ratelimitRejected == nil {
		return
	}
	r.ratelimitRejected.With(enumValue(ratelimitDims, dim)).Inc()
}

// RatelimitDegraded is the ratelimit.Metrics surface.
func (r *Recorder) RatelimitDegraded(dim string) {
	if r == nil || r.ratelimitDegraded == nil {
		return
	}
	r.ratelimitDegraded.With(enumValue(ratelimitDims, dim)).Inc()
}

// RatelimitUnavailable is the ratelimit.Metrics surface.
func (r *Recorder) RatelimitUnavailable(dim string) {
	if r == nil || r.ratelimitUnavailable == nil {
		return
	}
	r.ratelimitUnavailable.With(enumValue(ratelimitDims, dim)).Inc()
}

// Inflight sets the process in-flight gauge.
func (r *Recorder) Inflight(n int64) {
	if r == nil || r.inflight == nil {
		return
	}
	r.inflight.Set(n)
}

// PreholdOrphan records one expired prehold recovered by jobsd. reason is
// ttl_expired for the 60s scan; db_sweep is reserved for a future sweep path.
func (r *Recorder) PreholdOrphan(reason string) {
	if r == nil || r.preholdOrphan == nil {
		return
	}
	r.preholdOrphan.With(enumValue(preholdOrphanReasons, reason)).Inc()
}

// OrderAutoClosed records one unpaid order the expiry job moved to CLOSED.
// reason is a closed set so a cardinality accident cannot reach the label.
func (r *Recorder) OrderAutoClosed(reason string) {
	if r == nil || r.orderAutoClosed == nil {
		return
	}
	r.orderAutoClosed.With(enumValue(orderAutoCloseReasons, reason)).Inc()
}

// OrderAutoCloseFailed records one order the expiry job could not close. It is
// deliberately a separate counter from the closed one: a failure is not a
// close, and summing the two would make the close rate unreadable.
func (r *Recorder) OrderAutoCloseFailed(stage string) {
	if r == nil || r.orderAutoCloseFail == nil {
		return
	}
	r.orderAutoCloseFail.With(enumValue(orderAutoCloseFailureStages, stage)).Inc()
}

// SettleFallback records a settlement that could not use upstream usage.
func (r *Recorder) SettleFallback(reason string) {
	if r == nil || r.settleFallback == nil {
		return
	}
	r.settleFallback.With(enumValue(settleFallbackReasons, reason)).Inc()
}

// LedgerReconcileDiff sets the latest daily reconciliation difference count.
// The series is lazy: it is created by the first successful run, not at
// startup, so a quiet jobsd does not export a zero that would hide the P0
// "diff != 0" alert.
func (r *Recorder) LedgerReconcileDiff(n int64) {
	if r == nil || r.ledgerReconcileDiff == nil {
		return
	}
	r.ledgerReconcileDiff.With().Set(n)
}

// BalanceCASContended records one account-version CAS loss. V1 has no retry
// loop, so contention itself is the signal (TDS §5.3 deviation note).
func (r *Recorder) BalanceCASContended() {
	if r == nil || r.balanceCASRetry == nil {
		return
	}
	r.balanceCASRetry.With("contended").Inc()
}

// ConfigStale sets the staleness gauge. instance is the pod name.
func (r *Recorder) ConfigStale(instance string, seconds int64) {
	if r == nil || r.configStale == nil {
		return
	}
	if seconds < 0 {
		seconds = 0
	}
	r.configStale.With(instanceValue(instance)).Set(seconds)
}

// ConfigBootstrapUsed records that the process started on built-in defaults
// instead of a loaded snapshot. The series is lazy for the same reason as
// LedgerReconcileDiff: a fabricated zero would disarm the P1 alert.
func (r *Recorder) ConfigBootstrapUsed() {
	if r == nil || r.configBootstrapUsed == nil {
		return
	}
	r.configBootstrapUsed.With().Inc()
}

// StrategyFallback records a configured strategy name that is not registered.
// The TDS names the second label "configured"; the original value is
// deliberately not used here (it is operator input and would make cardinality
// a function of configuration). The value is always "unregistered"; the
// original name is already in the config_field_fallback WARN line.
func (r *Recorder) StrategyFallback(kind, configured string) {
	if r == nil || r.strategyFallback == nil {
		return
	}
	r.strategyFallback.With(
		enumValue(strategyKinds, kind),
		enumValue(configuredLabels, configured),
	).Inc()
}

// RegionGuardViolation records an overseas channel that entered the candidate
// loop and was excluded by the hard region filter.
func (r *Recorder) RegionGuardViolation(channelID int64) {
	if r == nil || r.regionGuard == nil {
		return
	}
	r.regionGuard.With(channelValue(channelID)).Inc()
}

// BackpressureRejected records a shed request. tier is stream (60%..90%,
// streaming only), reject (>90%), or backlog (GR-03 settlement backlog).
func (r *Recorder) BackpressureRejected(tier string) {
	if r == nil || r.backpressure == nil {
		return
	}
	r.backpressure.With(enumValue(backpressureTiers, tier)).Inc()
}

// UpstreamFrameMalformed records one malformed upstream stream frame.
func (r *Recorder) UpstreamFrameMalformed(channelID int64) {
	if r == nil || r.upstreamMalformed == nil {
		return
	}
	r.upstreamMalformed.With(channelValue(channelID)).Inc()
}

// DedicatedFallback records one request whose dedicated channels were all
// unavailable and whose configuration allowed the shared pool to serve it
// (PRD F14-6 edge 4). It is label-less and called at most once per request:
// the monthly reconciliation needs a request count, not a per-channel count.
func (r *Recorder) DedicatedFallback() {
	if r == nil || r.dedicatedFallback == nil {
		return
	}
	r.dedicatedFallback.With().Inc()
}

// SettleBacklogTasks sets the PENDING settlement-task count sampled by jobsd.
// It is the data source for the "approaching the GR-03 line" alert: the
// backpressure counter only moves after the gateway has already started
// refusing traffic, so capacity warning needs the queue depth itself.
//
// The series is lazy (created on the first successful sample) like
// LedgerReconcileDiff: a jobsd that cannot reach MySQL must not export a
// fabricated zero that hides an unmeasured backlog.
func (r *Recorder) SettleBacklogTasks(n int64) {
	if r == nil || r.settleBacklogTasks == nil {
		return
	}
	if n < 0 {
		n = 0
	}
	r.settleBacklogTasks.With().Set(n)
}

// TicketSLAOverdue sets the number of OPEN tickets past their first-response
// deadline, sampled by jobsd from ticket.FirstResponseOverdueSQL. Lazy for the
// same reason as SettleBacklogTasks. It is a gauge, not a counter: a ticket
// stops being overdue when it is answered or closed, and a counter would only
// ever climb.
func (r *Recorder) TicketSLAOverdue(n int64) {
	if r == nil || r.ticketSLAOverdue == nil {
		return
	}
	if n < 0 {
		n = 0
	}
	r.ticketSLAOverdue.With().Set(n)
}

// LogWritten records one request-log entry appended to the Redis stream.
func (r *Recorder) LogWritten() {
	if r == nil || r.logWritten == nil {
		return
	}
	r.logWritten.Inc()
}

// LogDropped records one lost request-log entry.
func (r *Recorder) LogDropped(reason string) {
	if r == nil || r.logDropped == nil {
		return
	}
	r.logDropped.With(enumValue(logDropReasons, reason)).Inc()
}

// LogBacklog sets the request-log stream length.
func (r *Recorder) LogBacklog(n int64) {
	if r == nil || r.logBacklog == nil {
		return
	}
	if n < 0 {
		n = 0
	}
	r.logBacklog.Set(n)
}

// LogBodyWritten records one request body sealed and persisted.
func (r *Recorder) LogBodyWritten() {
	if r == nil || r.logBodyWritten == nil {
		return
	}
	r.logBodyWritten.Inc()
}

// LogBodyDropped records one request body discarded before persistence.
// reason is one of the bounded logBodyDropReasons; anything else becomes
// "other" so a malformed label cannot grow cardinality.
func (r *Recorder) LogBodyDropped(reason string) {
	if r == nil || r.logBodyDropped == nil {
		return
	}
	r.logBodyDropped.With(enumValue(logBodyDropReasons, reason)).Inc()
}

// LogBodyOpenFailed records one stored body whose envelope could not be
// opened: a missing/rotated LOG_BODY_KEK, an unknown body_key_id, or tampered
// ciphertext. The HTTP response stays 404; this counter is the only place the
// difference between "never stored" and "stored but unreadable" is visible.
func (r *Recorder) LogBodyOpenFailed() {
	if r == nil || r.logBodyOpen == nil {
		return
	}
	r.logBodyOpen.Inc()
}

// ConfigItemRejected records one t_config_item row that was skipped while
// applying a snapshot. namespace is one of the five namespaces or
// "unrecognized"; reason is a bounded loader reason.
func (r *Recorder) ConfigItemRejected(namespace, reason string) {
	if r == nil || r.configItemRejected == nil {
		return
	}
	r.configItemRejected.With(
		enumValue(configNamespaces, namespace),
		enumValue(configRejectReasons, reason),
	).Inc()
}

// ContentPolicyFailOpen records one P06.5 check that could not get a verdict
// from the external service and was allowed through. reason is a bounded
// fail-open label; an unknown value becomes "other" so a vendor error string
// can never grow cardinality.
func (r *Recorder) ContentPolicyFailOpen(reason string) {
	if r == nil || r.policyFailOpen == nil {
		return
	}
	r.policyFailOpen.With(enumValue(policyFailOpenReasons, reason)).Inc()
}

func modelValue(model string) string {
	v := sanitizeLabelValue(model)
	if v == "" {
		return "unknown"
	}
	return v
}

func instanceValue(instance string) string {
	v := sanitizeLabelValue(instance)
	if v == "" {
		return "unknown"
	}
	return v
}

func channelValue(channelID int64) string {
	if channelID <= 0 {
		return "0"
	}
	return strconv.FormatInt(channelID, 10)
}

func protocolValue(protocol string) string {
	if protocol == "" {
		return "none"
	}
	return enumValue(protocolLabels, protocol)
}

func enumValue(allowed map[string]struct{}, value string) string {
	if _, ok := allowed[value]; ok {
		return value
	}
	return "other"
}

func labelSet(values ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(values))
	for _, v := range values {
		m[v] = struct{}{}
	}
	return m
}

var (
	protocolLabels = labelSet("OPENAI_COMPAT", "ANTHROPIC_COMPAT", "BAILIAN", "none")

	attemptOutcomes = labelSet("SUCCESS", "FAILED", "PARTIAL", "CLIENT_ABORT", "UNKNOWN")

	ratelimitDims = labelSet("rpm", "tpm", "concurrency", "daily")

	preholdOrphanReasons = labelSet("ttl_expired", "db_sweep")

	// expire_at is the only trigger an unpaid order has: the 30-minute
	// payment window set at creation time.
	orderAutoCloseReasons = labelSet("expire_at")

	// Where the auto-close job broke: the scan query, a per-order database
	// statement, or the audit insert. An operator reads the stage to decide
	// whether to look at MySQL or at the audit trail.
	orderAutoCloseFailureStages = labelSet("scan", "db", "audit")

	settleFallbackReasons = labelSet("no_usage", "disconnect")

	strategyKinds = labelSet(
		"deduct_policy", "refund_policy", "cost_source", "daily_meter",
		"key_daily_quota", "reset_scope", "tier_chain", "group_switch_mode",
	)
	configuredLabels = labelSet("unregistered")

	backpressureTiers = labelSet("stream", "reject", "backlog")

	logDropReasons = labelSet("redis_down", "max_deliveries", "bad_payload", "unknown_version")

	logBodyDropReasons = labelSet("queue_full", "seal_failed", "db_error", "no_kek")

	configNamespaces = labelSet(
		"ROUTING", "RATELIMIT", "PRICING", "UPSTREAM", "FEATURE", "unrecognized",
	)
	configRejectReasons = labelSet(
		"unknown_key", "wrong_namespace", "invalid_value", "strategy_unregistered",
	)

	policyFailOpenReasons = labelSet(
		"timeout", "http_status", "bad_response", "transport", "budget_exhausted", "other",
	)
)

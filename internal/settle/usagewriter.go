package settle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/cryptobox"
	obslog "yunshu.dev/core/internal/obs/log"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/platform/clock"
)

// P28/P29 batching. A full batch or the 3s ticker triggers a flush.
const (
	DefaultFlushInterval = 3 * time.Second
	DefaultFlushBatch    = 500
	// usageQueueSize bounds memory when the database is slow. A full queue
	// drops log rows; it never blocks the request and never touches money.
	usageQueueSize = 4096
	// bodyQueueSize bounds the number of retained bodies. Bodies are handed
	// over by reference (the 10 MB P02 cap is an upper bound), so this queue is
	// deliberately much smaller than usageQueueSize: a full queue drops the
	// body and leaves the usage row untouched.
	bodyQueueSize = 64
	// bodyFlushBatch caps rows per INSERT. A 10 MB body becomes a ~13.3 MB
	// envelope after base64; 4 rows stay below MySQL's default 64 MB
	// max_allowed_packet instead of failing the whole batch.
	bodyFlushBatch = 4
	// DefaultBodyRetentionDays matches log.retention_days' default. It is used
	// only when the caller supplies no RetentionDays function.
	DefaultBodyRetentionDays = 180
)

// bodyItem is one queued request body. Ciphertext is filled by the flush
// goroutine; the plaintext reference is cleared as soon as Seal returns.
type bodyItem struct {
	RequestID  string
	UserID     int64
	Body       []byte
	Ciphertext []byte
	// KeyID is the body key that sealed Ciphertext. It is read from the ring at
	// seal time and written to t_request_body.body_key_id, so a later rotation
	// still knows which retired key opens this row (G3).
	KeyID     string
	CreatedAt time.Time
}

// UsageRecord is one t_usage_record row (P28). IDs of 0 mean SQL NULL.
type UsageRecord struct {
	RequestID     string
	UserID        int64
	APIKeyID      int64
	APIKeyName    string
	ModelID       int64
	ModelName     string
	RequestModel  string
	InboundProto  string
	ChannelID     int64
	ChannelName   string
	OutboundProto string
	RouteID       int64
	PriceRuleID   int64
	ConfigVersion int64
	GroupCode     string
	PriceRatioBP  int64
	BillingTier   string
	IsStream      bool
	HTTPStatus    int
	BizCode       string
	Result        string
	FailStage     string
	FailOwner     string
	TokensIn      int64
	TokensOut     int64
	TokensInEst   int64
	TokensOutEst  int64
	UsageSource   string
	ImageCount    int64
	PreholdMicro  int64
	ConsumedMicro int64
	// ConsumedRechargeMicro / ConsumedBonusMicro are the split of
	// ConsumedMicro; the first delivery of T09 leaves them 0 and T04's ledger
	// result is the authority.
	ConsumedRechargeMicro int64
	ConsumedBonusMicro    int64
	RefundedMicro         int64
	UpstreamAttempts      int64
	TTFTMS                *int64
	TotalMS               int64
	UpstreamMS            int64
	ClientIP              []byte
	UserAgent             string
	ClientAborted         bool
	CreatedAt             time.Time
	// RawBody is the request body handed over by the relay after the response
	// is complete. It is never inserted as-is: WriteUsage transfers it to the
	// body queue and the async flush seals it with BodyKeys. Nil means there is
	// no body to keep (for example GET /v1/models).
	RawBody []byte
	// FullLogSaved is written to t_usage_record.full_log_saved. The console
	// body endpoint reads it before touching t_request_body, so it is set only
	// when the body was accepted into the queue.
	FullLogSaved bool
}

// AttemptRecord is one t_upstream_attempt row (P29).
type AttemptRecord struct {
	RequestID     string
	AttemptIndex  int
	UserID        int64
	ModelID       int64
	RouteID       int64
	ChannelID     int64
	ChannelName   string
	ProtocolCode  string
	UpstreamModel string
	Phase         string
	Result        string
	HTTPStatus    int
	UpstreamCode  string
	UpstreamMsg   string
	IsRetryable   bool
	RetryDecision string
	ConnectMS     int64
	FirstByteMS   int64
	TotalMS       int64
	TokensIn      int64
	TokensOut     int64
	BytesIn       int64
	BytesOut      int64
	ErrorClass    string
	CreatedAt     time.Time
}

// UsageWriter is the asynchronous P28/P29 writer.
//
// 丢日志不丢资金：WriteUsage/WriteAttempt never block and never return an error
// to the request path. A failed flush is logged at WARN and the batch is
// dropped; the settlement already committed in t_ledger_entry is untouched.
type UsageWriter struct {
	DB            *sql.DB
	Logger        *obslog.Logger
	Clock         clock.Clock
	FlushInterval time.Duration
	BatchSize     int

	// BodyMode reports log.body_mode. A nil function means meta_only: bodies
	// are never queued. The function is read per WriteUsage so a runtime config
	// change (opt_in ↔ meta_only) takes effect without a restart.
	BodyMode func() string
	// BodyKeys seals bodies with the current LOG_BODY_KEK and reports the id
	// to store next to each row. Nil means no usable body key: bodies are
	// dropped and counted with reason="no_kek"; usage rows are still written.
	//
	// It is a ring (current + retired keys) rather than one Box so that a key
	// rotation does not orphan the rows written before it; reading is the other
	// half, in internal/control/logq (G3, errata E-39).
	BodyKeys *cryptobox.BodyKeyRing
	// RetentionDays reports log.retention_days. Nil or <=0 means 180.
	RetentionDays func() int
	// Metrics receives the body counters. Nil is safe.
	Metrics *obsmetrics.Recorder

	usageCh   chan UsageRecord
	attemptCh chan AttemptRecord
	bodyCh    chan bodyItem

	written     atomic.Int64
	dropped     atomic.Int64
	bodyDropped atomic.Int64
}

// NewUsageWriter builds the writer.
func NewUsageWriter(db *sql.DB, logger *obslog.Logger, clk clock.Clock) *UsageWriter {
	if clk == nil {
		clk = clock.Real{}
	}
	return &UsageWriter{
		DB:            db,
		Logger:        logger,
		Clock:         clk,
		FlushInterval: DefaultFlushInterval,
		BatchSize:     DefaultFlushBatch,
		usageCh:       make(chan UsageRecord, usageQueueSize),
		attemptCh:     make(chan AttemptRecord, usageQueueSize),
		bodyCh:        make(chan bodyItem, bodyQueueSize),
	}
}

// WriteUsage enqueues one request-level row. It also hands the body to the
// body queue when log.body_mode=opt_in; sealing happens later, on the flush
// goroutine, never on the gateway request path.
func (w *UsageWriter) WriteUsage(r UsageRecord) {
	if w == nil {
		return
	}
	body := r.RawBody
	r.RawBody = nil
	r.FullLogSaved = false
	if len(body) > 0 && w.bodyMode() == config.EnumOptIn {
		switch {
		case w.BodyKeys == nil:
			w.dropBody("no_kek")
		default:
			select {
			case w.bodyCh <- bodyItem{
				RequestID: r.RequestID,
				UserID:    r.UserID,
				Body:      body,
				CreatedAt: r.CreatedAt,
			}:
				r.FullLogSaved = true
			default:
				w.dropBody("queue_full")
			}
		}
	}
	select {
	case w.usageCh <- r:
	default:
		w.drop("usage")
	}
}

func (w *UsageWriter) bodyMode() string {
	if w.BodyMode == nil {
		return config.EnumMetaOnly
	}
	return w.BodyMode()
}

func (w *UsageWriter) retentionDays() int {
	if w.RetentionDays != nil {
		if v := w.RetentionDays(); v > 0 {
			return v
		}
	}
	return DefaultBodyRetentionDays
}

// WriteAttempt enqueues one attempt-level row.
func (w *UsageWriter) WriteAttempt(r AttemptRecord) {
	if w == nil {
		return
	}
	select {
	case w.attemptCh <- r:
	default:
		w.drop("attempt")
	}
}

// Stats returns flushed and dropped row counts.
func (w *UsageWriter) Stats() (written, dropped int64) {
	if w == nil {
		return 0, 0
	}
	return w.written.Load(), w.dropped.Load()
}

// Run flushes every FlushInterval until ctx is cancelled. The final flush uses
// a detached context so a shutdown does not discard rows already accepted.
func (w *UsageWriter) Run(ctx context.Context) error {
	if w == nil || w.DB == nil {
		return nil
	}
	interval := w.FlushInterval
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_ = w.Flush(finalCtx)
			cancel()
			return nil
		case <-ticker.C:
			if err := w.Flush(ctx); err != nil && w.Logger != nil {
				w.Logger.Warn(ctx, "usage_flush_failed: "+err.Error(), obslog.Fields{Stage: "P28"})
			}
		}
	}
}

// Flush drains both queues in batches of BatchSize.
func (w *UsageWriter) Flush(ctx context.Context) error {
	if w == nil || w.DB == nil {
		return nil
	}
	usage := w.drainUsage()
	attempts := w.drainAttempts()
	bodies := w.drainBodies()
	// Bodies go first: a usage row carrying full_log_saved=1 must not become
	// visible before the row it points at exists.
	w.flushBodies(ctx, bodies)
	var firstErr error
	for start := 0; start < len(usage); start += w.batchSize() {
		end := start + w.batchSize()
		if end > len(usage) {
			end = len(usage)
		}
		if err := w.insertUsage(ctx, usage[start:end]); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if w.Logger != nil {
				w.Logger.Warn(ctx, "usage_insert_failed: "+err.Error(), obslog.Fields{Stage: "P28"})
			}
			continue
		}
		w.written.Add(int64(end - start))
	}
	for start := 0; start < len(attempts); start += w.batchSize() {
		end := start + w.batchSize()
		if end > len(attempts) {
			end = len(attempts)
		}
		if err := w.insertAttempts(ctx, attempts[start:end]); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if w.Logger != nil {
				w.Logger.Warn(ctx, "upstream_attempt_insert_failed: "+err.Error(), obslog.Fields{Stage: "P29"})
			}
			continue
		}
		w.written.Add(int64(end - start))
	}
	return firstErr
}

func (w *UsageWriter) drainUsage() []UsageRecord {
	var out []UsageRecord
	for {
		select {
		case r := <-w.usageCh:
			out = append(out, r)
		default:
			return out
		}
	}
}

func (w *UsageWriter) drainAttempts() []AttemptRecord {
	var out []AttemptRecord
	for {
		select {
		case r := <-w.attemptCh:
			out = append(out, r)
		default:
			return out
		}
	}
}

func (w *UsageWriter) drainBodies() []bodyItem {
	var out []bodyItem
	for {
		select {
		case r := <-w.bodyCh:
			out = append(out, r)
		default:
			return out
		}
	}
}

// flushBodies seals and inserts the queued bodies. Every failure drops that
// body and increments gw_log_body_dropped_total{reason}; it never returns an
// error to Flush, because a body failure must not be reported as a usage
// failure and must not affect the settlement rows.
func (w *UsageWriter) flushBodies(ctx context.Context, rows []bodyItem) {
	if len(rows) == 0 {
		return
	}
	if w.BodyKeys == nil {
		for range rows {
			w.dropBody("no_kek")
		}
		return
	}
	sealed := make([]bodyItem, 0, len(rows))
	for _, r := range rows {
		keyID, envelope, err := w.BodyKeys.Seal(r.Body)
		r.Body = nil // release the plaintext reference before the batch is built
		if err != nil {
			w.dropBody("seal_failed")
			continue
		}
		r.Ciphertext = []byte(envelope)
		r.KeyID = keyID
		sealed = append(sealed, r)
	}
	for start := 0; start < len(sealed); start += bodyFlushBatch {
		end := start + bodyFlushBatch
		if end > len(sealed) {
			end = len(sealed)
		}
		if err := w.insertBodies(ctx, sealed[start:end]); err != nil {
			for range sealed[start:end] {
				w.dropBody("db_error")
			}
			if w.Logger != nil {
				w.Logger.Warn(ctx, "log_body_insert_failed: "+err.Error(), obslog.Fields{Stage: "P28"})
			}
			continue
		}
		if w.Metrics != nil {
			for range sealed[start:end] {
				w.Metrics.LogBodyWritten()
			}
		}
	}
}

const bodyColumns = `request_id, user_id, ciphertext, body_key_id, created_at, expires_at`

const bodyColumnCount = 6

func (w *UsageWriter) insertBodies(ctx context.Context, rows []bodyItem) error {
	if len(rows) == 0 {
		return nil
	}
	args := make([]any, 0, len(rows)*bodyColumnCount)
	for _, r := range rows {
		created := w.createdAt(r.CreatedAt)
		expires := created.AddDate(0, 0, w.retentionDays())
		args = append(args,
			cut(r.RequestID, 36), r.UserID, r.Ciphertext, r.KeyID,
			created.UTC(), expires.UTC(),
		)
	}
	q := "INSERT INTO t_request_body (" + bodyColumns + ") VALUES " +
		placeholders(len(rows), bodyColumnCount) +
		" ON DUPLICATE KEY UPDATE ciphertext = VALUES(ciphertext), body_key_id = VALUES(body_key_id), expires_at = VALUES(expires_at)"
	if _, err := w.DB.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("settle: insert request bodies: %w", err)
	}
	return nil
}

const usageColumns = `request_id, user_id, api_key_id, api_key_name_snap,
model_id, model_name_snap, request_model_req, inbound_protocol,
channel_id, channel_name_snap, outbound_protocol, route_id, price_rule_id,
config_version, group_code_snap, price_ratio_bp, billing_tier,
cache_hit_tokens, cache_write_tokens, is_stream,
http_status, biz_code, result, fail_stage, fail_owner,
tokens_in, tokens_out, tokens_in_est, tokens_out_est, usage_source,
image_count, prehold_micro, consumed_micro, consumed_recharge_micro,
consumed_bonus_micro, refunded_micro, upstream_attempts, ttft_ms, total_ms,
upstream_ms, client_ip, user_agent, client_aborted, full_log_saved,
volume_basis, created_at`

const usageColumnCount = 46

func (w *UsageWriter) insertUsage(ctx context.Context, rows []UsageRecord) error {
	if len(rows) == 0 {
		return nil
	}
	args := make([]any, 0, len(rows)*usageColumnCount)
	created := make([]time.Time, len(rows))
	for i, r := range rows {
		created[i] = w.createdAt(r.CreatedAt)
		args = append(args,
			cut(r.RequestID, 36), r.UserID, r.APIKeyID, cut(r.APIKeyName, 64),
			nullID(r.ModelID), cut(r.ModelName, 128), cut(r.RequestModel, 128), cut(r.InboundProto, 24),
			nullID(r.ChannelID), nullableString(cut(r.ChannelName, 64)),
			nullableString(cut(r.OutboundProto, 24)), nullID(r.RouteID), nullID(r.PriceRuleID),
			r.ConfigVersion, firstNonEmpty(cut(r.GroupCode, 24), "standard"),
			clampInt(r.PriceRatioBP, 0, 65535), firstNonEmpty(cut(r.BillingTier, 16), "BASE"),
			0, 0, boolInt(r.IsStream),
			maxInt(r.HTTPStatus, 200), cut(r.BizCode, 32), firstNonEmpty(cut(r.Result, 16), "FAILED"),
			nullableString(cut(r.FailStage, 24)), nullableString(cut(r.FailOwner, 16)),
			clampInt(r.TokensIn, 0, 4294967295), clampInt(r.TokensOut, 0, 4294967295),
			clampInt(r.TokensInEst, 0, 4294967295), clampInt(r.TokensOutEst, 0, 4294967295),
			firstNonEmpty(cut(r.UsageSource, 16), "NONE"),
			clampInt(r.ImageCount, 0, 255), r.PreholdMicro, r.ConsumedMicro,
			r.ConsumedRechargeMicro, r.ConsumedBonusMicro, r.RefundedMicro,
			clampInt(r.UpstreamAttempts, 0, 255), r.TTFTMS,
			clampInt(r.TotalMS, 0, 4294967295), clampInt(r.UpstreamMS, 0, 4294967295),
			nullBytes(r.ClientIP), nullableString(cut(r.UserAgent, 255)),
			boolInt(r.ClientAborted), boolInt(r.FullLogSaved), "meta", created[i].UTC(),
		)
	}
	q := "INSERT INTO t_usage_record (" + usageColumns + ") VALUES " +
		placeholders(len(rows), usageColumnCount) +
		" ON DUPLICATE KEY UPDATE request_id = VALUES(request_id)"
	_, err := w.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("settle: insert usage rows: %w", err)
	}
	// Request-level log lines are emitted only after the row is durable, so a
	// dropped batch never leaves a log line for a row that was not written.
	if w.Logger != nil {
		for i, r := range rows {
			w.Logger.WriteRequest(ctx, requestLogRecord(r, created[i]))
		}
	}
	return nil
}

const attemptColumns = `request_id, attempt_index, user_id, model_id, route_id, channel_id,
channel_name_snap, protocol_code, upstream_model, phase, result,
http_status, upstream_code, upstream_msg, is_retryable, retry_decision,
connect_ms, first_byte_ms, total_ms, tokens_in, tokens_out, bytes_in, bytes_out,
error_class, created_at`

const attemptColumnCount = 25

func (w *UsageWriter) insertAttempts(ctx context.Context, rows []AttemptRecord) error {
	if len(rows) == 0 {
		return nil
	}
	args := make([]any, 0, len(rows)*attemptColumnCount)
	created := make([]time.Time, len(rows))
	for i, r := range rows {
		created[i] = w.createdAt(r.CreatedAt)
		args = append(args,
			cut(r.RequestID, 36), clampInt(int64(r.AttemptIndex), 0, 255), r.UserID, r.ModelID,
			r.RouteID, r.ChannelID, cut(r.ChannelName, 64), cut(r.ProtocolCode, 32),
			cut(r.UpstreamModel, 128), cut(r.Phase, 16), firstNonEmpty(cut(r.Result, 16), "FAILED"),
			nullPositive(r.HTTPStatus), nullableString(cut(r.UpstreamCode, 64)),
			nullableString(cut(r.UpstreamMsg, 512)), boolInt(r.IsRetryable),
			nullableString(cut(r.RetryDecision, 24)), nullPositive64(r.ConnectMS),
			nullPositive64(r.FirstByteMS), clampInt(r.TotalMS, 0, 4294967295),
			clampInt(r.TokensIn, 0, 4294967295), clampInt(r.TokensOut, 0, 4294967295),
			clampInt(r.BytesIn, 0, 4294967295), clampInt(r.BytesOut, 0, 4294967295),
			nullableString(cut(r.ErrorClass, 24)), created[i].UTC(),
		)
	}
	q := "INSERT INTO t_upstream_attempt (" + attemptColumns + ") VALUES " +
		placeholders(len(rows), attemptColumnCount) +
		" ON DUPLICATE KEY UPDATE request_id = VALUES(request_id)"
	_, err := w.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("settle: insert attempt rows: %w", err)
	}
	if w.Logger != nil {
		for _, r := range rows {
			w.Logger.WriteAttempt(ctx, attemptLogRecord(r))
		}
	}
	return nil
}

func (w *UsageWriter) batchSize() int {
	if w.BatchSize <= 0 {
		return DefaultFlushBatch
	}
	return w.BatchSize
}

func (w *UsageWriter) drop(kind string) {
	n := w.dropped.Add(1)
	if w.Logger != nil && n%100 == 1 {
		w.Logger.Warn(context.TODO(), fmt.Sprintf("usage_queue_full kind=%s dropped=%d", kind, n),
			obslog.Fields{Stage: "P28"})
	}
}

// dropBody records a discarded body. The metric is unconditional (it is the
// only signal that separates "logging off" from "logging on but broken"); the
// WARN line is throttled like drop() so a full queue cannot flood the log.
func (w *UsageWriter) dropBody(reason string) {
	n := w.bodyDropped.Add(1)
	if w.Metrics != nil {
		w.Metrics.LogBodyDropped(reason)
	}
	if w.Logger != nil && n%100 == 1 {
		w.Logger.Warn(context.TODO(), fmt.Sprintf("log_body_dropped reason=%s dropped=%d", reason, n),
			obslog.Fields{Stage: "P28"})
	}
}

func (w *UsageWriter) now() time.Time {
	if w.Clock == nil {
		return time.Now().UTC()
	}
	return w.Clock.Now().UTC()
}

// createdAt resolves the value written to created_at once, so the row and its
// log line cannot disagree at the UTC+8 midnight boundary.
func (w *UsageWriter) createdAt(t time.Time) time.Time {
	if t.IsZero() {
		return w.now()
	}
	return t.UTC()
}

func placeholders(rows, cols int) string {
	one := "(" + strings.TrimSuffix(strings.Repeat("?,", cols), ",") + ")"
	return strings.TrimSuffix(strings.Repeat(one+",", rows), ",")
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullID(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullPositive(v int) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullPositive64(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullBytes(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func firstNonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func maxInt(v, floor int) int {
	if v < floor {
		return floor
	}
	return v
}

func clampInt(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

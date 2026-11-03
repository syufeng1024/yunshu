package settle

import (
	"context"
	"testing"
	"time"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/cryptobox"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/platform/clock"
)

// testBodyKEK is base64 of "0123456789abcdef0123456789abcdef" (32 bytes). It is
// a fixed test key, never a deployment value.
const testBodyKEK = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func newBodyTestBox(t *testing.T) *cryptobox.Box {
	t.Helper()
	box, err := cryptobox.NewBodyFromBase64(testBodyKEK)
	if err != nil {
		t.Fatalf("body KEK: %v", err)
	}
	return box
}

// bodyTestRing wraps one box in a single-key ring: the writer asks the ring for
// the id to store, never for a bare Box (G3).
func bodyTestRing(t *testing.T, id string, box *cryptobox.Box) *cryptobox.BodyKeyRing {
	t.Helper()
	ring, err := cryptobox.NewBodyKeyRingFromBoxes(id, map[string]*cryptobox.Box{id: box})
	if err != nil {
		t.Fatalf("body key ring: %v", err)
	}
	return ring
}

func newBodyTestWriter(t *testing.T, mode string, box *cryptobox.Box, retentionDays int) (*UsageWriter, *obsmetrics.Registry) {
	t.Helper()
	reg := obsmetrics.NewRegistry()
	w := NewUsageWriter(testDB, nil, clock.Real{})
	w.BodyMode = func() string { return mode }
	if box != nil {
		w.BodyKeys = bodyTestRing(t, cryptobox.BodyKeyID, box)
	}
	w.RetentionDays = func() int { return retentionDays }
	w.Metrics = obsmetrics.NewRecorder(reg)
	return w, reg
}

func metricTotal(reg *obsmetrics.Registry, name string) int64 {
	var total int64
	for _, f := range reg.Snapshot().Families {
		if f.Name != name {
			continue
		}
		for _, s := range f.Samples {
			total += s.Value
		}
	}
	return total
}

func bodyUsageRecord(rid string, userID int64, raw []byte, at time.Time) UsageRecord {
	return UsageRecord{
		RequestID:     rid,
		UserID:        userID,
		APIKeyID:      5,
		ModelName:     "gpt-4o-mini",
		RequestModel:  "gpt-4o-mini",
		InboundProto:  "OPENAI_COMPAT",
		OutboundProto: "OPENAI_COMPAT",
		GroupCode:     "standard",
		PriceRatioBP:  10000,
		BillingTier:   "BASE",
		HTTPStatus:    200,
		Result:        "SUCCESS",
		UsageSource:   "UPSTREAM_API",
		TotalMS:       12,
		CreatedAt:     at,
		RawBody:       raw,
	}
}

func cleanupBodyRows(t *testing.T, rid string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM t_request_body WHERE request_id = ?`, rid)
		_, _ = testDB.Exec(`DELETE FROM t_usage_record WHERE request_id = ?`, rid)
	})
}

// Hard constraint 4: meta_only is a row-count promise, not an empty-column
// promise. The usage row is still written; t_request_body has no row at all.
func TestBodyMetaOnlyWritesZeroRows(t *testing.T) {
	ctx := context.Background()
	rid := newRequestID(t)
	userID := newFundedUser(t)
	cleanupBodyRows(t, rid)

	w, reg := newBodyTestWriter(t, config.EnumMetaOnly, newBodyTestBox(t), 180)
	w.WriteUsage(bodyUsageRecord(rid, userID, []byte(`{"messages":[{"role":"user","content":"secret"}]}`), time.Now().UTC()))
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if n := countRows(t, `SELECT COUNT(*) FROM t_request_body WHERE user_id = ?`, userID); n != 0 {
		t.Fatalf("t_request_body rows = %d under meta_only, want 0", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM t_usage_record WHERE request_id = ?`, rid); n != 1 {
		t.Fatalf("usage rows = %d, want 1 (meta_only must not affect usage)", n)
	}
	if saved := countRows(t, `SELECT full_log_saved FROM t_usage_record WHERE request_id = ?`, rid); saved != 0 {
		t.Fatalf("full_log_saved = %d under meta_only, want 0", saved)
	}
	if got := metricTotal(reg, "gw_log_body_written_total"); got != 0 {
		t.Fatalf("gw_log_body_written_total = %d under meta_only, want 0", got)
	}
}

// opt_in persists an envelope, never the plaintext, and expires_at is exactly
// created_at + log.retention_days.
func TestBodyOptInWritesCiphertextAndExpiry(t *testing.T) {
	ctx := context.Background()
	rid := newRequestID(t)
	userID := newFundedUser(t)
	cleanupBodyRows(t, rid)
	box := newBodyTestBox(t)
	plaintext := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello-body"}]}`)
	createdAt := time.Now().UTC().Truncate(time.Millisecond)

	w, reg := newBodyTestWriter(t, config.EnumOptIn, box, 30)
	w.WriteUsage(bodyUsageRecord(rid, userID, plaintext, createdAt))
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var (
		ciphertext []byte
		keyID      string
		seconds    int64
	)
	if err := testDB.QueryRow(`
		SELECT ciphertext, body_key_id,
		       TIMESTAMPDIFF(SECOND, created_at, expires_at)
		  FROM t_request_body
		 WHERE request_id = ? AND user_id = ?`, rid, userID,
	).Scan(&ciphertext, &keyID, &seconds); err != nil {
		t.Fatalf("read body row: %v", err)
	}
	if keyID != cryptobox.BodyKeyID {
		t.Fatalf("body_key_id = %q, want %q", keyID, cryptobox.BodyKeyID)
	}
	if string(ciphertext) == string(plaintext) {
		t.Fatal("t_request_body.ciphertext equals the plaintext")
	}
	if len(ciphertext) < len(cryptobox.EnvelopePrefix) ||
		string(ciphertext[:len(cryptobox.EnvelopePrefix)]) != cryptobox.EnvelopePrefix {
		t.Fatalf("stored value is not a %q envelope: %q", cryptobox.EnvelopePrefix, ciphertext)
	}
	opened, err := box.Open(string(ciphertext))
	if err != nil {
		t.Fatalf("open stored body: %v", err)
	}
	if string(opened) != string(plaintext) {
		t.Fatalf("round-trip = %q, want %q", opened, plaintext)
	}
	if seconds != int64(30*24*3600) {
		t.Fatalf("expires_at - created_at = %d seconds, want %d", seconds, 30*24*3600)
	}
	if saved := countRows(t, `SELECT full_log_saved FROM t_usage_record WHERE request_id = ?`, rid); saved != 1 {
		t.Fatalf("full_log_saved = %d under opt_in, want 1", saved)
	}
	if got := metricTotal(reg, "gw_log_body_written_total"); got != 1 {
		t.Fatalf("gw_log_body_written_total = %d, want 1", got)
	}
}

// Hard constraint 5: a full body queue drops the body and nothing else. The
// usage row is still written and full_log_saved stays 0, so the console does
// not advertise a body that is not there.
func TestBodyQueueFullDropsBodyKeepsUsage(t *testing.T) {
	ctx := context.Background()
	rid := newRequestID(t)
	userID := newFundedUser(t)
	cleanupBodyRows(t, rid)
	now := time.Now().UTC()

	w, reg := newBodyTestWriter(t, config.EnumOptIn, newBodyTestBox(t), 180)
	// Fill the queue directly: bodyQueueSize writes would otherwise behave the
	// same but make the assertion depend on batching order.
	for i := 0; i < bodyQueueSize; i++ {
		w.bodyCh <- bodyItem{RequestID: newRequestID(t), UserID: userID, Body: []byte("filler"), CreatedAt: now}
	}
	w.WriteUsage(bodyUsageRecord(rid, userID, []byte(`{"dropped":true}`), now))
	if got := len(w.bodyCh); got != bodyQueueSize {
		t.Fatalf("body queue length = %d after a full-queue write, want %d (body was not dropped)", got, bodyQueueSize)
	}
	// Drain the filler items without inserting them, so the test's row counts
	// stay exact. The production Flush would seal and insert them.
	_ = w.drainBodies()
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if n := countRows(t, `SELECT COUNT(*) FROM t_usage_record WHERE request_id = ?`, rid); n != 1 {
		t.Fatalf("usage rows = %d, want 1 (a dropped body must not drop usage)", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM t_request_body WHERE request_id = ?`, rid); n != 0 {
		t.Fatalf("body rows = %d for a dropped body, want 0", n)
	}
	if saved := countRows(t, `SELECT full_log_saved FROM t_usage_record WHERE request_id = ?`, rid); saved != 0 {
		t.Fatalf("full_log_saved = %d for a dropped body, want 0", saved)
	}
	if got := metricTotal(reg, "gw_log_body_dropped_total"); got != 1 {
		t.Fatalf("gw_log_body_dropped_total = %d, want 1", got)
	}
	if got := metricTotal(reg, "gw_log_body_written_total"); got != 0 {
		t.Fatalf("gw_log_body_written_total = %d, want 0", got)
	}
}

// A missing body KEK is a drop, not a request failure: the usage row is still
// written and the reason is counted as no_kek.
func TestBodyNoKEKDropsWithoutFailingUsage(t *testing.T) {
	ctx := context.Background()
	rid := newRequestID(t)
	userID := newFundedUser(t)
	cleanupBodyRows(t, rid)

	w, reg := newBodyTestWriter(t, config.EnumOptIn, nil, 180)
	w.WriteUsage(bodyUsageRecord(rid, userID, []byte("body"), time.Now().UTC()))
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if n := countRows(t, `SELECT COUNT(*) FROM t_usage_record WHERE request_id = ?`, rid); n != 1 {
		t.Fatalf("usage rows = %d, want 1", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM t_request_body WHERE request_id = ?`, rid); n != 0 {
		t.Fatalf("body rows = %d without a KEK, want 0", n)
	}
	if got := metricTotal(reg, "gw_log_body_dropped_total"); got != 1 {
		t.Fatalf("gw_log_body_dropped_total = %d, want 1", got)
	}
}

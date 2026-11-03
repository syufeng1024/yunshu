package logq

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/cryptobox"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
)

// decryptTestKEK is base64 of "0123456789abcdef0123456789abcdef" (32 bytes).
const decryptTestKEK = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// newDecryptRing wraps one box in a single-key ring. The reader is keyed by the
// row's body_key_id, so even the single-key case goes through the ring.
func newDecryptRing(t *testing.T, id string, box *cryptobox.Box) *cryptobox.BodyKeyRing {
	t.Helper()
	ring, err := cryptobox.NewBodyKeyRingFromBoxes(id, map[string]*cryptobox.Box{id: box})
	if err != nil {
		t.Fatalf("body key ring: %v", err)
	}
	return ring
}

func newDecryptBox(t *testing.T) *cryptobox.Box {
	t.Helper()
	box, err := cryptobox.NewBodyFromBase64(decryptTestKEK)
	if err != nil {
		t.Fatalf("body KEK: %v", err)
	}
	return box
}

func seedBody(t *testing.T, uid int64, requestID string, ciphertext []byte, keyID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM t_request_body WHERE user_id = ?`, uid)
	})
	_, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_request_body
			(request_id, user_id, ciphertext, body_key_id, created_at, expires_at)
		VALUES (?,?,?,?,?,?)`,
		requestID, uid, ciphertext, keyID, testBase, testBase.AddDate(0, 0, 180))
	if err != nil {
		t.Fatalf("seed body %s: %v", requestID, err)
	}
}

func openFailedTotal(reg *obsmetrics.Registry) int64 {
	var total int64
	for _, f := range reg.Snapshot().Families {
		if f.Name != "gw_log_body_open_failed_total" {
			continue
		}
		for _, s := range f.Samples {
			total += s.Value
		}
	}
	return total
}

// The production store opens the envelope and returns the plaintext unchanged.
func TestBodyDecryptSuccess(t *testing.T) {
	uid := newTestUser(t, "PERSONAL")
	svc := newTestService(t, newFakeClock(testBase))
	requestID := newRequestID()
	seedUsage(t, usageSeed{
		requestID: requestID, uid: uid, fullLog: true, createdAt: testBase,
	})
	box := newDecryptBox(t)
	plain := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}]}`
	envelope, err := box.Seal([]byte(plain))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	seedBody(t, uid, requestID, []byte(envelope), cryptobox.BodyKeyID)

	reg := obsmetrics.NewRegistry()
	svc.BodyStore = &sqlBodyStore{DB: testDB, Keys: newDecryptRing(t, cryptobox.BodyKeyID, box), Metrics: obsmetrics.NewRecorder(reg)}

	rec := callUserID(t, svc.Body, http.MethodGet,
		"/api/console/logs/"+requestID+"/body", requestID, uid)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var out bodyResponse
	decodeBody(t, rec, &out)
	if out.Body != plain {
		t.Fatalf("body = %q, want %q", out.Body, plain)
	}
	if out.Redacted {
		t.Fatal("redacted = true for a body with no sensitive values")
	}
	if got := openFailedTotal(reg); got != 0 {
		t.Fatalf("gw_log_body_open_failed_total = %d on success, want 0", got)
	}
}

// A tampered envelope is a 404, never a 500 and never an empty success.
func TestBodyDecryptCorruptCiphertextIs404(t *testing.T) {
	uid := newTestUser(t, "PERSONAL")
	svc := newTestService(t, newFakeClock(testBase))
	requestID := newRequestID()
	seedUsage(t, usageSeed{
		requestID: requestID, uid: uid, fullLog: true, createdAt: testBase,
	})
	box := newDecryptBox(t)
	// 20-byte plaintext → 48-byte payload → 64 base64 chars, no padding.
	envelope, err := box.Seal([]byte("0123456789abcdefghij"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	tampered := []byte(envelope)
	tampered[10] ^= 0x01 // inside the nonce: the GCM tag must reject it
	if string(tampered) == envelope {
		t.Fatal("tamper did not change the envelope")
	}
	seedBody(t, uid, requestID, tampered, cryptobox.BodyKeyID)

	reg := obsmetrics.NewRegistry()
	svc.BodyStore = &sqlBodyStore{DB: testDB, Keys: newDecryptRing(t, cryptobox.BodyKeyID, box), Metrics: obsmetrics.NewRecorder(reg)}

	rec := callUserID(t, svc.Body, http.MethodGet,
		"/api/console/logs/"+requestID+"/body", requestID, uid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
	if code := errorOf(t, rec).Error.Code; code != "resource_not_found" {
		t.Fatalf("code=%q want resource_not_found", code)
	}
	if got := openFailedTotal(reg); got != 1 {
		t.Fatalf("gw_log_body_open_failed_total = %d, want 1", got)
	}
}

// A row naming a key this node does not hold is indistinguishable from an
// absent body to the caller, but it is counted for the operator.
func TestBodyUnknownKeyIDIs404(t *testing.T) {
	uid := newTestUser(t, "PERSONAL")
	svc := newTestService(t, newFakeClock(testBase))
	requestID := newRequestID()
	seedUsage(t, usageSeed{
		requestID: requestID, uid: uid, fullLog: true, createdAt: testBase,
	})
	box := newDecryptBox(t)
	envelope, err := box.Seal([]byte("body"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	seedBody(t, uid, requestID, []byte(envelope), "body-v0")

	reg := obsmetrics.NewRegistry()
	svc.BodyStore = &sqlBodyStore{DB: testDB, Keys: newDecryptRing(t, cryptobox.BodyKeyID, box), Metrics: obsmetrics.NewRecorder(reg)}

	rec := callUserID(t, svc.Body, http.MethodGet,
		"/api/console/logs/"+requestID+"/body", requestID, uid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
	if got := openFailedTotal(reg); got != 1 {
		t.Fatalf("gw_log_body_open_failed_total = %d, want 1", got)
	}
}

// A node without LOG_BODY_KEK cannot open a perfectly valid envelope. The
// request still answers 404 (the row is unreadable, not the server broken).
func TestBodyMissingKEKIs404(t *testing.T) {
	uid := newTestUser(t, "PERSONAL")
	svc := newTestService(t, newFakeClock(testBase))
	requestID := newRequestID()
	seedUsage(t, usageSeed{
		requestID: requestID, uid: uid, fullLog: true, createdAt: testBase,
	})
	box := newDecryptBox(t)
	envelope, err := box.Seal([]byte("body"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	seedBody(t, uid, requestID, []byte(envelope), cryptobox.BodyKeyID)

	t.Setenv(config.EnvLogBodyKEK, "")
	reg := obsmetrics.NewRegistry()
	// Box is nil, so the store lazily reads the (empty) environment variable.
	svc.BodyStore = &sqlBodyStore{DB: testDB, Metrics: obsmetrics.NewRecorder(reg)}

	rec := callUserID(t, svc.Body, http.MethodGet,
		"/api/console/logs/"+requestID+"/body", requestID, uid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
	body := errorOf(t, rec)
	if body.Error.Code != "resource_not_found" {
		t.Fatalf("code=%q want resource_not_found", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "正文") {
		t.Fatalf("message must explain the missing body: %+v", body.Error)
	}
	if got := openFailedTotal(reg); got != 1 {
		t.Fatalf("gw_log_body_open_failed_total = %d, want 1", got)
	}
}

// The body window and the store are independent: an old usage row is 404
// before the store is touched, even when a real row exists.
func TestBodyDecryptRespectsWindow(t *testing.T) {
	uid := newTestUser(t, "PERSONAL")
	svc := newTestService(t, newFakeClock(testBase))
	requestID := newRequestID()
	seedUsage(t, usageSeed{
		requestID: requestID, uid: uid, fullLog: true,
		createdAt: testBase.Add(-bodyWindow - time.Hour),
	})
	box := newDecryptBox(t)
	envelope, err := box.Seal([]byte("old body"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	seedBody(t, uid, requestID, []byte(envelope), cryptobox.BodyKeyID)
	svc.BodyStore = &sqlBodyStore{DB: testDB, Keys: newDecryptRing(t, cryptobox.BodyKeyID, box)}

	rec := callUserID(t, svc.Body, http.MethodGet,
		"/api/console/logs/"+requestID+"/body", requestID, uid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 for a body past the window", rec.Code)
	}
}

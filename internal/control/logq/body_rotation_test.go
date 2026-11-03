package logq

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/cryptobox"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
)

// rotateKEK2 is base64 of "abcdef0123456789abcdef0123456789" (32 bytes) — a
// second, distinct test key. Fixed test material, never a deployment value.
const rotateKEK2 = "YWJjZGVmMDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODk="

func newRotateBox(t *testing.T, kek string) *cryptobox.Box {
	t.Helper()
	box, err := cryptobox.NewBodyFromBase64(kek)
	if err != nil {
		t.Fatalf("body KEK: %v", err)
	}
	return box
}

// TestBodySurvivesKeyRotation is the G3 acceptance case.
//
// Before this, body_key_id was a compile-time constant and the reader accepted
// exactly that value: rotating LOG_BODY_KEK made every stored body unreadable
// for the rest of its 180-day window. Now the row names the key that sealed it,
// and the ring keeps the retired key available for reading.
func TestBodySurvivesKeyRotation(t *testing.T) {
	uid := newTestUser(t, "PERSONAL")
	svc := newTestService(t, newFakeClock(testBase))
	requestID := newRequestID()
	seedUsage(t, usageSeed{
		requestID: requestID, uid: uid, fullLog: true, createdAt: testBase,
	})

	// --- before rotation: key v1 seals the row -------------------------------
	v1 := newRotateBox(t, decryptTestKEK)
	ringV1, err := cryptobox.NewBodyKeyRingFromBoxes("body-v1", map[string]*cryptobox.Box{
		"body-v1": v1,
	})
	if err != nil {
		t.Fatalf("ring v1: %v", err)
	}
	const plain = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"rotate me"}]}`
	keyID, envelope, err := ringV1.Seal([]byte(plain))
	if err != nil {
		t.Fatalf("seal with v1: %v", err)
	}
	if keyID != "body-v1" {
		t.Fatalf("sealed with key id %q, want body-v1", keyID)
	}
	seedBody(t, uid, requestID, []byte(envelope), keyID)

	// --- after rotation: v2 is current, v1 is retired but still readable -----
	v2 := newRotateBox(t, rotateKEK2)
	ringV2, err := cryptobox.NewBodyKeyRingFromBoxes("body-v2", map[string]*cryptobox.Box{
		"body-v1": v1,
		"body-v2": v2,
	})
	if err != nil {
		t.Fatalf("ring v2: %v", err)
	}
	reg := obsmetrics.NewRegistry()
	svc.BodyStore = &sqlBodyStore{DB: testDB, Keys: ringV2, Metrics: obsmetrics.NewRecorder(reg)}

	// ① the old row still opens
	rec := callUserID(t, svc.Body, http.MethodGet,
		"/api/console/logs/"+requestID+"/body", requestID, uid)
	if rec.Code != http.StatusOK {
		t.Fatalf("old body after rotation: status=%d body=%s (want 200)", rec.Code, rec.Body.String())
	}
	var out bodyResponse
	decodeBody(t, rec, &out)
	if out.Body != plain {
		t.Fatalf("old body decrypted to %q, want the original plaintext", out.Body)
	}
	if n := openFailedTotal(reg); n != 0 {
		t.Fatalf("gw_log_body_open_failed_total = %d after a successful retired-key open, want 0", n)
	}

	// ② a new row under the current key names body-v2
	keyID2, envelope2, err := ringV2.Seal([]byte(plain))
	if err != nil {
		t.Fatalf("seal with v2: %v", err)
	}
	if keyID2 != "body-v2" {
		t.Fatalf("sealed with key id %q, want body-v2", keyID2)
	}
	newRequestIDValue := newRequestID()
	seedUsage(t, usageSeed{
		requestID: newRequestIDValue, uid: uid, fullLog: true, createdAt: testBase,
	})
	seedBody(t, uid, newRequestIDValue, []byte(envelope2), keyID2)
	rec = callUserID(t, svc.Body, http.MethodGet,
		"/api/console/logs/"+newRequestIDValue+"/body", newRequestIDValue, uid)
	if rec.Code != http.StatusOK {
		t.Fatalf("new body: status=%d body=%s", rec.Code, rec.Body.String())
	}
	t.Logf("rotation: v1 row and v2 row both readable, ids %q / %q", keyID, keyID2)
}

// TestBodyKeyIDNotConfiguredIs404 is ③: a row naming a key this node does not
// have must stay a 404 plus gw_log_body_open_failed_total — never a 500, and
// never a silent fallback to the current key (which would report a decrypt
// failure for a row whose only problem is a retired key).
func TestBodyKeyIDNotConfiguredIs404(t *testing.T) {
	uid := newTestUser(t, "PERSONAL")
	svc := newTestService(t, newFakeClock(testBase))
	requestID := newRequestID()
	seedUsage(t, usageSeed{
		requestID: requestID, uid: uid, fullLog: true, createdAt: testBase,
	})

	// Sealed by a key this node will not be given.
	other := newRotateBox(t, rotateKEK2)
	ringV9, err := cryptobox.NewBodyKeyRingFromBoxes("body-v9", map[string]*cryptobox.Box{
		"body-v9": other,
	})
	if err != nil {
		t.Fatalf("ring v9: %v", err)
	}
	_, envelope, err := ringV9.Seal([]byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	seedBody(t, uid, requestID, []byte(envelope), "body-v9")

	// This node only knows body-v1.
	reg := obsmetrics.NewRegistry()
	ring := newDecryptRing(t, cryptobox.BodyKeyID, newDecryptBox(t))
	svc.BodyStore = &sqlBodyStore{DB: testDB, Keys: ring, Metrics: obsmetrics.NewRecorder(reg)}

	rec := callUserID(t, svc.Body, http.MethodGet,
		"/api/console/logs/"+requestID+"/body", requestID, uid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unconfigured body_key_id (body=%s)", rec.Code, rec.Body.String())
	}
	if n := openFailedTotal(reg); n != 1 {
		t.Fatalf("gw_log_body_open_failed_total = %d, want 1", n)
	}
	t.Logf("unconfigured key id: status=%d open_failed=%d", rec.Code, openFailedTotal(reg))
}

// TestBodyRingFromEnvReadsRotationVars proves the env wiring, including the
// default id for a deployment that never sets LOG_BODY_KEY_ID.
func TestBodyRingFromEnvReadsRotationVars(t *testing.T) {
	t.Setenv(config.EnvLogBodyKEK, decryptTestKEK)
	t.Setenv(cryptobox.EnvBodyKeyID, "")
	t.Setenv(cryptobox.EnvBodyKeyPrevious, "")
	ring, err := cryptobox.BuildBodyKeyRing(decryptTestKEK)
	if err != nil {
		t.Fatalf("ring from empty rotation env: %v", err)
	}
	if ring.CurrentID() != cryptobox.BodyKeyID {
		t.Fatalf("default current id = %q, want %q (existing rows carry it)",
			ring.CurrentID(), cryptobox.BodyKeyID)
	}

	t.Setenv(cryptobox.EnvBodyKeyID, "body-v2")
	t.Setenv(cryptobox.EnvBodyKeyPrevious, "body-v1:"+decryptTestKEK+",body-v0:"+rotateKEK2)
	ring, err = cryptobox.BuildBodyKeyRing(rotateKEK2)
	if err != nil {
		t.Fatalf("ring from rotation env: %v", err)
	}
	if ring.CurrentID() != "body-v2" {
		t.Fatalf("current id = %q, want body-v2", ring.CurrentID())
	}
	if got, want := ring.KeyIDs(), []string{"body-v0", "body-v1", "body-v2"}; len(got) != len(want) {
		t.Fatalf("configured ids = %v, want %v", got, want)
	}
}

// TestBodyRotationDuplicateIDFailsStartup is ④: an id configured twice (or an
// id that collides with the current one) is ambiguous — the wrong key would win
// non-deterministically — so the ring refuses to be built and the daemon logs a
// startup warning instead of guessing.
func TestBodyRotationDuplicateIDFailsStartup(t *testing.T) {
	t.Setenv(cryptobox.EnvBodyKeyID, "body-v2")
	t.Setenv(cryptobox.EnvBodyKeyPrevious, "body-v2:"+decryptTestKEK+",body-v1:"+rotateKEK2)
	if _, err := cryptobox.BuildBodyKeyRing(rotateKEK2); err == nil {
		t.Fatalf("duplicate body-v2 in the previous list was accepted; the wrong key could win")
	} else if !isDuplicateKeyErr(err) {
		t.Fatalf("error = %v; want a duplicate-id error", err)
	}

	// Also duplicated *within* the previous list.
	t.Setenv(cryptobox.EnvBodyKeyID, "body-v3")
	t.Setenv(cryptobox.EnvBodyKeyPrevious, "body-v1:"+decryptTestKEK+",body-v1:"+rotateKEK2)
	if _, err := cryptobox.BuildBodyKeyRing(rotateKEK2); err == nil {
		t.Fatalf("duplicate body-v1 inside the previous list was accepted")
	}
}

func isDuplicateKeyErr(err error) bool {
	return errors.Is(err, cryptobox.ErrDuplicateBodyKeyID) ||
		strings.Contains(err.Error(), "duplicate body key id")
}

package key

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"testing"

	"yunshu.dev/core/internal/cryptobox"
)

// newBox returns a cryptobox backed by a deterministic 32-byte KEK so the test
// never depends on the environment.
func newBox(t *testing.T) *cryptobox.Box {
	t.Helper()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	box, err := cryptobox.New(kek)
	if err != nil {
		t.Fatalf("cryptobox.New: %v", err)
	}
	return box
}

// createOne creates a key through the real handler so key_enc is written the way
// production writes it, and returns the id plus the plaintext.
func createOne(t *testing.T, svc *Service, uid int64, name string) (int64, string) {
	t.Helper()
	modelID := newTestModel(t)
	rec := call(t, svc.Create, http.MethodPost, "/api/console/keys",
		createBody(name, []int64{modelID}, 1), uid, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp createResponse
	decodeBody(t, rec, &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("created %d keys, want 1", len(resp.Items))
	}
	return resp.Items[0].ID, resp.Items[0].Key
}

func TestRevealReturnsTheSamePlaintext(t *testing.T) {
	svc, _ := newTestService(t)
	svc.Box = newBox(t)
	uid := newTestUser(t, nil)
	id, plain := createOne(t, svc, uid, "reveal-"+futureExpiry())

	rec := call(t, svc.Reveal, http.MethodPost, "/api/console/keys/1/reveal", nil, uid, strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Available bool   `json:"available"`
		Key       string `json:"key"`
	}
	decodeBody(t, rec, &resp)
	if !resp.Available || resp.Key != plain {
		t.Fatalf("reveal = %+v, want the create-time plaintext", resp)
	}

	// The copy must be auditable: handing out a credential is exactly what an
	// owner should be able to find later.
	var audits int64
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM t_api_key_audit WHERE api_key_id = ? AND action = 'VIEW_SECRET'`,
		id).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("VIEW_SECRET audit rows = %d, want 1", audits)
	}
}

func TestRevealIsScopedToTheOwner(t *testing.T) {
	svc, _ := newTestService(t)
	svc.Box = newBox(t)
	owner := newTestUser(t, nil)
	id, _ := createOne(t, svc, owner, "reveal-owner-"+futureExpiry())
	other := newTestUser(t, nil)

	rec := call(t, svc.Reveal, http.MethodPost, "/api/console/keys/1/reveal", nil, other, strconv.FormatInt(id, 10))
	// 404, not 403: a stranger must not learn that this id exists.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

// A key created before key_enc existed cannot be restored — the hash is
// one-way. The endpoint says so instead of inventing a code.
func TestRevealReportsMissingSecretWithoutFailing(t *testing.T) {
	svc, _ := newTestService(t)
	svc.Box = newBox(t)
	uid := newTestUser(t, nil)
	id, _ := createOne(t, svc, uid, "reveal-legacy-"+futureExpiry())

	if _, err := testDB.ExecContext(context.Background(),
		`UPDATE t_api_key SET key_enc = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	rec := call(t, svc.Reveal, http.MethodPost, "/api/console/keys/1/reveal", nil, uid, strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (availability is an answer, not an error)", rec.Code)
	}
	var resp struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	decodeBody(t, rec, &resp)
	if resp.Available || resp.Reason != "not_stored" {
		t.Fatalf("resp = %+v, want available=false reason=not_stored", resp)
	}
}

func TestRevealWithoutKEKExplainsWhy(t *testing.T) {
	svc, _ := newTestService(t)
	svc.Box = newBox(t)
	uid := newTestUser(t, nil)
	id, _ := createOne(t, svc, uid, "reveal-nokek-"+futureExpiry())

	// Simulate a deployment whose KEK broke after the key was created.
	svc.Box = nil
	rec := call(t, svc.Reveal, http.MethodPost, "/api/console/keys/1/reveal", nil, uid, strconv.FormatInt(id, 10))
	var resp struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	decodeBody(t, rec, &resp)
	if resp.Available || resp.Reason != "kek_unavailable" {
		t.Fatalf("resp = %+v, want available=false reason=kek_unavailable", resp)
	}
}

// Deleting a key must take the secret with it: the envelope is the plaintext.
func TestDeletedKeyCannotBeRevealed(t *testing.T) {
	svc, _ := newTestService(t)
	svc.Box = newBox(t)
	uid := newTestUser(t, nil)
	id, _ := createOne(t, svc, uid, "reveal-deleted-"+futureExpiry())

	rec := call(t, svc.Delete, http.MethodDelete, "/api/console/keys/1",
		map[string]any{"confirm": true}, uid, strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
	}

	var enc []byte
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT key_enc FROM t_api_key WHERE id = ?`, id).Scan(&enc); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	rec = call(t, svc.Reveal, http.MethodPost, "/api/console/keys/1/reveal", nil, uid, strconv.FormatInt(id, 10))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reveal after delete = %d, want 404", rec.Code)
	}
}

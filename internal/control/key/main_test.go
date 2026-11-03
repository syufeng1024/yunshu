package key

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/control/ctltest"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

var testDB *sql.DB

func TestMain(m *testing.M) {
	db, err := ctltest.SetupDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "key tests: %v\n", err)
		os.Exit(1)
	}
	testDB = db
	code := redistest.Setup(m)
	_ = db.Close()
	os.Exit(code)
}

func newTestService(t *testing.T) (*Service, *redisx.Client) {
	t.Helper()
	rdb := redistest.NewClient(10)
	t.Cleanup(func() { _ = rdb.Close() })
	return New(testDB, rdb, clock.Real{}, nil), rdb
}

var userSeq uint64

// newTestUser inserts one account. planID nil means the free tier (NULL
// plan_id), which is what the 3-key limit applies to.
func newTestUser(t *testing.T, planID *int64) int64 {
	t.Helper()
	ctx := context.Background()
	userSeq++
	var uid int64
	var lastErr error
	for attempt := 0; attempt < 5 && uid == 0; attempt++ {
		var noise [4]byte
		_, _ = rand.Read(noise[:])
		userNo := fmt.Sprintf("U%s%03d", time.Now().Format("20060102"),
			(int(binary.BigEndian.Uint32(noise[:]))+int(userSeq))%1000)
		email := fmt.Sprintf("t12-key-%d-%d-%d@test.local", time.Now().UnixNano(), userSeq, attempt)
		res, err := testDB.ExecContext(ctx, `
			INSERT INTO t_user
				(user_no, email, password_hash, password_algo, password_updated_at,
				 nickname, role, status, plan_id, created_at, updated_at)
			VALUES (?,?,'not-a-real-hash','bcrypt',?,'t12','PERSONAL','NORMAL',?,?,?)`,
			userNo, email, time.Now(), planID, time.Now(), time.Now())
		if err != nil {
			lastErr = err
			if !strings.Contains(err.Error(), "uk_user_no") {
				t.Fatalf("insert user: %v", err)
			}
			continue
		}
		uid, err = res.LastInsertId()
		if err != nil {
			t.Fatalf("user id: %v", err)
		}
	}
	if uid == 0 {
		t.Fatalf("insert user after retries: %v", lastErr)
	}
	if _, err := testDB.ExecContext(ctx,
		`INSERT INTO t_credit_account (user_id, created_at, updated_at) VALUES (?,?,?)`,
		uid, time.Now(), time.Now()); err != nil {
		t.Fatalf("insert credit account: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_api_key_audit WHERE api_key_id IN (SELECT id FROM t_api_key WHERE user_id = ?)`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_api_key_model WHERE api_key_id IN (SELECT id FROM t_api_key WHERE user_id = ?)`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_api_key_ip_rule WHERE api_key_id IN (SELECT id FROM t_api_key WHERE user_id = ?)`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_api_key WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_credit_account WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

func planIDByCode(t *testing.T, code string) int64 {
	t.Helper()
	var id int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT id FROM t_plan WHERE code = ?`, code).Scan(&id); err != nil {
		t.Fatalf("plan %s: %v", code, err)
	}
	return id
}

var modelSeq uint64

func newTestModel(t *testing.T) int64 {
	t.Helper()
	modelSeq++
	name := fmt.Sprintf("t12-key-model-%d-%d", time.Now().UnixNano(), modelSeq)
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_model
			(model_name, display_name, region, model_type, protocol_id, context_length,
			 listing_state, published_at, created_at, updated_at)
		SELECT ?, ?, 'DOMESTIC', 'TEXT', p.id, 8192, 'LISTED', NOW(3), NOW(3), NOW(3)
		  FROM t_protocol p WHERE p.code = 'OPENAI_COMPAT'`, name, name)
	if err != nil {
		t.Fatalf("insert model: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("model id: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_model WHERE id = ?`, id)
	})
	return id
}

func futureExpiry() string {
	return time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
}

func createBody(name string, modelIDs []int64, count int) map[string]any {
	body := map[string]any{
		"name":         name,
		"model_ids":    modelIDs,
		"expire_at":    futureExpiry(),
		"ip_allowlist": []string{},
	}
	if count > 0 {
		body["count"] = count
	}
	return body
}

// call invokes a handler with a synthetic session. pathID is installed as the
// {id} path value so ServeMux routing does not need to run.
func call(t *testing.T, h http.HandlerFunc, method, target string, body any, uid int64, pathID string) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = jsonx.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(payload))
	req = req.WithContext(mw.WithSession(req.Context(), &mw.Session{UID: uid, Role: "PERSONAL"}))
	if pathID != "" {
		req.SetPathValue("id", pathID)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := jsonx.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response (%d %s): %v", rec.Code, rec.Body.String(), err)
	}
}

type apiErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	} `json:"error"`
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return errorBody(t, rec).Error.Code
}

// errorBody decodes the unified error envelope so a test can assert on the
// message and hint as well as the code.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) apiErrorBody {
	t.Helper()
	var body apiErrorBody
	decodeBody(t, rec, &body)
	return body
}

func mustCreate(t *testing.T, svc *Service, uid int64, name string, modelIDs []int64) createdKey {
	t.Helper()
	rec := call(t, svc.Create, http.MethodPost, "/api/console/keys",
		createBody(name, modelIDs, 0), uid, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp createResponse
	decodeBody(t, rec, &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("create items = %d, want 1", len(resp.Items))
	}
	return resp.Items[0]
}

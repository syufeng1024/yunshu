package ops

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/auth"
	"yunshu.dev/core/internal/control/ctltest"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/cryptobox"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx/redistest"
)

var testDB *sql.DB

func TestMain(m *testing.M) {
	db, err := ctltest.SetupDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ops tests: %v\n", err)
		os.Exit(1)
	}
	testDB = db
	code := m.Run()
	_ = db.Close()
	os.Exit(code)
}

// newLoginService builds the auth service the way controld does. The login
// guard reads K28/K29 through the session manager's Redis client, so a bare
// &auth.Service{DB: ...} would fail closed (auth_unavailable) instead of
// reaching the account-status branch this package's tests assert on.
func newLoginService(t *testing.T) *auth.Service {
	t.Helper()
	env, err := redistest.Open()
	if err != nil {
		t.Fatalf("redis for login guard: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })
	rdb := redistest.NewClientAt(env.Addr, env.DB, 2)
	t.Cleanup(func() { _ = rdb.Close() })
	sessMgr, err := mw.NewSessionManager(testDB, rdb, "0123456789abcdef")
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	return &auth.Service{DB: testDB, Sessions: sessMgr}
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	box, err := cryptobox.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("cryptobox: %v", err)
	}
	return New(testDB, &audit.Writer{DB: testDB, Now: clock.Real{}}, clock.Real{}, nil, nil, box)
}

var userSeq uint64

// newTestUser inserts an operator account with the given role.
func newTestUser(t *testing.T, role string) int64 {
	t.Helper()
	ctx := context.Background()
	userSeq++
	var uid int64
	for attempt := 0; attempt < 5 && uid == 0; attempt++ {
		var noise [4]byte
		_, _ = crand.Read(noise[:])
		userNo := fmt.Sprintf("O%s%03d", time.Now().Format("20060102"),
			(int(binary.BigEndian.Uint32(noise[:]))+int(userSeq)+attempt)%1000)
		email := fmt.Sprintf("t15-ops-%d-%d-%d@test.local", time.Now().UnixNano(), userSeq, attempt)
		res, err := testDB.ExecContext(ctx, `
			INSERT INTO t_user
				(user_no, email, password_hash, password_algo, password_updated_at,
				 nickname, role, status, created_at, updated_at)
			VALUES (?,?,'not-a-real-hash','bcrypt',?,'t15',?,'NORMAL',?,?)`,
			userNo, email, time.Now(), role, time.Now(), time.Now())
		if err != nil {
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
		t.Fatal("insert user after retries")
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_admin_audit_log WHERE operator_id = ?`, uid)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_credit_account WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_risk_hit_log WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

func protocolID(t *testing.T, code string) int64 {
	t.Helper()
	var id int64
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT id FROM t_protocol WHERE code = ?`, code).Scan(&id); err != nil {
		t.Fatalf("protocol %s: %v", code, err)
	}
	return id
}

type testCallOptions struct {
	uid     int64
	role    string
	pathVal map[string]string
}

func call(t *testing.T, h http.HandlerFunc, method, target string, body any, opts testCallOptions) *httptest.ResponseRecorder {
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
	req = req.WithContext(mw.WithSession(req.Context(), &mw.Session{UID: opts.uid, Role: opts.role}))
	for k, v := range opts.pathVal {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

type apiErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	} `json:"error"`
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) apiErrorBody {
	t.Helper()
	var body apiErrorBody
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body (%d %s): %v", rec.Code, rec.Body.String(), err)
	}
	return body
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := jsonx.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response (%d %s): %v", rec.Code, rec.Body.String(), err)
	}
}

func uniqueSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// createTestChannel creates one channel through the handler and registers
// cleanup.
func createTestChannel(t *testing.T, svc *Service, uid int64, body map[string]any) channelView {
	t.Helper()
	rec := call(t, svc.CreateChannel, http.MethodPost, "/api/admin/channels", body,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create channel status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp channelDetailResponse
	decodeBody(t, rec, &resp)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_channel_dedicated_user WHERE channel_id = ?`, resp.Channel.ID)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_route WHERE channel_id = ?`, resp.Channel.ID)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_channel WHERE id = ?`, resp.Channel.ID)
	})
	return resp.Channel
}

func createTestModel(t *testing.T, svc *Service, uid int64, body map[string]any) modelView {
	t.Helper()
	rec := call(t, svc.CreateModel, http.MethodPost, "/api/admin/models", body,
		testCallOptions{uid: uid, role: "OPS"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create model status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp modelDetailResponse
	decodeBody(t, rec, &resp)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_announcement WHERE content LIKE ?`,
			"%"+resp.Model.ModelName+"%")
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_price_rule WHERE scope_level = 'MODEL' AND scope_id = ?`,
			resp.Model.ID)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_model_alias WHERE model_id = ?`, resp.Model.ID)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_model_capability WHERE model_id = ?`, resp.Model.ID)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_model WHERE id = ?`, resp.Model.ID)
	})
	return resp.Model
}

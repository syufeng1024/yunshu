package auth

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/ctltest"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

const (
	testPassword    = "CorrectHorse1"
	testNewPassword = "NewCorrectHorse2"
)

var testDB *sql.DB

func TestMain(m *testing.M) {
	db, err := ctltest.SetupDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth tests: %v\n", err)
		os.Exit(1)
	}
	testDB = db
	code := redistest.Setup(m)
	_ = db.Close()
	os.Exit(code)
}

func newAuthService(t *testing.T) (*Service, *mw.SessionManager, *redisx.Client) {
	t.Helper()
	// 2s command budget, not redisx.DefaultCommandTimeout (50ms). The 50ms is a
	// production latency policy; as a fixture budget it fails this package under
	// `go test ./internal/control/...`, where a dozen binaries share one Redis:
	// Issue answers "write K20/K30: context deadline exceeded" and Load answers
	// auth_unavailable (503) for a session/cancel assertion that has nothing to
	// do with latency.
	rdb := redisx.New(redisx.Options{
		Addr:           redistest.Addr(),
		DB:             redistest.DB(),
		PoolSize:       10,
		CommandTimeout: 2 * time.Second,
		LuaTimeout:     2 * time.Second,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	sessMgr, err := mw.NewSessionManager(testDB, rdb, "0123456789abcdef")
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	svc := &Service{
		DB:       testDB,
		Sessions: sessMgr,
		Audit:    &audit.Writer{DB: testDB, Now: clock.Real{}},
		Now:      clock.Real{},
	}
	return svc, sessMgr, rdb
}

var authUserSeq int64
var authIPSeq int64

func uniqueEmail() string {
	n := atomic.AddInt64(&authUserSeq, 1)
	return fmt.Sprintf("t11-auth-%d-%d@test.local", time.Now().UnixNano(), n)
}

func uniqueIP() string {
	n := atomic.AddInt64(&authIPSeq, 1)
	return fmt.Sprintf("198.51.100.%d", n%250+1)
}

// newAuthUser inserts a NORMAL PERSONAL account with a real bcrypt hash.
func newAuthUser(t *testing.T, email string) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcryptCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UTC()
	// Test user_no must be unique across processes AND across runs. A
	// per-process counter restarts at 1 on every run, so the Nth account of
	// the day always got the same `P<date><seq>` — a row left behind by a
	// failed earlier run then collided on uk_user_no and failed this package
	// for the rest of the day. Deriving it from the nanosecond clock keeps it
	// 12 chars (char(12) column) and unique without a retry loop.
	userNo := fmt.Sprintf("P%011d", now.UnixNano()%100000000000)
	res, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_user
			(user_no, email, password_hash, password_algo, password_updated_at,
			 nickname, role, status, created_at, updated_at)
		VALUES (?,?,?,'bcrypt',?,'t11-auth','PERSONAL','NORMAL',?,?)`,
		userNo, email, string(hash), now, now, now)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_admin_audit_log WHERE operator_id = ? OR target_id = ?`,
			uid, fmt.Sprint(uid))
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_credit_account WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_user WHERE id = ?`, uid)
	})
	return uid
}

func doLogin(t *testing.T, svc *Service, identifier, password, ip string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(map[string]string{
		"identifier": identifier,
		"password":   password,
	})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(payload)))
	req.RemoteAddr = ip + ":12345"
	rec := httptest.NewRecorder()
	svc.Login(rec, req)
	return rec
}

func changePasswordRequest(t *testing.T, svc *Service, sess *mw.Session, oldPassword, newPassword string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := jsonx.Marshal(map[string]string{
		"old_password": oldPassword,
		"new_password": newPassword,
	})
	if err != nil {
		t.Fatalf("marshal password body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password", strings.NewReader(string(payload)))
	req = req.WithContext(mw.WithSession(req.Context(), sess))
	rec := httptest.NewRecorder()
	svc.ChangePassword(rec, req)
	return rec
}

type errorBody struct {
	Error struct {
		Type              string `json:"type"`
		Code              string `json:"code"`
		Message           string `json:"message"`
		Hint              string `json:"hint"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	} `json:"error"`
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body (%d %s): %v", rec.Code, rec.Body.String(), err)
	}
	return body
}

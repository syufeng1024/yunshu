package mw

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
)

// Session constants. K20 stores only the uid: permissions are derived from
// t_user.role on every request so a role change takes effect immediately
// (T11 hard constraint 7).
const (
	SessionCookieName = "sid"
	SessionTTL        = 2 * time.Hour
	// SessionAbsoluteTTL caps sliding renewal. Without it a daily user keeps
	// one sid alive forever, which would survive a password reset.
	SessionAbsoluteTTL = 8 * time.Hour
	sessionIDBytes     = 32
)

// Session is the authenticated principal loaded for one request.
type Session struct {
	SID      string
	UID      int64
	Role     string
	Status   string
	Email    string
	Phone    string
	Username string
	Nickname string
	// PasswordIsInitial mirrors t_user.password_is_initial: the account still
	// uses the password written at provisioning time. It only drives a UI
	// reminder; nothing in the authorization path reads it.
	PasswordIsInitial bool
	IssuedAt          time.Time
	ExpiresAt         time.Time
}

// DisplayName returns the name the console shows.
func (s *Session) DisplayName() string {
	if s.Nickname != "" {
		return s.Nickname
	}
	if s.Email != "" {
		return s.Email
	}
	return s.Phone
}

type sessionKey struct{}

// WithSession stores the principal in the request context.
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// SessionFrom reads the principal from the request context.
func SessionFrom(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(*Session)
	return s, ok
}

// SessionManager issues, validates and revokes sid sessions.
type SessionManager struct {
	DB     *sql.DB
	Redis  *redisx.Client
	Secret []byte

	TTL         time.Duration
	AbsoluteTTL time.Duration
	Now         clock.Clock
	Secure      bool
	Path        string
}

// NewSessionManager validates the signing secret. A short secret is rejected
// at construction: a guessable HMAC key would let anyone forge an iat and
// extend a stolen sid past the absolute cap.
func NewSessionManager(db *sql.DB, rdb *redisx.Client, secret string) (*SessionManager, error) {
	secret = strings.TrimSpace(secret)
	if len(secret) < 16 {
		return nil, fmt.Errorf("control: SESSION_SECRET must be at least 16 bytes")
	}
	if db == nil || rdb == nil {
		return nil, fmt.Errorf("control: session manager requires mysql and redis")
	}
	return &SessionManager{
		DB:          db,
		Redis:       rdb,
		Secret:      []byte(secret),
		TTL:         SessionTTL,
		AbsoluteTTL: SessionAbsoluteTTL,
		Now:         clock.Real{},
		Secure:      true,
		Path:        "/",
	}, nil
}

func (m *SessionManager) now() time.Time {
	if m.Now != nil {
		return m.Now.Now().UTC()
	}
	return clock.Real{}.Now()
}

func (m *SessionManager) ttl() time.Duration {
	if m.TTL > 0 {
		return m.TTL
	}
	return SessionTTL
}

func (m *SessionManager) absoluteTTL() time.Duration {
	if m.AbsoluteTTL > 0 {
		return m.AbsoluteTTL
	}
	return SessionAbsoluteTTL
}

// Issue creates a sid, stores uid in K20 and returns the signed cookie value.
// The plaintext cookie is `sid.iat.signature`; K20's key is still
// sess:admin:{sid} and its value is the uid only.
func (m *SessionManager) Issue(ctx context.Context, uid int64) (*Session, string, error) {
	if uid <= 0 {
		return nil, "", fmt.Errorf("control: session uid must be positive")
	}
	var raw [sessionIDBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, "", fmt.Errorf("control: generate session id: %w", err)
	}
	sid := hex.EncodeToString(raw[:])
	issued := m.now()

	cctx, cancel := m.Redis.CommandContext(ctx)
	defer cancel()
	// K20 and the K30 uid index are written together: a session that exists in
	// K20 but not in K30 would survive a password change, which is the exact
	// bug this index exists to prevent.
	if _, err := m.Redis.Raw().TxPipelined(cctx, func(pipe goredis.Pipeliner) error {
		pipe.Set(cctx, redisx.AdminSessionKey(sid), strconv.FormatInt(uid, 10), m.ttl())
		pipe.SAdd(cctx, redisx.AdminUserSessionSetKey(uid), sid)
		pipe.Expire(cctx, redisx.AdminUserSessionSetKey(uid), m.ttl())
		return nil
	}); err != nil {
		return nil, "", fmt.Errorf("control: write K20/K30: %w", err)
	}
	return &Session{
		SID:       sid,
		UID:       uid,
		IssuedAt:  issued,
		ExpiresAt: issued.Add(m.ttl()),
	}, m.cookieValue(sid, issued), nil
}

// SetCookie writes the sid cookie: HttpOnly, Secure and SameSite=Lax. The
// cookie's Max-Age follows the absolute cap, not the sliding TTL; the server
// still rejects an sid older than the cap even if the browser sends it.
func (m *SessionManager) SetCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     m.path(),
		HttpOnly: true,
		Secure:   m.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(m.absoluteTTL().Seconds()),
	})
}

// ClearCookie expires the sid cookie.
func (m *SessionManager) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     m.path(),
		HttpOnly: true,
		Secure:   m.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (m *SessionManager) path() string {
	if m.Path != "" {
		return m.Path
	}
	return "/"
}

// Load validates the cookie signature, the absolute age and K20, then reads the
// current role/status from MySQL. Role and status are never cached in Redis.
func (m *SessionManager) Load(ctx context.Context, r *http.Request) (*Session, *apierr.APIError) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return nil, SessionMissing()
	}
	sid, issued, ok := m.parseCookie(cookie.Value)
	if !ok {
		return nil, SessionMissing()
	}
	now := m.now()
	if issued.After(now.Add(time.Minute)) {
		// A clock more than a minute ahead is a forged or badly skewed cookie.
		return nil, SessionMissing()
	}
	if now.Sub(issued) > m.absoluteTTL() {
		m.revoke(ctx, sid)
		return nil, SessionMissing()
	}

	cctx, cancel := m.Redis.CommandContext(ctx)
	uidRaw, err := m.Redis.Raw().Get(cctx, redisx.AdminSessionKey(sid)).Result()
	cancel()
	if errors.Is(err, goredis.Nil) {
		return nil, SessionMissing()
	}
	if err != nil {
		return nil, apierr.New(apierr.CodeAuthUnavailable).WithCause(err)
	}
	uid, err := strconv.ParseInt(strings.TrimSpace(uidRaw), 10, 64)
	if err != nil || uid <= 0 {
		m.revoke(ctx, sid)
		return nil, SessionMissing()
	}

	sess := &Session{SID: sid, UID: uid, IssuedAt: issued, ExpiresAt: now.Add(m.ttl())}
	var role, status, email, phone, username, nickname sql.NullString
	err = m.DB.QueryRowContext(ctx, `
		SELECT role, status, email, phone, username, nickname, password_is_initial
		  FROM t_user
		 WHERE id = ? AND deleted_at IS NULL`, uid,
	).Scan(&role, &status, &email, &phone, &username, &nickname, &sess.PasswordIsInitial)
	if errors.Is(err, sql.ErrNoRows) {
		m.revoke(ctx, sid)
		return nil, SessionMissing()
	}
	if err != nil {
		return nil, apierr.New(apierr.CodeAuthUnavailable).WithCause(err)
	}
	sess.Role = role.String
	sess.Status = status.String
	sess.Email = email.String
	sess.Phone = phone.String
	sess.Username = username.String
	sess.Nickname = nickname.String

	switch sess.Status {
	case "NORMAL":
	case "DISABLED":
		return nil, apierr.New(apierr.CodeAccountDisabled)
	case "FROZEN_OVERDRAFT":
		return nil, apierr.New(apierr.CodeAccountFrozenDueToOverdraft)
	default:
		// An unknown status is not a licence to continue.
		return nil, apierr.New(apierr.CodeAccountDisabled)
	}

	// Sliding renewal, bounded by IssueAt above. SAdd re-indexes sessions
	// issued before K30 existed (deploy window) and heals a missing set after a
	// Redis flush; both TTLs move together so K30 can never outlive K20 or
	// expire while a session is still active.
	ecctx, ecancel := m.Redis.CommandContext(ctx)
	pipe := m.Redis.Raw().Pipeline()
	pipe.SAdd(ecctx, redisx.AdminUserSessionSetKey(uid), sid)
	pipe.Expire(ecctx, redisx.AdminSessionKey(sid), m.ttl())
	pipe.Expire(ecctx, redisx.AdminUserSessionSetKey(uid), m.ttl())
	_, _ = pipe.Exec(ecctx)
	ecancel()
	return sess, nil
}

// Middleware loads the session and aborts with 401 on failure.
func (m *SessionManager) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, apiErr := m.Load(r.Context(), r)
			if apiErr != nil {
				WriteError(w, r, apiErr)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), sess)))
		})
	}
}

// Revoke deletes K20 so the old sid stops working immediately.
//
// It returns an error when the DEL was not acknowledged. The caller must not
// report a successful logout in that case: with a 204 the client believes the
// session is gone while the server-side key is still alive, which is exactly
// the "logged out everywhere, except actually" failure this signature exists to
// prevent (G34).
func (m *SessionManager) Revoke(ctx context.Context, sid string) error {
	return m.revoke(ctx, sid)
}

func (m *SessionManager) revoke(ctx context.Context, sid string) error {
	if sid == "" {
		return nil
	}
	// The uid read and the DEL get **separate** command budgets. They used to
	// share one 50ms context: a slow GET consumed it, the DEL was never sent,
	// pipe.Exec's error was discarded and Logout still answered 204 — the
	// session survived a logout that reported success (G34).
	//
	// Read the uid before deleting K20 so the K30 member can be removed in the
	// same round trip. This read is best-effort: a logout that leaves the sid
	// in K30 is harmless (RevokeAll is idempotent) but accumulates dead
	// members, so failing it must not block the delete that actually matters.
	readCtx, readCancel := m.Redis.CommandContext(ctx)
	uidRaw, _ := m.Redis.Raw().Get(readCtx, redisx.AdminSessionKey(sid)).Result()
	readCancel()

	pipe := m.Redis.Raw().Pipeline()
	delCtx, delCancel := m.Redis.CommandContext(ctx)
	defer delCancel()
	if uid, err := strconv.ParseInt(strings.TrimSpace(uidRaw), 10, 64); err == nil && uid > 0 {
		pipe.SRem(delCtx, redisx.AdminUserSessionSetKey(uid), sid)
	}
	pipe.Del(delCtx, redisx.AdminSessionKey(sid))
	if _, err := pipe.Exec(delCtx); err != nil {
		// No sid in the error: it is a bearer credential.
		return fmt.Errorf("mw: revoke session: %w", err)
	}
	return nil
}

// revokeAllScript deletes every K20 named by the K30 member set and then the
// set itself in one atomic step. A WATCH/retry loop would abort on every
// concurrent Load (which refreshes K30's TTL) and could fail under normal
// traffic; the script has no abort window. KEYS[1] is K30, ARGV[1] is the K20
// prefix ("sess:admin:").
var revokeAllScript = goredis.NewScript(`
local sids = redis.call('SMEMBERS', KEYS[1])
for i = 1, #sids do
  redis.call('DEL', ARGV[1] .. sids[i])
end
redis.call('DEL', KEYS[1])
return #sids
`)

// RevokeAll deletes every session owned by uid. It is the password-change
// path: K30 is the only index from uid to sid, and the alternative (scan the
// keyspace for sess:admin:*) is not acceptable at production size.
func (m *SessionManager) RevokeAll(ctx context.Context, uid int64) error {
	if uid <= 0 {
		return nil
	}
	cctx, cancel := m.Redis.LuaContext(ctx)
	defer cancel()
	if err := revokeAllScript.Run(cctx, m.Redis.Raw(),
		[]string{redisx.AdminUserSessionSetKey(uid)},
		redisx.AdminSessionKey(""),
	).Err(); err != nil {
		return fmt.Errorf("control: revoke all sessions: %w", err)
	}
	return nil
}

func (m *SessionManager) cookieValue(sid string, issued time.Time) string {
	iat := strconv.FormatInt(issued.Unix(), 10)
	return sid + "." + iat + "." + m.sign(sid, iat)
}

func (m *SessionManager) sign(sid, iat string) string {
	mac := hmac.New(sha256.New, m.Secret)
	mac.Write([]byte(sid))
	mac.Write([]byte("|"))
	mac.Write([]byte(iat))
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *SessionManager) parseCookie(value string) (string, time.Time, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return "", time.Time{}, false
	}
	sid, iat, sig := parts[0], parts[1], parts[2]
	if len(sid) != sessionIDBytes*2 || !isHex(sid) {
		return "", time.Time{}, false
	}
	sec, err := strconv.ParseInt(iat, 10, 64)
	if err != nil || sec <= 0 {
		return "", time.Time{}, false
	}
	want := m.sign(sid, iat)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return "", time.Time{}, false
	}
	return sid, time.Unix(sec, 0).UTC(), true
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

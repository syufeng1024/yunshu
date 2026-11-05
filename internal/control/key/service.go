// Package key implements the console's API-key lifecycle: list, create
// (including batch), detail, update, enable/disable, rotate, delete and the
// append-only audit read.
//
// Two boundaries are architectural, not cosmetic:
//
//   - The plaintext key is returned exactly twice: in the create/rotate response,
//     and on an explicit POST .../reveal (2026-11-05, owner-initiated copy).
//     t_api_key stores SHA-256, the first eight characters, the last four, and
//     an envelope-encrypted copy in key_enc for that reveal path only. Every
//     other read path returns a mask assembled from the hash columns.
//   - group_code is a price dimension owned by operations. No console write
//     request may carry it; the field is detected as raw JSON so even
//     "group_code": null is rejected instead of silently ignored.
package key

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/cryptobox"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
)

// Service owns the console key surface.
type Service struct {
	DB      *sql.DB
	Redis   *redisx.Client
	Now     clock.Clock
	Logger  *obslog.Logger
	KeyAuth redisx.KeyAuthWriter
	RT      *Realtime

	// PublishRoutes republishes cfg:snap:ROUTING. P06 reads IP whitelists from
	// that snapshot, so a rule change must be followed by a publish before the
	// new K13 ipm is written; otherwise the two versions disagree (deny) or the
	// snapshot has no entry (allow all). Nil in unit tests.
	PublishRoutes func(ctx context.Context) error

	// Box seals the key plaintext into t_api_key.key_enc so the owner can copy
	// the key again later (2026-11-05: 「密钥不能完整显示，但必须可复制」).
	//
	// It reuses CHANNEL_KEY_KEK rather than adding another secret: one KEK to
	// rotate, one place to lose. Nil (KEK missing or malformed) degrades to
	// "keys created while it was missing cannot be copied" — creation itself
	// still succeeds, because a working key that cannot be re-read is better
	// than no key at all, and the gateway never reads this column.
	Box *cryptobox.Box
}

// sealSecret returns the envelope to store in key_enc, or nil when the secret
// cannot be sealed. Never fails the caller: an unsealed key still authenticates
// (auth reads key_hash), it just cannot be copied again.
func (s *Service) sealSecret(ctx context.Context, plain string) any {
	if s.Box == nil {
		return nil
	}
	envelope, err := s.Box.SealString(plain)
	if err != nil {
		if s.Logger != nil {
			s.logError(ctx, "key_seal_failed: "+err.Error())
		}
		return nil
	}
	return []byte(envelope)
}

// publishIPRules refreshes the routing snapshot when a callback is wired.
func (s *Service) publishIPRules(ctx context.Context) error {
	if s.PublishRoutes == nil {
		return nil
	}
	return s.PublishRoutes(ctx)
}

// New wires the service. A nil clock or logger falls back to the real
// implementations; Redis is mandatory because every state change must
// write through K13.
func New(db *sql.DB, rdb *redisx.Client, now clock.Clock, logger *obslog.Logger) *Service {
	if now == nil {
		now = clock.Real{}
	}
	s := &Service{DB: db, Redis: rdb, Now: now, Logger: logger}
	if rdb != nil {
		s.KeyAuth = redisx.NewKeyAuthCache(rdb)
		s.RT = &Realtime{Redis: rdb, Now: now, TTL: time.Second}
	}
	return s
}

func (s *Service) now() time.Time { return s.Now.Now().UTC() }

// cst is the quota calendar zone (UTC+8, TDS §1.4 t_user_daily_quota). A fixed
// zone is intentional: China has no DST, so there is no transition to model.
var cst = time.FixedZone("CST", 8*3600)

func startOfDayCST(t time.Time) time.Time {
	local := t.In(cst)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, cst)
}

func (s *Service) logError(ctx context.Context, msg string) {
	if s.Logger != nil {
		s.Logger.Error(ctx, msg, obslog.Fields{Stage: "control", Status: http.StatusInternalServerError})
	}
}

func (s *Service) logWarn(ctx context.Context, msg string) {
	if s.Logger != nil {
		s.Logger.Warn(ctx, msg, obslog.Fields{Stage: "control"})
	}
}

// ---------------------------------------------------------------------------
// shared row shape
// ---------------------------------------------------------------------------

type keyRecord struct {
	ID              int64
	UserID          int64
	Name            string
	Prefix          string
	Hash            string
	Last4           string
	Status          string
	ModelScopeMode  string
	IPWhitelistMode string
	GroupCode       string
	ExpireAt        sql.NullTime
	LastUsedAt      sql.NullTime
	CreatedAt       time.Time
	UpdatedAt       time.Time
	GroupRatioBP    sql.NullInt64
}

const keySelectColumns = `
	k.id, k.user_id, k.key_name, k.key_prefix, k.key_hash, k.key_last4,
	k.status, k.model_scope_mode, k.ip_whitelist_mode, k.group_code,
	k.expire_at, k.last_used_at, k.created_at, k.updated_at, g.ratio_bp`

type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// loadOwnedKey is the only read path for a single key. It filters by user_id
// in SQL: an id belonging to another account returns key_not_found, which is
// also what a nonexistent id returns. The two cases are indistinguishable on
// purpose (T12 hard constraint 11).
func (s *Service) loadOwnedKey(ctx context.Context, q rowQueryer, uid, id int64, forUpdate bool) (keyRecord, *apierr.APIError) {
	query := `SELECT ` + keySelectColumns + `
		FROM t_api_key k
		LEFT JOIN t_price_group g ON g.code = k.group_code
		WHERE k.id = ? AND k.user_id = ? AND k.deleted_at IS NULL`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var rec keyRecord
	err := q.QueryRowContext(ctx, query, id, uid).Scan(
		&rec.ID, &rec.UserID, &rec.Name, &rec.Prefix, &rec.Hash, &rec.Last4,
		&rec.Status, &rec.ModelScopeMode, &rec.IPWhitelistMode, &rec.GroupCode,
		&rec.ExpireAt, &rec.LastUsedAt, &rec.CreatedAt, &rec.UpdatedAt, &rec.GroupRatioBP,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return keyRecord{}, apierr.New(apierr.CodeKeyNotFound)
	}
	if err != nil {
		s.logError(ctx, "key_lookup_failed: "+err.Error())
		return keyRecord{}, apierr.New(apierr.CodeInternalError)
	}
	return rec, nil
}

// pathKeyID reads {id}. A non-numeric id is reported as key_not_found rather
// than as a validation error so the response cannot distinguish "bad id" from
// "someone else's id".
func pathKeyID(r *http.Request) (int64, *apierr.APIError) {
	raw := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, apierr.New(apierr.CodeKeyNotFound)
	}
	return id, nil
}

// mask renders the only form of a key a read endpoint may return.
func mask(prefix, last4 string) string { return prefix + "****" + last4 }

// visibleGroup hides the internal group from every user-facing response
// (PRD §5.2/§5.3). The zero value plus a nil ratio is the wire shape.
func visibleGroup(code string, ratio sql.NullInt64) (string, *int64) {
	if code == "" || code == "internal" {
		return "", nil
	}
	if !ratio.Valid {
		return code, nil
	}
	r := ratio.Int64
	return code, &r
}

func nullableTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

// ---------------------------------------------------------------------------
// free-tier limit
// ---------------------------------------------------------------------------

// freeMaxActiveKeys reads quota.free_max_active_keys from t_config_item. The
// built-in default of 3 is used when the row is missing or unparsable: a
// missing configuration must not silently remove the limit.
func (s *Service) freeMaxActiveKeys(ctx context.Context) int64 {
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `
		SELECT cfg_value FROM t_config_item
		 WHERE cfg_key = ? AND enabled = 1 LIMIT 1`,
		string(config.KeyQuotaFreeMaxActiveKey)).Scan(&raw)
	if err == nil {
		if v, perr := config.Parse(config.KeyQuotaFreeMaxActiveKey, raw); perr == nil {
			if n, ok := v.Int(); ok && n > 0 {
				return n
			}
		}
	}
	return 3
}

// isFreeTier reports whether the account is on FREE_DEFAULT. A NULL plan_id
// means "never bought a plan" (TDS §1.3), which is the free tier.
func isFreeTier(ctx context.Context, q rowQueryer, uid int64) (bool, error) {
	var code sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT p.code
		  FROM t_user u
		  LEFT JOIN t_plan p ON p.id = u.plan_id
		 WHERE u.id = ? AND u.deleted_at IS NULL`, uid).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !code.Valid || code.String == "FREE_DEFAULT", nil
}

func (s *Service) activeKeyCount(ctx context.Context, q rowQueryer, uid int64, now time.Time) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM t_api_key
		 WHERE user_id = ? AND status = 'ENABLED' AND deleted_at IS NULL
		   AND (expire_at IS NULL OR expire_at > ?)`, uid, now).Scan(&n)
	return n, err
}

// checkFreeLimit runs inside the creation transaction, after the user row is
// locked FOR UPDATE. Locking is what makes the check correct under concurrency:
// without it two requests both read 2 and both insert.
//
// A breach returns key_limit_reached (409, conflict) rather than a request-body
// error; callers reach this from create (count keys at once) and from rotate
// (one key, when the old one is not active).
func (s *Service) checkFreeLimit(ctx context.Context, tx *sql.Tx, uid int64, adding int64, now time.Time) *apierr.APIError {
	free, err := isFreeTier(ctx, tx, uid)
	if err != nil {
		s.logError(ctx, "key_plan_lookup_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	if !free {
		return nil
	}
	limit := s.freeMaxActiveKeys(ctx)
	active, err := s.activeKeyCount(ctx, tx, uid, now)
	if err != nil {
		s.logError(ctx, "key_active_count_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	if active+adding > limit {
		// A quota shortfall is a state conflict, not a malformed body: the
		// request may be perfectly valid and still be refused. Answering
		// invalid_request_body with a field path sent callers to edit `count`,
		// a change that can never lift a ceiling (T12 P0). key_limit_reached
		// (409) carries the quota copy required by T12 hard constraint 2, with
		// the ceiling interpolated from quota.free_max_active_keys.
		return apierr.Newf(apierr.CodeKeyLimitReached, limit, active, adding)
	}
	return nil
}

// ---------------------------------------------------------------------------
// IP rules
// ---------------------------------------------------------------------------

type ipRule struct {
	CIDR   string
	Family int
	Note   string
}

const maxIPRules = 50

// normalizeIPRules validates and canonicalizes a whitelist. A bare address
// becomes a host prefix; a CIDR keeps its network bits. Duplicates collapse.
func normalizeIPRules(raw []string) ([]ipRule, *apierr.APIError) {
	if len(raw) > maxIPRules {
		return nil, mw.InvalidField("ip_allowlist")
	}
	seen := make(map[string]bool, len(raw))
	out := make([]ipRule, 0, len(raw))
	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			addr, aerr := netip.ParseAddr(entry)
			if aerr != nil || addr.Zone() != "" {
				return nil, mw.InvalidField("ip_allowlist")
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefix = prefix.Masked()
		cidr := prefix.String()
		if seen[cidr] {
			continue
		}
		seen[cidr] = true
		family := 6
		if prefix.Addr().Is4() {
			family = 4
		}
		out = append(out, ipRule{CIDR: cidr, Family: family})
	}
	if len(out) > maxIPRules {
		return nil, mw.InvalidField("ip_allowlist")
	}
	return out, nil
}

func ipMode(rules []ipRule) string {
	if len(rules) == 0 {
		return "OFF"
	}
	return "ON"
}

func (s *Service) loadIPRules(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, keyID int64) ([]ipRule, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT cidr, ip_family, COALESCE(note, '')
		  FROM t_api_key_ip_rule WHERE api_key_id = ? ORDER BY id`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ipRule, 0, 4)
	for rows.Next() {
		var r ipRule
		if err := rows.Scan(&r.CIDR, &r.Family, &r.Note); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) replaceIPRules(ctx context.Context, tx *sql.Tx, keyID int64, rules []ipRule, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM t_api_key_ip_rule WHERE api_key_id = ?`, keyID); err != nil {
		return fmt.Errorf("delete ip rules: %w", err)
	}
	for _, r := range rules {
		var note any
		if strings.TrimSpace(r.Note) != "" {
			note = r.Note
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO t_api_key_ip_rule (api_key_id, cidr, ip_family, note, created_at)
			VALUES (?,?,?,?,?)`, keyID, r.CIDR, r.Family, note, now); err != nil {
			return fmt.Errorf("insert ip rule: %w", err)
		}
	}
	return nil
}

func (s *Service) maxIPRuleID(ctx context.Context, keyID int64) int64 {
	var n int64
	_ = s.DB.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM t_api_key_ip_rule WHERE api_key_id = ?`, keyID).Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// realtime per-key counters
// ---------------------------------------------------------------------------

// RealtimeSnapshot is the per-key counter view. A nil pointer means "Redis did
// not answer"; it must render as null, never as 0, because 0 would claim the
// key has used nothing (T12 hard constraint 10).
type RealtimeSnapshot struct {
	Available   bool
	RPM         *int64
	TPM         *int64
	Concurrency *int64
}

type realtimeEntry struct {
	at   time.Time
	snap RealtimeSnapshot
}

// Realtime reads K1/K2/K3 with a one-second in-process cache. The cache keeps
// a 5–10 s polling detail page from turning into a Redis amplifier.
type Realtime struct {
	Redis *redisx.Client
	Now   clock.Clock
	TTL   time.Duration

	mu    sync.Mutex
	cache map[int64]realtimeEntry
}

// Snapshot returns the current counters for one key.
func (rt *Realtime) Snapshot(ctx context.Context, keyID int64) RealtimeSnapshot {
	if rt == nil || rt.Redis == nil {
		return RealtimeSnapshot{}
	}
	now := rt.Now.Now()
	ttl := rt.TTL
	if ttl <= 0 {
		ttl = time.Second
	}

	rt.mu.Lock()
	if rt.cache == nil {
		rt.cache = make(map[int64]realtimeEntry)
	}
	if entry, ok := rt.cache[keyID]; ok && now.Sub(entry.at) < ttl {
		rt.mu.Unlock()
		return entry.snap
	}
	rt.mu.Unlock()

	snap := rt.fetch(ctx, keyID, now)

	rt.mu.Lock()
	rt.cache[keyID] = realtimeEntry{at: now, snap: snap}
	rt.mu.Unlock()
	return snap
}

func (rt *Realtime) fetch(ctx context.Context, keyID int64, now time.Time) RealtimeSnapshot {
	cctx, cancel := rt.Redis.CommandContext(ctx)
	defer cancel()
	minute := now.Unix() / 60
	pipe := rt.Redis.Raw().Pipeline()
	rpm := pipe.Get(cctx, redisx.RPMLKey(keyID, minute))
	tpm := pipe.Get(cctx, redisx.TPMKey(keyID, minute))
	conc := pipe.ZCard(cctx, redisx.ConcurrencyKey(keyID))
	_, err := pipe.Exec(cctx)
	if err != nil && !errors.Is(err, goredis.Nil) {
		return RealtimeSnapshot{}
	}
	return RealtimeSnapshot{
		Available:   true,
		RPM:         counterValue(rpm),
		TPM:         counterValue(tpm),
		Concurrency: int64Value(conc),
	}
}

// counterValue maps a missing counter to 0 (the key simply made no request in
// this minute) and a transport error to nil.
func counterValue(cmd *goredis.StringCmd) *int64 {
	n, err := cmd.Int64()
	if err == nil {
		return &n
	}
	if errors.Is(err, goredis.Nil) {
		zero := int64(0)
		return &zero
	}
	return nil
}

func int64Value(cmd *goredis.IntCmd) *int64 {
	n, err := cmd.Result()
	if err != nil {
		return nil
	}
	return &n
}

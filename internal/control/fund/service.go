// Package fund implements the console money surface: plan display, order
// creation and listing, the payment callback, plan entitlement grant and
// redeem codes.
//
// Two rules shape every file here:
//
//   - Money never touches a float. Pay amounts are integer fen; balances and
//     ledger amounts are integer micro-credits (MICROPerCredit). A "100 元"
//     price is 10000 fen and 100000000 micro-credits, never 100.0.
//   - The balance is written only by internal/ledger. This package composes
//     transactions (ledger.GrantTx inside its own tx) but never issues
//     UPDATE t_credit_account itself.
package fund

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/ledger"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
)

// MicroPerCredit is the frozen conversion: 1 积分 = 10 000 微积分.
// PRD §3.1 / TDS §1.4 fix it, so it is a constant rather than configuration.
const MicroPerCredit int64 = 10000

// FenPerYuan is 100. 1 元 = 100 分 = 100 积分 = 1 000 000 微积分, therefore
// one fen is exactly MicroPerCredit micro-credits. Both directions of the
// fen→micro conversion use this identity and nothing else.
const FenPerYuan int64 = 100

// orderExpiryMinutes is PRD §5.6 / TDS t_recharge_order.expire_at: an unpaid
// order closes 30 minutes after creation. No configuration key covers it, so
// the value lives here with the citation.
const orderExpiryMinutes = 30

// customRechargeMaxFen is B-09: a self-service top-up is capped at 50 000 元.
const customRechargeMaxFen int64 = 5000000

// Service owns the console fund surface and the public payment callback.
type Service struct {
	DB     *sql.DB
	Redis  *redisx.Client
	Ledger *ledger.Store
	Audit  *audit.Writer
	Now    clock.Clock
	Logger *obslog.Logger

	// PaySecrets maps a pay channel (WECHAT / ALIPAY) to the callback signing
	// secret. It comes from the process environment, not from t_config_item:
	// the 45 frozen configuration keys carry no payment credential, and a
	// secret in the configuration table would be readable by every process.
	// A missing entry disables the channel (503), never the verification.
	PaySecrets map[string]string

	// PublishRoutes republishes cfg:snap:ROUTING after a plan change so the
	// gateway sees the new entitlement without waiting for the timer. Nil in
	// unit tests.
	PublishRoutes func(ctx context.Context) error

	// Mailer sends the e-mail half of a notification. Nil is allowed: the
	// in-app row is still written and a WARN records the skip.
	Mailer Mailer

	// FingerprintKey is FP_HMAC_KEY, read from the process environment. The
	// 45 frozen configuration keys carry no HMAC key, and a key stored in
	// t_config_item would be readable by every process that loads config.
	// An empty key makes the trial-claim endpoint answer 503 rather than hash
	// device identifiers with an empty secret.
	//
	// The *phone* half of the anti-abuse check needs no key and must not grow
	// one: ClaimTrial compares the phone_hmac values already stored in t_user
	// (written at registration with PHONE_HMAC_KEY), it never hashes a phone
	// number itself. Adding a key dependency here would make the trial endpoint
	// start answering 503 for a reason that has nothing to do with it.
	FingerprintKey string

	// IdemWindow is idem.duplicate_window_seconds resolved lazily; zero means
	// "read the configuration".
	idemWindow time.Duration
}

// New wires the service. Redis may be nil only in unit tests of pure SQL
// paths; the order endpoint then skips Idempotency-Key deduplication.
func New(db *sql.DB, rdb *redisx.Client, ldg *ledger.Store, aud *audit.Writer, now clock.Clock, logger *obslog.Logger) *Service {
	if now == nil {
		now = clock.Real{}
	}
	if ldg == nil {
		ldg = ledger.NewStore(db, now)
	}
	if aud == nil {
		aud = &audit.Writer{DB: db, Now: now}
	}
	s := &Service{DB: db, Redis: rdb, Ledger: ldg, Audit: aud, Now: now, Logger: logger}
	s.PaySecrets = map[string]string{}
	return s
}

func (s *Service) now() time.Time { return s.Now.Now().UTC() }

func (s *Service) logError(ctx context.Context, msg string) {
	if s.Logger != nil {
		s.Logger.Error(ctx, msg, obslog.Fields{Stage: "fund", Status: http.StatusInternalServerError})
	}
}

func (s *Service) logWarn(ctx context.Context, msg string) {
	if s.Logger != nil {
		s.Logger.Warn(ctx, msg, obslog.Fields{Stage: "fund"})
	}
}

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------

// configInt reads one integer key from t_config_item. A missing or unparsable
// row yields def: configuration must never silently zero a money threshold.
func (s *Service) configInt(ctx context.Context, key config.Key, def int64) int64 {
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `
		SELECT cfg_value FROM t_config_item
		 WHERE cfg_key = ? AND enabled = 1 LIMIT 1`, string(key)).Scan(&raw)
	if err != nil {
		return def
	}
	v, perr := config.Parse(key, raw)
	if perr != nil {
		return def
	}
	if n, ok := v.Int(); ok {
		return n
	}
	return def
}

// configString reads one string key, returning def when absent.
func (s *Service) configString(ctx context.Context, key config.Key, def string) string {
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `
		SELECT cfg_value FROM t_config_item
		 WHERE cfg_key = ? AND enabled = 1 LIMIT 1`, string(key)).Scan(&raw)
	if err != nil {
		return def
	}
	v, perr := config.Parse(key, raw)
	if perr != nil {
		return def
	}
	if str, ok := v.String(); ok && strings.TrimSpace(str) != "" {
		return str
	}
	return def
}

// bonusExpireDays resolves billing.bonus_expire_days. The plan row's own
// bonus_valid_days is the fallback, so a missing configuration row cannot
// turn a bonus batch into a permanent balance.
func (s *Service) bonusExpireDays(ctx context.Context, planBonusDays int64) int64 {
	if n := s.configInt(ctx, config.KeyBillingBonusExpireDays, 0); n > 0 {
		return n
	}
	if planBonusDays > 0 {
		return planBonusDays
	}
	return 90
}

// idemWindowDuration resolves idem.duplicate_window_seconds (default 10s).
func (s *Service) idemWindowDuration(ctx context.Context) time.Duration {
	if s.idemWindow > 0 {
		return s.idemWindow
	}
	secs := s.configInt(ctx, config.KeyIdemDuplicateWindowSeconds, 10)
	if secs <= 0 {
		secs = 10
	}
	return time.Duration(secs) * time.Second
}

// ---------------------------------------------------------------------------
// request plumbing
// ---------------------------------------------------------------------------

// decodeJSON is the bounded body reader for fund writes.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *apierr.APIError {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	payload, err := io.ReadAll(r.Body)
	if err != nil || len(strings.TrimSpace(string(payload))) == 0 {
		return mw.InvalidField("body")
	}
	if err := jsonx.Unmarshal(payload, dst); err != nil {
		return mw.InvalidField("body")
	}
	return nil
}

// likeEscape is the LIKE escape character. '!' instead of the default
// backslash keeps the query correct on servers with NO_BACKSLASH_ESCAPES.
const likeEscape = '!'

// likePattern wraps a search term for LIKE with the escape character applied.
func likePattern(q string) string {
	replacer := strings.NewReplacer(
		"!", "!!",
		"%", "!%",
		"_", "!_",
	)
	return "%" + replacer.Replace(q) + "%"
}

// sessionUID returns the console session's user id or the 401 to write.
func sessionUID(r *http.Request) (int64, *apierr.APIError) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok || sess == nil {
		return 0, mw.SessionMissing()
	}
	return sess.UID, nil
}

// ---------------------------------------------------------------------------
// identifiers
// ---------------------------------------------------------------------------

// orderNo builds t_recharge_order.order_no: yyyymmddHHmmss + 14 random digits
// (28 characters, TDS §1.5). Collisions are caught by uk_ro_order_no and
// retried by the caller, never assumed away.
func orderNo(now time.Time, rand func(int) string) string {
	return now.UTC().Format("20060102150405") + rand(14)
}

// fenToMicro converts an integer fen amount to micro-credits. 1 分 = 1 积分 by
// PRD §5.6 (1 元 = 100 积分 = 100 分), so the factor is exactly 10 000.
func fenToMicro(fen int64) int64 { return fen * MicroPerCredit }

// microToFen converts back for display. Integer division is exact for every
// value this system produces (micro is always a multiple of 10 000).
func microToFen(micro int64) int64 { return micro / MicroPerCredit }

// randomDigits returns exactly n decimal digits from crypto/rand.
func randomDigits(n int) string {
	if n <= 0 {
		return ""
	}
	var raw [8]byte
	if _, err := crand.Read(raw[:]); err != nil {
		return strings.Repeat("0", n)
	}
	v := binary.BigEndian.Uint64(raw[:])
	mod := uint64(1)
	for i := 0; i < n; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", n, v%mod)
}

// isDuplicateKey reports a MySQL unique-key violation (1062).
func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// isRetryableTxError reports the two InnoDB errors that mean "this
// transaction lost a race, run it again": 1213 deadlock and 1205 lock wait
// timeout. Concurrent quota approvals insert adjacent rows into
// t_rate_limit_policy, so InnoDB may pick one as the deadlock victim even
// though both transactions are individually correct.
func isRetryableTxError(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// ---------------------------------------------------------------------------
// review queue (T16 审核中心)
// ---------------------------------------------------------------------------

// The five t_review_task.biz_type values (0015 DDL). REDEEM_BATCH and
// BALANCE_ADJUST have no submission path in this repository yet; the three
// constants below are the ones the create transactions can enqueue.
const (
	reviewBizQuotaApply   = "QUOTA_APPLY"
	reviewBizInvoiceApply = "INVOICE_APPLY"
	reviewBizRefundApply  = "REFUND_APPLY"
)

// insertReviewTaskTx enqueues the pending row in the same transaction as the
// business row (TDS §1.9: the queue and the business state must never
// disagree). uk_rt_biz(biz_type, biz_id) makes a retry a no-op instead of a
// duplicate; a failure aborts the submission rather than leaving an
// application no operator can see.
func insertReviewTaskTx(ctx context.Context, tx *sql.Tx, bizType string, bizID, applicantID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO t_review_task (biz_type, biz_id, applicant_id, state, created_at)
		VALUES (?,?,?, 'PENDING', ?)
		ON DUPLICATE KEY UPDATE id = id`,
		bizType, bizID, applicantID, now)
	if err != nil {
		return fmt.Errorf("fund: enqueue review task %s#%d: %w", bizType, bizID, err)
	}
	return nil
}

// mustMarshal returns nil on an encoding failure. Audit "after" payloads are
// best-effort: losing one must not fail the money write that produced it.
func mustMarshal(v any) []byte {
	raw, err := jsonx.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

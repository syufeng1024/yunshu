// Package recon implements T09's reconciliation kernel: hanging-prehold
// recovery (TDS §5.3), the daily internal/ external reconciliation (TDS §5.4
// and §5.4.1, D-19) and append-only reversal entries (TDS §8.3.5).
//
// 资金真相只在 DB：对账任务除 t_reconcile_diff 与 t_notification 外不写任何
// 业务表。所有修正都走「冲正分录 + 人工确认」，本包不提供 UPDATE 余额的入口。
package recon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// Metrics is the optional observability surface for the reconciliation jobs.
// A nil value disables recording; it never changes a reconciliation result.
type Metrics interface {
	PreholdOrphan(reason string)
	LedgerReconcileDiff(n int64)
}

// Match modes stored in t_reconcile_diff.match_mode.
//
// INTERNAL is the D-19 namespace for R-01..R-08 violations. The four upstream
// bill diff types use DAILY / HOURLY / APPROX exactly as TDS §5.4.1 defines.
const (
	MatchModeInternal = "INTERNAL"
	MatchModeDaily    = "DAILY"
	MatchModeHourly   = "HOURLY"
	MatchModeApprox   = "APPROX"
)

// Alert levels. CRITICAL/HIGH/MEDIUM are the internal thresholds from
// TDS §5.4; P0/P1 are the external-bill thresholds from §5.4.1.
const (
	LevelCritical = "CRITICAL"
	LevelHigh     = "HIGH"
	LevelMedium   = "MEDIUM"
	LevelP0       = "P0"
	LevelP1       = "P1"
)

// Internal diff types. Lengths are checked by a test: diff_type is
// varchar(24). R08_COST_MARGIN_MISMATCH is exactly 24 characters.
const (
	DiffR01 = "R01_BALANCE_MISMATCH"
	DiffR02 = "R02_TWO_ACCOUNTS"
	DiffR03 = "R03_LEDGER_CONTINUITY"
	DiffR04 = "R04_BATCH_REMAINING"
	DiffR05 = "R05_DAILY_QUOTA"
	DiffR06 = "R06_HANGING_PREHOLD"
	DiffR07 = "R07_IDEM_EXPIRED"
	DiffR08 = "R08_COST_MARGIN_MISMATCH"
)

// External diff types from TDS §5.4.1.
const (
	DiffTokenMismatch   = "TOKEN_MISMATCH"
	DiffAmountMismatch  = "AMOUNT_MISMATCH"
	DiffMissingLocal    = "MISSING_LOCAL"
	DiffMissingUpstream = "MISSING_UPSTREAM"
)

// Diff is one reconciled difference.
type Diff struct {
	BizDate       string // YYYY-MM-DD (UTC+8)
	DiffType      string
	Subject       string // request_id / trade_no / user_id / batch_id / global key
	RequestID     string
	TradeNo       string
	LocalValue    int64
	UpstreamValue int64
	DeltaMicro    int64
	MatchMode     string
	Level         string
}

// DiffKey is sha256(biz_date|diff_type|subject), the uk_rd_key value.
func DiffKey(bizDate, diffType, subject string) string {
	sum := sha256.Sum256([]byte(bizDate + "|" + diffType + "|" + subject))
	return hex.EncodeToString(sum[:])
}

// Recorder is the only writer of t_reconcile_diff.
//
// Idempotency (TDS §5.4.1): the same (biz_date, diff_type, subject) converges
// on one row. A CLOSED row is not reopened unless the delta changed; when it
// did change the row is updated in place and reset to OPEN.
type Recorder struct {
	DB     *sql.DB
	Logger *obslog.Logger
	Clock  clock.Clock
}

// NewRecorder builds the writer.
func NewRecorder(db *sql.DB, logger *obslog.Logger, clk clock.Clock) *Recorder {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Recorder{DB: db, Logger: logger, Clock: clk}
}

// Record upserts one diff. It reports whether the row was inserted or its
// delta changed (false means an identical rerun).
func (r *Recorder) Record(ctx context.Context, d Diff) (bool, error) {
	if r == nil || r.DB == nil {
		return false, fmt.Errorf("recon: recorder requires a database")
	}
	if len(d.DiffType) > 24 {
		return false, fmt.Errorf("recon: diff_type %q exceeds varchar(24)", d.DiffType)
	}
	if d.Subject == "" {
		return false, fmt.Errorf("recon: diff %s requires a subject", d.DiffType)
	}
	if d.MatchMode == "" {
		d.MatchMode = MatchModeInternal
	}
	key := DiffKey(d.BizDate, d.DiffType, d.Subject)
	now := r.now()
	res, err := r.DB.ExecContext(ctx,
		`INSERT INTO t_reconcile_diff
		   (diff_key, biz_date, request_id, trade_no, diff_type, local_value,
		    upstream_value, delta_micro, match_mode, status, detected_at,
		    created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?, 'OPEN', ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   local_value    = IF(delta_micro <> VALUES(delta_micro), VALUES(local_value), local_value),
		   upstream_value = IF(delta_micro <> VALUES(delta_micro), VALUES(upstream_value), upstream_value),
		   request_id     = IF(delta_micro <> VALUES(delta_micro), VALUES(request_id), request_id),
		   trade_no       = IF(delta_micro <> VALUES(delta_micro), VALUES(trade_no), trade_no),
		   match_mode     = IF(delta_micro <> VALUES(delta_micro), VALUES(match_mode), match_mode),
		   status         = IF(delta_micro <> VALUES(delta_micro), 'OPEN', status),
		   detected_at    = IF(delta_micro <> VALUES(delta_micro), VALUES(detected_at), detected_at),
		   updated_at     = IF(delta_micro <> VALUES(delta_micro), VALUES(updated_at), updated_at),
		   delta_micro    = VALUES(delta_micro)`,
		key, d.BizDate, nullable(d.RequestID), nullable(d.TradeNo), d.DiffType,
		d.LocalValue, d.UpstreamValue, d.DeltaMicro, d.MatchMode, now, now, now,
	)
	if err != nil {
		return false, fmt.Errorf("recon: record diff %s/%s: %w", d.DiffType, d.Subject, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("recon: record diff rows: %w", err)
	}
	return n > 0, nil
}

// Notify writes one t_notification row. user_id 0 is the operations sentinel:
// t_notification has no role column and the ops console reads event_code.
//
// It is idempotent per (event_code, body). The hanging scanner runs every 60s
// and re-derives exactly the same body for the same stuck request — the same
// request_id, the same token counters — so a bare INSERT wrote 1,440 rows per
// day per hung request and buried the rest of the operations inbox. Duplicate
// means the same document: a genuinely different finding carries a different
// body (the tokens/consumed values are part of it).
//
// t_notification has no unique key usable for this: body is varchar(1024) and
// an index over it exceeds InnoDB's 3072-byte limit under utf8mb4. The check is
// therefore part of the statement (INSERT ... SELECT ... WHERE NOT EXISTS).
// jobsd owns each reconciliation task through K21/K22, so there is no
// concurrent writer racing this read within one round.
func (r *Recorder) Notify(ctx context.Context, eventCode, title, body string, channelMask int) error {
	if r == nil || r.DB == nil {
		return fmt.Errorf("recon: recorder requires a database")
	}
	now := r.now()
	noticeTitle := truncate(title, 128)
	noticeBody := truncate(body, 1024)
	_, err := r.DB.ExecContext(ctx,
		`INSERT INTO t_notification
		   (user_id, event_code, title, body, channel_mask, send_state,
		    created_at, expired_at)
		 SELECT 0, ?, ?, ?, ?, ?, ?, ? FROM DUAL
		  WHERE NOT EXISTS (
		        SELECT 1 FROM t_notification
		         WHERE user_id = 0 AND event_code = ? AND body = ?)`,
		eventCode, noticeTitle, noticeBody, channelMask,
		[]byte(`{"inbox":"SENT"}`), now, now.AddDate(0, 0, 180),
		eventCode, noticeBody,
	)
	if err != nil {
		return fmt.Errorf("recon: notify %s: %w", eventCode, err)
	}
	return nil
}

// Alert logs one violation at its TDS level. It never writes business data.
func (r *Recorder) Alert(ctx context.Context, d Diff) {
	if r == nil || r.Logger == nil {
		return
	}
	msg := fmt.Sprintf("reconcile_diff level=%s type=%s subject=%s local=%d upstream=%d delta=%d",
		d.Level, d.DiffType, d.Subject, d.LocalValue, d.UpstreamValue, d.DeltaMicro)
	switch d.Level {
	case LevelCritical, LevelP0:
		r.Logger.Error(ctx, msg, obslog.Fields{Stage: "reconcile"})
	case LevelHigh, LevelMedium, LevelP1:
		r.Logger.Warn(ctx, msg, obslog.Fields{Stage: "reconcile"})
	default:
		r.Logger.Info(ctx, msg, obslog.Fields{Stage: "reconcile"})
	}
}

func (r *Recorder) now() time.Time {
	if r == nil || r.Clock == nil {
		return time.Now().UTC()
	}
	return r.Clock.Now().UTC()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

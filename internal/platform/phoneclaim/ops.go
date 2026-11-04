// This file is the operator half of the phone ledger: the five read/write
// operations behind cmd/backfill-phone-claim.
//
// Why a command and not a migration: the release is not a schema change and it
// is not reversible. It decides, row by row, whether a live number belongs to an
// account that cancelled itself (release it) or to one an operator banned (keep
// it) — and it must run in a specific order relative to the code deploy, which
// only a human can choose. Expressing that as SQL inside the checksum-locked
// migration set would make it un-schedulable and un-reportable.
//
// The two write verbs are deliberately separate, and one does not imply the
// other:
//
//	backfill  copies t_user numbers into the ledger. Touches no t_user byte.
//	release   records the release in the ledger, then clears phone/phone_hmac.
//
// Order is not advice. Releasing before backfilling leaves old cancelled numbers
// with no ledger row, so the next registration of that number gets a free trial
// (design §9). Release therefore refuses to start while backfill is pending.
package phoneclaim

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"yunshu.dev/core/internal/platform/phonehmac"
)

// Exit codes. The runbook quotes them, so they are part of the interface.
const (
	ExitOK             = 0
	ExitRuntime        = 1
	ExitBlocking       = 2
	ExitOrderViolation = 3
)

// Anomaly kinds reported by detect and verify. Every row the commands refuse to
// handle is classified, so an operator never has to guess why a count does not
// match.
const (
	// AnomalyPhoneWithoutHMAC is a live row with a plaintext number and no hash:
	// the backfill that fills phone_hmac has not covered it yet
	// (docs/ops-runbook.md §10.3). Nothing here can repair it — the hash needs
	// the phone HMAC key, and the plaintext must survive until it is recomputed.
	AnomalyPhoneWithoutHMAC = "PHONE_WITHOUT_HMAC"
	// AnomalyHMACNot64 is a hash that is not a SHA-256 hex digest. It cannot be
	// matched against the ledger or against a client's login, so it is reported
	// rather than silently copied.
	AnomalyHMACNot64 = "HMAC_NOT_64"
	// AnomalyOverReleased is a number that was released without the account
	// having cancelled itself (OPS_MANUAL / RISK_AUTO / FROZEN_OVERDRAFT). Those
	// numbers must stay occupied; a release here lets a banned account register
	// again with the same handset.
	AnomalyOverReleased = "OVER_RELEASED"
)

// Defaults for the batch knobs. They match backfill-phone-hmac so an operator
// does not have to learn a second set of numbers.
const (
	defaultBatch = 500
	defaultSleep = 50 * time.Millisecond
)

// Options configures Backfill and Release.
type Options struct {
	// Batch is the number of rows handled per round trip (default 500).
	Batch int
	// Sleep is the pause between batches (default 50ms), so a production run
	// does not present itself as a sustained write spike.
	Sleep time.Duration
	// MaxRows stops after this many rows; 0 means no limit.
	MaxRows int64
	// FromID skips every row with id <= FromID: for "an operator already handled
	// this segment by hand", not a required resume mechanism — the WHERE clauses
	// make a rerun resume by itself.
	FromID int64
	// Commit false is a dry run: the same SELECT and the same decisions, and not
	// one row is written.
	Commit bool
	// UID, when non-zero, restricts release to that one account. It is the
	// recovery path for the "plaintext number, no hash" state: repair the hash,
	// then release that row only.
	UID int64
	// Out receives progress lines when non-nil.
	Out io.Writer
}

func (o Options) batch() int {
	if o.Batch > 0 {
		return o.Batch
	}
	return defaultBatch
}

func (o Options) sleep() time.Duration {
	if o.Sleep > 0 {
		return o.Sleep
	}
	return defaultSleep
}

// Anomaly is one row the commands will not touch, classified and masked. The
// masked value is what a human needs to find the account in the console; the
// plaintext never leaves the database.
type Anomaly struct {
	Kind   string
	UID    int64
	Masked string
}

// DetectReport is the read-only survey.
type DetectReport struct {
	Scanned           int64
	PendingBackfill   int64
	ReleaseCandidates int64
	OverReleased      int64
	Anomalies         []Anomaly
	Elapsed           time.Duration
}

// Blocking reports whether the survey found something a human must decide
// before any write runs.
func (r DetectReport) Blocking() bool { return r.OverReleased > 0 || len(r.Anomalies) > 0 }

// ExitCode maps the survey to the command's exit status (runbook contract).
func (r DetectReport) ExitCode() int {
	if r.Blocking() {
		return ExitBlocking
	}
	return ExitOK
}

// BackfillReport is what one backfill run did.
type BackfillReport struct {
	Scanned int64
	Pending int64
	Changed int64
	Healed  int64
	Skipped int64
	Failed  int64
	Batches int
	Elapsed time.Duration
}

// ReleaseReport is what one release run did.
type ReleaseReport struct {
	Scanned    int64
	Candidates int64
	Changed    int64
	Skipped    int64
	Failed     int64
	Batches    int
	Elapsed    time.Duration
}

// VerifyReport is the post-run drift check. Every field must be zero.
type VerifyReport struct {
	PendingBackfill      int64
	UnreleasedSelfCancel int64
	OverReleased         int64
	BadHMAC              int64
	Elapsed              time.Duration
}

// Blocking reports whether any of the four assertions failed.
func (r VerifyReport) Blocking() bool {
	return r.PendingBackfill > 0 || r.UnreleasedSelfCancel > 0 ||
		r.OverReleased > 0 || r.BadHMAC > 0
}

// ExitCode maps the verification to the command's exit status.
func (r VerifyReport) ExitCode() int {
	if r.Blocking() {
		return ExitBlocking
	}
	return ExitOK
}

// LookupReport answers the support question "what is this number's history".
type LookupReport struct {
	PhoneMasked  string
	HMACShort    string
	Found        bool
	Claim        Claim
	HolderUID    sql.NullInt64
	HolderStatus string
	HolderMasked string
}

// overReleasedWhere is the single predicate for "a number was released that
// should still be occupied". It lives here because three readers ask the same
// question (detect's counter, verify's assertion, and the anomaly listing) and
// three hand-written copies is how the answers drift apart.
//
// `phone IS NOT NULL` is load-bearing, not cosmetic:
//
//   - Both columns NULL is the state of an account that never had a number
//     (every e-mail account an operator bans gets disable_source='OPS_MANUAL'),
//     and it is also the state left behind by a genuine release. The two are
//     indistinguishable, so asserting on both-NULL makes the assertion
//     unsatisfiable: any deployment with one banned e-mail account would fail
//     verify forever and the gate would be ignored within a week.
//   - `phone` present with `phone_hmac` NULL is the only shape that proves a
//     release happened to a row that should keep its number (the plaintext was
//     left behind). Nothing in the codebase produces it; a hand-run UPDATE does.
//
// Consequence, stated plainly: an over-release that clears both columns cannot
// be detected here. It is recorded as a known limit in the design's AC-12 and
// must not be "fixed" by re-adding the both-NULL case.
const overReleasedWhere = `phone_hmac IS NULL AND phone IS NOT NULL AND deleted_at IS NULL
	AND (disable_source IN ('OPS_MANUAL', 'RISK_AUTO') OR status = 'FROZEN_OVERDRAFT')`

// Detect surveys the ledger without writing anything.
func Detect(ctx context.Context, db *sql.DB) (DetectReport, error) {
	started := time.Now()
	report := DetectReport{}

	counts := []struct {
		query string
		dst   *int64
	}{
		{`SELECT COUNT(*) FROM t_user WHERE deleted_at IS NULL`, &report.Scanned},
		{`SELECT COUNT(*) FROM t_user u
		    LEFT JOIN t_phone_claim c ON c.phone_hmac = u.phone_hmac
		   WHERE u.phone_hmac IS NOT NULL AND u.deleted_at IS NULL
		     AND u.phone_hmac NOT LIKE '#del#%' AND c.phone_hmac IS NULL`,
			&report.PendingBackfill},
		{`SELECT COUNT(*) FROM t_user
		   WHERE status = 'DISABLED' AND disable_source = 'SELF_CANCEL'
		     AND deleted_at IS NULL AND phone_hmac IS NOT NULL
		     AND phone_hmac NOT LIKE '#del#%'`, &report.ReleaseCandidates},
		{`SELECT COUNT(*) FROM t_user WHERE ` + overReleasedWhere, &report.OverReleased},
	}
	for _, c := range counts {
		if err := db.QueryRowContext(ctx, c.query).Scan(c.dst); err != nil {
			return report, fmt.Errorf("phoneclaim: detect count: %w", err)
		}
	}

	anomalies, err := collectAnomalies(ctx, db, 0)
	if err != nil {
		return report, err
	}
	report.Anomalies = anomalies
	report.Elapsed = time.Since(started)
	return report, nil
}

// Backfill copies numbers from t_user into the ledger, and heals the window of
// rows that already exist. It never writes t_user.
func Backfill(ctx context.Context, db *sql.DB, opts Options) (BackfillReport, error) {
	started := time.Now()
	report := BackfillReport{}

	lastID := opts.FromID
	healed, err := healGrants(ctx, db, opts, &report)
	if err != nil {
		return report, err
	}
	report.Healed = healed

	for {
		remaining := opts.MaxRows - report.Changed
		if opts.MaxRows > 0 && remaining <= 0 {
			break
		}
		limit := opts.batch()
		if opts.MaxRows > 0 && int64(limit) > remaining {
			limit = int(remaining)
		}
		rows, err := selectBackfillBatch(ctx, db, lastID, limit)
		if err != nil {
			return report, err
		}
		if len(rows) == 0 {
			break
		}
		report.Batches++
		report.Pending += int64(len(rows))
		lastID = rows[len(rows)-1].id

		if !opts.Commit {
			progress(opts.Out, "backfill: 待回填 %d 行（id ≤ %d），dry-run 不写库", len(rows), lastID)
			continue
		}
		changed, failed, err := insertBatch(ctx, db, rows)
		report.Changed += changed
		report.Failed += failed
		if err != nil {
			return report, err
		}
		progress(opts.Out, "backfill: 批 %d 处理 %d 行（changed=%d failed=%d），last id=%d",
			report.Batches, len(rows), changed, failed, lastID)
		if opts.Commit && opts.sleep() > 0 {
			time.Sleep(opts.sleep())
		}
	}
	report.Elapsed = time.Since(started)
	return report, nil
}

// Release clears the phone columns of accounts that cancelled themselves, after
// recording the release in the ledger.
//
// The gate is not advisory: while a number is missing from the ledger, releasing
// it would let the next registration of that number claim a trial the previous
// owner already consumed. The command therefore refuses to start, with a
// dedicated exit code, instead of "doing what it can".
func Release(ctx context.Context, db *sql.DB, opts Options) (ReleaseReport, error) {
	started := time.Now()
	report := ReleaseReport{}

	pending, err := countPendingBackfill(ctx, db)
	if err != nil {
		return report, err
	}
	if pending > 0 {
		return report, &OrderViolation{Pending: pending}
	}

	lastID := opts.FromID
	for {
		remaining := opts.MaxRows - report.Changed
		if opts.MaxRows > 0 && remaining <= 0 {
			break
		}
		limit := opts.batch()
		if opts.MaxRows > 0 && int64(limit) > remaining {
			limit = int(remaining)
		}
		rows, err := selectReleaseBatch(ctx, db, lastID, limit, opts.UID)
		if err != nil {
			return report, err
		}
		if len(rows) == 0 {
			break
		}
		report.Batches++
		report.Candidates += int64(len(rows))
		lastID = rows[len(rows)-1].id

		for _, row := range rows {
			report.Scanned++
			if !opts.Commit {
				progress(opts.Out, "release: 待释放 uid=%d phone=%s（dry-run）",
					row.id, phonehmac.Mask(row.phone))
				continue
			}
			changed, err := releaseOne(ctx, db, row.id, time.Now().UTC())
			if err != nil {
				// One bad row must not stop a migration: record it, count it,
				// carry on, and fail the run at the end.
				report.Failed++
				progress(opts.Out, "release: uid=%d 失败：%v", row.id, err)
				continue
			}
			if changed {
				report.Changed++
				progress(opts.Out, "release: uid=%d phone=%s 已释放", row.id, phonehmac.Mask(row.phone))
			} else {
				report.Skipped++
			}
			if opts.sleep() > 0 {
				time.Sleep(opts.sleep())
			}
		}
		if opts.UID != 0 {
			break
		}
	}
	report.Elapsed = time.Since(started)
	return report, nil
}

// Verify re-runs the four invariants the increment is supposed to establish.
func Verify(ctx context.Context, db *sql.DB) (VerifyReport, error) {
	started := time.Now()
	report := VerifyReport{}

	pending, err := countPendingBackfill(ctx, db)
	if err != nil {
		return report, err
	}
	report.PendingBackfill = pending

	queries := []struct {
		query string
		dst   *int64
	}{
		{`SELECT COUNT(*) FROM t_user
		   WHERE status = 'DISABLED' AND disable_source = 'SELF_CANCEL'
		     AND deleted_at IS NULL AND phone_hmac IS NOT NULL
		     AND phone_hmac NOT LIKE '#del#%'`, &report.UnreleasedSelfCancel},
		{`SELECT COUNT(*) FROM t_user WHERE ` + overReleasedWhere, &report.OverReleased},
		{`SELECT COUNT(*) FROM t_phone_claim
		   WHERE CHAR_LENGTH(phone_hmac) <> 64
		      OR phone_hmac <> LOWER(phone_hmac)`, &report.BadHMAC},
	}
	for _, q := range queries {
		if err := db.QueryRowContext(ctx, q.query).Scan(q.dst); err != nil {
			return report, fmt.Errorf("phoneclaim: verify count: %w", err)
		}
	}
	report.Elapsed = time.Since(started)
	return report, nil
}

// Lookup answers a support call: the ledger row for a number, and who holds it
// now. It is read-only and prints only masked values.
func Lookup(ctx context.Context, db *sql.DB, key, rawPhone string) (LookupReport, error) {
	canonical, ok := phonehmac.Canonical(rawPhone)
	if !ok {
		return LookupReport{}, errors.New("phoneclaim: 手机号不是可解析的号码")
	}
	hmac, err := phonehmac.Hash(key, canonical)
	if err != nil {
		return LookupReport{}, err
	}
	report := LookupReport{PhoneMasked: phonehmac.Mask(canonical), HMACShort: shortHash(hmac)}

	claim, err := Get(ctx, db, hmac)
	switch {
	case errors.Is(err, ErrNotFound):
		// No ledger row: the number was never claimed, or the backfill has not
		// run. Report the holder anyway — the answer is still useful.
	case err != nil:
		return report, err
	default:
		report.Found = true
		report.Claim = claim
	}

	var uid int64
	var status, phone string
	err = db.QueryRowContext(ctx, `
		SELECT id, status, COALESCE(phone, '') FROM t_user
		 WHERE phone_hmac = ? AND deleted_at IS NULL LIMIT 2`, hmac).Scan(&uid, &status, &phone)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return report, err
	default:
		report.HolderUID = sql.NullInt64{Int64: uid, Valid: true}
		report.HolderStatus = status
		report.HolderMasked = phonehmac.Mask(phone)
	}
	return report, nil
}

// OrderViolation reports "release was started before backfill finished". It
// carries the pending count because that number is the answer to "how far off
// am I".
type OrderViolation struct {
	Pending int64
}

func (o *OrderViolation) Error() string {
	return fmt.Sprintf("还有 %d 个号码未回填台账；先跑 backfill --commit"+
		"（顺序反了会给老注销号白送一次体验版）", o.Pending)
}

// ExitCode lets the CLI answer 3 for a scheduling mistake, as distinct from 1
// (a runtime fault) and 2 (a blocking finding).
func (o *OrderViolation) ExitCode() int { return ExitOrderViolation }

func countPendingBackfill(ctx context.Context, db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM t_user u
		  LEFT JOIN t_phone_claim c ON c.phone_hmac = u.phone_hmac
		 WHERE u.phone_hmac IS NOT NULL AND u.deleted_at IS NULL
		   AND u.phone_hmac NOT LIKE '#del#%' AND c.phone_hmac IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("phoneclaim: count pending: %w", err)
	}
	return n, nil
}

type backfillRow struct {
	id        int64
	hmac      string
	trialAt   sql.NullTime
	createdAt time.Time
}

func selectBackfillBatch(ctx context.Context, db *sql.DB, afterID int64, limit int) ([]backfillRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT u.id, u.phone_hmac, u.trial_claimed_at, u.created_at
		  FROM t_user u
		 WHERE u.id > ? AND u.phone_hmac IS NOT NULL AND u.deleted_at IS NULL
		   AND u.phone_hmac NOT LIKE '#del#%'
		   AND NOT EXISTS (SELECT 1 FROM t_phone_claim c WHERE c.phone_hmac = u.phone_hmac)
		 ORDER BY u.id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("phoneclaim: select backfill batch: %w", err)
	}
	defer rows.Close()
	var out []backfillRow
	for rows.Next() {
		var r backfillRow
		if err := rows.Scan(&r.id, &r.hmac, &r.trialAt, &r.createdAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func insertBatch(ctx context.Context, db *sql.DB, rows []backfillRow) (changed, failed int64, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	now := time.Now().UTC()
	for _, r := range rows {
		// The ledger row is the account's own history: first_user_id and
		// created_at come from the moment the number was first claimed, and
		// trial_granted_at from the account's own claim flag. This is the only
		// place the past can be reconstructed, so it is reconstructed exactly.
		res, err := tx.ExecContext(ctx, `
			INSERT INTO t_phone_claim
			  (phone_hmac, first_user_id, last_claim_user_id, last_claim_at,
			   cancel_count, trial_granted_at, last_trial_user_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE phone_hmac = phone_hmac`,
			r.hmac, r.id, r.id, r.createdAt, nullTime(r.trialAt),
			TrialGrantUserID(r.trialAt, r.id), r.createdAt, now)
		if err != nil {
			// A duplicate hmac between two accounts is a data problem no retry
			// fixes (uk_user_phone_hmac should make it impossible); count it and
			// keep the rest of the batch.
			failed++
			continue
		}
		if n, rowsErr := res.RowsAffected(); rowsErr == nil && n == 1 {
			changed++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	committed = true
	return changed, failed, nil
}

// healGrants fills trial_granted_at on rows that exist but lost the window, and
// returns how many rows it touched (0 in a dry run beyond counting).
func healGrants(ctx context.Context, db *sql.DB, opts Options, report *BackfillReport) (int64, error) {
	const join = `
		FROM t_phone_claim c JOIN t_user u ON u.phone_hmac = c.phone_hmac
		 WHERE c.trial_granted_at IS NULL AND u.trial_claimed_at IS NOT NULL`
	if !opts.Commit {
		var n int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) `+join).Scan(&n); err != nil {
			return 0, fmt.Errorf("phoneclaim: count heal: %w", err)
		}
		if n > 0 {
			progress(opts.Out, "backfill: %d 行台账缺 trial_granted_at（dry-run 不写库）", n)
		}
		return n, nil
	}
	res, err := db.ExecContext(ctx, `
		UPDATE t_phone_claim c JOIN t_user u ON u.phone_hmac = c.phone_hmac
		   SET c.trial_granted_at = u.trial_claimed_at,
		       c.last_trial_user_id = COALESCE(c.last_trial_user_id, u.id),
		       c.updated_at = ?
		 WHERE c.trial_granted_at IS NULL AND u.trial_claimed_at IS NOT NULL`,
		time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("phoneclaim: heal grants: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		progress(opts.Out, "backfill: 补齐 %d 行台账的 trial_granted_at", n)
	}
	return n, nil
}

type releaseRow struct {
	id    int64
	phone string
}

func selectReleaseBatch(ctx context.Context, db *sql.DB, afterID int64, limit int, uid int64) ([]releaseRow, error) {
	query := `
		SELECT id, COALESCE(phone, '')
		  FROM t_user
		 WHERE id > ? AND status = 'DISABLED' AND disable_source = 'SELF_CANCEL'
		   AND deleted_at IS NULL AND phone_hmac IS NOT NULL
		   AND phone_hmac NOT LIKE '#del#%'`
	args := []any{afterID}
	if uid != 0 {
		// --uid addresses exactly one row, so the id range and the LIMIT are
		// irrelevant; the id predicate keeps the row identity explicit.
		query += ` AND id = ?`
		args = append(args, uid)
	} else {
		query += ` ORDER BY id LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("phoneclaim: select release batch: %w", err)
	}
	defer rows.Close()
	var out []releaseRow
	for rows.Next() {
		var r releaseRow
		if err := rows.Scan(&r.id, &r.phone); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// releaseOne is one row's transaction, in the ledger's lock order: t_user first
// (locked), then t_phone_claim, then the t_user update.
func releaseOne(ctx context.Context, db *sql.DB, uid int64, now time.Time) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var hmac, source, status string
	var trialAt, cancelAt, disabledAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(phone_hmac, ''), COALESCE(disable_source, ''), COALESCE(status, ''),
		       trial_claimed_at, cancel_apply_at, disabled_at
		  FROM t_user WHERE id = ? AND deleted_at IS NULL FOR UPDATE`, uid).
		Scan(&hmac, &source, &status, &trialAt, &cancelAt, &disabledAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if hmac == "" {
		// Already released (a concurrent run, or a retry of the same batch).
		return false, nil
	}
	if source != "SELF_CANCEL" || status != "DISABLED" {
		// The boundary: only a self-cancellation releases. Answering "skipped"
		// instead of an error keeps the run going and the report honest.
		return false, nil
	}
	releaseAt := now
	if cancelAt.Valid {
		releaseAt = cancelAt.Time.UTC()
	} else if disabledAt.Valid {
		releaseAt = disabledAt.Time.UTC()
	}
	if _, err := RecordCancelTx(ctx, tx, hmac, uid, releaseAt, trialAt,
		TrialGrantUserID(trialAt, uid)); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE t_user SET phone = NULL, phone_hmac = NULL, updated_at = ?
		 WHERE id = ? AND phone_hmac = ?`, now, uid, hmac)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	return n == 1, nil
}

func collectAnomalies(ctx context.Context, db *sql.DB, limit int) ([]Anomaly, error) {
	out := []Anomaly{}
	queries := []struct {
		kind  string
		query string
		mask  func(phone, hmac string) string
	}{
		{
			kind: AnomalyPhoneWithoutHMAC,
			query: `SELECT id, COALESCE(phone, ''), COALESCE(phone_hmac, '') FROM t_user
			         WHERE phone IS NOT NULL AND phone_hmac IS NULL AND deleted_at IS NULL
			         ORDER BY id`,
			mask: func(phone, _ string) string { return phonehmac.Mask(phone) },
		},
		{
			kind: AnomalyHMACNot64,
			query: `SELECT id, COALESCE(phone, ''), phone_hmac FROM t_user
			         WHERE phone_hmac IS NOT NULL AND CHAR_LENGTH(phone_hmac) <> 64
			           AND phone_hmac NOT LIKE '#del#%' ORDER BY id`,
			mask: func(phone, hmac string) string {
				if phone != "" {
					return phonehmac.Mask(phone)
				}
				return shortHash(hmac)
			},
		},
		{
			kind: AnomalyOverReleased,
			query: `SELECT id, COALESCE(phone, ''), '' FROM t_user
			         WHERE ` + overReleasedWhere + ` ORDER BY id`,
			mask: func(phone, _ string) string { return phonehmac.Mask(phone) },
		},
	}
	for _, q := range queries {
		rows, err := db.QueryContext(ctx, q.query)
		if err != nil {
			return nil, fmt.Errorf("phoneclaim: anomaly %s: %w", q.kind, err)
		}
		for rows.Next() {
			var a Anomaly
			var phone, hmac string
			if err := rows.Scan(&a.UID, &phone, &hmac); err != nil {
				rows.Close()
				return nil, err
			}
			if limit > 0 && len(out) >= limit {
				continue
			}
			a.Kind = q.kind
			a.Masked = q.mask(phone, hmac)
			out = append(out, a)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// shortHash is the only form of a hash that may reach a terminal or a ticket:
// six characters identify the row to whoever is looking at the database and
// reveal nothing about the number.
func shortHash(hmac string) string {
	if len(hmac) <= 6 {
		return hmac
	}
	return hmac[:6]
}

func progress(out io.Writer, format string, args ...any) {
	if out == nil {
		return
	}
	fmt.Fprintf(out, format+"\n", args...)
}

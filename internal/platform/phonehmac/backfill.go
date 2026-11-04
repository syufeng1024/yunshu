// Package phonehmac's backfill half: the three read/write operations that fill
// t_user.phone_hmac for historical rows.
//
// Why this is Go and not a migration: MySQL has no HMAC() function (it offers
// SHA2() and AES_ENCRYPT()), and the migration runner is an embedded-SQL
// executor whose bytes are checksum-locked. A data migration that needs a
// secret, batching, rate limiting and a human decision on duplicates cannot be
// expressed as SQL, and pretending otherwise would either break the checksum
// guard or turn a duplicate row into a half-applied batch.
//
// The three operations are deliberately separate commands:
//
//	Detect   read-only; reports duplicates and anomalies; writes nothing
//	Apply    fills the column; refuses to start when Detect is not clean
//	Verify   recomputes every row and compares; the drift detector
//
// Order is not advisory. Applying over duplicates leaves the batch half-done at
// whatever row hits 1062, and the state ("some rows filled, some not, one
// failure in the middle") is exactly the state nobody can explain afterwards.
package phonehmac

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Anomaly kinds. detect and verify classify every row they refuse to fill, so an
// operator never has to guess why a count does not match.
const (
	// AnomalyNonCanonical means t_user.phone is not the value the login
	// normalizer would produce ("+8613800138000", "138 0013 8000", full-width
	// digits). Hashing it would create a hash no login can reproduce, so the row
	// is reported instead of repaired: rewriting PII needs a human.
	AnomalyNonCanonical = "NON_CANONICAL"
	// AnomalyEmptyPhone means phone = ''. The empty string is not "no number"
	// (that state is NULL); hashing it would give every such row one shared
	// value and manufacture fake duplicates.
	AnomalyEmptyPhone = "EMPTY_PHONE"
	// AnomalyStaleHMAC means the row already has a hash that does not match the
	// current key and phone. Causes: a manual DB edit, a previous run with the
	// wrong key, or an interrupted rotation.
	AnomalyStaleHMAC = "STALE_HMAC"
	// AnomalyGhostHMAC means the row is soft-deleted but its hash is not the
	// #del#{id}#{unix_ms} form, so the number is still occupied by a ghost row
	// (a new registration of that number would answer 409 forever).
	AnomalyGhostHMAC = "GHOST_HMAC"
	// AnomalyPhoneWithoutHMAC is verify-only: a phone is present and the hash is
	// NULL, i.e. the backfill has not covered this row.
	AnomalyPhoneWithoutHMAC = "PHONE_WITHOUT_HMAC"
)

// Exit codes. They are part of the operator contract: a runbook step says "if
// detect returns 2, stop and merge the groups".
const (
	ExitOK        = 0
	ExitRuntime   = 1
	ExitDuplicate = 2
	ExitAnomaly   = 3
	ExitConflict  = 4
)

// DuplicateMergePrefix is the marker a soft-deleted row's phone_hmac must carry
// (docs/schema.md: a column that participates in a unique index is rewritten to
// "#del#{id}{unix_ms}" on deletion, otherwise the value is occupied forever).
const DuplicateMergePrefix = "#del#"

// ErrConflict reports "the value this row needs is already taken by another
// row": another account registered the same number between Detect and the
// UPDATE, or two rows in the table hold the same number and one of them was
// filled first. It is never resolved automatically — one of the two rows is a
// real account and the other must be merged by an operator.
var ErrConflict = errors.New("phonehmac: phone_hmac already taken by another row")

// Options configures Apply.
type Options struct {
	// Batch is the number of rows read per SELECT (default 500).
	Batch int
	// Sleep is the pause between batches (default 50ms). It exists so a
	// production backfill does not present itself as a sustained write spike.
	Sleep time.Duration
	// MaxRows stops after filling this many rows; 0 means no limit.
	MaxRows int64
	// FromID skips every row with id <= FromID. It is for "an operator already
	// handled this segment by hand", not a required resume mechanism: the
	// WHERE phone_hmac IS NULL gate makes a rerun resume by itself.
	FromID int64
	// Commit false is a dry run: the report is computed from the same SELECT and
	// the same hashing, and not one row is written.
	Commit bool
	// Out receives progress lines. Nil discards them.
	Out io.Writer
}

// Row is one t_user row as the backfill sees it. Only the columns the three
// operations reason about are read.
type Row struct {
	ID     int64
	UserNo string
	// phone is the plaintext column. It is unexported on purpose: every report
	// this package hands out (and everything the CLI prints) carries Mask(phone),
	// and a future caller that JSON-marshals a Row must not be able to leak a
	// handset number into a log just by passing the struct along.
	phone         string
	HMAC          string
	Status        string
	CreatedAt     time.Time
	DeletedAt     sql.NullTime
	CancelApplyAt sql.NullTime
}

// DuplicateGroup is one phone number that more than one row claims.
type DuplicateGroup struct {
	// Masked is the phone number reduced to its 3+2 visible bytes; the plaintext
	// never enters the report (these lines get pasted into tickets).
	Masked string
	HMAC   string
	Rows   []Row
}

// Anomaly is one row the backfill refuses to fill, with the reason.
type Anomaly struct {
	Kind   string
	ID     int64
	UserNo string
	Masked string
	Detail string
}

// Report is the shared result of Detect and Verify.
type Report struct {
	// Scanned is the number of rows read (phone IS NOT NULL).
	Scanned int
	// Pending is the number of rows that still need a hash.
	Pending int
	// Filled is the number of rows that already carry the correct hash.
	Filled int
	// Skipped is the number of soft-deleted rows left alone on purpose (see
	// Apply): they must be rewritten to #del# by an operator, not filled with a
	// live hash.
	SkippedGhosts int

	Duplicates []DuplicateGroup
	Anomalies  []Anomaly
}

// Blocked reports whether the report stops an Apply.
func (r Report) Blocked() bool { return len(r.Duplicates) > 0 || len(r.Anomalies) > 0 }

// ExitCode is the process exit code this report maps to.
func (r Report) ExitCode() int {
	switch {
	case len(r.Duplicates) > 0:
		return ExitDuplicate
	case len(r.Anomalies) > 0:
		return ExitAnomaly
	default:
		return ExitOK
	}
}

// ApplyReport is the result of one Apply run.
type ApplyReport struct {
	// DryRun is true when nothing was written.
	DryRun bool
	// Scanned is the number of rows read from the Pending set.
	Scanned int
	// Filled is the number of rows that received a hash (0 in a dry run: the
	// count that *would* have been written is WouldFill).
	Filled int
	// WouldFill is the dry-run projection.
	WouldFill int
	// SkippedGhosts is the number of soft-deleted rows passed over.
	SkippedGhosts int
	// Batches is the number of SELECT round trips.
	Batches int
	// Elapsed is the wall time of the run.
	Elapsed time.Duration
	// Conflict, when non-nil, describes the rows that collided on
	// uk_user_phone_hmac. The caller then exits with ExitConflict.
	Conflict *Conflict
}

// Conflict names the two rows that want the same hash.
type Conflict struct {
	// RowID is the row the backfill tried to fill.
	RowID int64
	// HolderID is the row that already owns that hash (0 if unknown).
	HolderID int64
	// Masked is the phone number, reduced.
	Masked string
}

// Error describes the conflict without a phone number in plaintext.
func (c *Conflict) Error() string {
	return fmt.Sprintf("phonehmac: phone %s already owned by row %d (row %d not written)",
		c.Masked, c.HolderID, c.RowID)
}

// Unwrap lets a caller test for ErrConflict with errors.Is while keeping the ids.
func (c *Conflict) Unwrap() error { return ErrConflict }

// Detect scans every row with a phone number and reports what would block an
// Apply: duplicate numbers and anomalous values.
//
// Scope is deliberately wider than "rows needing a fill": soft-deleted rows and
// rows that already carry a hash are scanned too. A duplicate that is hidden
// because one of its two rows was already filled is still a duplicate, and a
// ghost row that already has a live hash still blocks the number.
func Detect(ctx context.Context, db *sql.DB, key string) (Report, error) {
	// Validate the key before reading anything: on an empty table no row would
	// ever be hashed, and "0 duplicates" would then be reported for a command
	// that cannot work at all.
	if _, err := Hash(key, "probe"); err != nil {
		return Report{}, err
	}
	rows, err := scanRows(ctx, db,
		`SELECT id, user_no, COALESCE(phone, ''), COALESCE(phone_hmac, ''), status,
		        created_at, deleted_at, cancel_apply_at
		   FROM t_user
		  WHERE phone IS NOT NULL
		  ORDER BY id`)
	if err != nil {
		return Report{}, err
	}
	report := Report{Scanned: len(rows)}

	type group struct {
		masked string
		rows   []Row
	}
	byHash := make(map[string]*group, len(rows))
	order := make([]string, 0, len(rows))

	for _, row := range rows {
		canonical, ok := Canonical(row.phone)
		var anomaly *Anomaly
		switch {
		case row.phone == "":
			anomaly = &Anomaly{Kind: AnomalyEmptyPhone, ID: row.ID, UserNo: row.UserNo,
				Detail: "phone 是空串（应为 NULL）；空串会被哈希成同一个值，制造假重复"}
		case !ok || canonical != row.phone:
			anomaly = &Anomaly{Kind: AnomalyNonCanonical, ID: row.ID, UserNo: row.UserNo,
				Masked: Mask(row.phone),
				Detail: "phone 不是规范形态（含 + 号/分隔符/全角），直接哈希会与登录时的规范化结果不一致"}
		default:
			want, hashErr := Hash(key, canonical)
			if hashErr != nil {
				return Report{}, hashErr
			}
			masked := Mask(canonical)
			if row.DeletedAt.Valid {
				// A soft-deleted row must have released the number; the remedy
				// is the #del# marker, written by a human — never by apply.
				if !strings.HasPrefix(row.HMAC, DuplicateMergePrefix) {
					anomaly = &Anomaly{Kind: AnomalyGhostHMAC, ID: row.ID, UserNo: row.UserNo,
						Masked: masked, Detail: ghostDetail(row.HMAC)}
				} else {
					report.SkippedGhosts++
				}
			} else if row.HMAC == "" {
				report.Pending++
			} else if row.HMAC != want {
				anomaly = &Anomaly{Kind: AnomalyStaleHMAC, ID: row.ID, UserNo: row.UserNo,
					Masked: masked,
					Detail: "phone_hmac 与 HMAC(phone) 不一致：人工改库 / 上一次用了错 key / 半途中断"}
			} else {
				report.Filled++
			}
			if anomaly == nil && !row.DeletedAt.Valid {
				g := byHash[want]
				if g == nil {
					g = &group{masked: masked}
					byHash[want] = g
					order = append(order, want)
				}
				g.rows = append(g.rows, row)
			}
		}
		if anomaly != nil {
			report.Anomalies = append(report.Anomalies, *anomaly)
		}
	}

	for _, hash := range order {
		g := byHash[hash]
		if len(g.rows) < 2 {
			continue
		}
		group := DuplicateGroup{Masked: g.masked, HMAC: hash, Rows: g.rows}
		sort.Slice(group.Rows, func(i, j int) bool { return group.Rows[i].ID < group.Rows[j].ID })
		report.Duplicates = append(report.Duplicates, group)
	}
	return report, nil
}

// ghostDetail is the one-line remedy for a soft-deleted row that still holds the
// number. Both detect and verify use it, so the two commands never describe the
// same defect in two different words.
func ghostDetail(hmac string) string {
	switch {
	case hmac == "":
		return "行已软删但 phone_hmac 为空，号段仍被幽灵行占用：需人工改写为 #del#{id}{unix_ms}"
	default:
		return "行已软删但 phone_hmac 不是释放标记，号段仍被幽灵行占用：需人工改写为 #del#{id}{unix_ms}"
	}
}

// Verify recomputes every row's hash and compares it with what is stored. It is
// the only way to detect plaintext/hash drift — SQL cannot compute an HMAC, so
// "the column looks populated" is not the same statement as "the value is
// right".
//
// Unlike Detect, a missing hash is an anomaly here: Verify runs after Apply, so
// PHONE_WITHOUT_HMAC means the backfill did not cover the row.
func Verify(ctx context.Context, db *sql.DB, key string) (Report, error) {
	if _, err := Hash(key, "probe"); err != nil {
		return Report{}, err
	}
	rows, err := scanRows(ctx, db,
		`SELECT id, user_no, COALESCE(phone, ''), COALESCE(phone_hmac, ''), status,
		        created_at, deleted_at, cancel_apply_at
		   FROM t_user
		  WHERE phone IS NOT NULL
		  ORDER BY id`)
	if err != nil {
		return Report{}, err
	}
	report := Report{Scanned: len(rows)}
	for _, row := range rows {
		if row.phone == "" {
			report.Anomalies = append(report.Anomalies, Anomaly{Kind: AnomalyEmptyPhone,
				ID: row.ID, UserNo: row.UserNo,
				Detail: "phone 是空串（应为 NULL）"})
			continue
		}
		canonical, ok := Canonical(row.phone)
		if !ok || canonical != row.phone {
			report.Anomalies = append(report.Anomalies, Anomaly{Kind: AnomalyNonCanonical,
				ID: row.ID, UserNo: row.UserNo, Masked: Mask(row.phone),
				Detail: "phone 不是规范形态"})
			continue
		}
		// A soft-deleted row is judged by one rule only: did it release the
		// number? Its correct value is the release marker, not HMAC(phone), so
		// neither comparing the hash nor reporting "no hash yet" describes what
		// has to happen. Both wrong shapes are GHOST_HMAC — the class the
		// operator already knows ("rewrite the marker") — instead of
		// PHONE_WITHOUT_HMAC, which reads like "the backfill has not reached
		// this row" and sends people to rerun apply for a row apply skips on
		// purpose.
		if row.DeletedAt.Valid {
			if strings.HasPrefix(row.HMAC, DuplicateMergePrefix) {
				report.Filled++
				continue
			}
			report.Anomalies = append(report.Anomalies, Anomaly{Kind: AnomalyGhostHMAC,
				ID: row.ID, UserNo: row.UserNo, Masked: Mask(canonical),
				Detail: ghostDetail(row.HMAC)})
			continue
		}
		if row.HMAC == "" {
			report.Anomalies = append(report.Anomalies, Anomaly{Kind: AnomalyPhoneWithoutHMAC,
				ID: row.ID, UserNo: row.UserNo, Masked: Mask(canonical),
				Detail: "phone 有值但 phone_hmac 为空：回填未覆盖"})
			report.Pending++
			continue
		}
		want, hashErr := Hash(key, canonical)
		if hashErr != nil {
			return Report{}, hashErr
		}
		if row.HMAC != want {
			report.Anomalies = append(report.Anomalies, Anomaly{Kind: AnomalyStaleHMAC,
				ID: row.ID, UserNo: row.UserNo, Masked: Mask(canonical),
				Detail: "phone_hmac 与 HMAC(phone) 不一致"})
			continue
		}
		report.Filled++
	}
	return report, nil
}

// Apply fills phone_hmac for every row that needs it.
//
// Safety properties, all of them structural rather than procedural:
//
//   - It runs Detect first and writes nothing when Detect is not clean, so
//     "somebody skipped the detect step" cannot produce a half-filled batch.
//   - Every UPDATE carries `AND phone_hmac IS NULL`, which makes a rerun resume
//     by itself and makes two concurrent applies harmless (the slower one
//     updates zero rows). No state file is needed.
//   - Every UPDATE is one row, keyed by primary key: single-row locks, no table
//     lock, no long transaction, and a failure names the row.
//   - A 1062 stops the run with the ids of both sides and writes nothing for
//     that row. Skipping it would leave two rows claiming one number, which is
//     the state the whole exercise is meant to end.
//   - Soft-deleted rows are left alone: their correct value is a release marker
//     (#del#...), not a live hash, and filling them would let a ghost row
//     occupy the number permanently.
func Apply(ctx context.Context, db *sql.DB, key string, opts Options) (ApplyReport, error) {
	if opts.Batch <= 0 {
		opts.Batch = 500
	}
	if opts.Sleep < 0 {
		opts.Sleep = 0
	}
	started := time.Now()
	report := ApplyReport{DryRun: !opts.Commit}

	gate, err := Detect(ctx, db, key)
	if err != nil {
		return report, err
	}
	if gate.Blocked() {
		// Not an error value: the caller maps this to exit 2/3 with the detail
		// in hand. Returning the gate report keeps the reason visible.
		return report, &GateBlocked{Report: gate}
	}

	fromID := opts.FromID
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		batch, err := scanRows(ctx, db,
			`SELECT id, user_no, COALESCE(phone, ''), COALESCE(phone_hmac, ''), status,
			        created_at, deleted_at, cancel_apply_at
			   FROM t_user
			  WHERE phone IS NOT NULL AND phone_hmac IS NULL AND id > ?
			  ORDER BY id
			  LIMIT ?`, fromID, opts.Batch)
		if err != nil {
			return report, err
		}
		if len(batch) == 0 {
			break
		}
		report.Batches++
		for _, row := range batch {
			fromID = row.ID
			report.Scanned++
			if row.DeletedAt.Valid {
				// See the doc comment: a deleted row's correct value is a
				// release marker, which only an operator may write.
				report.SkippedGhosts++
				continue
			}
			canonical, ok := Canonical(row.phone)
			if !ok || canonical != row.phone {
				// Detect already blocked on this, but the row could have been
				// edited in between; refuse rather than write a wrong hash.
				return report, fmt.Errorf("phonehmac: row id=%d has a non-canonical phone", row.ID)
			}
			want, hashErr := Hash(key, canonical)
			if hashErr != nil {
				return report, hashErr
			}
			if opts.MaxRows > 0 && int64(report.Filled+report.WouldFill) >= opts.MaxRows {
				return finish(report, started), nil
			}
			if !opts.Commit {
				report.WouldFill++
				continue
			}
			res, execErr := db.ExecContext(ctx,
				`UPDATE t_user SET phone_hmac = ? WHERE id = ? AND phone_hmac IS NULL`,
				want, row.ID)
			if execErr != nil {
				if isDuplicate(execErr) {
					holder, _ := hashHolder(ctx, db, want, row.ID)
					report.Conflict = &Conflict{RowID: row.ID, HolderID: holder, Masked: Mask(canonical)}
					return finish(report, started), report.Conflict
				}
				return finish(report, started), execErr
			}
			if n, rowsErr := res.RowsAffected(); rowsErr == nil && n == 0 {
				// Another process filled it between the SELECT and the UPDATE.
				// Not a failure: the gate did its job.
				continue
			}
			report.Filled++
		}
		writeProgress(opts.Out, report, started)
		if len(batch) < opts.Batch {
			break
		}
		if opts.Sleep > 0 {
			select {
			case <-ctx.Done():
				return report, ctx.Err()
			case <-time.After(opts.Sleep):
			}
		}
	}
	return finish(report, started), nil
}

// GateBlocked reports that Apply refused to write because Detect found duplicate
// numbers or anomalous rows. It carries the report so the caller can print the
// same detail Detect would have printed.
type GateBlocked struct {
	Report Report
}

func (g *GateBlocked) Error() string {
	return fmt.Sprintf("phonehmac: refusing to apply: duplicates=%d anomalies=%d",
		len(g.Report.Duplicates), len(g.Report.Anomalies))
}

// ExitCode is the exit code the gate wants the process to use.
func (g *GateBlocked) ExitCode() int { return g.Report.ExitCode() }

func finish(report ApplyReport, started time.Time) ApplyReport {
	report.Elapsed = time.Since(started)
	return report
}

func writeProgress(out io.Writer, report ApplyReport, started time.Time) {
	if out == nil {
		return
	}
	elapsed := time.Since(started)
	rate := 0.0
	if elapsed > 0 {
		rate = float64(report.Filled+report.WouldFill) / elapsed.Seconds()
	}
	verb := "filled"
	if report.DryRun {
		verb = "would_fill"
	}
	_, _ = fmt.Fprintf(out, "batch %d: scanned=%d %s=%d skipped_ghosts=%d elapsed=%s rate=%.0f/s\n",
		report.Batches, report.Scanned, verb, report.Filled+report.WouldFill,
		report.SkippedGhosts, elapsed.Truncate(time.Millisecond), rate)
}

// hashHolder finds the row that already owns hash, excluding self.
//
// It is used only to describe a conflict; a failure to find one is not an error,
// because the point is "there is a conflict" and a READ COMMITTED read after the
// failed UPDATE may legitimately see nothing.
func hashHolder(ctx context.Context, db *sql.DB, hash string, self int64) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		`SELECT id FROM t_user WHERE phone_hmac = ? AND id <> ? LIMIT 1`, hash, self).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func scanRows(ctx context.Context, db *sql.DB, query string, args ...any) ([]Row, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only
	out := make([]Row, 0, 64)
	for rows.Next() {
		var row Row
		if err := rows.Scan(&row.ID, &row.UserNo, &row.phone, &row.HMAC, &row.Status,
			&row.CreatedAt, &row.DeletedAt, &row.CancelApplyAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// isDuplicate reports MySQL 1062.
func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

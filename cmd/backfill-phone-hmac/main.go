// Command backfill-phone-hmac fills t_user.phone_hmac for historical rows.
//
// Usage:
//
//	backfill-phone-hmac detect [--json] [--show=20]
//	backfill-phone-hmac apply  [--dry-run|--commit] [--batch=500] [--sleep=50ms]
//	                           [--max-rows=0] [--from-id=0]
//	backfill-phone-hmac verify [--json]
//
// Environment: DB_DSN (same source as cmd/migrate), PHONE_HMAC_KEY (required).
//
// Exit codes — the runbook quotes them, so they are part of the interface:
//
//	0  clean
//	1  runtime error (including a missing or too-short PHONE_HMAC_KEY)
//	2  duplicate phone numbers found (blocking; a human must merge them)
//	3  anomalous rows found (blocking; NON_CANONICAL / EMPTY_PHONE /
//	   STALE_HMAC / GHOST_HMAC / PHONE_WITHOUT_HMAC)
//	4  apply hit 1062: another row already owns this number (blocking)
//
// Order is not advisory: detect (read-only) → human decision on what it found →
// apply --commit → verify. Apply runs detect itself and writes nothing when the
// gate is not clean, so skipping the first step cannot leave a half-filled
// table.
//
// Every phone number this command prints is masked (first three bytes, last
// two). These lines get pasted into tickets and runbooks; the plaintext of a
// handset never appears on stdout or stderr. For the same reason the JSON form
// carries the masked value and the short hash, never the raw column.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/db"
	"yunshu.dev/core/internal/platform/migrate"
	"yunshu.dev/core/internal/platform/phonehmac"
)

// commandTimeout bounds one run. A production backfill of a large table can take
// a while, but it must not be unbounded: a stuck connection should fail the job
// rather than hold a lock forever.
const commandTimeout = 2 * time.Hour

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is the whole command, factored out so a test can drive it without a
// process exit.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "用法: backfill-phone-hmac detect | apply | verify（--help 看参数）")
		return phonehmac.ExitRuntime
	}
	command := args[0]
	if command != "detect" && command != "apply" && command != "verify" {
		fmt.Fprintf(stderr, "未知命令 %q；用法: detect | apply | verify\n", command)
		return phonehmac.ExitRuntime
	}

	// The key is checked before the database is opened: without it the command
	// can only fail, and it must fail without touching a connection or a row.
	key := strings.TrimSpace(getenv(config.EnvPhoneHMACKey))
	if key == "" {
		key = strings.TrimSpace(getenv("YUNSHU_" + config.EnvPhoneHMACKey))
	}
	if len(key) < phonehmac.MinKeyLen {
		fmt.Fprintf(stderr, "%s 未配置或短于 %d 字节，拒绝回填（不会写入任何行）\n",
			config.EnvPhoneHMACKey, phonehmac.MinKeyLen)
		return phonehmac.ExitRuntime
	}

	dsn := strings.TrimSpace(getenv("DB_DSN"))
	if dsn == "" {
		fmt.Fprintln(stderr, "DB_DSN 未设置；请先加载 .env")
		return phonehmac.ExitRuntime
	}

	conn, err := db.OpenMain(migrate.NormalizeDSN(dsn), 2, 1)
	if err != nil {
		fmt.Fprintln(stderr, "open mysql:", err)
		return phonehmac.ExitRuntime
	}
	defer conn.Close() //nolint:errcheck // process exit

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if err := db.Ping(ctx, conn); err != nil {
		fmt.Fprintln(stderr, "ping mysql:", err)
		return phonehmac.ExitRuntime
	}

	switch command {
	case "detect":
		return runDetect(ctx, conn, key, args[1:], stdout, stderr)
	case "verify":
		return runVerify(ctx, conn, key, args[1:], stdout, stderr)
	default:
		return runApply(ctx, conn, key, args[1:], stdout, stderr)
	}
}

func runDetect(ctx context.Context, conn *sql.DB, key string, args []string,
	stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("detect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "按 JSON 输出（便于工单系统解析）")
	show := fs.Int("show", 20, "重复组最多打印几组明细；0 表示全部")
	if err := fs.Parse(args); err != nil {
		return phonehmac.ExitRuntime
	}
	report, err := phonehmac.Detect(ctx, conn, key)
	if err != nil {
		fmt.Fprintln(stderr, "detect:", err)
		return phonehmac.ExitRuntime
	}
	printReport(stdout, "detect", report, *show, *asJSON)
	return report.ExitCode()
}

func runVerify(ctx context.Context, conn *sql.DB, key string, args []string,
	stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "按 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return phonehmac.ExitRuntime
	}
	report, err := phonehmac.Verify(ctx, conn, key)
	if err != nil {
		fmt.Fprintln(stderr, "verify:", err)
		return phonehmac.ExitRuntime
	}
	printReport(stdout, "verify", report, 20, *asJSON)
	if report.Blocked() {
		fmt.Fprintln(stdout, "结论：存在差异，唯一性仍不完整；按 docs/ops-runbook.md 修复后重跑")
		return report.ExitCode()
	}
	fmt.Fprintln(stdout, "结论：0 差异（phone_hmac 与 HMAC(phone) 逐行一致）")
	return phonehmac.ExitOK
}

func runApply(ctx context.Context, conn *sql.DB, key string, args []string,
	stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "只统计将回填多少行，不写库（默认行为）")
	commit := fs.Bool("commit", false, "真正写库；不带本参数等同于 --dry-run")
	batch := fs.Int("batch", 500, "每批读取行数")
	sleep := fs.Duration("sleep", 50*time.Millisecond, "批间暂停（限速）")
	maxRows := fs.Int64("max-rows", 0, "本次最多回填多少行；0 表示不限")
	fromID := fs.Int64("from-id", 0, "跳过 id <= 该值的行")
	if err := fs.Parse(args); err != nil {
		return phonehmac.ExitRuntime
	}
	if *dryRun && *commit {
		fmt.Fprintln(stderr, "--dry-run 与 --commit 只能给一个")
		return phonehmac.ExitRuntime
	}
	// Default is a dry run: the first run on production should always show the
	// numbers before anything is written.
	opts := phonehmac.Options{
		Batch:   *batch,
		Sleep:   *sleep,
		MaxRows: *maxRows,
		FromID:  *fromID,
		Commit:  *commit && !*dryRun,
		Out:     stdout,
	}
	report, err := phonehmac.Apply(ctx, conn, key, opts)
	if err != nil {
		var blocked *phonehmac.GateBlocked
		var conflict *phonehmac.Conflict
		switch {
		case errors.As(err, &blocked):
			// The gate found work a human has to do first. Print the same detail
			// detect would print, then map to its exit code (2 or 3). Nothing
			// was written.
			fmt.Fprintln(stdout, "apply: 前置 detect 不干净，未写入任何行")
			printReport(stdout, "detect", blocked.Report, 20, false)
			return blocked.ExitCode()
		case errors.As(err, &conflict):
			fmt.Fprintf(stdout, "CONFLICT phone=%s row=%d holder=%d\n",
				conflict.Masked, conflict.RowID, conflict.HolderID)
			fmt.Fprintln(stdout, "apply: 撞到 uk_user_phone_hmac；合并这两行后再重跑（幂等，已写入的行无需清理）")
			return phonehmac.ExitConflict
		default:
			fmt.Fprintln(stderr, "apply:", err)
			return phonehmac.ExitRuntime
		}
	}
	if report.DryRun {
		fmt.Fprintf(stdout, "dry-run: 将回填 %d 行，跳过软删行 %d 行，批数 %d，耗时 %s\n",
			report.WouldFill, report.SkippedGhosts, report.Batches,
			report.Elapsed.Truncate(time.Millisecond))
		fmt.Fprintln(stdout, "确认无误后加 --commit 真正写入")
		return phonehmac.ExitOK
	}
	fmt.Fprintf(stdout, "apply: 已回填 %d 行，跳过软删行 %d 行，批数 %d，耗时 %s\n",
		report.Filled, report.SkippedGhosts, report.Batches,
		report.Elapsed.Truncate(time.Millisecond))
	fmt.Fprintln(stdout, "下一步：backfill-phone-hmac verify（必须 0 差异）")
	return phonehmac.ExitOK
}

// jsonReport is the machine-readable shape. Fields are exported and the hash is
// shortened: a ticket system needs to correlate two runs, not to store a
// pseudonymous identifier at full length.
type jsonReport struct {
	Command       string    `json:"command"`
	Scanned       int       `json:"scanned"`
	Pending       int       `json:"pending"`
	Filled        int       `json:"filled"`
	SkippedGhosts int       `json:"skipped_ghosts"`
	ExitCode      int       `json:"exit_code"`
	Duplicates    []dupRow  `json:"duplicates"`
	Anomalies     []anomRow `json:"anomalies"`
}

type dupRow struct {
	Phone string    `json:"phone"`
	HMAC  string    `json:"hmac_prefix"`
	Rows  []dupUser `json:"rows"`
}

type dupUser struct {
	ID            int64  `json:"id"`
	UserNo        string `json:"user_no"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	CancelApplyAt string `json:"cancel_apply_at,omitempty"`
}

type anomRow struct {
	Kind   string `json:"kind"`
	ID     int64  `json:"id"`
	UserNo string `json:"user_no"`
	Phone  string `json:"phone"`
	Detail string `json:"detail"`
}

// printReport renders a Detect or Verify report.
//
// Text is the operator-facing form: one block per duplicate group with both ids,
// statuses and timestamps, which is exactly what the merge decision needs. JSON
// exists so a ticket script does not have to parse prose.
func printReport(w io.Writer, label string, r phonehmac.Report, show int, asJSON bool) {
	if asJSON {
		payload := jsonReport{
			Command: label, Scanned: r.Scanned, Pending: r.Pending, Filled: r.Filled,
			SkippedGhosts: r.SkippedGhosts, ExitCode: r.ExitCode(),
			Duplicates: make([]dupRow, 0, len(r.Duplicates)),
			Anomalies:  make([]anomRow, 0, len(r.Anomalies)),
		}
		for _, group := range r.Duplicates {
			users := make([]dupUser, 0, len(group.Rows))
			for _, row := range group.Rows {
				u := dupUser{ID: row.ID, UserNo: row.UserNo, Status: row.Status,
					CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339)}
				if row.CancelApplyAt.Valid {
					u.CancelApplyAt = row.CancelApplyAt.Time.UTC().Format(time.RFC3339)
				}
				users = append(users, u)
			}
			payload.Duplicates = append(payload.Duplicates, dupRow{
				Phone: group.Masked, HMAC: shortHash(group.HMAC), Rows: users,
			})
		}
		for _, a := range r.Anomalies {
			payload.Anomalies = append(payload.Anomalies, anomRow{
				Kind: a.Kind, ID: a.ID, UserNo: a.UserNo, Phone: a.Masked, Detail: a.Detail,
			})
		}
		encoded, err := jsonx.Marshal(payload)
		if err != nil {
			fmt.Fprintln(w, "encode:", err)
			return
		}
		fmt.Fprintln(w, string(encoded))
		return
	}

	fmt.Fprintf(w, "%s: scanned=%d pending=%d filled=%d skipped_ghosts=%d\n",
		label, r.Scanned, r.Pending, r.Filled, r.SkippedGhosts)
	for i, group := range r.Duplicates {
		if show > 0 && i >= show {
			fmt.Fprintf(w, "  … 还有 %d 组重复未显示（--show=0 看全部）\n", len(r.Duplicates)-show)
			break
		}
		fmt.Fprintf(w, "DUP  phone=%s  hmac=%s…  rows=%d\n",
			group.Masked, shortHash(group.HMAC), len(group.Rows))
		for _, row := range group.Rows {
			extra := ""
			if row.CancelApplyAt.Valid {
				extra = fmt.Sprintf("  cancel_apply_at=%s", row.CancelApplyAt.Time.UTC().Format(time.RFC3339))
			}
			fmt.Fprintf(w, "     id=%d  user_no=%s  status=%s  created=%s%s\n",
				row.ID, row.UserNo, row.Status,
				row.CreatedAt.UTC().Format(time.RFC3339), extra)
		}
	}
	for _, a := range r.Anomalies {
		fmt.Fprintf(w, "%s  id=%d  user_no=%s  phone=%s  %s\n",
			a.Kind, a.ID, a.UserNo, a.Masked, a.Detail)
	}
	switch {
	case len(r.Duplicates) > 0:
		fmt.Fprintf(w, "结论：%d 组重复手机号，阻塞（退出码 %d）；按 docs/ops-runbook.md 逐组裁决后再 apply\n",
			len(r.Duplicates), phonehmac.ExitDuplicate)
	case len(r.Anomalies) > 0:
		fmt.Fprintf(w, "结论：%d 条异常，阻塞（退出码 %d）；逐条修复后重跑\n",
			len(r.Anomalies), phonehmac.ExitAnomaly)
	default:
		fmt.Fprintln(w, "结论：无重复、无异常（退出码 0）")
	}
}

// shortHash prints the first 6 hex characters: two operators can compare that,
// and a report does not need the full 64.
func shortHash(hash string) string {
	if len(hash) <= 6 {
		return hash
	}
	return hash[:6]
}

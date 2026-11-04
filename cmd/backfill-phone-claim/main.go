// Command backfill-phone-claim operates the phone ledger (t_phone_claim).
//
// Usage:
//
//	backfill-phone-claim detect  [--json] [--show=20]
//	backfill-phone-claim backfill [--dry-run|--commit] [--batch=500] [--sleep=50ms]
//	                              [--max-rows=0] [--from-id=0]
//	backfill-phone-claim release  [--dry-run|--commit] [--batch=500] [--sleep=50ms]
//	                              [--max-rows=0] [--from-id=0] [--uid=0]
//	backfill-phone-claim verify  [--json]
//	backfill-phone-claim lookup  --phone=13800138000 [--json]
//
// Environment: DB_DSN (same source as cmd/migrate). PHONE_HMAC_KEY is required
// only by `lookup`, which has to turn a number into its ledger key; the other
// verbs copy or clear a value the database already holds, so a missing phone-only
// secret must not stop a migration.
//
// Exit codes — the runbook quotes them, so they are part of the interface:
//
//	0  clean (or a dry run finished)
//	1  runtime error (missing DB_DSN, unparseable flags, --dry-run with --commit,
//	   missing PHONE_HMAC_KEY for lookup, any single row failing)
//	2  detect / verify found something blocking (anomalies, drift, over-release)
//	3  `release` started before `backfill` finished
//
// Order is not advisory. Releasing a number the ledger has never seen gives the
// next registration of that number a free trial, so `release` runs the pending
// check first and refuses with exit code 3.
//
// Every number this command prints is masked and every hash is shortened to six
// characters. These lines get pasted into tickets; the plaintext of a handset
// never appears on stdout or stderr.
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
	"yunshu.dev/core/internal/platform/phoneclaim"
	"yunshu.dev/core/internal/platform/phonehmac"
)

// commandTimeout bounds one run: a production backfill may take a while, but a
// stuck connection must fail the job instead of holding a lock forever.
const commandTimeout = 2 * time.Hour

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is the whole command, factored out so a test can drive it without a
// process exit.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "用法: backfill-phone-claim detect|backfill|release|verify|lookup（--help 看参数）")
		return phoneclaim.ExitRuntime
	}
	command := args[0]
	switch command {
	case "detect", "backfill", "release", "verify", "lookup":
	default:
		fmt.Fprintf(stderr, "未知命令 %q；用法: detect|backfill|release|verify|lookup\n", command)
		return phoneclaim.ExitRuntime
	}

	key := ""
	if command == "lookup" {
		key = strings.TrimSpace(getenv(config.EnvPhoneHMACKey))
		if key == "" {
			key = strings.TrimSpace(getenv("YUNSHU_" + config.EnvPhoneHMACKey))
		}
		if len(key) < phonehmac.MinKeyLen {
			fmt.Fprintf(stderr, "%s 未配置或短于 %d 字节，无法计算号码台账键\n",
				config.EnvPhoneHMACKey, phonehmac.MinKeyLen)
			return phoneclaim.ExitRuntime
		}
	}

	dsn := strings.TrimSpace(getenv("DB_DSN"))
	if dsn == "" {
		fmt.Fprintln(stderr, "DB_DSN 未设置；请先加载 .env")
		return phoneclaim.ExitRuntime
	}

	conn, err := db.OpenMain(migrate.NormalizeDSN(dsn), 2, 1)
	if err != nil {
		fmt.Fprintln(stderr, "open mysql:", err)
		return phoneclaim.ExitRuntime
	}
	defer conn.Close() //nolint:errcheck // process exit

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if err := db.Ping(ctx, conn); err != nil {
		fmt.Fprintln(stderr, "ping mysql:", err)
		return phoneclaim.ExitRuntime
	}

	switch command {
	case "detect":
		return runDetect(ctx, conn, args[1:], stdout, stderr)
	case "backfill":
		return runBackfill(ctx, conn, args[1:], stdout, stderr)
	case "release":
		return runRelease(ctx, conn, args[1:], stdout, stderr)
	case "verify":
		return runVerify(ctx, conn, args[1:], stdout, stderr)
	default:
		return runLookup(ctx, conn, key, args[1:], stdout, stderr)
	}
}

func runDetect(ctx context.Context, conn *sql.DB, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("detect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "按 JSON 输出（便于工单系统解析）")
	show := fs.Int("show", 20, "最多打印多少条异常明细；0 表示全部")
	if err := fs.Parse(args); err != nil {
		return phoneclaim.ExitRuntime
	}
	report, err := phoneclaim.Detect(ctx, conn)
	if err != nil {
		fmt.Fprintln(stderr, "detect:", err)
		return phoneclaim.ExitRuntime
	}
	if *asJSON {
		return writeJSON(stdout, stderr, report, report.ExitCode())
	}
	fmt.Fprintf(stdout, "扫描账号 %d；待回填台账 %d；待释放 %d；过度释放 %d；异常 %d；耗时 %s\n",
		report.Scanned, report.PendingBackfill, report.ReleaseCandidates,
		report.OverReleased, len(report.Anomalies), report.Elapsed.Round(time.Millisecond))
	printAnomalies(stdout, report.Anomalies, *show)
	if report.Blocking() {
		fmt.Fprintln(stdout, "结论：存在阻塞项（异常或过度释放），先人工裁决再执行 backfill/release")
	} else {
		fmt.Fprintln(stdout, "结论：干净")
	}
	return report.ExitCode()
}

func runBackfill(ctx context.Context, conn *sql.DB, args []string, stdout, stderr io.Writer) int {
	_, opts, code := newBatchFlags("backfill", args, stderr)
	if code != 0 {
		return code
	}
	opts.Out = stdout
	report, err := phoneclaim.Backfill(ctx, conn, opts)
	if err != nil {
		fmt.Fprintln(stderr, "backfill:", err)
		return phoneclaim.ExitRuntime
	}
	printBackfill(stdout, report, opts.Commit)
	if report.Failed > 0 {
		return phoneclaim.ExitRuntime
	}
	return phoneclaim.ExitOK
}

func runRelease(ctx context.Context, conn *sql.DB, args []string, stdout, stderr io.Writer) int {
	_, opts, code := newBatchFlags("release", args, stderr)
	if code != 0 {
		return code
	}
	opts.Out = stdout
	report, err := phoneclaim.Release(ctx, conn, opts)
	if err != nil {
		var order *phoneclaim.OrderViolation
		if errors.As(err, &order) {
			fmt.Fprintln(stderr, "release 拒绝执行：", order)
			return order.ExitCode()
		}
		fmt.Fprintln(stderr, "release:", err)
		return phoneclaim.ExitRuntime
	}
	printRelease(stdout, report, opts.Commit)
	if report.Failed > 0 {
		return phoneclaim.ExitRuntime
	}
	return phoneclaim.ExitOK
}

func runVerify(ctx context.Context, conn *sql.DB, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "按 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return phoneclaim.ExitRuntime
	}
	report, err := phoneclaim.Verify(ctx, conn)
	if err != nil {
		fmt.Fprintln(stderr, "verify:", err)
		return phoneclaim.ExitRuntime
	}
	if *asJSON {
		return writeJSON(stdout, stderr, report, report.ExitCode())
	}
	fmt.Fprintf(stdout, "① 未回填台账 %d；② SELF_CANCEL 未释放 %d；③ 过度释放 %d；④ 台账 hmac 异常 %d；耗时 %s\n",
		report.PendingBackfill, report.UnreleasedSelfCancel, report.OverReleased,
		report.BadHMAC, report.Elapsed.Round(time.Millisecond))
	if report.Blocking() {
		fmt.Fprintln(stdout, "结论：四条断言未全 0，检查报告")
	} else {
		fmt.Fprintln(stdout, "结论：四条断言全 0")
	}
	return report.ExitCode()
}

func runLookup(ctx context.Context, conn *sql.DB, key string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lookup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	phone := fs.String("phone", "", "要查询的手机号（原样，命令内部归一化）")
	asJSON := fs.Bool("json", false, "按 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return phoneclaim.ExitRuntime
	}
	if strings.TrimSpace(*phone) == "" {
		fmt.Fprintln(stderr, "lookup 需要 --phone=<号码>")
		return phoneclaim.ExitRuntime
	}
	report, err := phoneclaim.Lookup(ctx, conn, key, *phone)
	if err != nil {
		fmt.Fprintln(stderr, "lookup:", err)
		return phoneclaim.ExitRuntime
	}
	if *asJSON {
		return writeJSON(stdout, stderr, report, phoneclaim.ExitOK)
	}
	fmt.Fprintf(stdout, "号码 %s（hmac %s…）\n", report.PhoneMasked, report.HMACShort)
	if report.Found {
		claim := report.Claim
		fmt.Fprintf(stdout, "台账：首登 uid=%d @%s；最近认领 uid=%s；注销次数=%d；最近注销=%s；体验版发放=%s（uid=%s）\n",
			claim.FirstUserID, claim.CreatedAt.UTC().Format(time.RFC3339),
			nullInt(claim.LastClaimUserID), claim.CancelCount, nullTime(claim.LastCancelAt),
			nullTime(claim.TrialGrantedAt), nullInt(claim.LastTrialUserID))
	} else {
		fmt.Fprintln(stdout, "台账：无（该号码从未被认领，或回填未跑）")
	}
	if report.HolderUID.Valid {
		fmt.Fprintf(stdout, "当前持有：uid=%d status=%s phone=%s\n",
			report.HolderUID.Int64, report.HolderStatus, report.HolderMasked)
	} else {
		fmt.Fprintln(stdout, "当前持有：无（号码空闲，可被任意账号注册）")
	}
	return phoneclaim.ExitOK
}

// newBatchFlags builds the flag set backfill and release share, and parses it.
// A nil *flag.FlagSet means "already known to be wrong": the caller returns the
// accompanying exit code without touching the database.
func newBatchFlags(name string, args []string, stderr io.Writer) (*flag.FlagSet, phoneclaim.Options, int) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	commit := fs.Bool("commit", false, "真正写入（默认 dry-run，只报告）")
	dryRun := fs.Bool("dry-run", false, "只读演练（默认行为；与 --commit 同给视为参数错误）")
	batch := fs.Int("batch", 500, "每批行数")
	sleep := fs.Duration("sleep", 50*time.Millisecond, "批间停顿")
	maxRows := fs.Int64("max-rows", 0, "最多处理多少行；0 表示不限")
	fromID := fs.Int64("from-id", 0, "跳过 id ≤ 该值的行（人工已处理的区段）")
	uid := fs.Int64("uid", 0, "release 专用：只处理该账号（第三态补释放）")
	if err := fs.Parse(args); err != nil {
		return nil, phoneclaim.Options{}, phoneclaim.ExitRuntime
	}
	if *commit && *dryRun {
		fmt.Fprintln(stderr, "--commit 与 --dry-run 互斥：请二选一（默认即 dry-run）")
		return nil, phoneclaim.Options{}, phoneclaim.ExitRuntime
	}
	return fs, phoneclaim.Options{
		Batch:   *batch,
		Sleep:   *sleep,
		MaxRows: *maxRows,
		FromID:  *fromID,
		Commit:  *commit,
		UID:     *uid,
	}, 0
}

func printBackfill(stdout io.Writer, report phoneclaim.BackfillReport, commit bool) {
	mode := "dry-run"
	if commit {
		mode = "commit"
	}
	fmt.Fprintf(stdout, "backfill(%s)：扫描 %d；待回填 %d；已写入 %d；补齐窗口 %d；失败 %d；批次 %d；耗时 %s\n",
		mode, report.Scanned, report.Pending, report.Changed, report.Healed,
		report.Failed, report.Batches, report.Elapsed.Round(time.Millisecond))
	if !commit && report.Pending > 0 {
		fmt.Fprintln(stdout, "以上均为演练结果，未写入任何行；确认后加 --commit")
	}
}

func printRelease(stdout io.Writer, report phoneclaim.ReleaseReport, commit bool) {
	mode := "dry-run"
	if commit {
		mode = "commit"
	}
	fmt.Fprintf(stdout, "release(%s)：候选 %d；已释放 %d；跳过 %d；失败 %d；批次 %d；耗时 %s\n",
		mode, report.Candidates, report.Changed, report.Skipped, report.Failed,
		report.Batches, report.Elapsed.Round(time.Millisecond))
	if !commit && report.Candidates > 0 {
		fmt.Fprintln(stdout, "以上均为演练结果，未写入任何行；确认后加 --commit")
	}
}

func printAnomalies(stdout io.Writer, anomalies []phoneclaim.Anomaly, show int) {
	for i, a := range anomalies {
		if show > 0 && i >= show {
			fmt.Fprintf(stdout, "…… 其余 %d 条省略（--show=0 打印全部）\n", len(anomalies)-i)
			return
		}
		fmt.Fprintf(stdout, "  异常 %s uid=%d value=%s\n", a.Kind, a.UID, a.Masked)
	}
}

func writeJSON(stdout, stderr io.Writer, v any, exitCode int) int {
	raw, err := jsonx.Marshal(v)
	if err != nil {
		fmt.Fprintln(stderr, "marshal json:", err)
		return phoneclaim.ExitRuntime
	}
	fmt.Fprintln(stdout, string(raw))
	return exitCode
}

func nullInt(v sql.NullInt64) string {
	if !v.Valid {
		return "-"
	}
	return fmt.Sprintf("%d", v.Int64)
}

func nullTime(v sql.NullTime) string {
	if !v.Valid {
		return "-"
	}
	return v.Time.UTC().Format(time.RFC3339)
}

// Command migrate applies the explicit SQL migrations under
// internal/platform/migrate/sql/mysql.
//
// Usage:
//
//	migrate up          apply every pending migration in version order
//	migrate down N      roll back the newest N migrations (N > 0)
//	migrate down all    roll back every applied migration
//	migrate status      show every file and whether it is applied
//	migrate checksum --verify
//	                    check the embedded SQL bytes against checksums.golden
//	                    (no database needed; used inside the release image)
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/db"
	"yunshu.dev/core/internal/platform/migrate"
)

const commandTimeout = 10 * time.Minute

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("用法: migrate up | down N | down all | status | checksum --verify")
	}

	// checksum 在连库之前分流：镜像内的守卫（release.yml 的 Verify embedded
	// SQL checksums 一步）跑在没有数据库、没有 Go 工具链的运行时镜像里，只
	// 校验编译进二进制的 SQL 字节与同一份二进制里的金标清单。
	if os.Args[1] == "checksum" {
		return runChecksum(os.Args[2:])
	}

	dsn := strings.TrimSpace(os.Getenv("DB_DSN"))
	if dsn == "" {
		return fmt.Errorf("DB_DSN 未设置；请先加载 .env")
	}

	migrations, err := migrate.LoadEmbedded()
	if err != nil {
		return err
	}

	conn, err := db.OpenMain(migrate.NormalizeDSN(dsn), 2, 1)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	if err := db.Ping(ctx, conn); err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}

	runner := migrate.NewRunner(conn, migrations, clock.Real{}.Now)
	// 0019 是本地联调种子（占位渠道 / 模型）。生产与其他环境跳过并打 WARN，
	// 不报错中断：一个开发期种子不得让生产升级失败。
	if env := strings.TrimSpace(os.Getenv("ENV")); !strings.EqualFold(env, "local") {
		runner.Skip("0019", "本地联调种子只在 ENV=local 执行，当前 ENV="+env)
	}

	switch os.Args[1] {
	case "up":
		results, err := runner.Up(ctx)
		printResults(results)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Println("没有待执行的迁移")
		}
		return nil

	case "down":
		if len(os.Args) < 3 {
			return fmt.Errorf("用法: migrate down N | down all")
		}
		steps := 0
		if os.Args[2] != "all" {
			n, err := strconv.Atoi(os.Args[2])
			if err != nil || n <= 0 {
				return fmt.Errorf("down 参数必须是正整数或 all，收到 %q", os.Args[2])
			}
			steps = n
		}
		results, err := runner.Down(ctx, steps)
		printResults(results)
		return err

	case "status":
		statuses, err := runner.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-8s %-32s %-10s %s\n", "VERSION", "NAME", "STATE", "CHECKSUM")
		for _, s := range statuses {
			state := "PENDING"
			checksum := "-"
			switch {
			case s.Applied && !s.Success:
				state = "FAILED"
				checksum = "MATCH"
			case s.Applied && !s.ChecksumMatch:
				state = "CHECKSUM_MISMATCH"
				checksum = "MISMATCH"
			case s.Applied:
				state = "APPLIED"
				checksum = "MATCH"
			}
			fmt.Printf("%-8s %-32s %-10s %s\n", s.Version, s.Name, state, checksum)
		}
		return nil
	}

	return fmt.Errorf("未知命令 %q；用法: migrate up | down N | down all | status | checksum --verify", os.Args[1])
}

// runChecksum verifies the SQL bytes compiled into this binary against the
// checksum manifest compiled into the same binary.
//
// It touches no database on purpose: the release pipeline runs it inside the
// built image (deploy/Dockerfile.app ships binaries only), where the question
// is not "what does production have applied" but "did the build embed the SQL
// the manifest was signed for". A build-context difference — line endings, a
// platform-specific checkout — changes the embedded bytes and is caught here,
// instead of at the first `migrate up` against production.
func runChecksum(args []string) error {
	if len(args) != 1 || args[0] != "--verify" {
		return fmt.Errorf("用法: migrate checksum --verify")
	}
	checked, problems, err := migrate.VerifyEmbedded()
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		for _, problem := range problems {
			fmt.Fprintln(os.Stderr, "  "+problem)
		}
		return fmt.Errorf("镜像内 SQL 与 %s 不一致（%d 项）", migrate.GoldenFile, len(problems))
	}
	fmt.Printf("checksum OK: %d 个迁移与 %s 一致\n", checked, migrate.GoldenFile)
	return nil
}

func printResults(results []migrate.Result) {
	for _, r := range results {
		fmt.Printf("%s %-32s %-4s %dms\n", r.Version, r.Name, r.Direction, r.Duration.Milliseconds())
	}
}

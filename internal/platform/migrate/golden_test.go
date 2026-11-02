package migrate

// T21 A10 第二层：已交付迁移的字节清单。
//
// TestAppliedMigrationChecksumTamperIsRejected 证明引擎会拒绝“库里已有记录”的
// 篡改，但它用的是 fstest.MapFS 自造 fixture，改真实文件仍不会变红。生产上的
// 真实事故是：有人给已应用的 0003 加一行注释，本地和 CI 都绿，直到生产执行
// migrate up 才因为 checksum 不匹配炸掉。
//
// 本用例直接拿 embedded 的 up 文件与 checksums.golden 逐条比对，不维护任何
// 硬编码版本列表：新增迁移而忘了重生成清单同样失败，这是预期行为。

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const checksumGoldenFile = "checksums.golden"

// readChecksumGolden 读取磁盘上的清单并解析。
// 解析逻辑只保留一份（golden.go 的 ParseChecksumGolden），测试与镜像内的
// `migrate checksum --verify` 共用，否则两边对“清单是否损坏”的判断会漂移。
func readChecksumGolden(path string) (map[string]string, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("打开 %s 失败: %w", path, err)
	}
	defer f.Close()
	return ParseChecksumGolden(f, path)
}

func TestEmbeddedMigrationsMatchGoldenChecksums(t *testing.T) {
	migrations, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("加载 embedded 迁移失败: %v", err)
	}

	golden, order, err := readChecksumGolden(checksumGoldenFile)
	if err != nil {
		t.Fatalf("%v\n提示：运行 bash scripts/gen-migration-checksums.sh 重生成清单", err)
	}

	embedded := make(map[string]Migration, len(migrations))
	for _, m := range migrations {
		embedded[m.Version] = m
	}

	for _, m := range migrations {
		want, ok := golden[m.Version]
		if !ok {
			t.Errorf("迁移 %s（版本 %s）不在 %s 中。\n"+
				"新增迁移后必须显式重生成清单：bash scripts/gen-migration-checksums.sh",
				m.UpFile, m.Version, checksumGoldenFile)
			continue
		}
		if want != m.Checksum {
			t.Errorf("迁移文件被修改：%s（版本 %s）\n"+
				"  期望: %s\n"+
				"  实际: %s\n"+
				"已应用的迁移不得修改；如果确实需要改，请新增迁移并在 golden 里重生成（bash scripts/gen-migration-checksums.sh）",
				m.UpFile, m.Version, want, m.Checksum)
		}
	}

	for _, version := range order {
		if _, ok := embedded[version]; !ok {
			t.Errorf("%s 列出的版本 %s 在 embedded 迁移中不存在。\n"+
				"删除或改名迁移文件后也要重生成清单：bash scripts/gen-migration-checksums.sh",
				checksumGoldenFile, version)
		}
	}
}

// VerifyEmbedded 是镜像内的守卫（release.yml 在 Build base image 之后执行
// `migrate checksum --verify`），它只读编译进二进制的 SQL 与清单。这里必须证明
// 它对当前仓库是干净的：否则那一步会在每次发布时误报。
func TestVerifyEmbeddedReportsNoProblem(t *testing.T) {
	migrations, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("加载 embedded 迁移失败: %v", err)
	}
	checked, problems, err := VerifyEmbedded()
	if err != nil {
		t.Fatalf("VerifyEmbedded 失败: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("VerifyEmbedded 报告 %d 个问题，期望 0：%v", len(problems), problems)
	}
	if checked != len(migrations) {
		t.Fatalf("VerifyEmbedded 校验了 %d 个迁移，期望 %d", checked, len(migrations))
	}
}

// CompareChecksums 是镜像守卫的判定核心，用自造数据覆盖三类问题：清单缺行、
// 字节被改、清单多出已不存在的版本；干净输入必须零告警。
func TestCompareChecksumsReportsEveryKind(t *testing.T) {
	migrations := []Migration{
		{Version: "0001", UpFile: "0001_a.sql", Checksum: "11"},
		{Version: "0002", UpFile: "0002_b.sql", Checksum: "22"},
		{Version: "0003", UpFile: "0003_c.sql", Checksum: "33"},
	}
	clean := map[string]string{"0001": "11", "0002": "22", "0003": "33"}
	if problems := CompareChecksums(migrations, clean, []string{"0001", "0002", "0003"}); len(problems) != 0 {
		t.Fatalf("干净输入的告警 = %v，期望空", problems)
	}

	broken := map[string]string{"0001": "11", "0002": "XX", "0004": "44"}
	problems := CompareChecksums(migrations, broken, []string{"0001", "0002", "0004"})
	if len(problems) != 3 {
		t.Fatalf("告警数量 = %d，期望 3（0002 字节不符 / 0003 缺清单行 / 0004 多出）: %v",
			len(problems), problems)
	}
	for _, want := range []string{"0002", "0003", "0004"} {
		found := false
		for _, p := range problems {
			if strings.Contains(p, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("告警里没有覆盖版本 %s: %v", want, problems)
		}
	}
}

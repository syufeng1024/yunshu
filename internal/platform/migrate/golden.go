package migrate

import (
	"bufio"
	_ "embed"
	"fmt"
	"io"
	"sort"
	"strings"
)

// GoldenFile is the reviewed byte manifest for every delivered up file.
const GoldenFile = "checksums.golden"

// goldenBytes is the manifest compiled into the binary. Embedding it is what
// makes the check usable inside a built image: the runtime image has no source
// tree and no Go toolchain (deploy/Dockerfile.app), so a check that only reads
// checksums.golden from disk cannot run there, while the pair
// (SQL bytes, expected hashes) is exactly what the image carries.
//
//go:embed checksums.golden
var goldenBytes []byte

// ParseChecksumGolden parses "<version> <sha256>" lines, skipping comments and
// blank lines. source only appears in error messages, so the caller can name
// the file or the embedded copy.
//
// A duplicate version or an out-of-order line is treated as a corrupt manifest
// rather than a warning: both are produced by a bad regeneration and would
// otherwise hide a real mismatch behind a confusing diff.
func ParseChecksumGolden(r io.Reader, source string) (map[string]string, []string, error) {
	golden := make(map[string]string)
	var order []string
	sc := bufio.NewScanner(r)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, nil, fmt.Errorf("%s 第 %d 行格式不是 \"<version> <sha256>\": %q", source, lineNo, line)
		}
		version, checksum := fields[0], fields[1]
		if _, exists := golden[version]; exists {
			return nil, nil, fmt.Errorf("%s 第 %d 行版本 %s 重复", source, lineNo, version)
		}
		if len(version) != 4 || len(checksum) != 64 {
			return nil, nil, fmt.Errorf("%s 第 %d 行版本或 sha256 长度不合法: %q", source, lineNo, line)
		}
		if len(order) > 0 && version <= order[len(order)-1] {
			return nil, nil, fmt.Errorf("%s 第 %d 行版本 %s 未按升序排列（前一条 %s）",
				source, lineNo, version, order[len(order)-1])
		}
		golden[version] = checksum
		order = append(order, version)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("读取 %s 失败: %w", source, err)
	}
	if len(order) == 0 {
		return nil, nil, fmt.Errorf("%s 中没有任何清单行", source)
	}
	return golden, order, nil
}

// GoldenChecksums returns the manifest compiled into this binary.
func GoldenChecksums() (map[string]string, []string, error) {
	return ParseChecksumGolden(strings.NewReader(string(goldenBytes)), GoldenFile+"（嵌入副本）")
}

// CompareChecksums returns one problem per version whose embedded SQL bytes and
// manifest entry disagree, in version order. An empty result means the tree is
// intact.
//
// It is deliberately pure: the same function backs the unit test that guards
// the source tree and the subcommand that guards the built image, so the two
// can never disagree about what "intact" means.
func CompareChecksums(migrations []Migration, golden map[string]string, order []string) []string {
	var problems []string
	embedded := make(map[string]Migration, len(migrations))
	for _, m := range migrations {
		embedded[m.Version] = m
	}

	versions := make([]string, 0, len(migrations))
	for _, m := range migrations {
		versions = append(versions, m.Version)
	}
	sort.Strings(versions)

	for _, version := range versions {
		m := embedded[version]
		want, ok := golden[version]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"迁移 %s（版本 %s）不在 %s 中：新增迁移后必须重生成清单（bash scripts/gen-migration-checksums.sh）",
				m.UpFile, version, GoldenFile))
			continue
		}
		if want != m.Checksum {
			problems = append(problems, fmt.Sprintf(
				"迁移文件被修改：%s（版本 %s）期望 %s，实际 %s",
				m.UpFile, version, want, m.Checksum))
		}
	}
	for _, version := range order {
		if _, ok := embedded[version]; !ok {
			problems = append(problems, fmt.Sprintf(
				"%s 列出的版本 %s 在 embedded 迁移中不存在：删除或改名迁移文件后也要重生成清单",
				GoldenFile, version))
		}
	}
	return problems
}

// VerifyEmbedded compares the SQL bytes and the manifest that were compiled
// into this binary and returns how many migrations it checked.
//
// This is the image-level guard: the release pipeline runs it inside the built
// image, where the only SQL that exists is what the build embedded, so a
// build-context difference (line endings, platform-specific checkout) shows up
// here instead of at the first `migrate up` in production.
func VerifyEmbedded() (int, []string, error) {
	migrations, err := LoadEmbedded()
	if err != nil {
		return 0, nil, err
	}
	golden, order, err := GoldenChecksums()
	if err != nil {
		return 0, nil, err
	}
	return len(migrations), CompareChecksums(migrations, golden, order), nil
}

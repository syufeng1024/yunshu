package migrate

import (
	"strings"
	"testing"
	"testing/fstest"
)

// wantMigrationCount is the delivered T02 + T03 + 0020 + 0021 + 0022 + 0023 + 0024 + 0025 + 0026 + 0027 + 0028 set:
// 0001-0006 (6) + 0009 (1) + 0010-0019 (10) + 0020 (1) + 0021 (1) + 0022 (1) + 0023 (1) + 0024 (1) + 0025 (1) + 0026 (1) + 0027 (1) + 0028 (1) = 26.
// 0007 / 0008 are permanent gaps; 0022 carries t_request_body (T25), 0026 carries t_integration (T33),
// 0027 adds t_user.username (INC-admin-bootstrap), 0028 adds t_user.password_is_initial.
const wantMigrationCount = 26

func loadEmbedded(t *testing.T) []Migration {
	t.Helper()
	migrations, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	return migrations
}

// ① 版本号唯一。
func TestVersionsAreUnique(t *testing.T) {
	seen := make(map[string]string)
	for _, m := range loadEmbedded(t) {
		if previous, exists := seen[m.Version]; exists {
			t.Fatalf("版本号 %s 重复：%s 与 %s", m.Version, previous, m.UpFile)
		}
		seen[m.Version] = m.UpFile
	}
}

// ② 版本号跳号只允许出现在预留位：0006 → 0009（0007 / 0008）。0022 已由 T25
// 落盘，0021 → 0022 不再是跳号；下方保留原 0021 → 0023 分支，不删除既有断言。
func TestVersionGapsOnlyAtReservedNumbers(t *testing.T) {
	migrations := loadEmbedded(t)
	for i := 1; i < len(migrations); i++ {
		previous := versionNumber(t, migrations[i-1].Version)
		current := versionNumber(t, migrations[i].Version)
		if current != previous+1 {
			if previous == 6 && current == 9 {
				continue
			}
			if previous == 21 && current == 23 {
				continue
			}
			t.Fatalf("非法跳号：%s → %s；只有 0007 / 0008 与 0022 可以空缺", migrations[i-1].Version, migrations[i].Version)
		}
	}
}

// ③ 每个 up 都有配对的 down。
func TestEveryUpHasDown(t *testing.T) {
	migrations := loadEmbedded(t)
	for _, m := range migrations {
		if strings.TrimSpace(m.DownSQL) == "" {
			t.Fatalf("%s 的 down 脚本为空", m.UpFile)
		}
		if !strings.HasSuffix(m.DownFile, ".down.sql") {
			t.Fatalf("%s 的配对文件名不合法：%s", m.UpFile, m.DownFile)
		}
		if !strings.HasPrefix(m.DownFile, m.Version+"_") {
			t.Fatalf("%s 与 down 文件 %s 版本号不一致", m.UpFile, m.DownFile)
		}
	}
}

// ④ 文件内容一个字节变化即触发 checksum 不匹配。
func TestChecksumChangeDetected(t *testing.T) {
	migrations := loadEmbedded(t)
	m := migrations[0]

	applied := []Applied{{Version: m.Version, Name: m.Name, Checksum: m.Checksum, Success: true}}
	if err := Verify(applied, migrations); err != nil {
		t.Fatalf("未修改文件时 Verify 不应报错: %v", err)
	}

	changed := append([]byte(nil), []byte(m.UpSQL)...)
	changed = append(changed, []byte("\n-- one comment character\n")...)
	modified := append([]Migration(nil), migrations...)
	modified[0].Checksum = Checksum(changed)

	err := Verify(applied, modified)
	if err == nil {
		t.Fatal("修改文件后 Verify 必须报错")
	}
	if !strings.Contains(err.Error(), "checksum 不匹配") {
		t.Fatalf("错误文案未包含 checksum 不匹配: %v", err)
	}
}

// ⑤ 重复版本号在加载阶段即报错，文案包含两个占用者文件名。
func TestDuplicateVersionRejected(t *testing.T) {
	fsys := fstest.MapFS{
		"sql/mysql/0001_first.sql":       {Data: []byte("SELECT 1;")},
		"sql/mysql/0001_first.down.sql":  {Data: []byte("SELECT 1;")},
		"sql/mysql/0001_second.sql":      {Data: []byte("SELECT 2;")},
		"sql/mysql/0001_second.down.sql": {Data: []byte("SELECT 2;")},
	}

	_, err := Load(fsys, "sql/mysql")
	if err == nil {
		t.Fatal("重复版本号必须报错")
	}
	for _, name := range []string{"0001_first.sql", "0001_second.sql"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("错误文案未包含 %s: %v", name, err)
		}
	}
	if !strings.Contains(err.Error(), "重复占用") {
		t.Fatalf("错误文案未包含「重复占用」: %v", err)
	}
}

// ⑥ 迁移条数 == 文件数，且版本集合与交付清单一致。
func TestMigrationCountMatchesFiles(t *testing.T) {
	migrations := loadEmbedded(t)
	if len(migrations) != wantMigrationCount {
		t.Fatalf("迁移条数 = %d，期望 %d；新增迁移时必须同步本常量", len(migrations), wantMigrationCount)
	}

	files, err := fsGlobSQL()
	if err != nil {
		t.Fatalf("枚举嵌入文件失败: %v", err)
	}
	if len(files) != wantMigrationCount*2 {
		t.Fatalf("嵌入的 .sql 文件数 = %d，期望 %d（%d 个 up + %d 个 down）",
			len(files), wantMigrationCount*2, wantMigrationCount, wantMigrationCount)
	}

	want := []string{
		"0001", "0002", "0003", "0004", "0005", "0006", "0009",
		"0010", "0011", "0012", "0013", "0014", "0015", "0016", "0017", "0018", "0019", "0020",
		"0021", "0022", "0023", "0024", "0025", "0026", "0027", "0028",
	}
	if len(migrations) != len(want) {
		t.Fatalf("版本数量 = %d，期望 %d", len(migrations), len(want))
	}
	for i, version := range want {
		if migrations[i].Version != version {
			t.Fatalf("第 %d 个版本 = %s，期望 %s", i+1, migrations[i].Version, version)
		}
	}
}

func TestMissingDownRejected(t *testing.T) {
	fsys := fstest.MapFS{
		"sql/mysql/0001_only_up.sql": {Data: []byte("SELECT 1;")},
	}
	if _, err := Load(fsys, "sql/mysql"); err == nil {
		t.Fatal("缺少 down 文件必须报错")
	}
}

func versionNumber(t *testing.T, version string) int {
	t.Helper()
	if len(version) != 4 {
		t.Fatalf("版本号 %q 不是四位", version)
	}
	n := 0
	for _, r := range version {
		if r < '0' || r > '9' {
			t.Fatalf("版本号 %q 含非数字字符", version)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

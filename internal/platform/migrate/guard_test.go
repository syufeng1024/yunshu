package migrate

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 交付物清单逐表相加的 61 张业务表（0001–0006、0009、0010–0017、0021、0022、0025、0026、0029）。
//
// T02 建 47 张：任务卡 §1 写「46 张」，但卡内 §4 的 0002 组包含
// t_config_publish（TDS 定义在 §1.8，§1.2 的分层清单未收录它），逐表相加是
// 8 + 11 + 26 + 2 = 47。T03 再建 7 张（0012 / 0014 / 0015×3 / 0016 / 0017），
// T09 再建 2 张（0021），T24 段 2 再建 2 张（0025：t_balance_adjust /
// t_announcement_model），T25 再建 1 张（0022：t_request_body），T33 再建 1 张
// （0026：t_integration），INC-phone-claim 再建 1 张（0029：t_phone_claim），合计 61。
// 这里按逐表清单钉死，防止建表时漏建或多建；不靠删表凑数。
var wantTables = []string{
	// 0001 账号与凭据（8）
	"t_api_key",
	"t_api_key_audit",
	"t_api_key_ip_rule",
	"t_api_key_model",
	"t_device_fingerprint",
	"t_user",
	"t_verification_code",
	"t_verify_record",
	// 0002 配置驱动层（11）
	"t_channel",
	"t_config_item",
	"t_config_publish",
	"t_config_runtime",
	"t_model",
	"t_model_alias",
	"t_model_capability",
	"t_plan",
	"t_price_rule",
	"t_protocol",
	"t_route",
	// 0003 资金 / 计量 / 限流 / 指标 / 运营（26）
	"t_admin_audit_log",
	"t_announcement",
	"t_announcement_read",
	"t_credit_account",
	"t_credit_batch",
	"t_idempotency_record",
	"t_invoice_apply",
	"t_ledger_entry",
	"t_metric_channel_health",
	"t_metric_daily",
	"t_metric_minute",
	"t_metric_model_daily",
	"t_notification",
	"t_prehold_hold",
	"t_quota_apply",
	"t_rate_limit_policy",
	"t_recharge_order",
	"t_redeem_batch",
	"t_redeem_code",
	"t_refund_order",
	"t_risk_hit_log",
	"t_risk_rule",
	"t_settle_task",
	"t_upstream_attempt",
	"t_usage_record",
	"t_user_daily_quota",
	// 0006 价格分组（2）
	"t_price_group",
	"t_price_group_scope",
	// 0012 内容安全（1）
	"t_content_policy_hit",
	// 0014 欠费（1）
	"t_arrears",
	// 0015 工单 / 审核（3）
	"t_ticket",
	"t_ticket_reply",
	"t_review_task",
	// 0016 上游账单（1）
	"t_upstream_bill",
	// 0017 专属名单（1）
	"t_channel_dedicated_user",
	// 0021 选主租约 / 对账差异（2）
	"t_job_lock",
	"t_reconcile_diff",
	// 0022 请求正文（1）
	"t_request_body",
	// 0025 余额调整 / 公告模型关联（2）
	"t_balance_adjust",
	"t_announcement_model",
	// 0026 工具集成深链模板（1）
	"t_integration",
	// 0029 号码台账（1）
	"t_phone_claim",
}

var (
	sqlFileNameRE = regexp.MustCompile(`^\d{4}_[a-z0-9_]+(\.down)?\.sql$`)
	createTableRE = regexp.MustCompile(`(?m)^CREATE TABLE ([a-z0-9_]+)`)
	rateColumnRE  = regexp.MustCompile(`(?i)\b(decimal|float|double)\b`)
)

func fsGlobSQL() ([]string, error) {
	return fs.Glob(Embedded(), MySQLDir+"/*.sql")
}

func readEmbedded(t *testing.T, name string) string {
	t.Helper()
	body, err := fs.ReadFile(Embedded(), MySQLDir+"/"+name)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return string(body)
}

func upFileNames(t *testing.T) []string {
	t.Helper()
	names, err := fsGlobSQL()
	if err != nil {
		t.Fatalf("枚举 SQL 文件失败: %v", err)
	}
	var ups []string
	for _, name := range names {
		base := strings.TrimPrefix(name, MySQLDir+"/")
		if strings.HasSuffix(base, ".down.sql") {
			continue
		}
		ups = append(ups, base)
	}
	sort.Strings(ups)
	return ups
}

// 文件名必须是四位版本号 + 小写 snake_case；down 只能是 <up>.down.sql。
func TestFileNamesFollowConvention(t *testing.T) {
	names, err := fsGlobSQL()
	if err != nil {
		t.Fatalf("枚举 SQL 文件失败: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("sql/mysql 下没有 SQL 文件")
	}
	for _, name := range names {
		base := strings.TrimPrefix(name, MySQLDir+"/")
		if !sqlFileNameRE.MatchString(base) {
			t.Fatalf("文件名 %q 不符合 ^\\d{4}_[a-z0-9_]+(\\.down)?\\.sql$", base)
		}
	}
}

// 0007 / 0008 永久预留，任何文件名前缀都不得占用。
func TestReservedVersionsNotUsed(t *testing.T) {
	names, err := fsGlobSQL()
	if err != nil {
		t.Fatalf("枚举 SQL 文件失败: %v", err)
	}
	for _, name := range names {
		base := strings.TrimPrefix(name, MySQLDir+"/")
		if strings.HasPrefix(base, "0007_") || strings.HasPrefix(base, "0008_") {
			t.Fatalf("0007 / 0008 永久预留，不得创建迁移文件：%s", base)
		}
	}
}

// 时间列一律 datetime(3)；timestamp 在 2038 上限与隐式时区转换上都不可接受。
func TestNoTimestampKeyword(t *testing.T) {
	names, err := fsGlobSQL()
	if err != nil {
		t.Fatalf("枚举 SQL 文件失败: %v", err)
	}
	for _, name := range names {
		base := strings.TrimPrefix(name, MySQLDir+"/")
		if strings.Contains(strings.ToLower(readEmbedded(t, base)), "timestamp") {
			t.Fatalf("%s 出现 timestamp；时间列只能用 datetime(3)", base)
		}
	}
}

// 不建外键：一致性由应用层事务保证，避免迁移期锁表与级联删除。
func TestNoForeignKey(t *testing.T) {
	for _, name := range upFileNames(t) {
		if strings.Contains(strings.ToUpper(readEmbedded(t, name)), "FOREIGN KEY") {
			t.Fatalf("%s 出现 FOREIGN KEY；本项目不建外键", name)
		}
	}
}

// decimal / float / double 只允许出现在 t_metric_daily.success_rate 一行。
func TestDecimalOnlyForSuccessRate(t *testing.T) {
	hits := 0
	for _, name := range upFileNames(t) {
		for _, line := range strings.Split(readEmbedded(t, name), "\n") {
			if !rateColumnRE.MatchString(line) {
				continue
			}
			hits++
			if !strings.Contains(line, "success_rate") {
				t.Fatalf("%s 的比率列 %q 不在 success_rate 上；金额必须用整数微积分", name, strings.TrimSpace(line))
			}
		}
	}
	if hits != 1 {
		t.Fatalf("decimal / float / double 命中 %d 处，期望仅 success_rate 一处", hits)
	}
}

// 每张建表语句都必须显式声明库级排序规则，避免继承服务端默认值。
func TestEveryCreateTableDeclaresCollation(t *testing.T) {
	const wanted = "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs"
	for _, name := range upFileNames(t) {
		body := readEmbedded(t, name)
		creates := strings.Count(body, "CREATE TABLE ")
		collated := strings.Count(body, wanted)
		if creates != collated {
			t.Fatalf("%s：CREATE TABLE %d 处，声明 %s 的 %d 处", name, creates, wanted, collated)
		}
		if strings.Contains(body, "CHARSET=utf8mb4;") {
			t.Fatalf("%s 存在未声明 COLLATE 的建表语句", name)
		}
	}
}

// 建表集合必须与交付物清单逐表一致（59 张；0004–0006 的两张分组表在 0006）。
func TestCreatedTableSetMatchesDeliveredGroups(t *testing.T) {
	seen := make(map[string]string)
	for _, name := range upFileNames(t) {
		for _, match := range createTableRE.FindAllStringSubmatch(readEmbedded(t, name), -1) {
			if previous, exists := seen[match[1]]; exists {
				t.Fatalf("表 %s 在 %s 与 %s 重复创建", match[1], previous, name)
			}
			seen[match[1]] = name
		}
	}

	want := append([]string(nil), wantTables...)
	sort.Strings(want)
	got := make([]string, 0, len(seen))
	for table := range seen {
		got = append(got, table)
	}
	sort.Strings(got)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("建表集合与交付清单不一致\n实际 %d 张: %v\n期望 %d 张: %v",
			len(got), got, len(want), want)
	}
}

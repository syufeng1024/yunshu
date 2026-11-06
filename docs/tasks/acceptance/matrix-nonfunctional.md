# 非功能验收对照表（matrix-nonfunctional）

| 项 | 值 |
| :--- | :--- |
| 数据来源 | PRD §9.3–§9.6、§9.8–§9.11；T23 §3 第 5 项 |
| 生成日期 | 2026-10-29 |
| 测量环境 | 本机 Docker 栈：gatewayd `127.0.0.1:18080`、controld `127.0.0.1:18081`、web `127.0.0.1:8088`；业务库 `yunshu`（`yunshu-stack-mysql`）；测试库 `yunshu_test`（原生 MySQL `127.0.0.1:3306`，`scripts/test-env.sh` 解析）；Redis 容器与测试实例 `127.0.0.1:16379` |
| **版本注记（2026-10-30 补，不改上格原文）** | 上格「原生 MySQL `127.0.0.1:3306`」在本机实测为 **MySQL 8.4.11**，而 GR-01 把项目钉在 **8.0**（容器 `yunshu-mysql` = 8.0.46）。即 2026-10-29 本表的测量实例版本与项目钉版**不一致**。影响范围：① 本表依赖 Go 后端用例的 ✅ 项，已于 2026-10-30 在 8.0.46 上全量复跑通过（`go test ./...` = 45 包 ok / 0 fail，见 `BACKLOG.md` G18）；② 依赖前端用例的 ✅ 项（如 §4 F6-3 的 `KeySecretOnce.test.tsx`、`KeysPage.test.tsx`）已于 2026-10-30 全量复跑通过（`npx vitest run` = 48 文件 / 211 用例，exit 0）；注意该项在本机受限沙箱下默认会漏跑 2 个文件（见 `BACKLOG.md` G19），须把 `TMPDIR` 指到可写目录并核对 `Test Files` 数；③ ⚠️/❌ 项的探针口径见 §1–§2：探针链路虽经鉴权/预扣/限流（确实读写业务库与 Redis），但其判定口径是网关侧 HTTP 延迟与状态码，MySQL 8.0/8.4 的差异不改变 200/429/502 的判定，故这些结论不受版本影响。收口方式登记在 `BACKLOG.md` G18 |
| 探针账号与 Key | 控制台 `demo@yunshu.test`；延迟探针临时建 Key `latency-probe`（`id=3`，`sk-6EKEM…AEBW`），测完已走 `DELETE /api/console/keys/3` 软删，`{"id":3,"deleted":true}` |
| 上游现场 | demo 渠道 `base_url=https://local.invalid/v1`（保留域名，DNS 必然失败），因此网关请求全部止于 P15 |
| 结论口径 | T23 §4：有自动化用例且通过 = ✅；用例通过但覆盖不全或指标口径受限 = ⚠️；无证据、未执行、用例被跳过 = ❌。「未验证」不折算为通过 |

章节对应说明：PRD 的 SLO 定义在 §9.3–§9.5，**不在 §9.9–§9.11**；§9.9 是备份与灾难恢复，§9.10 是灰度与回滚，§9.11 是对账差异处置。本表第 1–3 类对应 §9.3–§9.5，第 5 类对应 §9.9；§9.10 无发布记录、§9.11 的 8 条对账断言无恢复后演练，两者已计入 `go-live.md` 门槛 4/7 与 `open-issues.md` OI-09，本表不重复计数。

## 1. SLO 指标

| 项 | 指标或要求 | 实测值或证据 | 结论 |
| :--- | :--- | :--- | :--- |
| 网关月可用性（§9.3） | 99.5%；分母 = 全部到达网关的请求，分子 = `Spec.Owner=platform` 白名单 8 码 | 未取得月度数据。`gw_requests_total` 带 `status` 与 `err_code` 标签（`internal/obs/metrics/recorder.go:161-171`），但仓库内**没有**按 `Owner=platform` 白名单聚合的计数器，也没有 Prometheus 告警规则 / 看板（`deploy/` 只有 compose 与 k8s 清单）；本轮 30 次探针全部是上游侧 502，不构成可用性样本 | ❌ 未验证 |
| 对内成功率（§5.1） | 分母 = 成功 + 失败，429 / 402 不计入 | 实现存在：`internal/control/usage/metrics.go:173`、`internal/control/ops/overview.go:94`；无生产流量，未取到数值，未验证与 §9.3 全量口径是否使用了两套独立计数器 | ⚠️ 有条件通过 |
| 业务拒绝率（§9.3 观测项） | 402 / 429 / 400 占全部请求比例，不设目标 | 本轮 30 次探针 0 条 402 / 429 / 400（全部 502），样本不构成拒绝率 | ❌ 未验证 |
| 上游依赖可用性（§9.3 单列） | 上游失败 ÷ 该渠道尝试数 | 指标 `gw_upstream_attempts_total{channel,outcome}` 已注册（`internal/obs/metrics/a8_test.go:15`）；无生产数据。本轮渠道 `https://local.invalid/v1` 全部 DNS 失败 | ❌ 未验证 |

## 2. 延迟

| 项 | 指标或要求 | 实测值或证据 | 结论 |
| :--- | :--- | :--- | :--- |
| 网关附加延迟（§9.4） | P99 ≤ 200ms；P99.9 ≤ 500ms；口径为「接收请求 → 向上游发出」 | 正式门禁是 `TestSoak`（`internal/load/soak_test.go:44`、`:301`，200 QPS × 10 分钟，P99 > 200ms 即 `t.Fatalf`），本轮**未跑**（`-tags soak`，默认 10 分钟）。替代测量：30 次 curl 到 DNS 失败为止，P50 **22.6ms**、P95 **30.4ms**、P99 **35.6ms**、max **41.7ms**，30/30 都是 502。该值是「接收 → DNS 失败 → 写错误」的**上界**，不等于「接收 → 发出上游」，且 n=30 不足以计算 P99.9 | ⚠️ 有条件通过 |
| 非流式总时长（§9.4） | 120s 硬上限，超时 504；单次尝试 50s | 配置默认值存在：`gw.upstream_timeout_ms=50000`、`gw.client_timeout_budget_ms=120000`（`docs/config-keys.md:41-42`）、`internal/dispatch/firstbyte.go:15-17`。仓库内 `grep -rn upstream_timeout --include=*_test.go` **无命中**，没有超时触发的端到端计时用例；本轮未构造慢上游 | ❌ 未验证 |
| 流式首字节 / 总时长（§9.4） | 首字节 90s（单次 30s + 缓冲 29.5s）；总时长 600s | `FirstByteTimeout` 默认 `30_000`（`internal/gw/server.go:146-147`）；无 SSE 端到端计时用例，本轮未测 | ❌ 未验证 |
| 控制台首屏可交互（§9.4） | 2.5s，3G Fast、冷缓存 | 仓库无 Lighthouse / Performance 基线脚本，本轮未测 | ❌ 未验证 |

测量命令与限制：

```bash
# 30 次串行探测；样本写入 /tmp/times.txt（本机临时目录，不随仓库交付）
KEY=sk-6EKEM57BRPMNBF5FJZVYZ96P2HYN75J1PW4X74KAQ0J4AEBW
for i in $(seq 1 30); do
  curl -s --max-time 70 -o /dev/null -X POST http://127.0.0.1:18080/v1/chat/completions \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d '{"model":"qwen-max","messages":[{"role":"user","content":"ping"}],"max_tokens":1}' \
    -w '%{time_total} %{http_code}\n'
done | tee /tmp/times.txt
sort -n /tmp/times.txt | awk '{a[NR]=$1*1000} END {n=NR; printf "n=%d min=%.1fms p50=%.1fms p95=%.1fms p99=%.1fms max=%.1fms\n", n,a[1],a[int((n+1)/2)],a[int(n*0.95)],a[int(n*0.99)],a[n]}'
```

单节点、单 Key、单模型、无并发，数值只能说明「本地链路没有明显的平台侧延迟」，不能替代生产 SLO。

## 3. 限流误杀

| 项 | 指标或要求 | 实测值或证据 | 结论 |
| :--- | :--- | :--- | :--- |
| 429 契约（§9.5 / G-10） | 429；`type=rate_limit_exceeded`、`code=rpm_limit_exceeded`；`limit=60`、`remaining=0`、`retry_after∈[1,60]`、`reset_at` 为整分；三个 `X-RateLimit-*` 头 | `bash scripts/test-gwt.sh` → `TestGWT_G10_429Contract` **PASS**（0.06s）。断言路径：前 60 次 `Allowed=true`，第 61 次 429；四维同时触顶取 `daily_quota_exceeded` 并在 message 列其余维度 | ✅ |
| 正常请求不被误杀 | 未超配置上限的请求必须放行 | 同用例前 60 次全部放行；本轮 30 次 curl 的 RPM 远低于上限，未出现 429，全部到达 P15 并返回 502 | ✅ |
| 误杀率（§9.5） | 月度 ≤ 0.1%，可接受上限 ≤ 0.5%；抽样重算 429 / 402 中本可放行的比例 | 无生产 429 / 402 样本，仓库内无 §9.5 的抽样对账作业；本轮构造不出误杀样本。已知误杀来源「预扣上限 ≤ 可用余额 × 50%」只在 G-02 断言预扣额，不构成误杀率测量 | ❌ 未验证 |

## 4. 安全基线

| 项 | 指标或要求 | 实测值或证据 | 结论 |
| :--- | :--- | :--- | :--- |
| 上游 Key 信封加密（§9.6 / F14-2） | 库内为 `v1:` 密文，接口只回掩码 | `go test ./internal/control/ops -run TestChannelKeyIsSealedAndNeverReturned` → **PASS**（0.06s）；输出 `SQL: SELECT HEX(LEFT(upstream_key_enc,3))=76313A LENGTH=67 upstream_key_mask=sk-****1234`（`internal/control/ops/channel_test.go:56`） | ✅ |
| 平台 Key 只完整展示一次（§9.6 / F6-3） | 创建响应是唯一明文出现点；详情 / 列表恒掩码 | `matrix-82.md` F6-3（第 36 行）已登记三个用例：`TestCreatePlaintextOnceAndDetailMasked`、`KeySecretOnce.test.tsx`、`KeysPage.test.tsx`，均于 2026-10-29 前通过；本轮未复跑前端用例 | ✅ |
| CSRF 双提交（`docs/control-api.md:351-377`） | 状态变更须 `X-CSRF-Token` == `csrf` cookie == HMAC(secret, sid) | `go test ./internal/control -run TestSessionMutationRequiresCSRF` → **PASS**（0.04s）；缺 token、错 token、他会话 token 均 `403 permission_denied`；登出后旧 token `401 session_expired`（`internal/control/server_authz_test.go:282`） | ✅ |
| 越权 403（22 页后端接口） | 运营接口对 PERSONAL 返回 403，匿名返回 401 | `matrix-pages.md` 2026-10-29 实测：8 个后台页面背后的 10 个 `/api/admin/*` 接口 demo 403 `permission_denied`、匿名 401；控制台接口匿名 401 | ✅ |
| 日志不落敏感字段（§9.6） | Authorization / 凭据不进日志行；正文默认不落库 | `go test ./internal/obs/log -run TestA10CredentialsNeverReachTheLine` → **PASS**（`internal/obs/log/record_test.go:208`）；`TestRedactCredentials`（`internal/obs/mask/mask_test.go:54`）；`log.body_mode` 默认 `meta_only`（`docs/config-keys.md:35`） | ✅ |
| 全站 HTTPS / TLS 1.2+（§9.6） | API 入口强制 HTTPS，TLS 1.2+ | 本机栈全部为 HTTP（`127.0.0.1:18080/18081/8088`），`deploy/nginx/` 未见 `listen 443 ssl` 或证书配置；仓库内无 TLS 强制验证记录 | ❌ 未验证 |

## 5. 备份恢复

| 项 | 指标或要求 | 实测值或证据 | 结论 |
| :--- | :--- | :--- | :--- |
| 备份 / 恢复脚本与 DR 计划（§9.9、T22 段 4） | `scripts/backup.sh`、`scripts/restore.sh`、`docs/dr-plan.md` | **2026-11-06 复核：三者均存在** —— `scripts/backup.sh`（12 KB）、`scripts/restore.sh`（13 KB）、`docs/dr-plan.md`（17 KB），均创建于 2026-10-29 14:30–14:40，晚于本表生成。已用它跑通一次完整演练：备份 `yunshu`（56562 B、sha256 `1bae3744…`、schema 0031、62 表）→ `restore.sh` 强制校验 sha256 通过 → 恢复到独立库 `yunshu_drill`，62 表、schema 0031、6 张关键表行数与 manifest 逐项相等，耗时 15s。记录见 `e2e-drill-2026-11-06.md` §2。**原文「三者均不存在」是本表 2026-10-29 生成时的状态，现已过期** | ✅ 通过（2026-11-06 重测） |
| RPO / RTO（§9.9） | RPO ≤ 5 分钟；RTO ≤ 1 小时；每日全量 + 每 5 分钟增量 | **2026-11-06 实测：全量恢复到独立库 15s，但不构成 RTO 证据** —— 数据量仅 55.7 KiB / 62 表 / 7 行业务行，生产量级下 `DROP+CREATE+INSERT` 的耗时曲线完全不同，15s 不可外推。RPO 侧仍无证据：仓库内没有 binlog / 增量备份机制，也没有「最近一次全量 + 增量重放」的演练。另缺异地副本投递与故障注入（主库宕机 / 磁盘满 / 备份中断） | ❌ 未验证 |
| 演练与抽检（§9.9） | 每季度全量恢复演练、每月可读性抽检、记录留存 365 天 | **2026-11-06：已完成一次人工全量恢复演练并留证**（`e2e-drill-2026-11-06.md` §2，含完整命令、真实输出、行数与 sha256 比对）。但「按季度 / 按月为制度」不成立：这是首次留档演练，无周期计划、无 365 天记录链；且本次只比对了 6 张关键表的行数，**没有做业务级「全量恢复后 0 差异」比对**（原 `go-live.md` 门槛 4 要求的口径） | ⚠️ 有条件通过（一次演练，非制度） |

> **版本注记（2026-11-06 补，不改上方原文事实）**：§5 的三行原结论来自 2026-10-29 的核查。第一行在生成当时成立——三个文件是同日 14:30 之后才创建的，晚于本表。本轮按原文逐项复核并实跑演练，故分别更新为 ✅ / ❌ / ⚠️；旧结论以「原文…已过期」的形式保留在单元格内，未删除。**RPO / RTO 仍未验证，不得因为「脚本能跑通」而判通过**——这正是本次演练最容易产生的误读。

## 6. 降级可解释

| 项 | 指标或要求 | 实测值或证据 | 结论 |
| :--- | :--- | :--- | :--- |
| 上游不可达（§9.8） | 明确拒绝并给可执行 hint，不静默变慢 | 实测 `POST /v1/chat/completions`（`model=qwen-max`，demo 渠道 `https://local.invalid/v1`）→ `502`、`code=upstream_dns_failure`、message「上游服务暂时不可用」、hint「携 request_id 联系客服。不要重试」、`retry_after_seconds=0`、`request_id` 非空；30/30 一致。规格见 `internal/apierr/codes.go:499-504` | ✅ |
| 无可用上游（§9.8 / P12） | 503 `no_available_upstream`，`retry_after_seconds=30` | 规格：`internal/apierr/codes.go:475-480`；用例：`TestCI_F14_4`（`internal/acceptance/ci14_test.go:379`）、`internal/gw/pipeline_test.go:123`、`internal/gw/server_extra_test.go:226`。本轮未构造「渠道全停用」现场，未取线上响应 | ⚠️ 有条件通过 |
| Redis / 配置降级（§9.8、§9.3） | RPM / TPM / 日额度 fail-close；配置中心不可达用本地快照；降级可观测 | 指标 `gw_ratelimit_degraded_total`、`gw_config_stale_seconds`、`gw_config_bootstrap_used` 已注册（`internal/obs/metrics/a8_test.go:18`、`:25-26`）；`auth_unavailable` 无自动化触发用例（`open-issues.md` OI-07）；本轮未做 Redis 停机演练 | ⚠️ 有条件通过 |

## 7. 日志与审计

| 项 | 指标或要求 | 实测值或证据 | 结论 |
| :--- | :--- | :--- | :--- |
| 资金写操作进 `t_admin_audit_log`（§9.11 / F20-3） | 审批 / 余额调整须留 `operator_id`、`before_json`、`after_json`，且余额调整后账本平衡 | `go test ./internal/control/review -run 'TestReviewApproveIsIdempotent\|TestBalanceAdjustApproveKeepsLedgerBalanced'` → **PASS**（0.09s / 0.08s）。A17 审计行 `before={"state":"PENDING","biz_type":"QUOTA_APPLY"}`、`after={"state":"APPROVED","biz_type":"QUOTA_APPLY"}`；A8 `balance=130000`、`SUM(ledger)=balance`、`ADJUST entries=2`。**抽样 2 条路径，未做全部资金接口的审计覆盖矩阵** | ✅ |
| 请求可定位（§3.7） | 每个响应带 `request_id`，日志可按其检索 | 抽查的 2 次探针响应均带 `request_id`，示例 `01a185cb-dce2-71dc-b385-747439db9de3`；`TestGWT_G10_429Contract` 断言 429 响应 `request_id` 非空。30 次探针只采集了耗时与状态码，未逐条校验响应体；日志落盘检索（Trace ID 串联）未验证 | ⚠️ 有条件通过 |

## 8. 数据保留

| 项 | 指标或要求 | 实测值或证据 | 结论 |
| :--- | :--- | :--- | :--- |
| 请求正文 180 天（§9.6 / F11-3） | `expires_at = created_at + log.retention_days`，到期批删 | `go test ./internal/jobs -run TestPurgeLogRetentionBoundaries` → **PASS**（1.74s）；输出 `body delete: t_request_body(type=range,key=idx_rb_expire,rows=1)`、`A11 body=0 meta=0 attempts=0 idem=0 batches=4 truncated=false`（`internal/jobs/purge_log_test.go:16`） | ✅ |
| 元数据 365 天（F11-3） | 元数据保留 365 天，与正文 180 天分开 | `go test ./internal/acceptance -run TestCI_F11_3` → **PASS**（0.14s，`internal/acceptance/ci14_test.go:299`） | ✅ |

## 汇总

| 结论 | 条数 | 条目 |
| :--- | ---: | :--- |
| ✅ | 12 | 429 契约、正常请求放行、上游 Key 密封、平台 Key 一次展示、CSRF、越权 403、日志脱敏、上游不可达提示、资金审计、正文 180 天、元数据 365 天、**备份 / 恢复脚本与 DR 计划（2026-11-06 重测）** |
| ⚠️ | 6 | 对内成功率、网关附加延迟、无可用上游、Redis / 配置降级、请求可定位、**演练与抽检（一次演练，非制度）** |
| ❌ | 9 | 月可用性、业务拒绝率、上游依赖可用性、非流式超时、流式超时、首屏 2.5s、误杀率、TLS、**RPO / RTO** |

## 本轮复跑记录

| 用例 / 命令 | 结果 | 耗时 |
| :--- | :--- | :--- |
| `bash scripts/test-gwt.sh` | 12/12 PASS | `ok 23.847s` |
| `TestGWT_G10_429Contract` | PASS | 0.06s |
| `TestCI_F11_3` | PASS | 0.14s |
| `TestPurgeLogRetentionBoundaries` | PASS | 1.74s |
| `TestChannelKeyIsSealedAndNeverReturned` | PASS | 0.06s |
| `TestSessionMutationRequiresCSRF` | PASS | 0.04s |
| `TestA10CredentialsNeverReachTheLine` | PASS | <0.01s |
| `go test ./internal/obs/metrics -run TestA8` | PASS | 0.018s |
| `TestReviewApproveIsIdempotent` / `TestBalanceAdjustApproveKeepsLedgerBalanced` | PASS | 0.09s / 0.08s |
| `scripts/backup.sh`（默认两库） | 备份成功；`yunshu` 56562 B / 62 表 / schema 0031；`yunshu_log` 0 表（触发新增 WARN） | 未计时 |
| `scripts/restore.sh --db yunshu_drill` | sha256 校验通过；62 表 / schema 0031 / 6 张关键表行数与 manifest 逐项相等 | 15s |
| `npx playwright test`（`web/e2e`，6 spec） | 22/22 PASS | `22 passed (31.2s)` |

复跑前置处置（只影响测试库；以下为 **2026-10-29 当时**的情形，该库现已改为容器 13306）：原生 MySQL `127.0.0.1:3306` 的 `yunshu_test.schema_migrations` 残留 `0022 success=0` 且 `t_request_body` 已建，导致所有引用该库的 TestMain 在迁移阶段直接退出。按 `open-issues.md` OI-10 的处置删行并 `DROP TABLE yunshu_test.t_request_body`，0022 重放成功；此后上述用例才可执行。

用例隔离缺陷（未改代码）：单独执行 `go test ./internal/acceptance -run TestGWT_G10_429Contract` 会因 `testRedis` 只在 `server(t)` 的 `sync.Once` 中初始化而在 `gwt_test.go:1015`（`ratelimit.New(testRedis, …)`）→ `redisx/client.go:96` 触发 nil panic。G-10 的可重复命令必须是完整的 `bash scripts/test-gwt.sh`，不能只跑单条。

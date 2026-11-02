# 上线前检查单（30 项，T22 交付物 16）

> **本文件是什么**：发版当天逐项打勾的**操作清单**。每项都给出可执行命令或可观察点，以及「不通过怎么办」。
> **本文件不是什么**：不是 Go/No-Go 判定依据。八条门槛的判定与证据在 `docs/tasks/delivery/go-live.md`（那张表回答「能不能上」，这张表回答「这一次按步骤做了没有」）。两者互不抄写：判定引用用例与演练报告，清单只给命令与勾。
>
> 用法：复制一份，把 `[ ]` 改成 `[x]` 并在行尾写执行人 + 时间。**任一项打不了勾就不发版**，不允许「先上再补」。

约定：`$TAG` 是本次发布 tag（如 `v1.2.3`）；命令都在仓库根目录执行。

---

## A 配置与密钥（1–6）

| # | 检查 | 怎么验 | 不通过怎么办 |
| :--- | :--- | :--- | :--- |
| 1 | 生产构建注入了 `VITE_API_BASE_URL`（RB-1 / OI-12） | `cd web && VITE_API_BASE_URL=https://api.<生产域名> npm run build && grep -rF "https://api.<生产域名>" dist/assets \| head` —— 必须**命中** | 构建参数缺失。补 CI/`deploy/.env.stack` 的注入后再构建；**不得**用回退 `window.location.origin` 的产物上线 |
| 2 | 用生产产物验证调试台「查看代码」（RB-2） | 用第 1 项的 `dist` 起静态服务 → 打开 `/console/playground` →「查看代码」：示例代码 base_url 必须是生产 API 域名 | 与第 1 项同因。若产物已含域名但页面仍回退，查 `web/src/pages/console/PlaygroundPage.tsx` 的取值顺序 |
| 3 | `.env.stack` 不进仓库 | `git ls-files \| grep -E '(^|/)\.env(\.stack)?$'` 必须**无输出**；`git check-ignore -v deploy/.env.stack` 必须有命中 | 有输出说明被误提交：立刻从索引移除并轮换其中全部密钥 |
| 4 | 内容安全外部适配器的数据处理协议（RB-3） | 若启用 `GW_POLICY_ENDPOINT`：确认供应商数据处理协议已签、隐私政策已写明「请求文本会发往该供应商」；未启用则**跳过本项**（关键词库仍生效） | 协议未签则**不许启用**该适配器：`GW_POLICY_ENDPOINT` 留空，仅用关键词库 |
| 5 | 策略适配器与上游凭据齐全（RB-4 / RB-5） | `docker run --rm --env-file deploy/.env.stack <镜像> /bin/sh -c 'env \| grep -cE "^(CHANNEL_KEY_KEK|DB_DSN|REDIS_ADDR)="'` → 期望 `3`；`grep -c "CHANNEL_KEY_KEK" <(ps -eo args)` → 期望 `0`（凭据不进 argv） | 缺一项就让容器起不来或加密失败：补齐 env 后重启；argv 命中说明启动脚本把密钥写进了命令行，改为只读 env |
| 6 | 配置键与种子一致 | `go run ./cmd/controld -h >/dev/null`（确认二进制可执行）后，`SELECT COUNT(*) FROM t_config_item WHERE enabled=1` 与 `docs/config-keys.md` 的键数对得上；`SELECT cfg_key FROM t_config_item WHERE cfg_namespace='unassigned'` 必须为空 | 键数不符：先 `diff` 键表与 `0018_seed_config.sql`，按 E-07 的 45 键口径补齐；命名空间为空说明有键写错前缀（E-08） |

## B 迁移与数据（7–12）

| # | 检查 | 怎么验 | 不通过怎么办 |
| :--- | :--- | :--- | :--- |
| 7 | 迁移已到最新（RB-6） | `go run ./cmd/migrate status` → 末行版本为 `0026`，且全部 `APPLIED` / `CHECKSUM MATCH` | 有 `PENDING` 先 `migrate up`；有 `CHECKSUM_MISMATCH` **停手**，按第 8 项处理 |
| 8 | 源码树迁移字节未被改动 | `go run ./cmd/migrate checksum --verify` → `checksum OK: N 个迁移与 checksums.golden 一致` | 有人改过已应用的迁移。按 `bash scripts/gen-migration-checksums.sh` 的说明新增迁移，**不要**改历史文件；已提交的改动要回退 |
| 9 | **镜像内**迁移字节一致（G1 守卫） | `docker run --rm --entrypoint /usr/local/bin/migrate "yunshu-app:${TAG}" checksum --verify` → 退出 0 | **本机踩坑**：Windows 的 Git Bash 会把 `/usr/local/bin/migrate` 改写成 `D:/…/usr/local/bin/migrate`，表现为 `exec: "...": no such file or directory` / 退出 127——加上 `MSYS_NO_PATHCONV=1` 前缀即可（CI 的 ubuntu runner 无此问题，`release.yml` 里不需要该变量）。若命令能跑但退出 1，说明构建上下文（行尾/平台）改了 embed 的 SQL 字节：重建镜像并查 `git config core.autocrlf`；**不得**跳过本项上线 |
| 10 | 行尾没有被工具批量改写（RB-6） | `bash scripts/check-eol.sh` → 退出 0 | 说明 `.gitattributes` 被改或某工具整文件换行：恢复文件后再提交，不要「顺手格式化」 |
| 11 | 迁移只前滚 | `git diff --name-only "$(git describe --tags --abbrev=0)..HEAD" \| grep 'migrate/sql/mysql'` 只能出现**新增**（`00NN_*.sql` + `.down.sql`），不得出现对既有迁移的修改 | 出现修改即违反 PRD §9.10「只前滚」：改为新增迁移，旧文件恢复原字节 |
| 12 | 备份可用（发版前最后一份） | `bash scripts/backup.sh` → 退出 0，manifest 的 `schema_version` 与第 7 项一致、`sha256` 与 dump 匹配 | 备份失败**不许发版**：先修备份链路（磁盘、权限、binlog 位点），再走第 7 项 |

## C 门禁与测试（13–18）

| # | 检查 | 怎么验 | 不通过怎么办 |
| :--- | :--- | :--- | :--- |
| 13 | 硬约束扫描 | `bash scripts/lint.sh` → `lint: all hard constraints hold` | 按报错定位（每条形如 `[N/M] 说明 + 文件:行`）；资金/编码类命中一律改代码，不放行 |
| 14 | 后端全量测试 | `. ./scripts/test-env.sh && go test ./... -count=1` → 无非 ok 行 | 先确认 MySQL 8.0@13306 与 Redis 已起（`scripts/test-env.sh` 会告警版本）；偶发失败重跑一次仍红则停发 |
| 15 | 前端门禁 | `cd web && npm run typecheck && npm test && npm run check:copy` → `Test Files` 数**等于** `find src -name '*.test.ts*' \| wc -l` | 文件数不等说明 vitest 漏收集（G19）：确认 `web/vitest.config.ts` 的 `cacheDir` 与临时目录固定仍在，再重跑 |
| 16 | 前端生产构建 | `cd web && npm run build` → 退出 0，`dist/assets` 有产物且体积与上次发布同量级 | 构建失败先看 `tsc --noEmit` 报错；体积异常增大查是否误打包测试夹具 |
| 17 | 验收用例（CI 门禁） | `. ./scripts/test-env.sh && go test ./internal/acceptance/ -run TestCI_ -count=1` → 全 PASS | 单条红按用例名定位；涉及账本断言的（F04/F11 系列）当天不许发 |
| 18 | 对账演练 | `bash scripts/recondrill.sh`（或按 `docs/dr-plan.md` 的演练库流程）→ `result=PASS total_diffs=0` | 有差异先按 R-01 定位是数据问题还是校验口径问题；**禁止**改历史数据让断言变绿 |

## D 可观测与告警（19–23）

| # | 检查 | 怎么验 | 不通过怎么办 |
| :--- | :--- | :--- | :--- |
| 19 | 三进程启动姿态与指标端口 | ① 启动日志里有 `ratelimit degrade mode=<姿态>`：**默认 `off`**（2026-11-02 需求方裁定暂缓上线配置，降级姿态保持 `off`；`concurrency_only` 的路径已由 G31 修通——副本数改为缓存、Redis 故障时本地满走 429、未满仍 503，见 errata E-38）。要改成 `concurrency_only` 是**上线配置阶段的产品决策**，与 OI-12 一并裁定（`go-live.md` §五），届时改一行常量即可、无需改代码；② `curl -s -H "Authorization: Bearer $METRICS_TOKEN" localhost:9090/metrics \| grep -c '^gw_'`（gatewayd）/ `:9091`（controld）/ `:9092`（jobsd）均 > 0 | ① 日志里没有这一行说明还是旧二进制；`GW_RATELIMIT_DEGRADE_MODE` 写了非法值会**启动失败**（这是设计，不是故障），按 `concurrency_only`\|`off` 改回；② 端口不通查 `METRICS_ADDR` 与网络策略，`gw_` 为 0 说明 registry 没挂上 |
| 20 | 告警规则可加载 | `promtool check rules internal/obs/alert/rules.yaml` → `SUCCESS: N rules found` | 规则语法错会让 Prometheus **整体拒绝**加载该文件（连带其他规则失效）；按报错修 YAML |
| 21 | 每条告警都指向 runbook | 打开 `internal/obs/alert/rules.yaml`：每条 `annotations.runbook` 指向 `docs/ops-runbook.md` 的**存在**章节（§3 / §7 / §8） | 指向不存在的章节等于没有处置流程：补 runbook 或改指向 |
| 22 | 限流拒绝率告警在真实计数器上可求值 | 网关有流量后：`curl -s :9090/metrics \| grep -E '^gw_ratelimit_rejected_total\{dim='` → 至少一个 dim 有值；再用 `internal/obs/alert/rules.yaml` 里 `RatelimitRejectRatioHigh` 的 expr 在 Prometheus 上点一次 **Query**，返回数值（0 也算通过） | 序列不存在说明该维度从未拒绝过（正常）；expr 报 `many-to-many` / 空向量说明规则写错或指标名改过——**不要**把这条改成"看到某条序列就通过"，那种验收会在没有流量时假过（本项上一版就踩过这个坑，见 errata E-37） |
| 23 | 工单 SLA 巡检有出口 | `curl -s -H "Authorization: Bearer $OPS_TOKEN" localhost:8080/api/admin/overview \| jq .alerts.sla_overdue` → 返回数字（无埋点项，靠巡检） | 接口 403 说明 token 缺 `admin.read`；数字持续 > 0 按 §8 的巡检项排班处理 |

## E 安全与合规（24–27）

| # | 检查 | 怎么验 | 不通过怎么办 |
| :--- | :--- | :--- | :--- |
| 24 | **内部 SLO 未出现在对外界面** | `grep -rnE "\bSLO\b\|99\.9\|P95\|RTO\|RPO" web/src/pages web/src/components web/src/content --include='*.tsx' --include='*.ts' \| grep -v '\.test\.'` → **无输出** | 命中即把内部目标写成了对外承诺（PRD §9.2 只承诺工单与故障通告）。删除或改为 PRD §9.2 的四项承诺口径 |
| 25 | 对外接口无内部指标字段 | `grep -rnE '"?(slo\|rto\|rpo\|p99)"?' internal/control/publicapi/` → 无输出 | 同上：公开接口不得回传内部目标值 |
| 26 | 密钥只展示一次、上游 Key 全掩码 | 控制台创建 Key 后刷新页面：明文不可再读；上游渠道详情里 `upstream_key_enc` 显示为掩码 | 明文可二次读取是 P0 安全缺陷：停发该版本，查 `internal/control/key` 与 `internal/control/ops/channel.go` 的展示逻辑 |
| 27 | 日志无正文、无明文密钥 | 抽查 `t_request_body`：`body_mode` 非 `opt_in` 时应为空；`grep -rF "sk-" <(docker logs --tail 2000 <gatewayd>)` → 无命中 | 出现明文：立刻关闭 `log.body_mode`，轮换受影响的 Key，并按 §8 记录事故 |

## F 灰度与回滚（28–30）

| # | 检查 | 怎么验 | 不通过怎么办 |
| :--- | :--- | :--- | :--- |
| 28 | 放量台阶就位 | 发布单写明台阶 10% → 30% → 100%（紧急修复至少 10% → 100%），每档**观察 ≥ 30 分钟且样本 ≥ 1,000 次** | 少一项即不许晋级：补观察时间或等样本，不得用「看起来没问题」代替 |
| 29 | 回滚脚本可用 | `bash scripts/rollback.sh --dry-run`（或按脚本 usage）→ 退出 0；镜像 `ghcr.io/<owner>/gatewayd:<上一版 tag>` 仍在 registry | 上一版镜像被清理 = 无回滚路径：先重新拉/重打上一版镜像并推到 registry，再发布 |
| 30 | 灰度记录留痕 | 发布结束后在发布单补：版本号、经过台阶、每档样本数与观察时长、三条判据（平台 5xx 比例 / 对账非零差异 / 平台白名单码是否从 0 变正）实测值、结论 | 没记等于没做灰度：本次发布视为未完成，下次发布前补记；判据命中却晋级按事故处理 |

---

## 与其他文档的关系

| 文档 | 回答的问题 |
| :--- | :--- |
| **本文件** | 这一次发版，逐项做了没有？（操作清单，30 项，含命令） |
| `docs/tasks/delivery/go-live.md` | 这款产品现在能不能上？（八条门槛的判定与证据） |
| `docs/tasks/BACKLOG.md` RB-1…RB-6 | 还有哪些**已知未做**的发布项（本清单 A/B 两节把其中 RB-1、RB-2、RB-3、RB-4、RB-5、RB-6 逐条落到可执行命令） |
| `docs/ops-runbook.md` | 上线后出事，谁按什么步骤处置 |
| `docs/degradation.md` | 降级时系统该放行还是拒绝，以及怎么对客户解释 |

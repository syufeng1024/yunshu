# Go / No-Go 判定

判定基准：T23 §5.6 的八条门槛，**全部满足才可 Go**。本页只引用已落盘证据；「未验证」一律记 `❌`，不折算为通过。

## 一、八条门槛逐条判定

| # | 门槛 | 判定 | 依据 |
| :--- | :--- | :--- | :--- |
| 1 | 82 项中 `❌ == 0`，`⚠️ ≤ 8` 且均已登记 | ✅ | `matrix-82.md`：`✅75 / ⚠️7 / ❌0`。T32（F14-6）、T33（F4-3）清掉两个 `❌`；T34 关闭 F2-1、F6-2、F8-1、F8-3、F20-4、F22-4、F22-6 七条 `⚠️`。剩余 7 条（F1-1、F2-2、F4-2、F10-3、F16-1、F16-3、F16-5）均已在 `deviations.md` 登记，且各有明确处置口径 |
| 2 | G-01–G-12 全 `✅` | ✅ | `matrix-gwt.md`：**12/12 ✅**。G-03/G-07 由 T35 按 ERR-03/ERR-04 勘误 PRD 后转 ✅；G-02/G-05/G-09 由 T37 按 ERR-09/ERR-10/ERR-11 裁定后转 ✅；G-12 由 T37 的 Playwright 用例（`web/e2e/pricing-price.spec.ts`）覆盖 `/pricing` DOM 后转 ✅（已做反证：改一位价格即红）。12/12 用例 PASS 且 12/12 断言与文档一致 |
| 3 | 65 码断言通过且枚举行数一致 | ✅ | **两个缺口已由 T41 / T42 关闭**：`matrix-codes.md` 现为 `✅61 + ⬛4 + 🟨0 = 65`、`⚠️ 0`。`duplicate_request_in_flight` 由 T41 补上生产者——P08 对**未带** `Idempotency-Key` 的请求按 `floor(now/window)` 合成键去重，流式命中/在途重复返回本码（用例 `TestAutoDedupStreamHitIs409`、`TestAutoDedupInFlightDuplicateIs409`；反证：窗口置 0 → 用例红）。`content_policy_violation` 由 T42 补上生产者——P06.5 关键词库 + 可配置外部适配器，命中写 `t_content_policy_hit` 并 403（用例 `TestContentPolicyHitIs403WithEvidenceAndNoMoney`、`TestT42Segment2ExternalBlockIsRecorded`；反证：Check 恒放行 → 命中用例红）。口径仍三方一致（65 + 11 == 76，OI-08 已关） |
| 4 | 8 条对账断言在演练中 0 差异 | ✅ | T40-b（`t_user_daily_quota` 生产者）/ T40-c（`t_upstream_bill` 导入端点）落地后，在重建的 `yunshu_drill`（61 表 / 迁移 0026）上于 2026-10-30 重跑 `recondrill -day 2026-10-25` → **`result=PASS total_diffs=0`**，R01–R09 全 0、退出码 0。三条此前空跑的检查现均有**非种子样本**且各自反证：**R05** `t_user_daily_quota` 1 行（`used_total_micro=4,300,000` == `SUM(consumed_micro)`），改成 5,000,000 → `delta=700000`、`result=FAIL`；**R09** `config_version=2` 的 usage 1 条（对应 `t_config_publish.version=2`），改成 9999 → `delta=9999`、`level=HIGH`、FAIL；**runExternal** `t_upstream_bill` 2 行 `MANUAL_UPLOAD`，改金额 → `delta=2000000`、FAIL。**限定（不得据此宣称生产数据已 0 差异）**：样本是 T40-b/T40-c 构造的 fixture（T36 的原始备份产物已丢失，`yunshu` 库亦未迁移），「0 差异」证明的是三条检查链路不再空跑且判定正确，不等价于生产库真实数据回放 |
| 5 | 14 条 CI 门禁全绿 | ✅ | 交付总监 2026-10-29 复跑：`go test ./internal/acceptance/ -run TestCI_ -count=1 -v` → **14/14 PASS**（F04_2/F06_7/F07_1/F07_4/F11_3/F14_2/F14_4/F14_5/F14_6/F16_4/F16_6/F19_1/F19_4/F20_3），`ok 4.498s`。此前记录的「卡在 0022 表已存在」是本机共享库 `yunshu_test` 的一次漂移，复跑即过，**不构成门禁失败**；`OI-10` 降级为环境清理项（非阻塞） |
| 6 | 无 P0 级开放问题 | ❌ | 本轮关闭：OI-03（T42，内容安全有真生产者）、OI-06（T39 段 1 逐码裁定：1 补生产者 + 4 登记不实现 + 2 转 T41/T42）、OI-07（T39 段 2 补 9 条自动化用例）、OI-08（T39 段 1 口径回写 65+11==76）、OI-15（T40-b / T40-c 补上两个生产者）、OI-16（T40-a）。**唯一剩余 P0 为 OI-12**：生产构建未注入 `VITE_API_BASE_URL`，调试台「查看代码」的 base_url 回退到 `window.location.origin`（`PlaygroundPage.tsx:174`），用户复制的示例代码域名错误。属**发布配置项、非代码缺陷**，已登记为上线 Blocking（见 §五）。其余未关项 OI-09（RPO/RTO 目标缺口）、OI-10（测试库漂移）、OI-11（G-13）、OI-13（套餐写入路径）均为**非阻塞** |
| 7 | 备份恢复演练通过 | ✅ | `dr-drill-2026-10-29.md`：`bash scripts/backup.sh` 退出 0（10.3s），manifest `schema_version=0026`、`sha256` 与 dump 一致（`3b261d6a…9358`）；连续两次恢复到 `yunshu_drill` 均退出 0（10.5s / 10.5s）；61 表、6 张关键表行数与 `yunshu` 逐表相等；`--db yunshu` 无 `--yes` 与 `--db yunshu_test` 均退出 1；口令未落 argv。残余：RPO ≤ 5 分钟未达标（无 binlog 归档），RTO 仅在 41 KB dump 上实测，见 `open-issues.md` OI-09 |
| 8 | 内部 SLO 未出现在任何对外页面 | ✅ | `grep -rnE "\bSLO\b\|99\.9\|P95\|RTO\|RPO" web/src/pages/site web/src/components/site web/src/content`（排除 `.test.`）无命中；公开接口 `internal/control/publicapi` 无 SLO / RTO / RPO 字段。唯一反向证据是 PRD §9.9–§9.11 明确「内部运维 SOP，不是对外承诺」 |

**通过数：7 / 8**（2026-10-30 复核：门槛 3 由 T41 / T42 补上两个码的生产者后转 `✅`；门槛 4 由 T40-b / T40-c 补上两个生产者、在演练库取得 R05 / R09 / `runExternal` 三处非种子样本并各自反证后转 `✅`；门槛 1 于 T34 收口、门槛 5 于 2026-10-29 复跑、门槛 7 于 T36 段 2 演练转 `✅`。**唯一未过的是门槛 6**，其剩余项 OI-12 属发布配置，不是代码缺陷）。

## 二、结论

**定位（2026-11-02 需求方裁定）：代码质量已符合上线条件；发布配置暂缓，不构成代码缺陷。**

- **代码质量 = 符合上线条件**：八条门槛中除门槛 6 外，其余 7 条（82 项 ❌0、G-01–G-12 全 ✅、65 码断言、对账 0 差异、14 条 CI 门禁、备份恢复演练、对外无 SLO）全部满足并留有落盘证据；G23（失败请求 usage 落库）与 G31（降级路径可达）已在本轮收口。代码侧不存在未关闭的 P0 缺陷。
- **上线 Blocking = 仅发布配置**：唯一未达标的门槛 6（OI-12 / B-1 / B-2）是「生产构建未注入 `VITE_API_BASE_URL`」，属发布配置项而非代码缺陷；需求方明确「暂时不进行上线配置」，故本项**按需求方决策暂缓**，不据此判定代码质量不合格。
- **状态口径**：在发布配置落地前，Go/No-Go 判定维持 **No-Go（7/8）**（T23 §5.6 要求八条全满足才可 Go，硬门槛不可因「暂缓」而绕过）；但这是「代码已就绪、等发布动作」的状态，而非「代码有缺陷待修」。上线触发条件见 §四：完成 B-1/B-2 即八条齐备，可升 Go。

逐条说明：

- 门槛 1 ✅：82 项 `✅75 / ⚠️7 / ❌0`，7 条 `⚠️` 均在 `deviations.md` 登记；
- 门槛 2 ✅：G-01–G-12 **12/12**，G-12 由 Playwright DOM 断言覆盖并做过反证；
- 门槛 3 ✅：65 码 `✅61 + ⬛4 + 🟨0`，最后两个无生产者码由 T41 / T42 补齐，均有用例与反证；
- 门槛 4 ✅：演练库九项检查 `total_diffs=0`，R05 / R09 / `runExternal` 三处均取得非种子样本且各自反证可检出。**限定**：样本为构造 fixture，非生产数据回放；
- 门槛 5 ✅：14 条 CI `TestCI_` 全绿（`ok 4.498s`）；
- 门槛 6 ❌：**仅剩 OI-12**——生产构建未注入 `VITE_API_BASE_URL`，调试台示例代码域名会回退到 `window.location.origin`；
- 门槛 7 ✅：备份/恢复演练通过（`dr-drill-2026-10-29.md`），RPO/RTO 目标缺口见 OI-09，不阻断本门槛；
- 门槛 8 ✅：对外页面与公开接口无 SLO / RTO / RPO 字样。

不把结论改成 Go 的理由只有一条：T23 §5.6 要求**八条全满足**才可 Go，而门槛 6 的 OI-12 尚未关闭。它是发布配置项而非代码缺陷——注入生产 `VITE_API_BASE_URL` 并跑一次生产构建验证即可关闭（见 §五），届时八条齐备。

## 三、剩余事项（按优先级）

八条门槛只剩门槛 6 的 OI-12；此前的 P0 清单（P0-1…P0-7）已由 T31–T42 全部关闭。

| 优先级 | 事项 | 关闭后达标 | 指向 |
| :--- | :--- | :--- | :--- |
| **上线 Blocking** | 生产构建注入 `VITE_API_BASE_URL`，并用生产产物验证调试台「查看代码」不再回退 `window.location.origin`（见 §四 B-1/B-2） | 门槛 6 → ✅，八条齐备 | OI-12 |
| P1 | 提额邮件通道（站内通知已达 PRD 最低要求） | 非门槛项 | OI-05 余项 |
| P1 | G-13 空白带：待需求方给 Chrome 版本 / 缩放 / 录屏，信息到位前不改源码 | 非门槛项 | OI-11 |
| P1 | OI-13 套餐配置写入路径：产品决定是否新增管理面；若不做，F16-5 套餐分支标注「不适用」 | 非门槛项 | OI-13 |
| P2 | OI-09 RPO ≤ 5 分钟 / 异地副本：binlog 每分钟归档 + XtraBackup 物理备份 | 不阻断门槛 7，但属生产可靠性缺口 | OI-09 |
| P2 | OI-10 重建 `yunshu_test` 并补远端 CI 运行记录 | 非门槛项 | OI-10 |

## 四、上线 Blocking 清单（发版前逐项打勾，缺一不得上线）

| # | 项 | 验收动作 | 现状 |
| :--- | :--- | :--- | :--- |
| B-1 | 生产构建注入 `VITE_API_BASE_URL` | 在 `deploy/.env.stack` 或 CI 构建参数注入生产 API 域名；构建后检查产物内 API base 不为空 | ❌ 未做（OI-12） |
| B-2 | 用**生产构建产物**验证调试台「查看代码」 | 打开 `/console/playground` →「查看代码」，确认示例代码域名为生产 API 域名，而非 `window.location.origin` | ❌ 未做（OI-12） |
| B-3 | 内容安全外部适配器的数据处理协议 | 若启用 `GW_POLICY_ENDPOINT`，须确认允许把用户请求文本发往该供应商并在隐私政策写明；未启用则关键词库仍生效，本项跳过 | ⬜ 待定（OI-13 类，产品/法务） |
| B-4 | `GW_POLICY_ENDPOINT` / `GW_POLICY_TOKEN` / `GW_POLICY_TIMEOUT_MS` | 未启用则留空（网关不构造适配器）；启用则注入且 token 不得进日志 | ⬜ 待定 |
| B-5 | `CHANNEL_KEY_KEK` 与 DB / Redis 凭据 | 由部署环境注入，不落仓库、不进 argv | ⬜ 待定 |
| B-6 | 迁移版本与行尾校验 | `migrate status` 到 0026、`checksums.golden` 与仓库一致、`bash scripts/check-eol.sh` 退出 0 | ⬜ 待定 |

> **B-1 / B-2 对应 OI-12，是门槛 6 的唯一阻塞项**；B-3…B-6 为企业发布必检项，不阻断 Go/No-Go 判定但发版前须逐项确认。

## 五、签署位

| 角色 | 结论 | 签署 | 日期 |
| :--- | :--- | :--- | :--- |
| 实现方（Claude Code） | 门槛 3 / 4 已补齐生产者（T41 / T42 / T40-b / T40-c） | — | 2026-10-30 |
| 交付总监 | **No-Go（7/8）**：仅剩门槛 6 的 OI-12，属发布配置 | 齐活林 | 2026-10-30 |
| 需求方 | **代码质量符合上线条件；发布配置暂缓（暂不做 OI-12 / B-1 / B-2）** | 需求方 | 2026-11-02 |

> 复核人若要把结论改为 Go，必须先复跑本页八条门槛并给出可核对证据（命令 + 输出 + 文件:行），不得只引用本页结论。
>
> **降级姿态决策（2026-11-02，随 G31 收口一并落定）**：`GW_RATELIMIT_DEGRADE_MODE` 保持默认 `off`，暂不改为 `concurrency_only`。理由：① 项目暂不上线，无需在上线前打开一个有局部可用性收益但会改变 `gw_ratelimit_degraded_total` 语义的开关；② `concurrency_only` 仅放宽并发一维，RPM/TPM/日额度仍 fail-close，收益本就局部；③ 是否打开是降级姿态的产品决策，留待上线配置阶段与 OI-12 一并裁定（见 `go-live-checklist.md` 第 19 项）。代码路径已修通（G31），届时改一行常量即可。

> 复核人若要把结论改为 Go，必须先复跑本页八条门槛并给出可核对证据（命令 + 输出 + 文件:行），不得只引用本页结论。

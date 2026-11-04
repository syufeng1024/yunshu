# 对账告警值班手册

`jobsd` 的 `daily_reconcile` 每日 03:00（UTC+8）跑前一自然日，差异落 `t_reconcile_diff`；P0 与 CRITICAL 告警落 `t_notification`。值班从告警接手到关闭差异的全部动作在下文，任何一步都不允许直接改余额或删流水。

## 1 告警入口与时限

| event_code | 触发判据（代码位置） | channel_mask | 时限 | 接手人 |
| --- | --- | --- | --- | --- |
| `RECONCILE_P0` | `runExternal`：单笔 `|delta_micro| > 1_000_000`（1 元）、当日 `sum_abs_delta_micro > 100_000_000`（100 元）、或 `MISSING_LOCAL` 笔数 > 0 | 7（站内信+邮件+短信） | **15 分钟**内电话确认接单 | 研发值班 + 财务 + SUPER_ADMIN |
| `RECONCILE_ALERT` | R-01/R-02/R-03 任一行，或 R-05 偏差 > 10,000 微积分 | 3（站内信+邮件） | 15 分钟（CRITICAL 即资损） | 研发值班 |

`channel_mask` 的 bit2 只发短信，仓库里没有外呼集成：**电话确认由值班手机/值班群完成**。15 分钟无人接单，由 SUPER_ADMIN 按值班表升级。HIGH/MEDIUM（R-04/R-06/R-07/R-08）只写 `ERROR`/`WARN` 日志，次日处理。

查看当前未关闭差异：

```sql
SELECT id, biz_date, diff_type, request_id, trade_no, local_value, upstream_value,
       delta_micro, match_mode, status, detected_at
  FROM t_reconcile_diff
 WHERE status IN ('OPEN','HANDLING')
 ORDER BY ABS(delta_micro) DESC, detected_at
 LIMIT 100;
```

## 2 接单：OPEN → HANDLING

拿到责任后立刻把行置为 `HANDLING`，防止第二个值班重复定位。仓库当前没有修改状态的管理接口（属 T13/T15），用受限运维账号执行：

```sql
UPDATE t_reconcile_diff
   SET status = 'HANDLING', updated_at = NOW(3)
 WHERE id = ? AND status = 'OPEN';
-- 期望 Rows matched: 1；0 行说明别人已接单或已关闭，停止操作
```

同时写审计（`t_admin_audit_log`，`module='FINANCE'`）：

```sql
INSERT INTO t_admin_audit_log
  (trace_id, operator_id, operator_role, module, action, target_type, target_id, reason, result, created_at)
VALUES (?, ?, 'OPS', 'FINANCE', 'UPDATE', 't_reconcile_diff', ?, '对账差异接单', 'OK', NOW(3));
```

## 3 R-01 六步处置

R-01 是「`t_credit_account.balance_micro` ≠ `SUM(t_ledger_entry.amount_total_micro)`」，没有容差。

**第 1 步：判定单点还是系统性**

```sql
-- Q1 近 1 小时 OPEN 的 R-01 差异涉及多少用户（subject 即 user_id）
SELECT COUNT(DISTINCT subject) AS users
  FROM t_reconcile_diff
 WHERE status = 'OPEN' AND diff_type = 'R01_BALANCE_MISMATCH'
   AND detected_at > NOW() - INTERVAL 1 HOUR;
-- Q2 差异方向：正数 = 余额少于流水（余额被少记），负数反之
SELECT user_id, balance_micro, ledger_sum FROM (
  SELECT a.user_id, a.balance_micro, SUM(l.amount_total_micro) AS ledger_sum
    FROM t_credit_account a JOIN t_ledger_entry l ON l.user_id = a.user_id
   WHERE a.user_id = ? GROUP BY a.user_id, a.balance_micro
) x;
```

**第 2 步：定位断点**。R-01 的 `subject` 是 `user_id`（十进制字符串），差异行本身不带断点位置，按流水逐条比对：

```sql
SELECT id, entry_no, balance_after_micro, next_before FROM (
  SELECT id, entry_no, balance_after_micro,
         LEAD(balance_before_micro) OVER (ORDER BY id) AS next_before
    FROM t_ledger_entry WHERE user_id = ?
) x
WHERE next_before IS NOT NULL AND next_before <> balance_after_micro;
```

**第 3 步：冻结范围 = 只冻结提现/退款，不冻结调用**。TDS §5.4 原文的「冻结消费写路径」已被需求方 2026-10-23 裁定作废（F-11）：差异不影响新请求的正确性，新预扣走 `t_credit_account` 的 CAS。V1.0 的提现/退款尚未实现（T13），上线后须在出口校验该用户近 7 天是否存在 `status='OPEN'` 的差异（TDS §8.3.2 方案②，零迁移）。

**第 4 步：核对成因**。按经验顺序排查，命中即停：

```sql
-- 悬挂预扣：过期仍 HELD 的行
SELECT COUNT(*) FROM t_prehold_hold WHERE user_id = ? AND state = 'HELD' AND expires_at < NOW();
-- 结算任务放弃：重试耗尽转 DEAD
SELECT COUNT(*) FROM t_settle_task WHERE user_id = ? AND state = 'DEAD';
-- 批次口径：remaining ≠ face - consumed（R-04 同源）
SELECT id, batch_no, remaining_micro, face_value_micro, consumed_micro
  FROM t_credit_batch WHERE user_id = ? AND remaining_micro <> face_value_micro - consumed_micro;
```

成因排序：① `t_settle_task` 转 `DEAD`；② 预扣后进程崩溃（半完成态，`prehold_release` 应已回收，未回收说明 K12 锁 `hold:recover:{request_id}` 仍被占用）；③ Redis 释放失败且 `pending_release` 缓冲溢出；④ 库里存在直接 `UPDATE t_credit_account` 的代码（`TestProductionSQLGuardRails` 每次 CI 扫描，仍有命中即先停发布）；⑤ 当日迁移漏执行。

**第 5 步：冲正**。只允许追加，不允许改历史：

- 入口 `recon.NewReverser(db, clock.Real{}).Reverse(ctx, recon.ReverseParams{EntryNo, Reason, OperatorID})`；
- `Reason` ≥ 10 字，入库为 `REVERSAL_OF:<原 entry_no>:<原因>`；
- 全额冲正，`idem_key='reversal:<原 entry_no>'` 由 `uk_le_idem` 保证一笔原流水只冲一次；第二次调用返回同一分录（`Idempotent=true`），不是错误；
- 先由财务/SUPER_ADMIN 写 `action='APPROVE'` 的审计（`after_json` 带 `approved_reversal_micro`），执行人不得与审批人相同；执行时再写 `action='ADJUST_BALANCE'`、`before_json` 带审批记录 id。

**第 6 步：复核后关闭**。`Reverse` 返回前已执行 `Verify`（R-01、R-03、冲正自洽三条），任一条失败会返回 `reversal committed but post-check failed`——此时**停止**，升级人工介入，不得再冲一笔。

## 4 HANDLING → CLOSED 手工流

只有同时满足以下四条才能关闭：

1. 第 3 步的 R-01 查询对该用户返回 0 行；
2. `Reverser.Verify(原 entry_no)` 无错；
3. 冲正流水 `idem_key='reversal:<原 entry_no>'` 存在且 `amount_total_micro = -原金额`；
4. 审批审计（`action='APPROVE'`）与执行审计（`action='ADJUST_BALANCE'`）都在，且 operator 不同。

```sql
UPDATE t_reconcile_diff
   SET status = 'CLOSED', updated_at = NOW(3)
 WHERE id = ? AND status = 'HANDLING';
```

`t_reconcile_diff` 没有 `note` / `resolved_by` / `resolution` 列（0021 未建），TDS §8.3.4 要求的处置说明写到 `t_admin_audit_log.reason` 与 `after_json`，`target_id` 填 diff 的 `id`，`reason` 写「根因 + 冲正 entry_no + 处置耗时」。

关闭不等于数据已修正：R-01 的修正是冲正分录，`status` 只表示差异已定位。下一次 03:00 重跑时，`Recorder` 对同 `diff_key`、同 `delta_micro` 的行保持 `CLOSED` 不重开；`delta_micro` 一旦变化会自动置回 `OPEN` 并刷新 `detected_at`——**这是差异复发的信号，不要当成误报**。

## 5 误报判定

| 现象 | 判据 | 动作 |
| --- | --- | --- |
| 当日 `t_upstream_bill` 无行 | `runExternal` 对 0 账单日直接返回，不产生 `MISSING_UPSTREAM` | 无需处置；**先确认当日账单已导入，否则 external 段无意义**——用 `POST /api/admin/upstream-bills/import`（`bill_adapter='MANUAL_UPLOAD'`）手工导入，未导入按 T08 渠道告警处理 |
| R-05 偏差 100~10,000 微积分 | 容忍带内（Redis 补偿失败、跨日结算写旧桶），HIGH 不告警 | 以 DB 为准重建当日计数：`SET rl:dq:{user_id}:{yyyymmdd} <db_value>`，次日重跑归零后关闭 |
| R-07 计数超阈值但删除任务已跑 | `t_idempotency_record` 中 `expires_at < NOW()` 的行确实在下降 | 关闭；若连续两日不降，按 `purge_log` 失败处理 |
| 同一差异在 `CLOSED` 后原样重复 | `delta_micro` 未变，状态仍是 `CLOSED` | 不是告警，忽略 |
| 差异由校验口径导致（阈值过紧、汇总键选错） | 手工 SQL 复算结果一致且无资金影响 | 关闭并在团队改校验代码或阈值；**禁止改历史数据来让断言变绿** |

误报与真实差异共用一张表，所以「删行」永远不是误报的处置方式；关闭后若 `delta_micro` 变化，行会重开。

## 6 禁止操作

| 禁止 | 原因 / 现有护栏 |
| --- | --- |
| `UPDATE t_credit_account SET balance_micro = ?` | 第二真相源；`TestProductionSQLGuardRails` 扫描全仓，命中即测试失败 |
| `UPDATE` / `DELETE t_ledger_entry` | append-only；同一测试断言 |
| `TRUNCATE` / `DROP PARTITION` 日志表 | 唯一键不含 `created_at`，分区会失去 `uk_ur_request` 全局唯一性；`purge_log` 用每批 5000 行的 DELETE |
| 手工 `INSERT INTO t_reconcile_diff` | 唯一生产者是 `recon.Recorder`，手插会算错 `diff_key=sha256(biz_date|diff_type|subject)` |
| 同一天并发跑两次 `daily_reconcile` | K21 `lock:recon:{yyyymmdd}` EX 7200 只放行一个副本，第二个 `Skipped=true` |
| 为「立即重跑」`DEL lock:recon:*` | 只读任务重跑不修数据，重复执行只会重复发通知；K21 过期（03:00 + 2h = 05:00）后再跑 |
| 用冻结 API 调用来「控制影响面」 | F-11 裁定：只冻结提现/退款 |

## 7 其余任务的失败处置

| 任务 | 失败日志 | 处置 |
| --- | --- | --- |
| `prehold_release`（60s） | `task_failed job=prehold_release` | 单轮失败不影响下一轮；连续 3 轮积压或 `hold:recover:{request_id}` 锁未释放，查 Redis 与 MySQL 连通性 |
| `expire_batch`（00:05） | `job_skipped_not_leader` / `task_failed` | 只处理已过期且 `remaining_micro > 0` 的 BONUS；重跑安全（`idem_key='expire:{batch_id}'`）。RECHARGE 批次永不选入 |
| `purge_log`（03:10） | `Truncated=true` | 达到 100 批上限，次日自动续删；不要手工 `TRUNCATE` |
| `purge_body_mode`（60s，leader） | `task_failed job=purge_body_mode` / 日志 `purge_body_mode_config_unreadable` / `purge_body_mode_audit_failed` | 「用户关闭即删」：`log.body_mode = meta_only` 时删光 `t_request_body`（不看 180 天）。`config_unreadable` = 读不到 `t_config_item`，**此时一行都没删**，先修配置面再等下一轮；`audit_failed` = 行已删但审计没写进去，先查 `t_admin_audit_log` 是否可写，并在**下一轮之前**手工补一条（动作名 `LOG_BODY_PURGED_ON_MODE_DISABLED`）。成功的删除只留一条审计，`result` 是 `n=<条数>`，删 0 行不写 |
| `rollup`（00:20） | `task_failed job=rollup` | 重跑收敛（四张表全 upsert）；`cost_configured=0` 是价格规则缺失，补 `t_price_rule` 后重跑 |
| `close_expired_order`（5min） | `task_failed job=close_expired_order` / 日志 `close_expired_order no_progress` | 只关 `state='PENDING' AND expire_at<=now` 的单；CAS 落空（已被回调支付或已关）不算失败。先看 `gw_order_auto_closed_total{reason="expire_at"}` 是否停涨，再看 `gw_order_auto_close_failed_total{stage}`：`scan`=扫描查询、`db`=逐单语句、`audit`=审计写入。`audit` 持续失败时单不会被关（状态变更随事务回滚），修好审计表后重跑即收敛，重复执行不产生第二条审计；确需人工兜底走 `POST /api/admin/orders/{id}/close`（`ORDER_CLOSED_BY_OPERATOR`），**该路由要 `fund.approve` 权限（财务角色，`internal/control/server.go` 挂的 `mw.PermFundApprove`）**，OPS 角色会被 403 |
| 任意任务 | `job_lock_held_by_other` | 另一副本持锁；租约 90s、每 20s 续约、剩余 < 30s 可接管。持续 5 分钟不接管说明持锁进程未退出，先确认再重启 |

## 8 容量与限流告警

`internal/obs/alert/rules.yaml` 里除对账（R-01）之外的告警都落到本节。每条都先判「要不要立刻动手」，再决定「动谁」。

| 告警 | 判据 | 动作 |
| --- | --- | --- |
| `RatelimitUnavailable`（P1） | `increase(gw_ratelimit_unavailable_total[5m]) > 0` | 每一次都是一批 503：查 Redis 连通性与 `maxmemory-policy`（被淘汰会让限流键凭空消失）。PRD §9.8 要求资金维度 fail-close，**不要为了「先恢复」临时改成放行**。Redis 恢复后自动退出降级（连续 3 次心跳 + 并发维度再等 `drain_window` 150s），见 `docs/degradation.md` §4 |
| `RatelimitRejectRatioHigh`（P1） | `sum(rate(gw_ratelimit_rejected_total[5m])) / sum(rate(gw_requests_total[5m])) > 0.005` 且样本 > 1000 | 拒绝率到 PRD §9.5 的可接受上限。先按 `dim` 拆分定位是哪一维在拒（`curl :9090/metrics \| grep gw_ratelimit_rejected_total`），再排除降级（`gw_ratelimit_degraded_total` 同维度在涨时属预期）。**注意口径**：这是「拒绝率」，不是 §9.5 的「误杀率」——本仓没有被拒请求的用量数据，误杀率无法计算（errata E-37），别用这个数对外声称误杀水平 |
| `RatelimitRejectRatioHighByDim`（P2） | 单维度拒绝率 `> 0.001`（= §9.5 月度目标 0.1%）且样本 > 1000 | 单一维度自己就吃掉整个月度目标额度。确认不是降级后，看该维度的阈值与套餐档是否匹配（`t_rate_limit_policy` 的 rpm/tpm/concurrency/daily_quota） |
| `SettleBacklogOverThreshold`（P1） | `max(gw_settle_backlog_tasks) > 4500` 持续 5 分钟 | 4500 是**提前预警**线：`settle.BacklogWarnThreshold = 4500`（= `settle.BacklogThreshold` 5000 的 90%）。越过 5000 后网关已开始拒绝新请求（`gw_backpressure_rejected_total{tier="backlog"}` 才动），那是滞后信号，不要等它。数值由 jobsd 每 30s 跑 `settle.PendingCountSQL` 采样，与背压判定同一条 SQL（`SELECT COUNT(*) FROM t_settle_task WHERE state='PENDING'`）。先确认 jobsd 的 P26 消费者是否存活，再确认结算是否整体变慢 |
| `PlatformErrorRateHigh`（P1） | 平台白名单 8 码占全部请求 > 1%（样本 > 100） | 分子按 PRD §9.3 的 8 条码算，**不要换成「全部 5xx」**——上游 5xx 属于「上游依赖可用性」，混进来会让归因失真。按 `err_code` 排序修占比最高的那条 |
| `GatewayLatencyP99High`（P2） | `histogram_quantile(0.99, …{stream="false"}) > 120` | 120s = `gw.client_timeout_budget_ms` 的种子值（`gw.DefaultClientBudgetMS`），**非流式**客户端超时预算；只统计非流式。先看 `gw_upstream_attempts_total` 的失败分类与 `gw_circuit_state`，再按 `channel_id` 判断是否单渠道劣化 |
| `GatewayStreamLatencyP99High`（P2） | `histogram_quantile(0.99, …{stream="true"}) > 540` | 540s = 600s 流式总预算（`gw.stream_total_ms`）的 90%，**不复用非流式的 120s**。P99 逼近上限说明有一批流正在接近「网关主动收尾」。先看 `gw_ttft_seconds`（P99 15s）区分首字节慢还是生成慢；单个长请求不是问题，P99 才是 |
| `CircuitBreakerOpen`（P2） | `gw_circuit_state == 1` 持续 2 分钟 | 单渠道 OPEN 看 `t_upstream_attempt` 的 `error_class`；全部渠道 OPEN 按「上游整体不可用」处理，**不要在网关侧反复重启**（重启不会恢复上游） |
| `OrderAutoCloseFailing`（P1） | `increase(gw_order_auto_close_failed_total[15m]) > 0` | 见 §7 的 `close_expired_order` 行 |
| `TicketSLAFirstResponseOverdue`（P1） | `max(gw_ticket_sla_overdue) > 0` | PRD §9.2 承诺首次响应 ≤ 4 小时，欠一张就已违约，所以阈值只能是 0。数值由 jobsd 每 30s 用 `ticket.FirstResponseOverdueSQL` 采样，与 `GET /api/admin/overview` 的 `alerts.sla_overdue` 同一条 SQL。按 `first_response_due_at` 升序认领（后台 `/admin/tickets` 的「SLA 超时」筛选）。**不要用自动关闭处置**——那会把「平台没回」记成「工单已了结」（PRD F21-2） |

**降级期间的统一口径**：先看 `gw_ratelimit_unavailable_total` 是否在涨（Redis 真不可用），再看 `gw_ratelimit_degraded_total{dim}` 是否同时在涨（并发维度确实退到了节点内计数）；两者的语义自 G29 起是分开的——`degraded` **只在因 Redis 失败而本地判定时**自增，`GW_RATELIMIT_DEGRADE_MODE=concurrency_only` 本身不会让它涨。然后再看 reject 比例是否被降级放大。降级是**预期行为**，不要当成故障处理；但 `concurrency` 降级意味着实际并发上限按副本数缩小了，如果同时在扩缩容，需要把「名义值 ÷ 副本数」算给业务方。

## 9 内置超级管理员（admin）

`controld` 启动时会把内置超管建成幂等的一步（`internal/control/bootstrap`）。它**只在控制面进程**执行，`gatewayd` / `jobsd` 完全不做；多副本同时启动时只有一个副本真的插入，其余两个拿到唯一键冲突后按「已存在」处理。

| 项 | 值 |
| :--- | :--- |
| 账号名 | `ADMIN_BOOTSTRAP_USERNAME`（默认 `admin`） |
| 初始口令 | `ADMIN_BOOTSTRAP_PASSWORD`（默认 `12345678`，**公开弱口令**） |
| 角色 / 状态 | `SUPER_ADMIN` / `NORMAL`；`nickname=超级管理员`；`email` 与 `phone` 均为 `NULL` |
| 幂等判据 | `t_user.uk_user_username`：已存在即跳过，**不改角色、不改状态、不重置口令** |

### 9.1 首次部署的凭据从哪来

1. 部署前把 `ADMIN_BOOTSTRAP_USERNAME` / `ADMIN_BOOTSTRAP_PASSWORD` 写进 env 文件（三个变量的清单见 `.env.example`；全栈容器化见 `deploy/README.md`）。口令长度必须 8..72 字节，不合法会让 `controld` **拒绝启动**。
2. 启动后检查 `controld` 日志：首次创建会出现**一条 WARN**「已创建内置超级管理员 username=admin（role=SUPER_ADMIN）；初始密码取 ADMIN_BOOTSTRAP_PASSWORD，请立即修改初始密码」。日志里不会出现口令本身、不会出现 bcrypt hash、不会出现 DSN。
3. 用该账号登录 `/admin`。**登录页要勾选用户协议**（对所有账号一致，不是超管特例）。
4. **尽快改密**（见 9.2）。系统**不强制**：登录后运营后台顶部会出现一条「当前账号仍在使用初始密码，请尽快修改」的提醒横幅（`password_warning=true`，只提示、不拦截任何接口），改密成功后自动消失。`docs/go-live-checklist.md` 仍把"已改密"作为上线自查项——那是流程要求，不是程序拦截。
5. **绑定登录邮箱**（`/console/account` 的「登录邮箱」卡片）。内置 `admin` 建号时 `email`/`phone` 均为 NULL，**这是超管唯一的 Web 侧自救入口**：绑定之后忘记口令可以自己走 `/reset-password` 重置，不必再落到「改环境变量重启」甚至改库。绑定入口只在 `email` 为空时开放；已绑定的更换需要提交工单（换绑等于更换账号接管主通道，需要额外身份核验）。上线检查单里这一项是**硬性**的（未绑定即视为未完成上线）。

### 9.2 改密与自助找回（正常路径）

**首选：自助找回（不需要任何人工介入）。** 已登录时先在 `/console/account` 绑定登录邮箱；
忘记口令时走登录页的「忘记密码」→ `/reset-password`：输入邮箱或手机号 → 收 6 位验证码 →
设新密码 → 回登录页用新密码登录。重置成功会吊销该 uid 的全部会话（含发起重置的那一个），
所以「必须再登一次」是正常的，不是故障。

| 前提 | 说明 |
| :--- | :--- |
| `AUTH_VERIFICATION_MODE=smtp`（或 `log`，仅开发） | `off` 的部署**没有任何**自助找回路径（缺验证码 = 缺身份证明），只能走 9.3 |
| 账号有可用渠道 | 邮箱（默认可用）；手机号需要 `AUTH_SMS_MODE=log|provider`，`off` 时接口返回 400 + 建议先绑定邮箱 |
| 账号 `status=NORMAL` | 已冻结/已禁用的账号**不会**收到重置码（冻结本身就是访问收回，不能靠找回绕过），接口仍返回 200 但什么都不发 |

**改密（知道旧口令时）。** 用会话调用 `POST /api/v1/auth/password`，body `{old_password, new_password}`，
需带 `X-CSRF-Token`（值取 `csrf` cookie）。成功后会吊销该 uid 的**全部**会话与 CSRF token，需要重新登录。

**兜底：9.3。** 只有在「渠道全丢且无法登录绑定邮箱」时才需要人工介入。

### 9.3 忘记密码：处置顺序（自助 → A 方案 → DBA 直改库）

「唯一超管忘记口令」按代价从低到高处置，**不要跳步**：

| 顺序 | 手段 | 可用条件 | 代价 |
| :--- | :--- | :--- | :--- |
| 1 | **自助找回**（`/reset-password`，见 9.2） | 该账号已绑定邮箱/手机号，且 `AUTH_VERIFICATION_MODE` 不是 `off` | 零人工 |
| 2 | **A 方案**：把 `ADMIN_BOOTSTRAP_USERNAME` 换成未占用的新用户名 → 重启 `controld` → 用新超管登录 → 自救 | 能改环境变量并重启 | 一次重启（引导幂等，会建出第二个 `SUPER_ADMIN`） |
| 3 | **DBA 直改库**（本节下半部分） | 上面两条都不可用，例如 `uk_user_username` 被软删行幽灵占用且又改不了环境变量 | 两人复核 + 手工清会话 + 手工补审计，**最后手段** |

> 本轮之后，Web 侧的自救路径已经闭环：超管在 `/console/account` 绑定登录邮箱，之后忘口令就走
> `/reset-password` 自助重置。**仍然没有**「渠道全丢（email/phone 均 NULL）且无法登录绑定」的 Web 侧
> 路径——那正是 A 方案要覆盖的场景。超管代重置接口（`POST /api/admin/users/{id}/password/reset`）
> 是下一轮的 P1-1，落地后第 3 行可以整段删除。

#### A 方案（首选的人工手段）

1. 选一个**未被占用**的用户名（`SELECT COUNT(*) FROM t_user WHERE username = '<新名>'` 必须为 0；
   注意软删行仍然占着 `uk_user_username`）。
2. 改 `ADMIN_BOOTSTRAP_USERNAME=<新名>`，同时确认 `ADMIN_BOOTSTRAP_PASSWORD` 是本次要用的口令、
   `ADMIN_BOOTSTRAP_DISABLED≠true`，然后重启 `controld`。
3. 启动日志出现 `admin bootstrap: outcome=created uid=<新 uid> username=<新名>`，说明第二个超管已建出。
   用新口令登录 `/admin`，立刻改密并**绑定登录邮箱**（9.1 第 5 步）。
4. 旧账号的处置：旧账号仍需旧口令才能改密，所以实际做法是由新超管在运营后台**新建**一个带邮箱的超管账号，
   把旧账号 `status=DISABLED`，走人员变更流程；或按业务需要保留旧账号并按第 3 行改库。

#### DBA 直改库（最后手段）

> 只有 A 方案也不可用时才走到这里。**四步都必须做**，少一步就是留后门。直改库写的口令是「别人给的」，
> 所以 `password_is_initial` **必须置 1**：否则「仍在使用初始密码」的横幅永远不会出现，接手的人不会知道
> 该立刻改密。

**执行前置条件（缺一不可）**

1. **确认这不是最后一个可用超管**。若结果 `0`，先不要改库——改完要么没人能进后台，要么口令只掌握在改库的人手里。此时正确做法是：先由另一次引导（把 `ADMIN_BOOTSTRAP_USERNAME` 换成新用户名）建出第二个超管，或接受“口令由单人掌握”的风险并记录在案。

   ```sql
   SELECT COUNT(*) AS other_super_admins
     FROM t_user
    WHERE role = 'SUPER_ADMIN' AND status = 'NORMAL' AND deleted_at IS NULL
      AND username <> 'admin';
   ```

2. **两人复核**：一人执行、一人在场核对 SQL 与目标 `id`；工单里记下两人工号。
3. 目标账号先用 `SELECT id, username, role, status FROM t_user WHERE username = 'admin'` 确认只有一行。
4. 任何自建连接/脚本的 DSN 必须带 `parseTime=true`：引导会读 `deleted_at` 到 `sql.NullTime`，驱动未开启 `parseTime` 时返回 `[]byte`，`Scan` 失败会让引导报错（`bootstrap: ...`）。症状是**进程照常启动、日志 ERROR、没有任何 WARN、`admin` 也登不上**（该错误走 `EnsureSuperAdmin` 的运行时错误分支，按设计不退出进程），不是崩溃重启。仓库的 `.env` / `.env.example` 已带该参数，只影响手工拼 DSN 的场景。

**生成 bcrypt cost 12 的 hash**（二选一，不要在命令行历史里留明文口令）

```bash
# 方式 A（推荐，仓库内一次性片段；口令从 stdin 读，不进 argv / history）
cd <仓库根目录>              # 必须在模块内，才用得到 go.mod 里的 golang.org/x/crypto
cat > /tmp/hashpw.go << 'EOF'
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	hash, err := bcrypt.GenerateFromPassword([]byte(strings.TrimRight(line, "\r\n")), 12)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(hash))
}
EOF
go run /tmp/hashpw.go        # 粘贴新口令后回车，输出 $2a$12$...；用完删掉该文件

# 方式 B（有 apache2-utils 时）
htpasswd -bnBC 12 "" '<新口令>' | tr -d ':\n'   # 输出 $2y$12$...，Go 的 bcrypt 同样可校验
```

**落库**

```sql
UPDATE t_user
   SET password_hash = '<上一步的 hash>',
       password_algo = 'bcrypt',
       password_updated_at = UTC_TIMESTAMP(3),
       -- 别人给的口令，语义上就是「初始口令」：置 1 才会出现「请尽快修改」的横幅。
       -- 用户自助重置/改密走接口，接口会把这一列置 0（见 docs/control-api.md）。
       password_is_initial = 1,
       updated_at = UTC_TIMESTAMP(3)
 WHERE username = 'admin' AND deleted_at IS NULL;
```

改完立刻验证：用新口令登录一次 `/admin`。**必须同时踢掉旧会话**——`POST /api/v1/auth/password` 改完库会主动吊销（K30 `sess:admin:uid:{uid}` → 逐个 DEL K20 `sess:admin:{sid}`），DBA 直改库绕过了这一步，**旧 cookie 仍然有效**，等于没改：

```bash
# 列出该 uid 的全部 sid，再逐个删除；最后删索引
redis-cli -h <host> -p <port> SMEMBERS "sess:admin:uid:<uid>"
redis-cli -h <host> -p <port> DEL "sess:admin:<sid>"     # 对每个 sid 执行
redis-cli -h <host> -p <port> DEL "sess:admin:uid:<uid>"
```

**补审计**（没有接口就必须手工留痕）

`action` 用 **`UPDATE`**，与 `POST /api/v1/auth/password` 成功路径写的行保持同一套词表（那边也是 `module='USER', action='UPDATE', result='OK'`）。`t_admin_audit_log.action` 是 `varchar(32)`，没有枚举约束，写 `RESET_PASSWORD` 不会报错，但会让“查所有改密审计”的 SQL 漏掉这一条。经手人与工单号进 `reason`，`after_json` 只放非敏感事实——**绝不能出现新口令或它的 hash**。

```sql
INSERT INTO t_admin_audit_log
  (trace_id, operator_id, operator_role, operator_ip, module, action,
   target_type, target_id, before_json, after_json, reason, result, created_at)
VALUES
  ('manual-reset', 0, 'SYSTEM', NULL, 'USER', 'UPDATE',
   't_user', '<uid>', NULL,
   JSON_OBJECT('password_reset', 'manual',
               'by', 'dba',
               'reviewed_by', '<复核人工号>',
               'sessions_revoked', true),
   'DBA 手工重置 admin 口令；工单 #<单号>；执行人 <姓名>；两人复核；已清全部会话；未记录口令与 hash',
   'OK', UTC_TIMESTAMP(3));
```

字段口径：`operator_id=0` + `operator_role='SYSTEM'` 是“没有具体操作人”的标记（引导程序用同一套），真实经手人写在 `reason` 里——**不要**把某个人的 `t_user.id` 填进 `operator_id`，那会让审计看起来像“该操作人自己改的”。注意 `target_id` 是 `varchar(64)`，与 `bigint` 的 `t_user.id` 比较时必须 `CAST(id AS CHAR)`，否则比较走隐式转换、可能漏行。

### 9.4 `ADMIN_BOOTSTRAP_DISABLED=true` 的适用场景

| 场景 | 为什么要关 |
| :--- | :--- |
| 存量环境已经有超管，且口令已改 | 让启动路径完全跳过这一步，日志里不再出现引导相关行 |
| 多套环境共用一个库 | 只有主环境该建号，其余环境关掉以免出现第二个同名超管 |
| 迁移 0027 尚未应用（引导只打 ERROR，进程照常启动） | 关掉引导可让日志里不再出现那条 ERROR；**随后必须补迁移并重新打开**，否则新环境永远没有超管 |
| 安全审计期间冻结账号创建 | 与变更窗口一致 |

### 9.5 引导失败的处置

引导的失败处置是**两档**：配置非法才拒绝启动；一切运行时 SQL 错误只打 ERROR、**进程继续运行**。理由：这是给存量生产进程加的一次性启动动作，把一个瞬时 DB 抖动或“迁移晚应用一个版本”放大成整台控制面起不来，代价远大于一行 ERROR 加 9.3 的手工兜底。缺 `username` 列只影响用户名登录一条路径，存量客户账号照旧用邮箱/手机号登录。

| 日志 | 含义 | 处置 |
| :--- | :--- | :--- |
| `admin bootstrap config: ... 必须是布尔值` | `ADMIN_BOOTSTRAP_DISABLED` 不是布尔 | 改成 `true`/`false`/`1`/`0`；**进程已退出** |
| `admin bootstrap config: ... 必须是合法用户名` | 用户名不合规（邮箱/手机号形态、全数字、含非法字符） | 改成 3..32 字节、字母开头的 `a-z0-9._-`；**进程已退出** |
| `admin bootstrap config: ... 不符合密码规则` | 口令不在 8..72 字节或纯空白 | 改口令；**进程已退出**。日志**不含口令值** |
| `admin bootstrap: ... t_user.username 不存在，迁移 0027 未应用；请先执行 make migrate（迁移 0027）后重启 controld` | MySQL 1054，schema 落后于二进制 | 进程**继续运行**（没有超管，也没关系旧账号可用）。执行 `make migrate` 后**重启一次**让引导补建 |
| `admin bootstrap failed: ...` | 其它运行时 DB 错误（连接、只读、死锁、审计表不可写） | 进程**继续运行**但可能没有超管。先修 DB/权限，再重启一次让引导重跑；期间可按 9.3 手工建号。这些 ERROR 是唯一信号，请配置告警抓 `stage=startup.bootstrap` |
| `admin bootstrap: username=admin 已存在（role=OPS status=DISABLED），未做任何修改`（INFO） | 库里已有同名活账号且**不是**预期的超管 | **设计如此**：引导不会修复。读一次 `t_user` 核对是人为降级还是历史遗留，按 R3 用另一个超管手工恢复。绝不要用“再启一次”去指望它自动提权 |
| `admin bootstrap: username=admin 被软删除账号占用，唯一键 uk_user_username 仍被占，需人工处理`（WARN） | 软删行仍占 `uk_user_username`（唯一键不含 `deleted_at`），而登录查询带 `deleted_at IS NULL` 永远查不到它 → 用户名被“幽灵占用” | **不要复活该行**（启动路径恢复已注销账号等于无授权提权）。按业务确认后二选一：把该行 `username` 置 NULL 释放唯一键，或换一个 `ADMIN_BOOTSTRAP_USERNAME` 再重启。处置后补一条审计说明 |

### 9.6 迁移时机与 `AUTO_MIGRATE`

「先迁移再启动」是硬要求：新二进制会查 `t_user.username` / `t_user.password_is_initial`，schema 落后一版时**所有登录**（不只用户名登录）都会 1054 → 500。差别只在“谁去跑迁移”。

| 场景 | 谁跑迁移 | 说明 |
| :--- | :--- | :--- |
| 本地 `ENV=dev` / `ENV=local` | `controld` 启动时自动跑 | 默认行为，`make run-ctl` 一条命令即可；待应用迁移会在启动日志留一行 `auto migrate: applied N pending migration(s)` |
| compose 全栈（`deploy/docker-compose.stack.yml`） | 独立 `migrate` 服务 | `gatewayd` / `controld` 都声明 `depends_on: migrate: service_completed_successfully`，不用人工介入 |
| k8s / 其它编排 | 独立 Job 或 initContainer | 应用账号通常没有 DDL 权限；`AUTO_MIGRATE` 默认 false 即为此 |
| 单机裸跑（无编排） | 可设 `AUTO_MIGRATE=true` | 让 `controld` 自己迁移，省掉独立步骤；需给应用账号 DDL 权限 |

- 多副本同时启动是安全的：启动迁移先取 MySQL 咨询锁 `yunshu_auto_migrate`（`GET_LOCK`，等待上限 300s）；没拿到锁的实例等前者跑完，随后发现无待应用迁移，不会重复执行 DDL。拿锁超时会**拒绝启动**并打 `auto migrate: 等待迁移锁超时`。
- 显式值永远覆盖默认：`AUTO_MIGRATE=true|false`（也接受 `1/0/yes/no/on/off`）。写成其它值会让 `controld` **拒绝启动**（`auto migrate config: ...`）——写错的布尔不能悄悄决定谁跑 DDL。
- 启动迁移失败即**拒绝启动**（与引导的两档处置不同）：引导失败只影响“有没有超管”，而 schema 落后会让每个登录 500，带着它对外服务只会放大故障。
- 手动兜底仍然是 `make migrate` / `go run ./cmd/migrate up`；`migrate status` 看末行是否为 `0028 APPLIED`。回滚顺序：先回滚二进制，再 `down 1`。
- 大表提醒：`0027` 的 `ADD UNIQUE KEY uk_user_username` 会重建索引，结束瞬间需短暂元数据锁（MDL）。生产建议低峰执行，并先确认没有长事务占着 `t_user`，否则迁移会排队等 MDL、表现为写入短暂卡住。

## 10 手机号 hmac（`t_user.phone_hmac`）回填与号段处置

### 10.1 这是什么、为什么需要人工跑

`t_user.phone_hmac` 是手机号唯一性（`uk_user_phone_hmac`）与体验版判重的取值列。`0011` 建了列与唯一键，但**存量行是 NULL**，NULL 不参与唯一键 → 唯一性对老数据无效。列有值之后仍然可能「有值但值错」（人工改库、上一次用了错 key、半途中断），而 SQL 算不了 HMAC，所以只能靠命令重算比对。

回填不是迁移：MySQL 没有 `HMAC()`，而 `migrate` 是「embedded SQL + checksum 金标」的只跑 SQL 的执行器。因此是独立命令 `cmd/backfill-phone-hmac`，与本仓其余写路径共用同一份 HMAC 实现（不会漂移）。

三个子命令与退出码（**退出码是流程的一部分，不要忽略**）：

| 退出码 | 含义 | 下一步 |
| :--- | :--- | :--- |
| `0` | 干净 | 继续 |
| `1` | 运行错误（含 `PHONE_HMAC_KEY` 未配/过短） | 修配置，不要重试 |
| `2` | 发现**重复手机号** | 停手，按 10.3 逐组裁决 |
| `3` | 发现**异常行**（`EMPTY_PHONE` / `NON_CANONICAL` / `STALE_HMAC` / `GHOST_HMAC` / `PHONE_WITHOUT_HMAC`） | 停手，按 10.3 分类处置 |
| `4` | `apply` 撞 `uk_user_phone_hmac`（另一行已占该号） | 按 10.3 合并后再重跑（幂等，无需清理已写行） |

### 10.2 四步流程（不可调换）

```bash
export DB_DSN='...'            # 与 migrate 同源
export PHONE_HMAC_KEY='...'    # 与 controld 同值；≥32 字节
make backfill-phone-hmac                       # ① detect（只读，退出码 2/3 时停手）
#   ② 按 10.3 逐组/逐条处置，直到 detect 退出 0
make backfill-phone-hmac ARGS="apply"          # ③a dry-run：只看数字，不写库
make backfill-phone-hmac ARGS="apply --commit" # ③b 真正写入（低峰、单副本）
make backfill-phone-hmac ARGS="verify"         # ④ 必须 0 差异
```

- `apply` 默认 **dry-run**，必须显式 `--commit` 才写；它会自己先跑一遍 `detect`，不干净就**一个字节都不写**（所以「有人跳过 detect 直接 apply」也不会烂尾）。
- 幂等：每条 `UPDATE` 带 `AND phone_hmac IS NULL`，连跑第二次 `filled=0`；中断后重跑自动续上，**不需要状态文件**。
- 限速：`--batch=500 --sleep=50ms`（可调）。逐行 `UPDATE ... WHERE id = ?` ⇒ 单行行锁、无表锁、无长事务。
- **同一时刻只跑一个 `apply`**；`detect` 只读，随时可跑。
- 发布与回填**不要同一分钟**：先在低峰把代码发布完（新写入开始生效），再回填存量。
- 每一步的手机号都**已掩码**（`861****00`），输出可以贴进工单。

#### 号码台账（`t_phone_claim`，0029 起）：backfill → release，顺序不可换

注销从 0029 起会**释放手机号**，释放的依据是号码台账 `t_phone_claim`。存量数据要分两步补齐，
两个动词**互不隐含**：

```bash
export DB_DSN='...'                                  # 只有 lookup 需要 PHONE_HMAC_KEY
make backfill-phone-claim                            # ① detect（只读；退出码 2 表示有异常/过度释放，停手）
make backfill-phone-claim ARGS="backfill"            # ②a dry-run：只报数字，不写库
make backfill-phone-claim ARGS="backfill --commit"   # ②b 只写 t_phone_claim，不动 t_user 一个字节
make backfill-phone-claim ARGS="release"             # ③a dry-run：列出待释放的 uid
make backfill-phone-claim ARGS="release --commit"    # ③b 逐行事务释放（低峰、单实例）
make backfill-phone-claim ARGS="verify"              # ④ 四条断言必须全 0
```

- **顺序禁令**：`release` 启动时先查「待回填行数」，>0 直接**退出码 3** 拒绝执行。反了会给老注销号
  白送一次体验版（号码已释放、台账里却没有 `trial_granted_at`）。`backfill` 可以先跑，无副作用。
- **发布顺序**：迁移 0029 → `backfill --commit` → `release --commit` → 发布代码。先发代码会有一段
  「号已释放、台账无窗口」的窗口，同样白送体验版。
- 只释放 `status='DISABLED' AND disable_source='SELF_CANCEL'`。`OPS_MANUAL` / `RISK_AUTO` /
  `FROZEN_OVERDRAFT` 的号码**必须保持占用**；`verify` 的 ③ 会报「过度释放」（退出码 2）。③ 的判定是
  **`phone_hmac` 为空但 `phone` 明文还在**：这两列同时为 NULL 时无法区分「从未有过号码」（每个被运营封禁的
  邮箱账号都长这样）与「号码被释放过」，对 both-NULL 计数会让本断言在任何有邮箱封禁账号的库上永远不为 0。
  所以手工清 hmac 时必须**先确认明文**，处置完再按 `release --uid` 或 `verify` 复核。
- 幂等：连跑第二次 `已释放 0`。单行失败记 uid 继续，末尾 `失败 N` 非 0 时退出码 1，重跑即可。
- 退出码：`0` 干净 / `1` 运行时错误或单行失败 / `2` 阻塞项 / `3` 顺序违规。
- 输出全部掩码：手机号 `861****00`，台账行只打 hmac 前 6 位。
- **第三态**（`phone` 有值、`phone_hmac` 为空）：注销时**两列都不动**。处置顺序是「先修 hmac
  （§10.3 的 `PHONE_WITHOUT_HMAC`）→ 再 `release --uid=<id>`」，因为明文是 hmac 的唯一重算输入。
- 已释放号码被他人注册后，**没有**「恢复原账号号码」的产品路径：只能走客服人工（新号主注销/释放后
  原用户重注册）。

### 10.3 重复 / 异常的人工处置（playbook）

**重复组**（`DUP phone=… rows=N`）：逐组由 **SUPER_ADMIN** 裁决「保留哪一行」，然后：

```sql
-- 1) 淘汰行：置空手机号、禁号、写清原因（不做 DELETE：账本/流水仍指向该 uid）
UPDATE t_user
   SET phone = NULL, phone_hmac = NULL,
       status = 'DISABLED', disable_source = 'OPS_MANUAL',
       disable_reason = '重复手机号合并:<保留行的 user_no>',
       disabled_at = NOW(3), updated_at = NOW(3)
 WHERE id = <淘汰行 id>;
-- 2) 同一事务写审计
INSERT INTO t_admin_audit_log
  (operator_id, operator_role, module, action, target_type, target_id, reason, result, created_at)
VALUES (<超管 uid>, 'SUPER_ADMIN', 'USER', 'PHONE_DEDUP', 't_user', '<淘汰行 id>',
        '重复手机号合并:<保留行 user_no>', 'OK', NOW(3));
```

保留行不需要手工写 hmac：`apply --commit` 会统一补。清完重跑 `detect`，必须退出 0。

**异常类**：

| 类别 | 判定 | 处置 | 为什么不能一键自动化 |
| :--- | :--- | :--- | :--- |
| `EMPTY_PHONE` | `phone = ''` | `UPDATE t_user SET phone = NULL WHERE id = ?` | 空串会被哈希成同一个值，制造假重复；改 PII 需要人确认 |
| `NON_CANONICAL` | `phone` 不是规范形态（含 `+`/分隔符/全角） | **逐行**人工确认该号是否还能登录，再写入规范值（`UPDATE ... SET phone = '<规范值>'`） | 一条自动 `trim/去空格` 的 UPDATE = 无人复核地改 PII |
| `STALE_HMAC` | `phone_hmac` 非空但与 `HMAC(phone)` 不一致 | 确认 phone 明文可信后：`UPDATE t_user SET phone_hmac = NULL WHERE id = ?`，再 `apply --commit` 重算 | 漂移原因可能是「上一次用了错 key」——先确认 key 现在是对的 |
| `GHOST_HMAC` | `deleted_at IS NOT NULL` 且 `phone_hmac` 不是 `#del#` 开头 | 按 `#del#{id}{unix_ms}` 改写（或把 `phone_hmac` 置 NULL 释放号段） | 软删行占着号段，重注册会 409 直到释放 |
| `PHONE_WITHOUT_HMAC`（仅 verify） | `phone` 有值、`phone_hmac` 为 NULL | 回填未覆盖：确认 `apply` 跑过、`PHONE_HMAC_KEY` 与写路径同值后重跑 | 该状态下**体验版会 500 拒绝发券**（宁可不发，不能给套利者发钱） |

`apply` 会**跳过软删行**（`deleted_at` 非空）：它们的正确值是释放标记 `#del#…`，填一个「live hmac」等于让幽灵行永久占号。这类行由 `detect` 的 `GHOST_HMAC` 拦下，交人工。

### 10.4 号码释放：常规路径已自动化，本节只作兜底

0029 起，**注销就是放号**：`POST /api/v1/auth/cancel` 在同一事务里把号码写进 `t_phone_claim` 并把
`phone` / `phone_hmac` 置 NULL，同一号码随后可被任何账号注册。所以本节只剩三种**兜底**场景，常规路径
不再需要它：

| 场景 | 做法 |
| :--- | :--- |
| 注销事务成功但号码未释放（第三态：`phone` 有值、`phone_hmac` 为空） | 先按 §10.3 修 hmac，再 `make backfill-phone-claim ARGS="release --uid=<注销行 id> --commit"` |
| 客服误判 / 需要人工纠正 | `release --uid=<id> --commit`（只放 SELF_CANCEL；其他来源会被跳过） |
| 需要解释「为什么这个号注册不了 / 为什么领不到体验版」 | `make backfill-phone-claim ARGS="lookup --phone=<客户给的号码>"` |

`lookup` 的输出就是客服话术的原料（号码已掩码、不出现明文与完整 hmac）：

- `台账：无` + `当前持有：无` → 号码空闲，可正常注册；若仍 409，按工单核对具体号码。
- `台账：无` + `当前持有：uid=…` → 号码被正常占用：请直接登录或找回密码；若近期注销过，该号码可能已被他人注册，需人工核验。
- `当前持有：无` + `体验版发放=<时间>`（90 天内）→ **号码已释放但仍在 90 天体验窗口内**：注册不受影响，但该号 90 天内不能再领体验版（`trial_already_claimed`）；如有异议，核身（uid/订单号）后经 `t_balance_adjust` 人工发放一次等值积分并留审计，原因写「号码回收误伤」。
- 「手机号能重新注册、邮箱目前不能」——邮箱侧对称缺口仍是 P1，话术不要含糊。

下面的手工 SQL 只在**没有可用命令行**（例如 DB 连接受限）时使用，且必须由 SUPER_ADMIN 审批、留工单号：

```sql
UPDATE t_user SET phone = NULL, phone_hmac = NULL, updated_at = NOW(3) WHERE id = <注销行 id>;
INSERT INTO t_admin_audit_log
  (operator_id, operator_role, module, action, target_type, target_id, reason, result, created_at)
VALUES (<超管 uid>, 'SUPER_ADMIN', 'USER', 'PHONE_RELEASE', 't_user', '<注销行 id>',
        '释放号段，工单 <no>', 'OK', NOW(3));
```

> 手工 SQL **不会**写 `t_phone_claim`：窗口与注销计数会因此缺失，体验版可能被白送一次。手工放号后请
> 立刻补 `backfill --commit`（补窗口）并跑 `verify`。这就是为什么常规路径必须是注销事务，而不是这条 SQL。

### 10.5 缺 `PHONE_HMAC_KEY` 时的降级

**进程照常启动**（把它升级成「整站不可用」是升级即事故）。行为矩阵：

| 路径 | 缺 key 时 | 日志 |
| :--- | :--- | :--- |
| 手机号注册 `POST /api/v1/auth/register` | **503 `config_unavailable`**，hint「手机号注册暂不可用，请使用邮箱注册」。**验证码不被消费**（拒绝发生在烧码之前），补 key 后同一个码仍可用 | `register_phone_hmac_unavailable env=PHONE_HMAC_KEY status=503 config_unavailable`（**WARN**：key 只在启动读一次，缺失已在启动日志里报过；这里再打 ERROR 会让每次注册尝试都变成假 5xx 告警） |
| 运营建号 `POST /api/admin/users`（手机号） | **503 `config_unavailable`**；邮箱建号不受影响 | `ops_create_operator_phone_hmac_unavailable …`（**WARN**，同上） |
| 邮箱注册 / 邮箱登录 / 手机号登录 / 找回密码 / 资料展示 | **完全不受影响**（读路径用明文 `phone`，不哈希） | — |
| 体验版 `POST /api/console/trial/claim` | 不受 key 影响（只比较库里已存的 hmac）；但「有手机号无 hmac」的行会 500（fail closed，见 10.3） | `fund_trial_phone_hmac_missing uid=…`（ERROR，不含手机号） |
| `cmd/backfill-phone-hmac` | **立即退出码 1**，不连库、不写行（命令行入口里最严格的一个：无条件失败，不看 ENV） | `PHONE_HMAC_KEY 未配置或短于 32 字节，拒绝回填（不会写入任何行）` |
| `cmd/seedctl`（含手机号种子） | 退出码 1 | `种子需要 PHONE_HMAC_KEY（≥32 字节）` |
| `controld` 启动 | **照常启动，不拒启**；`ENV=prod` 打 **ERROR**，其它环境 **WARN** | `PHONE_HMAC_KEY is not set: phone registration and operator user creation will answer 503 …` |

密钥要求与轮换：
- ≥ 32 字节，`openssl rand -base64 32`；**不得**与 `SESSION_SECRET` / `CHANNEL_KEY_KEK` / `LOG_BODY_KEK` / `FP_HMAC_KEY` / `EXPORT_HMAC_KEY` 相同（复用按「未配置」处理并打 ERROR）。
- **轮换代价极高，本轮不支持在线轮换**。停机重算流程：停写 → 换 key → `verify` 确认旧值范围 → 逐行重算（依赖明文 `phone`）→ 起服务 → 冒烟。硬限制：**已 PII 置空（`phone = NULL`）的行无法重算**，轮换后它们与新注册同号永久失配 → 判重失效。所以轮换只能在置空之前做，否则需要先一次性导出补偿。
- 泄漏影响量化：手机号空间 ~10⁹，拿到库里的 64 位 hmac + key 可用 GPU 在**分钟级**枚举全量反查。**key 与 DB 同时泄漏 ≈ 手机号明文泄漏**。

### 10.6 相关日志（都是 WARN，别当 5xx 处理）

| 日志 `msg` | 触发 | 含义 |
| :--- | :--- | :--- |
| `register_dup_phone_hmac` | 注册 409 | 该手机号已注册（`uk_user_phone_hmac`）。**不是故障**：用户重复提交或忘了自己注册过 |
| `register_dup_email` | 注册 409 | 该邮箱已注册（`uk_user_email`） |
| `register_dup_username` / `register_dup_identifier` | 注册 409 | 用户名重复 / 其它唯一键 |
| `ops_create_operator_dup_identifier: <key>` | 运营建号 409 | 同上，`msg` 尾部带索引名 |
| `register_phone_hmac_unavailable …` | 注册 503 | 缺 key，见 10.5 |
| `ops_create_operator_phone_hmac_unavailable …` | 运营建号 503 | 缺 key，见 10.5 |
| `fund_trial_phone_hmac_missing uid=…` | 体验版 500 | 有手机号无 hmac，见 10.3（唯一的 ERROR 类） |
| `trial_phone_claim_missing uid=…` | 体验版 500 | 有 hmac 但 `t_phone_claim` 没有行：**回填没跑**。跑 `make backfill-phone-claim ARGS="backfill --commit"` 后重试（fail-closed，宁可不发） |
| `trial_phone_window_hit` | 体验版 409 | 该号码 90 天内已领过体验版（可能来自另一个账号，含已注销的）。正常拒绝，不是故障 |
| `phone_claim_released uid=…` | 注销成功 | 号码已写进台账并释放，可被重新注册 |
| `cancel_phone_release_skipped reason=phone_without_hmac uid=…` | 注销成功 | 第三态：明文在、hmac 空，两列都没动。按 10.4 修 hmac 后 `release --uid` 补释放 |

判断口径：这些 409/503 记录**不带 `"status":500`**（WARN 行的 `status` 字段恒为 0）。若看到 `"level":"error"` + `"status":500` + `register_insert_user_failed` / `ops_create_operator_insert_user_failed`，那才是真故障（DB 不可写、死锁、缺列）。

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

### 9.2 改密（正常路径）

用会话调用 `POST /api/v1/auth/password`，body `{old_password, new_password}`，需带 `X-CSRF-Token`（值取 `csrf` cookie）。成功后会吊销该 uid 的**全部**会话与 CSRF token，需要重新登录。

### 9.3 忘记密码的人工兜底

> **当前没有任何重置口令的接口。** 唯一能写 `password_hash` 的入口是自助的 `POST /api/v1/auth/password`（要旧口令）。登录页的「忘记密码」也只描述“暂未开放”。因此在「唯一超管忘记口令」这一种情况下，唯一手段是 DBA 直接改库，且**四步都必须做**，少一步就是留后门。

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

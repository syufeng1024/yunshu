# 请求日志与用量的保留期边界

请求正文保留 180 天、请求元数据保留 365 天、在线查询窗口 365 天，三者不是同一个数字。混淆任意两个会让客服承诺错数据，或让运维误判“数据已删除”。

## 三种期限

| 对象 | 存储 | 保留期 | 在线可查范围 | 越界返回 |
| :--- | :--- | :--- | :--- | :--- |
| 请求正文 | `t_request_body`（T22 migration `0022` 建表） | 180 天 | 正文接口自身按 180 天判定 | `404 resource_not_found`，message「正文已过保留期或未开启」 |
| 请求元数据 | `t_usage_record`、`t_upstream_attempt` | 365 天 | 范围查询 `from >= now()-365d` | `400 invalid_query_parameter`，hint「请使用归档导出」 |
| 用量 | `t_usage_record`、`t_metric_daily` | 与元数据同源 | 范围查询 `from >= now()-365d` | `400 invalid_query_parameter`，hint「请使用归档导出」 |
| 账单 | `t_usage_record` | 与元数据同源 | 按 `month=YYYY-MM` 查 CST 自然月，不做 365 天窗口校验 | 早于清理范围的月份返回 0；更早数据走归档流程 |
| 导出文件 | `EXPORT_DIR`（默认 `os.TempDir()/yunshu-exports`） | 签名令牌 1 小时 | 令牌内含 `exp = 创建时刻 + 1h` | `404 resource_not_found`，hint「导出链接有效期为 1 小时」 |

## 边界判定

| 场景 | 判定点 | 结果 |
| :--- | :--- | :--- |
| 正文：`created_at >= now()-180d` | `GET /api/console/logs/{request_id}/body` | 200（前提 `full_log_saved=1`） |
| 正文：`created_at < now()-180d` | 同上 | 404，不返回空串 |
| 正文：`full_log_saved=0` | 同上 | 404；请求从未落正文，与过期不可区分 |
| 元数据：`from = now()-366d` 的范围查询 | `GET /api/console/logs`、`GET /api/console/usage` 的 `from` | 400 + 归档提示 |
| 元数据：366 天前行的精确查询 | `GET /api/console/logs/{request_id}` | 200；窗口只约束范围扫描，行是否还在由清理任务决定 |
| 导出：令牌 `exp <= now()` | `GET /api/console/exports/{token}` | 404；磁盘文件即使仍在也不可下载 |

`now()` 取服务注入的 UTC 时钟；正文判定用 `req.CreatedAt.Before(now.Add(-180*24h))`，等号当天仍可查。范围下界判定用 `from.Before(now.Add(-365*24h))`，恰好 365 天整可查，早 1 毫秒即拒绝。

## 各接口的过滤口径

| 接口 | 计费类指标（tokens/积分/金额） | 请求数与成功率 |
| :--- | :--- | :--- |
| `GET /api/console/logs` | 不适用 | 默认不过滤；失败请求必须可见，`state=failed` 只收窄，不改变默认 |
| `GET /api/console/logs/{request_id}` | 不适用 | 不过滤；单条请求详情，含失败与重试时间线 |
| `GET /api/console/usage` | 仅 `SUCCESS` | `request_total` / `request_success` / `success_rate_bp` 统计全部 `result` |
| `GET /api/console/bills` | 仅 `SUCCESS` | 同上，按 CST 自然月统计 |
| `POST /api/console/usage/export` | 仅 `SUCCESS` | CSV 的「请求数 / 成功数 / 成功率」按全部 `result`，其余列与用量页同口径 |

PRD §3.1 的「不计费、不计入用量排行」约束的是积分、token 与按 `consumed_micro DESC` 的排序，不约束成功率的分母。分母若只取 `SUCCESS`，`success_rate_bp` 恒为 10000，指标失去意义。实现上两者在**同一条聚合 SQL** 内完成：`COUNT(*)` 得到 `request_total`，`SUM(result='SUCCESS')` 得到 `request_success`，计费列用 `SUM(CASE WHEN result='SUCCESS' THEN ... END)`。`success_rate_bp = request_success * 10000 / request_total`，整数截断（2/3 → 6666），`request_total=0` 时为 0。

失败请求同时存在于日志列表与 `t_usage_record`（正常情况下 `consumed_micro=0`）。它计入用量页与账单的 `request_total` 与成功率分母，但不贡献 token、积分与金额，也不抬高按消耗积分排序的名次。日志列表的 SQL 与用量聚合的 SQL 分别写在 `internal/control/logq/list.go` 与 `internal/control/usage/query.go`，没有共用默认值。

## 超过 365 天怎么办

在线接口不提供更早数据，返回的 hint 指向归档导出。归档数据不经 `t_usage_record` 在线查询，也不受正文 180 天规则影响。客服答复“数据查不到”时，先确认是正文过期（180 天）还是范围越界（365 天）：前者换不了接口，后者需要走归档流程。

## 关闭即删（`log.body_mode = meta_only`）

正文的保留期有**两个**删除触发条件，不是只有一个 180 天：

| 触发 | 判定 | 任务 | 删除范围 |
| :--- | :--- | :--- | :--- |
| 到期 | `expires_at < now()` 或 `created_at < now()-180d` | `purge_log`（每天 03:10） | 到期的行 |
| **关闭正文保存** | 当前 `log.body_mode = meta_only` 且 `t_request_body` 还有行 | `purge_body_mode`（每 60s，leader） | **全部行**，不看 `expires_at` |

B-67 / TDS 4318 的承诺是「用户关闭即删」，不是「关闭后 180 天内删」。因此 `meta_only` 生效期间任何残留正文都是违约，`purge_body_mode` 按**当前模式**判定并删光全表：

- 判定不需要记住「上次是什么模式」——重启、故障转移、配置回滚（`opt_in → meta_only → opt_in`）都不会漏删，因为根本没有状态；
- 模式读取失败（`t_config_item` 不可读）时**一行都不删**并把任务报为失败：留一天可恢复，删掉承诺要留的数据不可恢复；
- 每轮删除 > 0 行才写一条审计（`t_admin_audit_log`：`action=LOG_BODY_PURGED_ON_MODE_DISABLED`、`operator_role=SYSTEM`、`module=CONFIG`、`result=n=<条数>`、`after_json` 含 `deleted`/`body_mode`）。删 0 行不写，否则每 60s 一条噪音，审计就没人看了；
- 与到期清理共用同一个批删循环（`BodyPurgeBatchSize` = 10000 行/批、批间 200ms、`ctx` 取消），两条路径都是幂等 `DELETE`，可安全重叠；
- 传播窗口：`log.body_mode` 由配置中心发布，网关侧最多滞后 4 分钟。若在 `meta_only` 生效后仍有节点按旧快照写入正文，那些行会在下一轮（≤60s）被删掉——它们同样是违约数据，不会留下。

客服口径：客户问「我关了正文保存，之前的记录清掉了吗」→ 答「关闭后一分钟内已删除，正文接口对历史请求返回 404，元数据与用量记录按 365 天保留」。

## 正文密钥轮换

`t_request_body.body_key_id` 不是装饰列：每一行记录的是**加密它那一版的 KEK 标识**，读取端按这个 id 选 key，而不是永远用当前 key 去试。

| 变量 | 作用 |
| :--- | :--- |
| `LOG_BODY_KEK` | 当前 KEK，新行用它加密 |
| `LOG_BODY_KEY_ID` | 当前 KEK 的 id，写入新行的 `body_key_id`（默认 `body-v1`） |
| `LOG_BODY_KEK_PREVIOUS` | 退休 KEK 列表，`id:base64` 逗号分隔，只读 |

轮换顺序（不可颠倒）：

1. 生成新 KEK，把**旧** KEK 追加进 `LOG_BODY_KEK_PREVIOUS`，`LOG_BODY_KEK` 换新，`LOG_BODY_KEY_ID` 改新 id；
2. 两个进程（gatewayd 写入、controld 读取）**同时**重启到这份配置——只改一边会让新行用新 key、读取端不认识这个 id，表现为正文接口 404 且 `gw_log_body_open_failed_total` 上涨；
3. 旧 key **至少保留到它加密的最后一行过期**（`expires_at`，最长 180 天）。提前摘掉旧 key 不会删数据，但那些行再也解不开。

未知 `body_key_id` 的处理与「从未存过」一致：404 + `gw_log_body_open_failed_total`，不返回 500，也不回落到当前 key 去试（回落只会把「key 没配」伪装成「解密失败」，让排障方向跑偏）。

## 不删除

`t_usage_record` 与 `t_upstream_attempt` 没有删除接口（T14 硬约束 12）。账号注销不删除日志，按 PRD §10.3 的独立流程处理。清理任务只按保留期批量删除，不做单条删除。

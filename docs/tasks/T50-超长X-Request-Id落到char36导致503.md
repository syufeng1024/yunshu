# T50 · 超长 `X-Request-Id` 落到 `char(36)` 列，整请求 503

| 项 | 值 |
| :--- | :--- |
| 卡片编号 | T50 |
| 阶段 | 6 · 交付（业务流审计第 21 项） |
| 优先级 | **P1**（不产生资损，但对合规客户端是 100% 不可用，且错误**可重试**） |
| 依赖 | 无（独立于 `94d4885` / `0415fb0`） |
| 交付根 | `internal/apierr/codes.go`、`internal/platform/httpsrv/middleware.go`、`internal/apierr/traceparent.go`、`internal/ledger/prehold.go`、`api/openapi.yaml` |
| 状态 | **方案 A 已实现**（入口按 36 拒收，400 `invalid_request_id`）；方案 B 未做，仅在确认有客户端需要 >36 时才考虑 |
| 分段 | 单段（无迁移） |

## 1. 事实（已核，勿重复调查）

| 事实 | 证据 |
| :--- | :--- |
| 存储契约是 36 字节 | `docs/技术架构设计文档 V1.2.md:2429`：`request_id char(36) COMMENT 'ULID/UUIDv7，对外 X-Request-Id'`；迁移 `0003_fund_usage_ops.sql:79`（`t_prehold_hold.request_id`）、`:273`（`t_usage_record.request_id`） |
| 入口放行到 128 字节 | `internal/apierr/error.go:19` `maxRequestIDLen = 128`；`sanitizeRequestID`（`:441-452`）只做「可见 ASCII + ≤128」 |
| 中间件用它决定 id | `internal/platform/httpsrv/middleware.go:37` → `apierr.ResolveRequestID`（`traceparent.go:44`）；**所以 37–128 的客户端值会一路被采用** |
| 37 字符进 Prehold 就是 MySQL 1406 | 独立复核实测：`Error 1406 (22001): Data too long for column 'request_id'`；`sql_mode` 含 `STRICT_TRANS_TABLES`、`innodb_strict_mode=1`（MySQL 8.0.46） |
| 该错误落到 503，且被标记为**可重试** | `internal/gw/p10_prehold.go:110-114`：既非 duplicate 也非 insufficient → `CodeSettlementUnavailable`（`codes.go:580`，`Retry: RetryYes`、`RetryAfterSeconds: 5`） |
| 后果 | 用 `req_<uuid>`（40 字符）或 `trace-<32hex>`（38 字符）做关联 id 的客户端**每个请求都 503**，且按 `retry_after_seconds` 无限重试；钱不动（插入回滚，实测 `holds=1 balance 500000→499000`，仅一次正常预扣） |
| 次生问题 | `usagewriter.go:532` `cut(r.RequestID, 36)`：即使能落库，前缀相同的两个长 rid 会塌到同一主键、互相静默抑制 |

## 2. 两个方案（**需拍板**）

**方案 A（推荐）：入口按 36 字节拒收，400 `invalid_request_id`**

- 依据：设计文档把 `X-Request-Id` 定义为 ULID/UUIDv7（26 或 36 字节），36 是**既有契约**，不是新限制。
- 改动：`codes.go` 新增 `CodeInvalidRequestID = "invalid_request_id"`（400 / `TypeInvalidRequest` / `RetryNo` / `OwnerBusiness` / `Stage: "P08"`）；`codes_test.go` 表加行并把两处 `76` 改为 `77`；`RequestID` 中间件在 37–128 时直接写 400 并返回，不进业务链。
- 风险：`RequestID` 是**所有监听器共用**的全局中间件（网关 / 控制台 / 运维），在那一层拒收会波及控制台内部转发（`internal/control/console/playground.go:192` 把 rid 原样转发给网关）。落地时需确认控制台自己生成的 rid 不超过 36。
- **禁止**改成「超长就生成一个新 id」：那会让客户端每次重试都拿到新 id，`request_id` 的幂等语义被静默取消，重试变成重复扣费——比现在报错更糟。

**方案 B：把 13 处 `char(36)` 放宽到 `varchar(128)`**

- 保住 128 的入口宽度，不拒绝任何客户端。
- 代价：新增迁移（ALTER 13 列，含 `t_prehold_hold` 主键、`t_usage_record` 唯一键），`wantMigrationCount` 与 `gen-migration-checksums.sh` 需同步；**在资金主表上做类型变更**，大表上是 COPY/INPLACE，需要停机窗口或 gh-ost 类工具。
- 只有在确认「确实有客户端需要 >36 的关联 id」时才值得。

## 3. 验收

1. `X-Request-Id` = 36 字符 → 正常预扣；= 37 字符 → **400 `invalid_request_id`**（不是 503），`retry_after_seconds` 为 0 或字段缺省，`X-Request-Id` 响应头仍是一个合法 id。
2. 同一超长 id 重复提交：每次都是同一个 400，不产生 hold、不产生 usage 行、不扣费。
3. `traceparent` 路径不受影响（32 hex = 32 字节，仍走 trace-id）。
4. 控制台 playground 转发链路回归（`playground.go:192`）。
5. `codes_test.go` 两处计数与 openapi 中错误码描述同步。

## 4. 备注

- 该缺陷与 T50 同期发现的 A-12 覆写（`905a7e3`）**不同类**：这里不产生错误账单，只是不可用。
- 若短期不做，至少应把 `CodeSettlementUnavailable` 的 `RetryYes` 在该路径改掉，避免客户端无限重试——但那只是止血，且会让 503 变成不可重试的 503，语义更怪，不推荐。

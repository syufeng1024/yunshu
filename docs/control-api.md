# 控制面 API 约定

适用路径：`/api/v1/auth/*`、`/api/console/*`、`/api/admin/*`。路由表与权限声明在 `internal/control/server.go`，权限定义与角色映射在 `internal/control/mw/authz.go`。

## 权限点

| 权限点 | 含义 | 持有角色 |
| :--- | :--- | :--- |
| `account.read` | 读取**自己**的账号概览（余额/套餐/额度/今日用量，按 session uid 过滤） | PERSONAL / SUPER_ADMIN |
| `user.read` | 读取**任意**账号（状态、套餐、禁用原因，运营目录读） | OPS / SUPER_ADMIN |
| `user.write` | 启用、禁用用户、提交余额调整（运营侧） | OPS / SUPER_ADMIN |
| `key.read` | 查看自己的 API Key 列表、详情与变更记录 | PERSONAL / SUPER_ADMIN |
| `key.write` | 创建、修改、轮换、删除自己的 API Key | PERSONAL / SUPER_ADMIN |
| `model.read` | 读取模型目录（控制台可用模型） | PERSONAL / OPS / FINANCE / SUPER_ADMIN |
| `model.write` | 创建、修改模型主数据、别名、上架状态与推荐权重 | OPS / SUPER_ADMIN |
| `price.write` | 创建、修改模型价格与价格分组 | OPS / SUPER_ADMIN |
| `group.write` | 修改价格分组倍率与可见范围 | OPS / SUPER_ADMIN |
| `channel.read` | 读取任意上游渠道（含 `base_url`、上游密钥掩码） | OPS / SUPER_ADMIN |
| `channel.write` | 创建、修改、停用、删除上游渠道 | OPS / SUPER_ADMIN |
| `notice.write` | 创建、修改、发布、下线公告 | OPS / SUPER_ADMIN |
| `risk.write` | 维护风控规则、查看命中、调整用户风险分 | OPS / SUPER_ADMIN |
| `config.write` | 读取与写入 `t_config_item` 运行时配置 | OPS / SUPER_ADMIN |
| `fund.read` | 查看自己的套餐、订单、退款、发票与提额单 | PERSONAL / FINANCE / SUPER_ADMIN |
| `fund.write` | 下单、兑换、申请退款/发票/提额、领取体验积分 | PERSONAL / SUPER_ADMIN |
| `fund.approve` | 审批退款、提额与资金类审核单 | FINANCE / SUPER_ADMIN |
| `ticket.read` | 读取自己提交的工单 | PERSONAL / SUPER_ADMIN |
| `ticket.write` | 创建工单、关闭自己的工单 | PERSONAL / SUPER_ADMIN |
| `ticket.reply` | 读取运营工单队列、回复、流转状态 | OPS / SUPER_ADMIN |
| `audit.read` | 查询 `t_admin_audit_log` | OPS / FINANCE / SUPER_ADMIN |
| `log.read` | 查询自己的请求日志与正文 | PERSONAL / SUPER_ADMIN |
| `usage.read` | 查询自己的用量、月账单、实时指标与 CSV 导出 | PERSONAL / SUPER_ADMIN |
| `admin.read` | 读取运营后台的聚合概览与模型主数据（含成本价） | OPS / FINANCE / SUPER_ADMIN |
| `admin.all` | 跨域诊断（`GET /api/admin/whoami`） | SUPER_ADMIN |

读权限按**资源作用域**拆分，而不只按资源名拆分。同一个资源在两个控制台看到的是不同列，就是两个权限点：`account.read` 是客户自助（只读自己那一行），`user.read` / `channel.read` 是运营作用域（读任意账号、任意上游渠道）。`admin.read` 与 `model.read` / `account.read` 也刻意分开：控制台读接口给客户看，运营读接口包含 `cost_*`、`base_url`、`key_mask`、`disable_reason`，共用权限点会把成本价和禁用原因泄露给普通账号（E-31 / E-33）。`admin.read` 只留在聚合概览与模型主数据上；用户目录与渠道各自有专名，因此「能看概览/成本表」不等于「能看全站账号与上游渠道」——FINANCE 持有 `admin.read`，但既没有 `user.read` 也没有 `channel.read`。价格、分组、公告、风控、配置的运营读接口分别复用对应的 `.write` 权限点，FINANCE 因此也看不了价格与风控。

## 角色 × 权限

| 权限点 | PERSONAL | OPS | FINANCE | SUPER_ADMIN |
| :--- | :---: | :---: | :---: | :---: |
| `account.read` | ✓ | — | — | ✓ |
| `user.read` | — | ✓ | — | ✓ |
| `user.write` | — | ✓ | — | ✓ |
| `key.read` | ✓ | — | — | ✓ |
| `key.write` | ✓ | — | — | ✓ |
| `model.read` | ✓ | ✓ | ✓ | ✓ |
| `model.write` | — | ✓ | — | ✓ |
| `price.write` | — | ✓ | — | ✓ |
| `group.write` | — | ✓ | — | ✓ |
| `channel.read` | — | ✓ | — | ✓ |
| `channel.write` | — | ✓ | — | ✓ |
| `notice.write` | — | ✓ | — | ✓ |
| `risk.write` | — | ✓ | — | ✓ |
| `config.write` | — | ✓ | — | ✓ |
| `fund.read` | ✓ | — | ✓ | ✓ |
| `fund.write` | ✓ | — | — | ✓ |
| `fund.approve` | — | — | ✓ | ✓ |
| `ticket.read` | ✓ | — | — | ✓ |
| `ticket.write` | ✓ | — | — | ✓ |
| `ticket.reply` | — | ✓ | — | ✓ |
| `audit.read` | — | ✓ | ✓ | ✓ |
| `log.read` | ✓ | — | — | ✓ |
| `usage.read` | ✓ | — | — | ✓ |
| `admin.read` | — | ✓ | ✓ | ✓ |
| `admin.all` | — | — | — | ✓ |

角色名以 `t_user.role` 为准，只有 `PERSONAL` / `OPS` / `FINANCE` / `SUPER_ADMIN` 四个（E-30）。未知角色在 `PermissionsFor` 中返回空集，登录后访问任何带权限的接口都是 403。

## 公开接口（官网）

5 条只读路径，无需 `sid` 会话，也无需 `X-CSRF-Token`；在 `internal/control/server.go` 中经 `s.public(...)` 注册（仅 panic 恢复，不进入 Session → CSRF → Require 链）。成功响应统一带 `Cache-Control: public, max-age=60`；错误响应（500 `internal_error`）不写该头，避免一次数据库故障被缓存 60 秒。

| 路径 | 数据源与过滤 | 排序 / 上限 | 响应 |
| :--- | :--- | :--- | :--- |
| `GET /api/public/plans` | `t_plan`，`enabled=1` | `sort_order ASC, id ASC` | `{data: PublicPlan[]}`；`daily_quota_micro` 为 NULL 时输出 `null`（FREE_DEFAULT 无上限），不转 0 |
| `GET /api/public/models` | `t_model`，`listing_state='LISTED'` 且 `deleted_at IS NULL`；`t_price_rule` 当前生效的 MODEL 级价格 | `recommend_weight DESC, published_at DESC` | `{data: PublicModel[]}`；无生效价或价格列为 NULL 的模型整行不返回；`context_window` 取自 `t_model.context_length` |
| `GET /api/public/announcements` | `t_announcement`，`state='PUBLISHED'`、`effective_from<=now`、`effective_to IS NULL OR > now`、`deleted_at IS NULL` | `effective_from DESC, id DESC`，最多 20 条 | `{data: PublicAnnouncement[]}`；`content→body`、`ann_type→category`、`effective_from→effective_at`、`created_at→published_at` |
| `GET /api/public/endpoints` | 无数据表（全部迁移中不存在 endpoint / integration / tool 表） | — | 恒为 `{data: []}`；`TODO(T24)` 在 `internal/control/publicapi/empty.go` |
| `GET /api/public/integration-links` | 同上 | — | 恒为 `{data: []}`；同上 |

公开模型的 `protocols` 只输出前端枚举内的 `OPENAI` / `ANTHROPIC`：`t_protocol.code='OPENAI_COMPAT'` → `OPENAI`，`ANTHROPIC_COMPAT` → `ANTHROPIC`，`BAILIAN` / `CUSTOM` 或 `protocol_id` 关联不到行时输出空数组。公告 `category` 只有 `PRICE` / `RATE_LIMIT` / `GENERAL` 三值：`ann_type='PRICE_CHANGE'` 映射为 `PRICE`，其余映射为 `GENERAL`（后端没有 `RATE_LIMIT` 枚举）。

## T24 段 1：运营只读接口与字段

### `GET /api/admin/overview`

权限 `admin.read`。只读，不写审计、不改状态。响应三组：

| 分组 | 字段 | 口径 |
| :--- | :--- | :--- |
| `today` | `requests` / `consumed_micro` / `success_rate_bp` | UTC+8 自然日；`requests` 计全部 `t_usage_record` 行，`consumed_micro` 求和全部行（失败行为 0）；`success_rate_bp = SUCCESS / (http_status NOT IN (402,429))`，分母为 0 时 **null**，不返回 0 |
| `alerts` | `dangling_prehold` | `t_prehold_hold.state='HELD' AND expires_at<now`，走 `idx_ph_suspend_scan` |
| `alerts` | `recon_diff` | 近 30 天（含今天）`t_reconcile_diff.status='OPEN'`，走 `idx_rd_date_type` |
| `alerts` | `sla_overdue` | `t_ticket.state='OPEN' AND first_responded_at IS NULL AND first_response_due_at<now`，走 `idx_tk_state_created` |
| `alerts` | `channel_abnormal` | 未删除、`region='DOMESTIC'`、`enabled=1`、`health_state<>'CLOSED'`，走 `idx_channel_region_enabled` |
| `counts` | `pending_reviews` | `t_review_task.state='PENDING'`，走 `idx_rt_state_created` |
| `counts` | `open_tickets` | `t_ticket.state IN ('OPEN','RESPONDED')`（未关闭），走 `idx_tk_state_created` |

今日聚合走 `t_usage_record.idx_ur_created` 的 `created_at` 范围；不使用 `t_metric_daily`（T+1 汇总可能滞后，不能用近似值冒充今日）。

### `GET /api/admin/tickets/{id}`

权限 `ticket.reply`。返回 `AdminTicketDetail`：列表字段 + `user_id` / `user_email` + `description` + `replies[]` + `sla`。`replies[].is_internal=true` 表示内部备注：运营可见，用户控制台不可见；该读接口不写审计、不改状态。缺失或畸形 id 都返回 404 `resource_not_found`；`ticket.reply` 是授权边界。

### 运营视图新增字段

| 结构 | 字段 | 口径 |
| :--- | :--- | :--- |
| `admin.userView` | `key_count` | `t_api_key` 中 `status='ENABLED' AND deleted_at IS NULL` 的行数；列表用一次 `GROUP BY user_id` 聚合，走 `idx_key_user_status`，无 Key 时为 0 |
| `channelView` | `success_rate_bp` | 近 15 分钟 `t_upstream_attempt` 中 `result='SUCCESS'` / 全部尝试（分母为尝试次数）；窗口内无尝试时为 **null**，不返回 10000；走 `idx_ua_channel_created` |

`success_rate_bp` 与 `key_count` 同时出现在列表和详情响应中。

### `GET /api/admin/audit-logs` 新增过滤

`target_type` / `target_id` 进入 SQL `WHERE`：`target_type` 按原样比较（如 `t_channel`，不转大写），`target_id` 按字符串比较；两者 trim 后最大 32 rune，超长返回 `invalid_query_parameter`。两者同传时走 `idx_aal_target (target_type,target_id,created_at)`；不传时查询与改造前一致。建议总是同时传 `target_type`，否则仅 `target_id` 无法使用该索引。

OpenAPI 已同步：`/api/admin/overview`、`/api/admin/tickets/{id}`、`/api/admin/users`（含 `key_count`）、`/api/admin/channels`（含 `success_rate_bp`）与相关 schema。

## T24 段 2：写接口

| 方法 + 路径 | 权限 | 落点 | 语义 |
| :--- | :--- | :--- | :--- |
| `POST /api/admin/users/{id}/balance-adjust` | `user.write` | `review/balance.go:SubmitBalanceAdjust` | 写 `t_balance_adjust(PENDING)` + `t_review_task(BALANCE_ADJUST)` + 审计，三者同一事务 |
| `POST /api/admin/reviews/{id}/approve` | `fund.approve` | `review/balance.go:approveBalanceAdjust` | `PENDING→APPROVED`，同一事务内经 `ledger.AdjustTx` 落账 |
| `POST /api/admin/reviews/{id}/reject` | `fund.approve` | `review/balance.go:rejectBalanceAdjust` | `PENDING→REJECTED`，不产生资金分录 |
| `POST /api/admin/announcements/{id}/offline` | `notice.write` | `ops/announcement.go:OfflineAnnouncement` | `DRAFT/SCHEDULED/PUBLISHED → OFFLINE` |
| `PATCH /api/admin/announcements/{id}` | `notice.write` | `ops/announcement.go:UpdateAnnouncement` | 新增 `effective_from` 写入与 `model_ids` 持久化 |
| `PATCH /api/admin/prices/{id}` | `price.write` | `ops/pricing.go:UpdatePrice` | 新增 `clear_fields` 显式置空 |
| `POST /api/v1/auth/cancel` | 登录即可 | `auth/cancel.go:Cancel` | 工单预检 → 禁用账号 → 删除全部会话 |

### 余额调整

`t_balance_adjust`（迁移 0025）：

| 列 | 类型 | 约束 |
| :--- | :--- | :--- |
| `direction` | `varchar(8)` | `CHECK direction IN ('INCREASE','DECREASE')` |
| `amount_micro` | `bigint` | `CHECK amount_micro > 0`，恒为正；方向单独存储 |
| `reason` | `varchar(255)` | 提交时 trim 后 5–255 个字符 |
| `state` | `varchar(16)` | `PENDING / APPROVED / REJECTED`，单向流转 |
| `pending_user_id` | `bigint unsigned` STORED 生成列 | `state='PENDING'` 时等于 `user_id`，否则 NULL；`uk_ba_user_pending` 保证同一用户同时最多一张待审单 |

- 提交上限：单张 `amount_micro ≤ 10,000,000,000`（1,000,000 积分）。这是输入防呆，不是产品额度；需要更多时拆成多张单据，各自留理由与审批记录。
- 提交时校验 `t_user` 存在且未删除、`t_credit_account` 存在；无账户返回 409 并提示排查数据完整性。
- 自审：`applicant_id == operator` 时审批返回 403 `permission_denied`，hint「审批人不得为申请人：请转交其他审核人处理」；余额、账本、单据状态都不变。
- 幂等：`UPDATE ... WHERE state='PENDING'` 的 `RowsAffected()` 是唯一闸门；重复审批返回 409 `resource_state_conflict`。账本侧 `uk_le_idem` 是第二道防线。
- 审计：提交、通过、驳回各一行，`target_type='t_balance_adjust'`、`target_id` 为单据 id，`before_json`/`after_json` 含状态、方向、金额、理由、审批人；`GET /api/admin/audit-logs?target_type=t_balance_adjust&target_id=<id>` 可直接命中。
- 用户详情 `GET /api/admin/users/{id}` 追加 `latest_balance_adjust`：最近一张单据（按 `created_at DESC, id DESC`），无记录为 `null`。
- `GET /api/admin/users` 与 `GET /api/admin/users/{id}` 返回 `username`（未设置时省略该字段）；列表新增 `?username=` 模糊筛选，与 email / phone / user_no 相互独立。内置超管只有用户名，没有邮箱/手机号，运营台据此展示与检索。

### 余额调整落账

审批通过时 `review/balance.go` 调用 `ledger.AdjustTx`，全程在调用方事务内，禁止 `UPDATE t_credit_account`：

| 方向 | 实现 | 批次 / 分录 |
| :--- | :--- | :--- |
| `INCREASE` | 复用 `ledger.GrantTx` | 一张 `COMPENSATION` 批次（`refundable=0`，`expire_at=now+90d`，`source_id=t_balance_adjust.id`）+ 一条 `CREDIT` 分录，`biz_type=ADJUST` |
| `DECREASE` | 新增 `ledger.AdjustTx` 减量分支 | 按结算同一 FIFO 顺序扣减批次（赠送先于充值），每个批次一条 `DEBIT` 分录，`biz_type=ADJUST`；可用余额不足或批次无法全覆盖时返回 409 |

减量不增加 `t_credit_account.total_consumed_micro`：人工调账不是计量消费。批次级 `consumed_micro` 仍会随扣减增加，这是 schema 表达「面额已被收回」的唯一方式；批量不变量 `remaining = face - consumed` 保持不变。`ledger.AdjustTx` 同时保留 `operator_id` 与 `reason`，日对账可按 `biz_type=ADJUST` 单独汇总。

### 公告：`effective_from`、下线与 `model_ids`

- `PATCH` 接受 `effective_from`（`datetime(3)`）。请求同时带 `publish_at` 时，`effective_from` 不得早于它，否则 400 并点名 `effective_from`；已 `PUBLISHED` / `OFFLINE` 的公告整条不可编辑（409）。
- `POST /api/admin/announcements/{id}/offline`：`DRAFT / SCHEDULED / PUBLISHED → OFFLINE`。已 `OFFLINE` 再调用返回 409。OFFLINE 只是停止展示，行与审计保留。
- `model_ids` 持久化在 `t_announcement_model(announcement_id, model_id)`（迁移 0025）。写入时去重、上限 100、每个 id 必须指向未删除的 `t_model`；`PATCH` 缺省保留现有关系，`[]` 清空。读接口（列表与详情）用一次 `WHERE announcement_id IN (...)` 聚合，无关联返回 `[]` 而非 `null`。

### 价格 `clear_fields`

`PATCH /api/admin/prices/{id}` 新增 `clear_fields`。白名单：

| 字段 | 清空后的语义 |
| :--- | :--- |
| `cache_hit_ratio_bp` / `cache_write_ratio_bp` | 恢复为 NULL（未配置） |
| `long_ctx_threshold_tokens` / `long_ctx_in_price_micro_per_k` / `long_ctx_out_price_micro_per_k` | 三项必须一起清空，否则 400 点名 `long_ctx_threshold_tokens` |
| `cost_in_micro_per_k` / `cost_out_micro_per_k` / `cost_per_call_micro` | 清空后缺成本校验仍生效：未传 `confirm_no_cost=true` 时 400 |
| `effective_to` | 恢复为长期有效 |

- 白名单外的键返回 400（message 为 `clear_fields`）。
- 同一字段既赋值又出现在 `clear_fields` → 400，message 点名该字段（如 `cache_write_ratio_bp`）。
- 必填列 `in_price_micro_per_k` / `out_price_micro_per_k` / `per_call_price_micro` 不在白名单内：清空它们会让模型无法定价，应改用新版本价格规则。

### `recommend_weight` 上限

`POST /api/admin/models/{id}/recommend-weight`、`PATCH /api/admin/models/{id}` 与创建模型统一校验 `0 ≤ recommend_weight ≤ 1000`；超限返回 400 且 message 点名 `recommend_weight`。

定档 1000 的理由：该列是 `int unsigned`，没有业务上限时一次写入 2^31-1 就能让单个模型霸占全部推荐位；1000 与运营台其余百分比/万分比字段同一量级，既能表达相对权重，又不会把排序键变成任意大数。默认值与推荐位排序（`recommend_weight DESC, published_at DESC`）不变。

### 注销

`POST /api/v1/auth/cancel`（登录即可，无额外权限点）：

1. 先调 `ticket.CheckAccountCancellable`。存在 `OPEN` / `RESPONDED` 工单时返回 409 `resource_state_conflict`，message 为「存在未结工单，无法注销：<工单号…>」，`CLOSED` 不阻塞。
2. 事务内 `t_user.status='DISABLED'`、`disable_source='SELF_CANCEL'`、`disable_reason='用户主动注销'`、`disabled_at=now`、`cancel_apply_at=now`，并写一条 `USER/CANCEL` 审计（before/after 含 status 与 disable_source）。不新增 `t_user.status` 枚举值。
3. 提交后调用 `SessionManager.RevokeAll`：删除 K30 `sess:admin:uid:{uid}` 中的全部 K20 `sess:admin:{sid}` 与集合本身。旧 sid 再访问返回 401 `session_expired`。吊销失败返回 503 `auth_unavailable`，不谎报成功。

## T27：调试台内网扣费链路

`POST /api/console/playground/chat` 把一次调试调用交给 gatewayd 的
`POST /internal/v1/chat/completions`。扣费走 gatewayd 既有的 P10 预扣与 P14 结算；
controld 不读余额、不扣余额，也不写 `t_usage_record`。

### 控制台端点

| 项 | 值 |
| :--- | :--- |
| 方法 + 路径 | `POST /api/console/playground/chat` |
| 权限 | `key.read`，叠加会话鉴权与 CSRF 双提交校验 |
| 请求体上限 | `gw.MaxBodyBytes + 65536` = 10551296 字节（10 MiB + 64 KiB）；超限 413 |
| 上游 | `<GATEWAY_INTERNAL_URL>/internal/v1/chat/completions`，必须是绝对 http(s) URL |
| 超时 | 建连 5s；响应头 130s（gatewayd 非流式 P01 预算 120s）；SSE 由 gatewayd 的 600s 预算兜底，controld 侧无读超时 |

请求体只有两个字段：

| 字段 | 类型 | 约束与处理 |
| :--- | :--- | :--- |
| `key_id` | `bigint` | `> 0`；必须属于当前会话用户且 `deleted_at IS NULL`。越权与不存在返回同一 404，不暴露 Key 是否存在 |
| `body` | JSON 对象 | trim 后非空；**原样转发**给 gatewayd，controld 不改写、不校验 `model` / `max_tokens` / `temperature`（校验属 P04） |

请求头透传：

| 头 | 行为 |
| :--- | :--- |
| `X-Request-Id` | 控制面已有时沿用；否则取合法的请求头值；仍为空则生成 UUIDv7。同一个值写入 gatewayd 请求头与错误体 `request_id` |
| `Idempotency-Key` | 非空时原样转发，交给 P08 幂等；controld 不缓存响应 |
| `Accept-Encoding` | 固定 `identity`；传输层压缩会破坏 SSE 字节流 |

响应：gatewayd 的状态码、响应体与端到端响应头逐项回传，包括 402 的
`X-Credits-Remaining`、429 的 `Retry-After` / `X-RateLimit-*` / `X-Quota-Daily-*`。
`stream=true` 时：

| 响应头 | 值 |
| :--- | :--- |
| `Content-Type` | `text/event-stream; charset=utf-8` |
| `Cache-Control` | `no-cache` |
| `X-Accel-Buffering` | `no` |
| `Content-Length` | 删除（分块传输） |

SSE 用 32 KiB 缓冲逐块读写，每块写后调用 `http.Flusher`；controld 的
`WriteTimeout` 是 30s，处理器在入口用 `http.ResponseController.SetWriteDeadline(time.Time{})`
只清掉本请求的写截止时间，不改变其他控制面路由的 30s 上限。

controld 侧错误码：

| 错误码 | HTTP | 触发条件 |
| :--- | :---: | :--- |
| `session_expired` | 401 | 无 `sid`、签名非法、K20 不存在 |
| `permission_denied` | 403 | 角色缺少 `key.read`；CSRF 校验失败 |
| `invalid_request_body` | 400 | 外层不是 JSON 对象；`key_id ≤ 0`；`body` 为空 |
| `request_too_large` | 413 | 外层请求体超过上限 |
| `key_not_found` | 404 | `key_id` 不属于当前用户，或行不存在 / 已软删 |
| `resource_state_conflict` | 409 | Key 状态不是 `ENABLED`；message 为 `当前状态不允许该操作：<status>`，hint 指向密钥管理页 |
| `key_expired` | 401 | `expire_at` 不晚于当前时间；message 为 `密钥已于 <RFC3339> 过期` |
| `config_unavailable` | 503 | `INTERNAL_CALL_SECRET` 缺失或短于 32 字节、`GATEWAY_INTERNAL_URL` 缺失/非绝对 URL、分组倍率 > 10000、签名前声明校验失败 |
| `upstream_all_failed` | 502 | 连接 gatewayd 失败或响应头前断开；`retry_after_seconds=0` |

gatewayd 返回的业务错误原样透传，不在 controld 重新映射：
`ip_not_allowed`（403）、`model_not_authorized` / `model_not_in_group`（403）、
`request_in_flight`（409）、`insufficient_credits`（402）、`rpm_limit_exceeded` /
`concurrency_limit_exceeded`（429）、`upstream_all_failed`（502）。

### 内网入口

`POST /internal/v1/chat/completions` 只存在于 gatewayd 的容器网络：

| 门 | 实现 |
| :--- | :--- |
| 端口 | compose 不新增 ports 映射；现有 `18080:8080` 只到公网 `/v1/` 路由 |
| 反向代理 | `deploy/nginx/gateway.conf` 的 `location /internal/` 返回 404 |
| 路由 | `INTERNAL_CALL_SECRET` 短于 32 字节时不注册 `POST /internal/v1/chat/completions`，启动日志 warn |

签名协议（段 1 唯一定义）：

```
canonical = "v1\n" + ts_sec + "\n" + body_sha256_hex + "\n" +
            user_id + "\n" + key_id + "\n" + key_name + "\n" +
            group_code + "\n" + group_ratio_bp + "\n" +
            ip_rule_ver + "\n" + key_expiry_ms
sig = base64(HMAC-SHA256(INTERNAL_CALL_SECRET, canonical))
X-Yunshu-Internal-Sig: v1:<ts_sec>:<sig>
```

| 项 | 约束 |
| :--- | :--- |
| `body_sha256_hex` | 请求体的 SHA-256 小写十六进制；改一字节即验签失败 |
| 时间窗 | `ts_sec` 与 gatewayd 时钟（Redis 校正）偏差 ≤ 60s；超出 401 |
| 比较 | `hmac.Equal` 常量时间；`sig ==` 形式的字符串比较零命中 |
| 失败响应 | 一律 401 `invalid_api_key`，不区分 MAC 错误、时间戳过期、头缺失 |
| 身份字段 | `X-Yunshu-Internal-User-Id`、`X-Yunshu-Internal-Key-Id`、`X-Yunshu-Internal-Key-Name`（base64url 传输）、`X-Yunshu-Internal-Group-Code`、`X-Yunshu-Internal-Group-Ratio-Bp`、`X-Yunshu-Internal-Ip-Rule-Ver`、`X-Yunshu-Internal-Key-Expiry-Ms`；逐字段单值，重复头拒绝 |
| `key_id` | 真实 `t_api_key.id`，`0` / `-1` 一律 401；注入后由 P06/P07/P08/P10/P11/模型列表/日志/并发槽使用 |
| 验签顺序 | P02 读体后立即验签，**先于 P04**；未通过签名的请求拿不到解析诊断 |
| Key 状态 | 非 `ENABLED` 或已过期由 controld 直接拒绝，gatewayd 不重复查主库（无主库连接） |

`Auth` 注入后流水线装配为 P06 → P07 → P08 → P08.5 → P12（P13 熔断状态在
RouterStage 内读取）→ P09 → P11 → P10 → P14，唯一缺席的是 P05：
`AuthStage.Run` 在 `c.Auth != nil` 时直接返回，避免用 K13 覆盖已验签的声明。

### 已知边界：P06 看到的客户端 IP

`/internal/` 的 TCP 对端是 controld 容器，gatewayd 按 `r.RemoteAddr` 取 IP；
协议不含客户端 IP 字段，controld 也没有可信的 XFF 来源，因此**不转发浏览器 IP**。
Key 开启 `ip_whitelist_mode=ON` 时，白名单必须包含 controld 所在网段（compose 为
`backend` 网络），否则调试台调用返回 403 `ip_not_allowed`。这是 IP 白名单与调试台
的既有冲突，段 1 不做绕过。

### 部署变量

| 变量 | 进程 | 说明 |
| :--- | :--- | :--- |
| `INTERNAL_CALL_SECRET` | gatewayd + controld | 同值，≥ 32 字节，不得与 `SESSION_SECRET` / `CHANNEL_KEY_KEK` / `LOG_BODY_KEK` 复用；controld 检测到复用会记 Error 并禁用转发 |
| `GATEWAY_INTERNAL_URL` | controld | 默认 `http://gatewayd:8080`；缺失或非绝对 URL 时端点返回 503 |

## 错误码映射

只列控制面会返回的码。完整 76 码见 `docs/error-codes.md`。

| 错误码 | HTTP | type | 控制面场景 |
| :--- | :---: | :--- | :--- |
| `invalid_credentials` | 401 | `authentication_error` | 登录账号/密码错误；改密时旧密码错误 |
| `session_expired` | 401 | `authentication_error` | 无 `sid`、签名非法、K20 不存在、超过 8 小时绝对上限、改密后旧 sid |
| `account_disabled` | 403 | `authorization_error` | 登录或会话加载时 `t_user.status=DISABLED` |
| `account_frozen_due_to_overdraft` | 402 | `billing_error` | `t_user.status=FROZEN_OVERDRAFT` |
| `permission_denied` | 403 | `authorization_error` | 角色缺少路由声明的权限点；CSRF 双提交校验失败 |
| `account_exists` | 409 | `conflict_error` | 注册时邮箱或手机号已存在 |
| `account_not_verified` | 403 | `authorization_error` | 未实名账号领取体验积分 |
| `invalid_request_body` | 400 | `invalid_request_error` | 请求体不是合法 JSON、字段类型/长度/取值不合法 |
| `invalid_query_parameter` | 400 | `invalid_request_error` | `page`/`page_size`/`from`/`to`/`sort`/`order` 非法，审计筛选值超长 |
| `unknown_endpoint` | 404 | `invalid_request_error` | 未注册的 `/api/*` 路径 |
| `resource_not_found` | 404 | `invalid_request_error` | 运营后台按 id 寻址的行不存在或已删除 |
| `key_not_found` | 404 | `invalid_request_error` | 控制台不存在或不属于当前账号的 Key |
| `channel_not_found` | 404 | `invalid_request_error` | 运营后台渠道 id 不存在 |
| `channel_in_use` | 409 | `conflict_error` | 删除仍被路由引用的渠道 |
| `key_limit_reached` | 409 | `conflict_error` | 免费档活跃 Key 达到上限 |
| `resource_state_conflict` | 409 | `conflict_error` | 订单/退款/提额单/模型上架状态非法流转 |
| `trial_already_claimed` | 409 | `conflict_error` | 设备、手机、IP 段或账号已领取体验积分 |
| `invoice_already_issued` | 409 | `conflict_error` | 同一订单重复开票 |
| `price_conflict` | 409 | `conflict_error` | 同一模型存在多处不一致的价格来源 |
| `request_in_flight` | 409 | `conflict_error` | 相同幂等键的首次请求仍在处理 |
| `config_value_invalid` | 400 | `invalid_request_error` | 运行时配置值不符合命名空间/类型/范围 |
| `config_unavailable` | 503 | `service_unavailable_error` | 配置快照缺失或发布失败 |
| `auth_unavailable` | 503 | `service_unavailable_error` | Redis 不可用导致会话或登录计数无法读写 |
| `login_locked` | 429 | `rate_limit_exceeded` | 账号密码失败次数达到阈值后的临时锁定；`retry_after_seconds`/`reset_at` 为剩余锁定秒数与结束时刻 |
| `rpm_limit_exceeded` | 429 | `rate_limit_exceeded` | 同一 IP 的登录失败次数达到阈值；锁定期内该地址下的所有账号都返回此码 |
| `internal_error` | 500 | `internal_error` | 未预期错误；响应体不含堆栈 |

`rate_limit_exceeded` 是 error **type**，不是 76 码表里的 code。登录的两个失败预算按维度分码：

| 维度 | 阈值 | Redis 键 | code | 客户端应做的事 |
| :--- | :--- | :--- | :--- | :--- |
| 账号 | 5 次 / 15 分钟 | K28 `auth:fail:acct:{sha256(identifier)}` | `login_locked` | 等锁定结束；忘记密码走找回（待开放） |
| IP | 20 次 / 10 分钟 | K29 `auth:fail:ip:{sha256(ip)}` | `rpm_limit_exceeded` | 等待后重试，或更换网络 |

两个维度同时触发时按剩余 TTL 较长者返回。账号锁定不能再借用 `rpm_limit_exceeded`：该码表达「请求频率超限」，客户端会降频重试，而账号锁定期间即使密码正确也继续 429。

## 会话与 CSRF

| 项 | 值 |
| :--- | :--- |
| 会话 Cookie | `sid`，值 `{sid}.{iat}.{HMAC-SHA256(secret, sid|iat)}` |
| Cookie 属性 | `HttpOnly`、`Secure`、`SameSite=Lax`、`Path=/`、`Max-Age=8h` |
| CSRF Cookie | `csrf`，值 `hex(HMAC-SHA256(SESSION_SECRET, "csrf|" + sid))` |
| CSRF Cookie 属性 | **非 HttpOnly**、`Secure`、`SameSite=Lax`、`Path=/`、`Max-Age=8h` |
| CSRF 请求头 | `X-CSRF-Token`，值必须等于 `csrf` cookie，且等于服务端按当前 sid 重新计算的值 |
| Redis 会话键 | K20 `sess:admin:{sid}` → uid，TTL 2h 滑动 |
| Redis 会话索引 | K30 `sess:admin:uid:{uid}` → SET(sid)，TTL 与 K20 同步 |
| 滑动续期上限 | 8h（从签发时刻算，不随请求延长） |
| 登录成功 | 清空 K28 账号失败计数；写 K20 + K30；下发 `sid` 与 `csrf`；响应体带 `csrf_token` |
| 注册成功 | 同登录成功 |
| `GET /api/v1/auth/me` | 重新下发 `csrf` cookie；响应头 `X-CSRF-Token` 与响应体 `csrf_token`（在 `CurrentUser` 字段之外追加）返回同一 token |
| 登出 | 删除 K20、从 K30 `SREM`、清除 `sid` 与 `csrf` cookie；旧 token 随旧 sid 一起失效 |
| 改密 | `POST /api/v1/auth/password`，验证旧密码后更新 bcrypt 哈希，删除该 uid 的全部 K20 与 K30；旧 sid 再请求返回 401 `session_expired` |

### 验证码（`POST /api/v1/auth/verification-code`）

无会话，body `{identifier, purpose}`，`purpose ∈ {login, register, reset}`。
`bind` **不是**该接口的合法值（传 `bind` 返回 `400`，`field=purpose`）：绑定发码必须
已登录，走 `POST /api/v1/auth/email/code`。

| 行为 | 说明 |
| :--- | :--- |
| 发送 | 6 位数字，存 Redis `vc:code:<purpose>:<sha256(identifier)[:32]>`，值只存 SHA-256 摘要，TTL 10 分钟 |
| 限流 | 同地址同 purpose 60 秒一次（`SETNX`）；地址日配额 `reset` = **10**、其余 20；同 IP 每小时 10（**跨用途共享**，新增用途不放大它）。超限返回 429 `rpm_limit_exceeded` 并带 `retry_after_seconds` |
| 消费 | 注册/验证码登录/重置时一次性消费；输错 5 次即作废该码；错误码统一 `invalid_credentials` + hint，不区分“过期/错误/未申请” |
| 命名空间 | `login` / `register` / `reset` / `bind` 互不通用；一个用途的码拿去另一个用途一律 401（防跨用途重放） |
| 响应 | `{channel, expires_in, resend_after}`，`channel ∈ {email, sms}`；`resend_after` 是前端倒计时的唯一来源 |
| 手机号 | `AUTH_SMS_MODE=off`（默认）时返回 400 + hint「短信验证码暂未开放，请使用邮箱；若账号未绑定邮箱，请登录后在「账户设置」绑定」 |
| 通道 | `AUTH_VERIFICATION_MODE=log`（默认，验证码打到日志，仅开发/CI）或 `smtp`（发邮件）；`smtp` 但未配 `SMTP_*` 时进程启动失败。短信由 `AUTH_SMS_MODE` 单独控制（`off`/`log`/`provider`），`provider` 缺 `ALIYUN_SMS_*` 同样拒绝启动 |

**`purpose=reset` 的「静默 200」**（零区分口径，评测红线）：

| 情况 | 状态 | 响应体 | 是否发信 |
| :--- | :--- | :--- | :--- |
| 账号可用 | 200 | `{channel, expires_in, resend_after}` | 发 |
| 账号不存在 / 已软删 / `status<>NORMAL` / 手机号匹配到多行 | 200 | **逐字节相同** | **不发**，且已存的码被丢弃 |

判定顺序固定：`decode → purpose 校验 → 归一化 → 邮箱/手机号校验 → 模式判定 →
渠道可用性 → （仅 register）已注册 409 → 发码（IP/冷却/日配额）→
（仅 reset）账号可用性 → 投递 → 200`。限流**先于**账号查询：否则查询耗时差会成为
存在性信号，且探测可以绕开限流预算。

登录 body 二选一：`{identifier, password}` 或 `{identifier, code}`；两者同时提交返回 400（避免“看起来两项都验过”）。验证码登录与密码登录共用同一套账号/IP 锁定计数。

### 自助找回密码（`POST /api/v1/auth/password/reset`）

匿名接口（无会话，**且不校验 CSRF**，理由见下面的豁免表）。body
`{identifier, code, new_password}`。

| 情况 | 状态 | code | hint |
| :--- | :--- | :--- | :--- |
| JSON 坏 / 空 body | 400 | `invalid_request_body` | — |
| `identifier` 无法归一化 | 400 | `invalid_request_body`（`field=identifier`） | — |
| `identifier` 是用户名 | 400 | `invalid_request_body`（`field=identifier`） | 用户名不能用于找回密码，请使用邮箱或手机号 |
| `new_password` 不合规（8..72 字节 / 纯空白） | 400 | `invalid_request_body`（`field=new_password`） | 与注册/改密同一套文案 |
| `code` 为空 | 400 | `invalid_request_body`（`field=code`） | — |
| `AUTH_VERIFICATION_MODE=off` | 400 | `invalid_request_body` | 本部署未启用验证码，无法自助重置，请联系管理员 |
| 码错 / 过期 / 未申请 / 尝试超限 / 账号不可用 | 401 | `invalid_credentials` | 验证码不正确或已过期 |
| Redis 不可用 / 吊销失败 | 503 | `auth_unavailable` | — |
| 成功 | **204** | — | 不签发任何 cookie |

**校验顺序不可调换**：`字段 → 密码规则 → 验码 → 写库`。弱密码不烧码；验码早于写库。

成功路径的三件事都必做：`password_hash`/`password_updated_at` 更新且
`password_is_initial=0`；`RevokeAll(uid)` 吊销该 uid 全部会话（失败 → 503，不能谎报
204）；清 K28 账号失败计数（否则「试错 5 次被锁 → 重置成功却登不进去」）。
审计与更新同事务：`module=USER`、`action=UPDATE`、`target_type=t_user`、
`target_id=<uid>`、`operator_id=<uid>`、`operator_role=<该账号 role>`、`result=OK`、
`after_json={password_updated_at, sessions_revoked:"all", via:"reset:email|reset:sms"}`，
**不含**密码、hash、验证码、明文邮箱/手机号。

### 绑定登录邮箱（`POST /api/v1/auth/email/code` + `POST /api/v1/auth/email`）

两个接口都是 `s.authed(mw.PermAccountWrite, …)`：**会话 + CSRF + `account.write`**，
与 `PATCH /auth/profile` 同一权限点（这是“改自己的资料”，不是运营侧的 `user.write`）。
目标账号一律取会话 uid，**不接受** `user_id` 参数。

| 接口 | body | 成功 |
| :--- | :--- | :--- |
| `POST /api/v1/auth/email/code` | `{email}` | `200 {channel, expires_in, resend_after}`（`purpose=bind`，绑定专用邮件正文） |
| `POST /api/v1/auth/email` | `{email, code}` | `200 {user}`（含新 `email`；**不吊销会话**） |

| 情况 | 状态 | code | hint |
| :--- | :--- | :--- | :--- |
| 邮箱格式非法（含手机号/用户名形态） | 400 | `invalid_request_body`（`field=email`） | — |
| 当前账号 `email` 非空 | 400 | `invalid_request_body`（`field=email`） | 当前账号已绑定登录邮箱；更换邮箱请提交工单 |
| `AUTH_VERIFICATION_MODE=off` | 400 | `invalid_request_body` | 本部署未启用验证码，无法验证邮箱 |
| 邮箱已被其他账号占用 | 409 | `account_exists` | 该邮箱已被使用（**不发信**） |
| 60s 冷却 / 地址日配额 10 / **账号日配额 5** / IP 小时 10 | 429 | `rpm_limit_exceeded` | 按命中维度给 hint |
| 码错 / 过期 / 未申请 / 超限 | 401 | `invalid_credentials` | 验证码不正确或已过期 |
| 唯一键冲突（`uk_user_email`，1062）或 `RowsAffected=0` | 409 | `account_exists` | 该邮箱已被使用 |
| 投递失败 | 503 | `auth_unavailable` | 验证码邮件发送失败，请稍后重试 |
| DB 错误 | 500 | `internal_error` | — |

写入语义（**不允许**覆盖已有邮箱）：

```sql
UPDATE t_user SET email = ?, updated_at = ?
 WHERE id = ? AND deleted_at IS NULL AND (email IS NULL OR email = '')
```

审计同事务：`module=USER`、`action=UPDATE`、`target_type=t_user`、`target_id=<uid>`、
`after_json={"email_changed":true,"email":"a***@x.com"}`（**不含明文地址**）。

不新增 `email_verified_at`：写入前必须消费 `purpose=bind` 的码，所以「`email` 非空」恒等于
「已验证」；再加一列只会造出第三种状态与一次回填。


### 找回密码与绑定邮箱的 `hint` 口径

本增量**零新增错误码**，只登记新场景下 `message`/`hint` 的取值，让 handler、前端文案与测试断言
对齐同一句话。（`docs/error-codes.md` 由生成器输出、不得手工编辑，所以口径写在这里。）

| 场景 | `code` / HTTP | `message` | `hint` |
| :--- | :--- | :--- | :--- |
| 手机号发码，`AUTH_SMS_MODE=off`（默认） | `invalid_request_body` / 400（`field=identifier`） | 默认 | 短信验证码暂未开放，请使用邮箱；若账号未绑定邮箱，请登录后在「账户设置」绑定 |
| 重置密码验码失败（码错/过期/未申请/账号不可用） | `invalid_credentials` / 401 | 验证码不正确或已过期 | 请确认 6 位数字是否输完整；已过期就重新获取（尝试超限时改「验证码错误次数过多，请重新获取」） |
| 重置密码但部署未启用验证码 | `invalid_request_body` / 400 | 默认 | 本部署未启用验证码，无法自助重置，请联系管理员 |
| 重置密码的 `identifier` 是用户名 | `invalid_request_body` / 400（`field=identifier`） | 默认 | 用户名不能用于找回密码，请使用邮箱或手机号 |
| 绑定邮箱时目标地址已被占用 | `account_exists` / 409 | 该邮箱已被使用 | 不发信。调用者已登录，且 `purpose=register` 早已匿名披露同一事实，不构成新增枚举面 |
| 绑定邮箱时当前账号已有邮箱 | `invalid_request_body` / 400（`field=email`） | 默认 | 当前账号已绑定登录邮箱；更换邮箱请提交工单 |
| 绑定邮箱但部署未启用验证码 | `invalid_request_body` / 400 | 默认 | 本部署未启用验证码，无法验证邮箱 |
| 短信通道未配置 / 投递失败 | `auth_unavailable` / 503 | 默认 | 短信通道未配置，请联系管理员 ／ 验证码短信发送失败，请稍后重试 |
| 邮件通道未配置 / 投递失败 | `auth_unavailable` / 503 | 默认 | 邮件通道未配置，请联系管理员 ／ 验证码邮件发送失败，请稍后重试 |

`auth_unavailable` 的 `code`/`type`/可重试性不变，上表只改 hint 文案；`rpm_limit_exceeded`（429）
沿用既有按维度给 hint 的做法，不新增文案族。

### 登录标识（identifier）

`POST /api/v1/auth/login` 的 `identifier` 接受**邮箱、手机号或运营账号用户名**三类，判定顺序固定、不可配置，客户端只提交这一个字段（不存在 `identifier_kind` 参数）：

| 顺序 | 判定 | 归一化 | 落的列 |
| :--- | :--- | :--- | :--- |
| 1 | 含 `@` | `TrimSpace` → 全角转半角 → 小写；沿用既有邮箱校验（≤254、单 `@`、域名含点、无控制字符） | `t_user.email` |
| 2 | 去分隔符（空格/`-`/`(`/`)`/`+`/`.`/换行）后**全为数字且 6..20 位** | 去掉分隔符与前缀 `+`（与本次改动前逐字一致） | `t_user.phone` |
| 3 | 其余 | 小写后匹配 `^[a-z][a-z0-9._-]{2,31}$` | `t_user.username` |

要点：

- **用户名对全部账号可选**：内置超管由引导写入；普通客户可在账户设置里自行设置，也可以留空。留空（`username IS NULL`）时只能用邮箱/手机号登录。
- 用户名规则：3..32 字节、首字节必须是 ASCII 字母、只允许 `a-z 0-9 . _ -`。由此**全数字用户名被禁止**，所以用户名永远不会与手机号歧义。大小写不敏感（归一化为小写后入库与查询）。
- 第三类之外（例如 `ab`、`_admin`、`1admin`、`admin$`、含中文）一律 `401 invalid_credentials`，与“账号不存在”不可区分；不新增错误码。
- **注册 / 发验证码 / 运营建号（`POST /api/admin/users`）不接受用户名**：这三条路径都要能向账号绑定的地址投递验证码或找回链接，用户名账号没有地址。传入用户名返回 `400 invalid_request_body` + field `identifier`。用户名只能由本人通过 `PATCH /api/v1/auth/profile` 设置。
- 锁定不因标识类型而豁免：K28 的键是 `sha256(归一化后的值)`，`admin` 与任何邮箱/手机号取值不同，但阈值（5 次 / 15 分钟）与 IP 维度（20 次 / 10 分钟）完全相同。

### 自助资料（`PATCH /api/v1/auth/profile`）

| 字段 | 语义 |
| :--- | :--- |
| `nickname` | 必填。去首尾空格，按 rune ≤ 64，不得含控制字符；空串表示清空（显示名回退到用户名 → 邮箱 → 手机号） |
| `username` | 可选。**字段缺省 = 不改动**；`""` = 清空；非空按上面的用户名校验并小写归一 |

- 用户名受 `uk_user_username` 唯一约束，重复返回 `409 resource_state_conflict`（“该用户名已被占用”）。
- **清空保护**：账号既无邮箱也无手机号时不允许清空用户名（返回 `400`），否则该账号将没有任何登录标识——内置超管正是这种账号。
- 改用户名不影响当前会话，因为会话只绑 uid。

### 初始口令提醒（`password_warning`）

- `t_user.password_is_initial = 1` 表示“仍在使用开通时下发的口令”。**只有两处会置 1**：启动引导（`internal/control/bootstrap`）与 DBA 直改库（运维手册 §9.3）。用户能自助完成的两条改密路径——`POST /api/v1/auth/password` 与 `POST /api/v1/auth/password/reset`——成功时都在同一事务里清零：用户自己设的口令再弹“仍在使用初始密码”是事实错误。
- 登录、`GET /api/v1/auth/me`、`PATCH /api/v1/auth/profile` 的 `user` 对象都会带 `password_warning: true|false`。
- **它只是提醒，不拦截**：账号照常登录、照常访问全部接口；前端在运营后台顶部显示一条“请尽快修改密码”的横幅，改密后自动消失。
- 存量账号升级后一律为 `0`（不凭空弹提醒）。

### 内置超级管理员（admin）

| 项 | 值 |
| :--- | :--- |
| 创建者 | `controld` 启动引导（`internal/control/bootstrap`），**只有控制面进程**执行；`gatewayd` / `jobsd` 不做任何引导 |
| 账号 | `ADMIN_BOOTSTRAP_USERNAME`，默认 `admin`；`role=SUPER_ADMIN`、`status=NORMAL`、`nickname=超级管理员`（控制台显示名取自它，该账号 `email`/`phone` 均为 NULL） |
| 初始口令 | `ADMIN_BOOTSTRAP_PASSWORD`，默认 `12345678`（**公开弱口令**，仅用于首次部署登录） |
| 幂等 | 用 `uk_user_username` 唯一键判“已存在”：先 INSERT、捕 1062，已存在时**不做任何修改**（不改角色、不改状态、不重置口令），记一条 INFO；若唯一键被**软删除**账号占用则记 WARN「需人工处理」且不复活该行（唯一键不含 `deleted_at`，登录侧带 `deleted_at IS NULL` 永远查不到它） |
| 开关 | `ADMIN_BOOTSTRAP_DISABLED=true` 完全跳过（存量环境已有超管时使用） |
| 失败处置 | **两档**：配置非法（用户名/口令不合规、开关非布尔）→ 进程**拒绝启动**；一切运行时 SQL 错误（含 1054「迁移 0027 未应用」）→ 只打 ERROR、**进程继续服务**，详见 `docs/ops-runbook.md` §9.5 |
| 首登后 | 登录响应带 `password_warning: true`，运营后台顶部显示“请尽快修改密码”横幅（**只提醒，不拦截**）；`POST /api/v1/auth/password` 改密成功后标记清零、横幅消失，同时吊销该 uid 的全部会话 |
| 审计 | 创建成功时写 1 条 `t_admin_audit_log`：`operator_id=0`、`operator_role=SYSTEM`、`module=USER`、`action=CREATE`、`target_type=t_user`、`after_json` 里的 identifier 已脱敏 |

`SUPER_ADMIN` 是唯一持有 `admin.all` 的角色，因此这是唯一能用 `POST /api/admin/users`、角色变更等运营接口的账号。

CSRF 校验只作用于**会话鉴权**的状态变更方法：`POST` / `PATCH` / `PUT` / `DELETE`，且路径以 `/api/` 开头。豁免：

| 路径 | 原因 |
| :--- | :--- |
| `POST /api/v1/auth/login` | 无会话；token 无从派生 |
| `POST /api/v1/auth/register` | 无会话；token 无从派生 |
| `POST /api/v1/auth/verification-code` | 无会话；token 无从派生（注册前、登录前都要能调用） |
| `POST /api/v1/auth/password/reset` | 无会话（`public()` 路由，结构上不进 CSRF 链）。伪造它能达成什么：伪造**受害者**的重置需要受害者的 6 位验证码（攻击者拿不到）；用受害者浏览器重置**攻击者自己**的账号，结果只是攻击者自己的口令变了。因此 CSRF 在这里不增加任何安全性，而“有会话就校验”的分支会让「已登录用户在公共设备上重置」出现无法解释的 403 |
| `POST /api/v1/pay/callback/*` | 公开回调，认证靠请求体 HMAC，不依赖浏览器会话 |

CSRF 失败返回 403 `permission_denied`，不是 400 `invalid_request_body`：请求体本身没有错，改 body 也修不好；缺的是“请求来自本站”的证明。客户端处理方式是刷新页面重新获取 `csrf` cookie，并把当前 token 放进 `X-CSRF-Token`。

# Schema 总表

库：`yunshu`（MySQL 8.0，`utf8mb4` / `utf8mb4_0900_as_cs`）。迁移文件在 `internal/platform/migrate/sql/mysql/`，只允许显式 SQL，无 ORM 自动迁移。

## 表计数口径

| 口径 | 数量 | 说明 |
| :--- | ---: | :--- |
| TDS §1.2 分层清单 | 46 | 只覆盖 §1.2 表格内列出的表，不含 `t_config_publish` |
| T02 实际建表（0001–0006） | 47 | 0002 组按任务卡另含 `t_config_publish`（TDS §1.8 `2866` 行定义） |
| T03 实际建表（0012 / 0014 / 0015 / 0016 / 0017） | 7 | `t_content_policy_hit`、`t_arrears`、`t_ticket`、`t_ticket_reply`、`t_review_task`、`t_upstream_bill`、`t_channel_dedicated_user` |
| T09 实际建表（0021） | 2 | `t_job_lock`、`t_reconcile_diff` |
| T24 段 2 实际建表（0025） | 2 | `t_balance_adjust`、`t_announcement_model` |
| T25 实际建表（0022） | 1 | `t_request_body` |
| T33 实际建表（0026） | 1 | `t_integration` |
| 当前业务表总量 | 60 | 47 + 7 + 2 + 2 + 1 + 1；0010 / 0011 / 0013 / 0020 / 0023 / 0024 只改列或加索引，0017 另建 1 表（已计入 7） |
| 当前 `information_schema.tables` | 61 | 60 张业务表 + `schema_migrations` |
| 当前迁移条数 | 24 | 0001–0006（6）、0009（1）、0010–0020（11）、0021（1）、0022（1）、0023（1）、0024（1）、0025（1）、0026（1）；0007 / 0008 永久空缺，不计 |

任务卡 §1 / §7 A2 写「46 张」，与卡内 §4 逐表清单（8 + 11 + 26 + 2 = 47）相差 1；差的是 `t_config_publish`。本表按逐表清单登记，未删表凑数。`information_schema` 计数含迁移记录表，因此比业务表多 1。

## 表 → 任务卡映射

| 任务卡 | 表 | 数量 |
| :--- | :--- | ---: |
| T02 | `t_user`、`t_verify_record`、`t_device_fingerprint`、`t_verification_code`、`t_api_key`、`t_api_key_model`、`t_api_key_ip_rule`、`t_api_key_audit`、`t_protocol`、`t_channel`、`t_model`、`t_model_alias`、`t_model_capability`、`t_route`、`t_price_rule`、`t_plan`、`t_config_item`、`t_config_runtime`、`t_config_publish`、`t_price_group`、`t_price_group_scope`、`t_credit_account`、`t_credit_batch`、`t_ledger_entry`、`t_prehold_hold`、`t_settle_task`、`t_recharge_order`、`t_refund_order`、`t_redeem_batch`、`t_redeem_code`、`t_invoice_apply`、`t_quota_apply`、`t_usage_record`、`t_upstream_attempt`、`t_idempotency_record`、`t_rate_limit_policy`、`t_user_daily_quota`、`t_metric_minute`、`t_metric_daily`、`t_metric_model_daily`、`t_metric_channel_health`、`t_admin_audit_log`、`t_risk_rule`、`t_risk_hit_log`、`t_announcement`、`t_announcement_read`、`t_notification` | 47 |
| T03 | `t_content_policy_hit`、`t_arrears`、`t_ticket`、`t_ticket_reply`、`t_review_task`、`t_upstream_bill`、`t_channel_dedicated_user` | 7 |
| T09 | `t_job_lock`、`t_reconcile_diff` | 2 |
| T24 段 2 | `t_balance_adjust`、`t_announcement_model` | 2 |
| T25 | `t_request_body` | 1 |
| T33 | `t_integration` | 1 |

## 编号占用

| 编号 | 内容 | 状态 |
| :--- | :--- | :--- |
| 0001 | 账号与凭据（8 表） | 已落盘 |
| 0002 | 配置驱动层（11 表） | 已落盘 |
| 0003 | 资金 / 计量 / 限流 / 指标 / 运营（26 表） | 已落盘 |
| 0004 | `t_config_item` 加 `scope` / `scope_id`（Expand） | 已落盘 |
| 0005 | `t_config_item` 唯一键换为 `uk_cfg_ns_key_scope`（Contract） | 已落盘 |
| 0006 | 价格分组两表 + `t_api_key.group_code` | 已落盘 |
| 0007 | `t_model.enabled → listing_state` Expand | 永久预留，不创建 |
| 0008 | `t_model` Contract 删旧列 | 永久预留，不创建 |
| 0009 | `t_plan` 五档种子 | 已落盘 |
| 0010 | 幂等 Expand：`t_idempotency_record` 加 `body_hash` + `uk_idem_key` | 已落盘（T03） |
| 0011 | `t_user.phone_hmac` + `uk_user_phone_hmac`，`phone` 去唯一键 | 已落盘（T03） |
| 0012 | `t_content_policy_hit` 建表 | 已落盘（T03） |
| 0013 | 幂等 Contract：删除 `uk_idem` | 已落盘（T03），**不可回滚** |
| 0014 | `t_arrears` 建表 | 已落盘（T03） |
| 0015 | `t_ticket` / `t_ticket_reply` / `t_review_task` 建表 | 已落盘（T03） |
| 0016 | `t_upstream_bill` 建表 | 已落盘（T03） |
| 0017 | `t_model.recommend_weight` + `idx_model_recommend`；`t_channel` 专属两列；`t_channel_dedicated_user` 建表 | 已落盘（T03） |
| 0018 | 34 配置键 + 3 价格分组 + 3 协议 + 5 档限流策略种子 | 已落盘（T03） |
| 0019 | 本地联调种子（仅 `ENV=local`） | 已落盘（T03），其他环境跳过并打 WARN |
| 0020 | 幂等租约 Expand：`t_idempotency_record` 加 `fence_token` / `lease_expires_at`（只加不删） | 已落盘（T06 遗留 #2，方案 A） |
| 0021 | `t_job_lock` / `t_reconcile_diff` 建表 | 已落盘（T09） |
| 0022 | `t_request_body` 建表 | 已落盘（T25） |
| 0023 | `t_admin_audit_log.result` 放宽到 `varchar(24)` | 已落盘（E-28） |
| 0024 | `t_admin_audit_log` 补 `idx_aal_created (created_at, id)` 与 `idx_aal_action_created (action, created_at, id)` | 已落盘（T11 第二段） |
| 0025 | `t_balance_adjust` / `t_announcement_model` 建表 | 已落盘（T24 段 2） |
| 0026 | `t_integration` 建表 + 5 个工具种子（`enabled=0`、模板 NULL） | 已落盘（T33 段 1） |
| 0027 | `t_user` 加 `username varchar(32) NULL`（`AFTER user_no`）+ `uk_user_username`；存量行不回填 | 已落盘（INC-admin-bootstrap） |
| 0028 | `t_user` 加 `password_is_initial tinyint(1) NOT NULL DEFAULT 0`（`AFTER password_updated_at`）：仅引导置 1，改密清零，只驱动前端提醒 | 已落盘（INC-admin-bootstrap） |

> **需回写 TDS §8.6**：0018 / 0019 / 0020 / 0021 是 TDS §8.6 裁定表之外的新增占用，由交付总监统一改源文档；实现方未改 TDS。

## 业务表

「迁移」列是创建该表的迁移；后被 `0004` / `0006` 改过结构的表在备注中标出。唯一键只列业务唯一键，`PRIMARY KEY(id)` 不重复登记。

| 表 | 迁移 | 分层 | 关键唯一键 |
| :--- | :--- | :--- | :--- |
| `t_user` | 0001 | 账号 | `uk_user_no`、`uk_user_email`、`uk_user_idno`；`0011` 起手机号唯一性迁到 `uk_user_phone_hmac`（**`phone_hmac` 由写路径维护**，见下节）；`0027` 起登录用户名唯一性由 `uk_user_username` 承担（列可空，NULL 可重复；用户名可由本人在账户设置里设置或清空）；`0028` 起 `password_is_initial` 标记初始口令（只用于提醒） |
| `t_verify_record` | 0001 | 账号 | — |
| `t_device_fingerprint` | 0001 | 账号 | `uk_fp_hash_user` |
| `t_verification_code` | 0001 | 账号 | — |
| `t_api_key` | 0001 | 凭据 | `uk_key_hash`；`0006` 加 `group_code` / `idx_key_group` |
| `t_api_key_model` | 0001 | 凭据 | `uk_akm_key_model` |
| `t_api_key_ip_rule` | 0001 | 凭据 | `uk_akip_key_cidr` |
| `t_api_key_audit` | 0001 | 凭据 | — |
| `t_protocol` | 0002 | 配置 | `uk_proto_code` |
| `t_channel` | 0002 | 配置 | `uk_channel_name`；`0017` 加 `dedicated_scope_mode` / `fallback_allowed` |
| `t_model` | 0002 | 配置 | `uk_model_name`；`0017` 加 `recommend_weight` / `idx_model_recommend` |
| `t_model_alias` | 0002 | 配置 | `uk_alias_name` |
| `t_model_capability` | 0002 | 配置 | `uk_mc_model_cap` |
| `t_route` | 0002 | 配置 | `uk_route_model_channel_proto_upstream` |
| `t_price_rule` | 0002 | 配置 | — |
| `t_plan` | 0002 | 配置 | `uk_plan_code` |
| `t_config_item` | 0002 | 配置 | `0005` 后为 `uk_cfg_ns_key_scope` |
| `t_config_runtime` | 0002 | 配置 | `uk_cr_runtime_key` |
| `t_config_publish` | 0002 | 配置 | `uk_cp_publish_id` |
| `t_price_group` | 0006 | 配置 | `uk_pg_code` |
| `t_price_group_scope` | 0006 | 配置 | `uk_pgs_group_scope` |
| `t_credit_account` | 0003 | 资金 | `uk_ca_user` |
| `t_credit_batch` | 0003 | 资金 | `uk_cb_no` |
| `t_ledger_entry` | 0003 | 资金 | `uk_le_idem`、`uk_le_entry_no` |
| `t_prehold_hold` | 0003 | 资金 | `uk_ph_request` |
| `t_settle_task` | 0003 | 资金 | `uk_st_request_type` |
| `t_recharge_order` | 0003 | 资金 | `uk_ro_order_no`、`uk_ro_pay_txn` |
| `t_refund_order` | 0003 | 资金 | `uk_rf_no` |
| `t_redeem_batch` | 0003 | 资金 | — |
| `t_redeem_code` | 0003 | 资金 | `uk_rc_hash` |
| `t_invoice_apply` | 0003 | 资金 | `uk_ia_no` |
| `t_quota_apply` | 0003 | 资金 | `uk_qa_no` |
| `t_usage_record` | 0003 | 计量 | `uk_ur_request` |
| `t_upstream_attempt` | 0003 | 计量 | `uk_ua_request_attempt` |
| `t_idempotency_record` | 0003 | 计量 | `0010` 加 `uk_idem_key (scope, api_key_id, idem_key)`；`0013` 删除 `uk_idem`，现仅 `uk_idem_key` |
| `t_rate_limit_policy` | 0003 | 限流 | `uk_rlp_scope_active` |
| `t_user_daily_quota` | 0003 | 限流 | `uk_udq_user_date` |
| `t_metric_minute` | 0003 | 指标 | `uk_mm_bucket_scope` |
| `t_metric_daily` | 0003 | 指标 | `uk_md_date_scope` |
| `t_metric_model_daily` | 0003 | 指标 | `uk_mmd_date_model` |
| `t_metric_channel_health` | 0003 | 指标 | `uk_mch_bucket_channel` |
| `t_admin_audit_log` | 0003 | 运营 | — |
| `t_risk_rule` | 0003 | 运营 | `uk_rr_name` |
| `t_risk_hit_log` | 0003 | 运营 | — |
| `t_announcement` | 0003 | 运营 | — |
| `t_announcement_read` | 0003 | 运营 | `uk_ar_ann_user` |
| `t_notification` | 0003 | 运营 | — |
| `t_content_policy_hit` | 0012 | 运营 | `uk_cph_request`；`matched_excerpt varchar(255)` 只存片段 |
| `t_arrears` | 0014 | 资金 | `uk_arr_request`、`uk_arr_no` |
| `t_ticket` | 0015 | 运营 | `uk_tk_no` |
| `t_ticket_reply` | 0015 | 运营 | — |
| `t_review_task` | 0015 | 运营 | `uk_rt_biz (biz_type, biz_id)` |
| `t_upstream_bill` | 0016 | 资金 | `uk_ub_channel_period_line (channel_id, bill_period, bill_no)` |
| `t_channel_dedicated_user` | 0017 | 配置 | `uk_cdu_channel_user (channel_id, user_id)` |
| `t_job_lock` | 0021 | 运维 | `uk_jl_job (job_name)` |
| `t_reconcile_diff` | 0021 | 对账 | `uk_rd_key (diff_key)`；`diff_key = sha256(biz_date|diff_type|subject)`；`idx_rd_date_type (biz_date, diff_type, status)` |
| `t_request_body` | 0022 | 计量 | `uk_rb_request_user (request_id, user_id)`；`ciphertext mediumblob` 存 `v1:` 信封密文，`body_key_id` 由 `LOG_BODY_KEK` 独立派生；`idx_rb_expire (expires_at)` 服务 10,000 行/批的保留期清理，`idx_rb_user_created (user_id, created_at)` 服务按用户审计 |
| `t_balance_adjust` | 0025 | 资金 | `uk_ba_user_pending (pending_user_id)`，生成列在 `state='PENDING'` 时等于 `user_id`，其余为 NULL；`chk_ba_amount`、`chk_ba_direction` |
| `t_announcement_model` | 0025 | 运营 | `PRIMARY KEY (announcement_id, model_id)`、`idx_am_model (model_id)` |
| `t_integration` | 0026 | 运营 | `uk_integration_tool (tool_key)`；5 行固定种子，`tool_key` 与 `web/src/content/toolIntegrations.ts` 逐字一致；`deep_link_template varchar(512)` 可空，服务端白名单占位符 `{key}` / `{base_url}` / `{model}` |

## 持有字段的迁移

| 迁移 | 改表 | 变更 |
| :--- | :--- | :--- |
| 0004 | `t_config_item` | 加 `scope varchar(16) NOT NULL DEFAULT 'GLOBAL'`、`scope_id bigint unsigned NOT NULL DEFAULT 0` |
| 0005 | `t_config_item` | `DROP INDEX uk_cfg_ns_key`，`ADD UNIQUE uk_cfg_ns_key_scope (cfg_namespace, cfg_key, scope, scope_id)` |
| 0006 | `t_api_key` | 加 `group_code varchar(32) NOT NULL DEFAULT 'standard'`、`idx_key_group (group_code, status)` |
| 0009 | `t_plan` | 插入 TRIAL / STANDARD / PRO / ENTERPRISE / FREE_DEFAULT 五行 |
| 0010 | `t_idempotency_record` | 加 `body_hash char(64) NOT NULL DEFAULT ''`、`uk_idem_key (scope, api_key_id, idem_key)`；`api_key_id` 改 `NOT NULL DEFAULT 0`；不回填 `body_hash` |
| 0011 | `t_user` | 加 `phone_hmac char(64) NULL` + `uk_user_phone_hmac`；删除 `uk_user_phone`，`phone` 保留可空 |
| 0013 | `t_idempotency_record` | `DROP INDEX uk_idem`；不可回滚 |
| 0017 | `t_model` / `t_channel` | `t_model` 加 `recommend_weight int unsigned NOT NULL DEFAULT 0` + `idx_model_recommend (recommend_weight DESC, published_at DESC)`；`t_channel` 加 `dedicated_scope_mode varchar(16) NOT NULL DEFAULT 'ALL'`、`fallback_allowed tinyint(1) NOT NULL DEFAULT 0` |
| 0018 | `t_config_item` / `t_price_group` / `t_protocol` / `t_rate_limit_policy` / `t_plan` | 34 配置键、3 分组、3 协议、5 档限流模板；`t_rate_limit_policy.daily_quota_micro` 改可空；付费三档 `invoice_supported=1` + SLA 文案 |
| 0019 | `t_channel` / `t_model` / `t_model_alias` / `t_route` / `t_price_rule` | 仅 `ENV=local`：1 渠道、2 文本模型、2 别名、2 路由、2 价目规则 |
| 0023 | `t_admin_audit_log` | `result` 改 `varchar(24) NOT NULL DEFAULT 'OK'`（E-28：`REJECTED_BY_GUARD` 17 字符超出原 `varchar(16)`）；回滚前校验超长值，有则 SIGNAL 拒绝 |
| 0024 | `t_admin_audit_log` | 加 `idx_aal_created (created_at, id)`、`idx_aal_action_created (action, created_at, id)`；只加索引，不改列、不回填；down 直接 DROP INDEX |

`t_route.region` 是枚举，只接受大写 `DOMESTIC`（冗余自 `t_channel.region`）。P12 在 `internal/gw/p12_router.go` 把非 `DOMESTIC` 的路由硬排除，写成 `cn` 会得到 `no_available_upstream`；种子与文档示例一律用 `DOMESTIC`。

`t_arrears.amount_micro` / `settled_micro` 对应 TDS §1.5 的 `due_micro` / `repaid_micro`；列名按 T03 卡落地，语义不变（欠费额 / 已抵扣额）。

`0018` 的 5 档限流策略是**模板行**：`scope_type='USER'`、`scope_id=0` 为哨兵，不对应任何真实用户；用 `effective_from` 的固定毫秒差满足唯一键。`source` 写档位标识（TRIAL / STANDARD / PRO / ENTERPRISE / FREE_DEFAULT）便于运营对照；运行时按用户惰性生成的策略行仍使用 `PLAN` / `TRIAL` / `FREE_DEFAULT`。`FREE_DEFAULT.daily_quota_micro` 为 `NULL`（无上限），不是 0。

## 全局约束

| 项 | 规则 |
| :--- | :--- |
| 字符集 | 每张表显式 `DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs` |
| 时间列 | 只用 `datetime(3)`，存 UTC；全仓 SQL 无 `timestamp` |
| 金额 | `bigint` + `_micro`；单价 `int unsigned` + `_micro_per_k`；唯一小数例外 `t_metric_daily.success_rate decimal(7,4)` |
| 人民币分 | TDS 明确定义为 `*_fen int unsigned`（`t_plan.price_fen`、`t_recharge_order.pay_amount_fen`、`t_refund_order.refund_fen` 等），按 TDS 保留 |
| 外键 | 全库不建；一致性由应用层事务保证 |
| 软删 | 参与唯一索引的 `email` / `phone_hmac` / `key_hash` / `model_name` 删除时改写为 `#del#{id}{unix_ms}`；`phone` 自 0011 起不再参与唯一索引 |
| 账本 | `t_ledger_entry` 只追加；三个唯一键 `PRIMARY KEY(id)`、`uk_le_idem`、`uk_le_entry_no` 均不含 `created_at` |

## `t_user.phone` 与 `t_user.phone_hmac` 的语义（0011 + INC-phone-hmac）

`phone` 是**明文展示列**，`phone_hmac` 是**唯一性与判重列**。两者用途不同，生命周期也不同：

| 项 | `phone` | `phone_hmac` |
| :--- | :--- | :--- |
| 内容 | E.164 去 `+`（`8613800138000`） | `lowercase_hex(HMAC-SHA256(phone, PHONE_HMAC_KEY))`，64 字符 |
| 谁写 | 注册 / 运营建号 /（未来）改号 | 与 `phone` **同一事务**由写路径计算：`auth.insertUserRow`（注册、运营建号、内置超管）、`cmd/seedctl`、回填命令 |
| 唯一性 | **无**唯一键（0011 已 `DROP uk_user_phone`） | `uk_user_phone_hmac`（全表唯一，NULL 可重复） |
| 读路径 | 登录、找回密码、绑定邮箱冲突、运营列表/详情**一律按明文 `phone` 查** | 只有体验版（`fund/trial.go`）的等值判重用它 |
| 注销 | 保留（未来 F-12 的 PII 清空任务只许置 `NULL`） | **保留，永不因注销而清空** |
| 软删 | 保留 | **改写为 `#del#{id}{unix_ms}`**，释放号段 |

**注销 = 占号，软删 = 放号**。两者不可混：

- 注销（`POST /api/v1/auth/cancel`）只改 `status/disable_source/disable_at/cancel_apply_at`，**不动任何 phone 列**。因此注销行的 `phone_hmac` 仍占着 `uk_user_phone_hmac`，同一手机号**不能再注册**（产品裁定 D1：注销即占号）。要复用号段走 `docs/ops-runbook.md` §10.4 的受控释放操作（SUPER_ADMIN 审批 + 审计行），不是注销的自动化副作用。
- 软删（`deleted_at` 非空，今天只有夹具/人工会产生）必须把 `phone_hmac` 改写成 `#del#…`，否则幽灵行永久占用号段，该号码再也注册不了。
- 明文与 hmac 可能漂移（人工改库、用错 key、半途中断）。SQL 算不了 HMAC，所以「列有值」不等于「值是对的」：唯一发现手段是 `backfill-phone-hmac verify`（重算比对，0 差异），已写进 `docs/go-live-checklist.md` 第 33 项。

`PHONE_HMAC_KEY` 是**独立于 `FP_HMAC_KEY` 的密钥**（手机号可枚举，泄漏等价于泄漏明文；设备指纹不可反查）。缺失时的行为见 `docs/ops-runbook.md` §10.5。

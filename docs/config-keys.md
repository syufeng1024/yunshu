# 配置键总表（45 键）

`t_config_item` 种子由迁移 `0018_seed_config.sql` 写入。全部为 `scope='GLOBAL'`、`scope_id=0`、`version=1`、`enabled=1`；`cfg_value` 是 JSON 列，下表「默认值」列即 JSON 字面量。

键数以 TDS §3.7 键表为准：**45 个唯一键**（原始 49 行，其中 4 行为续行注释）。TDS 正文重复写的「34 个键」是过期计数，见 `docs/tasks/errata.md` E-07。

## 基础 34 键（2026-10-23 口径）

| 配置键 | 命名空间 | 默认值 | 运行时可写白名单 | 消费方 | TDS §3.7 行号 |
| :--- | :--- | :--- | :---: | :--- | ---: |
| `billing.deduct_policy` | PRICING | `"fifo_expiry"` | 是 | P25~P27 结算扣减顺序 | 438 |
| `billing.refund_policy` | PRICING | `"by_order"` | 否 | 退款 / 回滚策略 | 439 |
| `billing.overdraft_limit_default` | PRICING | `50` | 否 | P27 透支上限（积分） | 440 |
| `billing.estimate_language_coef` | PRICING | `{"zh":1.0,"en":1.0,"code":1.0,"mixed":1.0}` | 否 | P09 token 预估 | 441 |
| `billing.prehold_ratio` | PRICING | `1.2` | 否 | P10 预扣比例 | 442 |
| `billing.prehold_max_ratio` | PRICING | `0.5` | 是 | P10 预扣上限 | 443 |
| `billing.settle_deadline_ms` | PRICING | `{"non_stream_ms":5000,"stream_ms":10000}` | 否 | P30 结算时限 | 444 |
| `billing.bonus_expire_days` | PRICING | `90` | 否 | 赠送积分批次到期 | 445 |
| `billing.long_ctx_prehold_guard_bp` | PRICING | `9000` | 否 | P09 阶梯预扣（估算落在阈值 90% 以上按阶梯价预扣） | 459 |
| `pricing.cost_source` | PRICING | `"manual"` | 否 | 毛利成本来源 | 450 |
| `pricing.tier_chain` | PRICING | `"model>channel"` | 否 | P09 取价层级 | 451 |
| `pricing.agent_markup` | PRICING | `"off"` | 否 | 代理加价（V1 恒 off） | 452 |
| `pricing.cache_billing_enabled` | PRICING | `false` | 否 | P09 缓存计价开关 | 453 |
| `pricing.min_margin_bp` | PRICING | `2500` | 否 | F16-5 单价毛利地板（毛利率口径） | 454 |
| `pricing.plan_min_margin_bp` | PRICING | `1000` | 否 | F16-5 套餐毛利地板 | 455 |
| `pricing.max_bonus_ratio_bp` | PRICING | `3000` | 否 | 赠送积分占套餐额度上限 | 457 |
| `pricing.group_switch_mode` | PRICING | `"ops_only"` | 否 | Key 分组切换（禁止用户自助） | 458 |
| `plan.default_tier` | PRICING | `"free"` | 否 | 默认套餐 | 464 |
| `plan.trial_days` | PRICING | `7` | 否 | 体验期天数 | 465 |
| `quota.daily_meter` | RATELIMIT | `"prehold"` | 是 | 日额度计量口径 | 446 |
| `quota.key_daily_mode` | RATELIMIT | `"none"` | 是 | Key 日额度模式（V1 只实现 none） | 447 |
| `quota.reset_scope` | RATELIMIT | `"account"` | 是 | 日额度重置维度 | 448 |
| `quota.reset_tz` | RATELIMIT | `"Asia/Shanghai"` | 是 | 日额度重置时区 | 449 |
| `quota.rate_limit_order` | RATELIMIT | `"concurrency>rpm>tpm>daily"` | 是 | P11 四维限流判定顺序 | 466 |
| `log.body_mode` | FEATURE | `"meta_only"` | 否 | 日志正文落库模式 | 460 |
| `log.volume_basis` | FEATURE | `"meta"` | 否 | 日志体积口径 | 461 |
| `log.retention_days` | FEATURE | `180` | 是 | 请求日志保留期 | 462 |
| `log.same_instance` | FEATURE | `true` | 否 | 日志库与主库同实例 | 463 |
| `gw.max_inflight` | UPSTREAM | `8000` | 是 | 单实例全局并发闸门 | 468 |
| `gw.drain_deadline_ms` | UPSTREAM | `120000` | 是 | 优雅退出等待上限 | 470 |
| `gw.upstream_timeout_ms` | UPSTREAM | `50000` | 是 | 单次上游超时（非流式） | 471 |
| `gw.client_timeout_budget_ms` | UPSTREAM | `120000` | 是 | 客户端总超时预算 | 476 |
| `gw.retry_max` | UPSTREAM | `1` | 否 | 最大重试次数（共 2 次尝试） | 477 |
| `quota.overdraft_warn_days` | UNASSIGNED | `7` | 否 | 负余额自动停用任务（未排期） | 467 |

## TA-1.2 追加的 11 键（2026-10-25）

| 配置键 | 命名空间 | 默认值 | 运行时可写白名单 | 消费方 | TDS §3.7 行号 |
| :--- | :--- | :--- | :---: | :--- | ---: |
| `billing.prestimate_safety_bp` | PRICING | `11000` | 否 | P09 token 预估的安全余量：预估值 ×1.10，只乘 token 数、不乘金额 | 478 |
| `pricing.tax_rate_multiplier` | PRICING | `1.00`（JSON 列归一显示 `1.0`，数值等价） | 否 | 税率占位；V1 恒 1.00，接入税制后由计价链路读取 | 456 |
| `gw.stream_first_byte_ms` | UPSTREAM | `30000` | 否 | 网关 P16 流式首字节超时（单次尝试，E-01 执行值） | 472 |
| `gw.stream_total_ms` | UPSTREAM | `600000` | 否 | 网关 P16 流式响应总时长上限 | 473 |
| `gw.settle_reserve_ms` | UPSTREAM | `15000` | 否 | P30 结算预留窗口 | 474 |
| `gw.retry_threshold_ms` | UPSTREAM | `{"unary":65000,"stream":45000}` | 否 | P16/P17 上游重试门槛，按请求流式与否取 `unary` / `stream`；JSON 双值 | 475 |
| `idem.duplicate_window_seconds` | FEATURE | `10` | 否 | 去重窗口，取值 `[0,3600]`（T41）。**两处消费、语义不同**：网关 P08 对**未带** `Idempotency-Key` 的请求按 `floor(now/window)` 自动去重，配 `0` 即**关闭自动去重**；fund 控制面写操作的幂等窗口在配 `0` 时**回落 10 秒**（不关闭）。带 `Idempotency-Key` 的请求不受本键影响 | 479 |
| `idem.lease_ttl_seconds` | FEATURE | `15` | 否 | P11 幂等租约 TTL | 480 |
| `idem.lease_renew_seconds` | FEATURE | `5` | 否 | P11 幂等租约心跳续约间隔 | 481 |
| `quota.free_max_active_keys` | RATELIMIT | `3` | 否 | 密钥创建校验 F6-7：`FREE_DEFAULT` 账户活跃 Key 上限 | 482 |
| `site.recommend_limit` | FEATURE | `8` | 否 | 官网首页「推荐模型」区块展示条数（PRD F22-6） | 483 |

## 进程环境变量（非 `t_config_item`）

下表前三个键是进程监听地址，不写入 `t_config_item`，不计入上面的 45 键。三进程共用 `GATEWAY_METRICS_TOKEN` 一个 Bearer token，隔离靠端口与网络策略。`LOG_BODY_KEK` 是正文加密密钥，同样不进入 `t_config_item`：配置表对所有加载快照的进程可读，放进去等于把密钥发给每一个进程。

| 环境变量 | 进程 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `GATEWAY_METRICS_ADDR` | gatewayd | `:9090` | 数据面 metrics 监听；保留旧键名向后兼容 |
| `CONTROL_METRICS_ADDR` | controld | `:9091` | 为空时回落 `:9091`，不再回落 `:9090` |
| `JOBSD_METRICS_ADDR` | jobsd | `:9092` | 与 `/healthz`、`/readyz` 同端口 |
| `LOG_BODY_KEK` | gatewayd、controld | 空 | `log.body_mode=opt_in` 时加密 `t_request_body.ciphertext` 的 KEK，base64 编码的 32 字节。独立于 `CHANNEL_KEY_KEK`：渠道密钥轮换不得使历史正文失效。为空或非法时进程照常启动，正文不落库（gatewayd 记 `gw_log_body_dropped_total{reason="no_kek"}`），读取端把已存正文一律按 404 处理并记 `gw_log_body_open_failed_total`；请求与账务不受影响。两进程必须配置同一值，否则写入端与读取端互相解不开 |
| `LOG_BODY_KEY_ID` | gatewayd、controld | `body-v1` | 当前正文 KEK 的版本标识，写入 `t_request_body.body_key_id`（varchar(32)，不得含 `:` 或 `,`）。不配置即沿用存量行已有的 `body-v1`，行为与改造前逐字节一致。轮换时把新 KEK 放进 `LOG_BODY_KEK` 并把本值改成新 id（如 `body-v2`） |
| `LOG_BODY_KEK_PREVIOUS` | gatewayd、controld | 空 | 已退休正文 KEK，格式 `id:base64`，多段用逗号分隔（如 `body-v1:<base64>`）。读取端按行内 `body_key_id` 选 key，因此轮换后旧行在 180 天保留期内仍可解密。**旧 key 至少保留到该 key 加密的最后一行过期**，否则那些请求的正文接口会返回 404 并记 `gw_log_body_open_failed_total`（数据仍在，只是解不开）。id 与当前 id 重复、base64 非法或长度非 32 字节时进程照常启动但正文不落库，并在启动日志打印原因 |
| `AUTH_VERIFICATION_MODE` | controld | `log` | 注册/登录验证码的投递方式：`log`（验证码打到进程日志，仅开发与 CI）、`smtp`（发邮件）、`off`（不要求验证码，端点返回 400）。配成其他值、或配 `smtp` 却没有可用 SMTP 通道时**进程拒绝启动**——否则第一次注册才会暴露，看起来像产品故障 |
| `AUTH_SMTP_CHANNEL` | controld | 见右 | 当前发信使用的 SMTP 通道名，大小写不敏感。缺省时：只有一个通道则用它；一个都没有则视为未配置 SMTP；**有多个通道却未指定则启动失败并列出候选名**——静默挑一个会让「为什么邮件从另一个账号发出」变成一下午的排查。配了它却没有任何通道时同样启动失败 |
| `SMTP_HOST` / `SMTP_PORT` | controld | 空 / `587` | 邮件中继，DEFAULT 通道写法（无前缀，等价于 `SMTP_DEFAULT_*`）。`SMTP_PORT=465` 时配 `SMTP_TLS=implicit`（连上即 TLS），587 用默认 `starttls` |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | controld | 空 | SMTP 认证凭据。用户名非空而服务器不提供 STARTTLS 时**拒绝发送**（不把凭据裸奔在明文连接上）。凭据只走 env，不进 `t_config_item`，也不出现在启动日志与错误信息里 |
| `SMTP_FROM` | controld | 空 | 信封发件人。含 CR/LF 直接启动失败（防头注入）。163/QQ 要求它与认证账号同址（否则 163 回 `553 Mail from must equal authorized user`） |
| `SMTP_TLS` | controld | `starttls` | `starttls` 或 `implicit`；其他值启动失败 |
| `SMTP_<NAME>_HOST` / `_PORT` / `_TLS` / `_USERNAME` / `_PASSWORD` / `_FROM` | controld | 同上一组 | **命名通道**：把通道名插在后缀前面即可，如 `SMTP_QQ_HOST`、`SMTP_163_PASSWORD`，通道名为 `QQ` / `163`。通道名只允许 `A-Z 0-9 _`（大小写不敏感、统一规范化为大写），`SMTP_1A-B_HOST` 这类名字直接报错。可同时声明任意多个通道，用 `AUTH_SMTP_CHANNEL` 选择，切换通道只改这一行。**「已声明」的判定**：`_HOST`/`_FROM`/`_USERNAME`/`_PASSWORD` 任一项非空；四项全空视为未声明（照抄模板里 `SMTP_PORT=587`/`SMTP_TLS=starttls` 的示例值只算样式，不算声明，不会导致启动失败），但 `_PORT`/`_TLS` 的取值仍会校验。已声明则必须填齐 HOST 与 FROM，只填一半启动失败。缺省同样是 `587` / `starttls`。启动日志打印 `smtp channels: configured=[163 QQ] selected=QQ` |

## 命名空间分布

| 命名空间 | 键数 | 明细 |
| :--- | ---: | :--- |
| PRICING | 21 | `billing.*` 10 + `pricing.*` 9 + `plan.*` 2 |
| RATELIMIT | 6 | `quota.*` 6（不含另计的 `quota.overdraft_warn_days`） |
| FEATURE | 8 | `log.*` 4 + `idem.*` 3 + `site.*` 1 |
| UPSTREAM | 9 | `gw.*` 9 |
| UNASSIGNED | 1 | `quota.overdraft_warn_days` |
| **合计** | **45** | |

`idem.*` 与 `site.recommend_limit` 按 errata E-08 归 `FEATURE`：幂等窗口与推荐位条数都是功能开关，既不决定怎么扣钱，也不决定路由与上游超时，因此不新增第六个命名空间。

白名单 12 键：`billing.deduct_policy`、`billing.prehold_max_ratio`、`gw.upstream_timeout_ms`、`gw.client_timeout_budget_ms`、`gw.max_inflight`、`gw.drain_deadline_ms`、`log.retention_days`、`quota.daily_meter`、`quota.key_daily_mode`、`quota.reset_scope`、`quota.reset_tz`、`quota.rate_limit_order`。其余 33 键写入 `/admin/config` 一律返回 400 `config_value_invalid`。

## 三条取值约定

1. **双值键用 JSON 对象承载**：`billing.settle_deadline_ms` 的 TDS 默认值是「非流式 5000 / 流式 10000」，`gw.retry_threshold_ms` 是「非流式 65000 / 流式 45000」。单值会丢掉流式口径，因此分别存 `{"non_stream_ms":5000,"stream_ms":10000}` 与 `{"unary":65000,"stream":45000}`。消费方按请求是否流式取对应字段，禁止把对象当整数读取。
2. **`quota.overdraft_warn_days` 不归属任何命名空间**：`cfg_namespace='UNASSIGNED'`。它不属于 ROUTING / RATELIMIT / PRICING / UPSTREAM / FEATURE，后台写入必须返回 `config_value_invalid`，不能「接受但无人读取」。
3. **`billing.prestimate_safety_bp` 只作用于 token 预估环节**：按万分比 11000 放大预估 token 数，不把最终金额整体 ×1.1；否则 G-02 的鉴别用例会翻转。

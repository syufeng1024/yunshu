# 错误码台账（76 条）

> 本文件由 `apierr.MarkdownTable()`（76 行码表）+ `apierr.MarkdownAvailabilitySection()`
> （末尾的平台可用性白名单）生成，**不得手工编辑**。真相源是 PRD 附录 E.1 与 `internal/apierr/codes.go`。
> 重新生成：写一段 10 行的临时程序把这两个函数的输出依次打印到本文件（临时程序不入库），或等 T15 的后台「错误码导出」接口。

## 与 PRD E.1 的关系

- HTTP 状态、`type`、可重试性、`hint` 规则、触发阶段 逐条对齐附录 E.1
- 码总数固定 **76**，`codes_test.go` 断言 `len(Specs()) == 76`
- 新增四码来自勘误 **E-21**（PRD E.1 尚未回写）：`session_expired`、`invalid_credentials`、`account_exists`、`invalid_query_parameter`；总数 65 → 69
- 新增一码来自 **T12 硬约束 2**（PRD E.1 无对应行）：`key_limit_reached`（409 `conflict_error`，不可重试）——免费档活跃 Key 已达 `quota.free_max_active_keys` 上限；总数 69 → 70
- 新增四码来自 **T15 状态类错误码收敛**（PRD E.1 无对应行）：`resource_state_conflict`、`trial_already_claimed`、`invoice_already_issued`、`price_conflict`（均 409 `conflict_error`，不可重试）。判定规则：按 hint 改请求体仍无法成功的拒绝属于状态/配额问题，必须有自己的码；T13 遗留的 12 处 `invalid_request_body` 中 10 处迁到新码，2 处保留（`amount_fen` 超过可开额度、`target_quota_micro` 超过 5 倍上限——改字段即可成功）；总数 70 → 74
- 新增一码来自 **T15 第二段**（PRD E.1 无对应行）：`resource_not_found`（404 `invalid_request_error`，不可重试）——后台详情/改/删按 id 寻址时行不存在或已软删。此前后台模型详情复用数据面的 `model_not_found`（HTTP 400），会把「行不存在」误报成「请求体有误」；渠道详情仍用更具体的 `channel_not_found`；总数 74 → 75
- 新增一码来自 **T11 第二段登录锁定**（PRD E.1 无对应行）：`login_locked`（429 `rate_limit_exceeded`，按剩余锁定秒数重试）——账号密码失败次数达到阈值后的临时锁定。此前复用 `rpm_limit_exceeded` 并用 `Message`/`Hint` 改写文案，但 `code` 语义仍是「每分钟请求数超限」，客户端会降频重试而不是等待锁定结束；同一 IP 的失败预算仍用 `rpm_limit_exceeded`，两者不合并；总数 75 → 76

## 全量清单

| `code` | `type` | HTTP | `retry_after_seconds` | 可重试 | 归属 | 触发阶段 |
| --- | --- | --- | --- | --- | --- | --- |
| `invalid_api_key` | `authentication_error` | 401 | 0 | no | BUSINESS | P05 |
| `invalid_credentials` | `authentication_error` | 401 | 0 | no | BUSINESS | 控制台登录 |
| `session_expired` | `authentication_error` | 401 | 0 | no | BUSINESS | 控制台会话 |
| `key_expired` | `authentication_error` | 401 | 0 | no | BUSINESS | P05 |
| `key_disabled` | `authorization_error` | 403 | 0 | no | BUSINESS | P05 |
| `account_disabled` | `authorization_error` | 403 | 0 | no | BUSINESS | P05 |
| `account_frozen` | `authorization_error` | 403 | 0 | no | BUSINESS | P05 |
| `ip_not_allowed` | `authorization_error` | 403 | 0 | no | BUSINESS | P06 |
| `conflicting_auth_headers` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P05 |
| `model_not_found` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P07 |
| `model_not_available` | `upstream_error` | 503 | 30 | yes | BUSINESS | P07 |
| `model_not_in_group` | `authorization_error` | 403 | 0 | no | BUSINESS | P07 |
| `model_not_authorized` | `authorization_error` | 403 | 0 | no | BUSINESS | P07 |
| `invalid_request_body` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P04 |
| `invalid_query_parameter` | `invalid_request_error` | 400 | 0 | no | BUSINESS | 控制台接口 |
| `invalid_request_id` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P08 |
| `invalid_messages` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P04 |
| `invalid_temperature` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P04 |
| `max_tokens_exceeded` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P04 |
| `invalid_tools` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P04 |
| `invalid_image_count` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P04 |
| `invalid_image_size` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P04 |
| `invalid_image_url` | `invalid_request_error` | 400 | 0 | no | BUSINESS | P04 |
| `request_too_large` | `invalid_request_error` | 413 | 0 | no | BUSINESS | P02 |
| `request_read_timeout` | `invalid_request_error` | 408 | 5 | yes | BUSINESS | P02 |
| `unknown_endpoint` | `invalid_request_error` | 404 | 0 | no | BUSINESS | 兜底 |
| `insufficient_credits` | `billing_error` | 402 | 0 | no | BUSINESS | P09/P10 |
| `account_frozen_due_to_overdraft` | `billing_error` | 402 | 0 | no | BUSINESS | P09 |
| `price_not_configured` | `internal_error` | 500 | 0 | no | PLATFORM | P09 |
| `rpm_limit_exceeded` | `rate_limit_exceeded` | 429 | 60 | duration | BUSINESS | P11 |
| `tpm_limit_exceeded` | `rate_limit_exceeded` | 429 | 60 | duration | BUSINESS | P11 |
| `concurrency_limit_exceeded` | `rate_limit_exceeded` | 429 | 3 | duration | BUSINESS | P11 |
| `daily_quota_exceeded` | `rate_limit_exceeded` | 429 | 86400 | duration | BUSINESS | P11 |
| `rate_limiter_unavailable` | `service_unavailable_error` | 503 | 5 | yes | PLATFORM | P11 |
| `request_in_flight` | `conflict_error` | 409 | 1 | duration | BUSINESS | P08 |
| `idempotency_key_reused` | `conflict_error` | 409 | 0 | no | BUSINESS | P08 |
| `idempotent_replay_unsupported` | `conflict_error` | 409 | 0 | no | BUSINESS | P08 |
| `duplicate_request_in_flight` | `conflict_error` | 409 | 1 | duration | BUSINESS | P08 |
| `duplicate_request_id` | `conflict_error` | 409 | 0 | no | BUSINESS | L3 |
| `no_available_upstream` | `upstream_error` | 503 | 30 | yes | CONFIG | P12/P13 |
| `no_available_upstream_for_group` | `upstream_error` | 503 | 0 | no | CONFIG | P12 |
| `upstream_capacity_exceeded` | `upstream_error` | 503 | 30 | yes | UPSTREAM | P16 |
| `upstream_timeout` | `upstream_error` | 504 | 30 | yes | UPSTREAM | P15/P16 |
| `upstream_dns_failure` | `upstream_error` | 502 | 0 | no | UPSTREAM | P15 |
| `upstream_tls_failure` | `upstream_error` | 502 | 0 | no | UPSTREAM | P15 |
| `upstream_auth_failed` | `upstream_error` | 502 | 0 | no | PLATFORM | P16 |
| `upstream_model_not_found` | `upstream_error` | 502 | 0 | no | PLATFORM | P16 |
| `upstream_invalid_request` | `upstream_error` | 400 | 0 | no | BUSINESS | P16 |
| `upstream_invalid_response` | `upstream_error` | 502 | 10 | yes | UPSTREAM | P19 |
| `upstream_empty_response` | `upstream_error` | 502 | 0 | no | UPSTREAM | P19 |
| `upstream_key_unavailable` | `upstream_error` | 503 | 60 | yes | PLATFORM | P15 |
| `upstream_all_failed` | `upstream_error` | 502 | 30 | yes | UPSTREAM | E-09 |
| `client_aborted` | `client_error` | 499 | 0 | not_applicable | BUSINESS | 内部归类 |
| `convert_failed` | `internal_error` | 500 | 0 | no | PLATFORM | P14/P20 |
| `settlement_unavailable` | `service_unavailable_error` | 503 | 5 | yes | PLATFORM | P10 |
| `backpressure_stream_rejected` | `service_unavailable_error` | 503 | 5 | yes | PLATFORM | 背压 60% 档 |
| `backpressure_rejected` | `service_unavailable_error` | 503 | 2 | yes | PLATFORM | 背压 90% 档 |
| `config_unavailable` | `service_unavailable_error` | 503 | 10 | yes | PLATFORM | E-60 |
| `auth_unavailable` | `service_unavailable_error` | 503 | 5 | yes | PLATFORM | P05 |
| `internal_error` | `internal_error` | 500 | 0 | no | PLATFORM | 全局兜底 |
| `key_not_found` | `invalid_request_error` | 404 | 0 | no | BUSINESS | 控制台接口 |
| `key_limit_reached` | `conflict_error` | 409 | 0 | no | BUSINESS | 控制台接口 |
| `account_exists` | `conflict_error` | 409 | 0 | no | BUSINESS | 控制台注册 |
| `login_locked` | `rate_limit_exceeded` | 429 | 900 | duration | BUSINESS | 控制台登录 |
| `account_not_verified` | `authorization_error` | 403 | 0 | no | BUSINESS | 控制台接口 |
| `permission_denied` | `authorization_error` | 403 | 0 | no | BUSINESS | 运营后台 |
| `channel_not_found` | `invalid_request_error` | 404 | 0 | no | BUSINESS | 运营后台 |
| `channel_in_use` | `conflict_error` | 409 | 0 | no | BUSINESS | 运营后台 |
| `resource_not_found` | `invalid_request_error` | 404 | 0 | no | BUSINESS | 运营后台 |
| `config_item_not_found` | `invalid_request_error` | 404 | 0 | no | BUSINESS | 运营后台 |
| `config_value_invalid` | `invalid_request_error` | 400 | 0 | no | BUSINESS | 运营后台 |
| `resource_state_conflict` | `conflict_error` | 409 | 0 | no | BUSINESS | 控制台接口 / 运营后台 |
| `trial_already_claimed` | `conflict_error` | 409 | 0 | no | BUSINESS | 控制台接口 |
| `invoice_already_issued` | `conflict_error` | 409 | 0 | no | BUSINESS | 控制台接口 |
| `price_conflict` | `conflict_error` | 409 | 0 | no | BUSINESS | 运营后台 |
| `account_rpm_exceeded` | `rate_limit_exceeded` | 429 | 60 | duration | BUSINESS | P11（V1.0 不实现） |
| `content_policy_violation` | `invalid_request_error` | 403 | 0 | no | BUSINESS | P04 / §8.4 |

## 平台可用性白名单（PRD §9.3，8 条）

网关月可用性的分子 = 下表 8 条码的请求数；分母 = 到达网关的全部请求
（429 / 402 / 400 / 404 / 上游 4xx 全在）。真相源是 `internal/apierr/availability.go`
的 `PlatformAvailabilityCodes()`，本表与 `internal/obs/alert/rules.yaml` 的
`PlatformErrorRateHigh` 正则由 `TestPlatformErrorRateRuleMatchesWhitelist` 钉住。

**不要用「归属 = PLATFORM」当白名单**：`OwnerPlatform` 有 12 条码，其中
`price_not_configured` 与三个上游凭据码（`upstream_auth_failed` /
`upstream_model_not_found` / `upstream_key_unavailable`）不在分子里——
按 Owner 过滤会把上游故障算成平台故障（G26）。

| `code` | 说明 |
| --- | --- |
| `rate_limiter_unavailable` | 触发阶段 P11 |
| `config_unavailable` | 触发阶段 E-60 |
| `auth_unavailable` | 触发阶段 P05 |
| `settlement_unavailable` | 触发阶段 P10 |
| `backpressure_rejected` | 触发阶段 背压 90% 档 |
| `backpressure_stream_rejected` | 触发阶段 背压 60% 档 |
| `internal_error` | 触发阶段 全局兜底 |
| `convert_failed` | 触发阶段 P14/P20 |

// Package apierr is the only writer of error responses for the API data plane.
//
// The 76 codes, their HTTP statuses and retry semantics are copied from PRD
// V1.2 appendix E.1 (the single source of truth). Four codes were added by
// errata E-21 because the console's session, credential, duplicate-account and
// query-parameter conditions had no semantically correct row:
//
//	session_expired         401  console session gone; "log in again"
//	invalid_credentials     401  wrong identifier/password (not a bad API key)
//	account_exists          409  registration collision
//	invalid_query_parameter 400  bad page / page_size / sort / order
//
// A fifth code was added after E-21 for a condition PRD E.1 has no row for at
// all: the free tier's active-key ceiling (T12 hard constraint 2). Answering
// that refusal with invalid_request_body told a caller whose body was perfectly
// valid that its "count" member was malformed — a diagnosis the caller can
// never act on, because editing count cannot lift a quota ceiling:
//
//	key_limit_reached       409  free tier already at its active-key ceiling
//
// T15 adds four conflict codes for refusals the caller cannot fix by editing
// the request body. The rule is: if correcting the hinted field could make the
// request succeed, the refusal is a body error (invalid_request_body); if no
// edit to the body can help — an order moved on, an entitlement is spent, an
// operator-side duplicate exists — it needs its own code. T13 had parked twelve
// such refusals on invalid_request_body for a smaller diff; this change moves
// ten of them (the other two really are body-value errors):
//
//	resource_state_conflict 409  illegal order/refund/quota-apply transition
//	trial_already_claimed   409  device, phone, IP segment or account already claimed
//	invoice_already_issued  409  the order already has an ISSUED invoice
//	price_conflict          409  one model has conflicting price sources (T15 pricing)
//
// T15's second segment added one generic 404 for the operator console:
//
//	resource_not_found      404  the addressed admin row does not exist
//
// It exists because model_not_found is a data-plane code with HTTP 400 ("模型
// 不在可用列表"): reusing it for GET /api/admin/models/{id} told an operator that
// their request body was wrong when the row had simply been deleted. Channel
// detail keeps channel_not_found because that code is more specific.
//
// T11 adds one code for the console login lockout:
//
//	login_locked            429  the account's password-failure budget is spent
//
// The lockout used to answer 429 rpm_limit_exceeded with a rewritten message.
// That code names a request-rate refusal, so a client honouring it backs off
// and retries — but the account is locked until the window ends and a correct
// password still fails. The IP-dimension failure budget keeps
// rpm_limit_exceeded: that refusal really is about request frequency.
//
// TDS §6.2 still carries older values for seven rows; those rows are
// deliberately not followed:
//
//	model_not_available             30 (not 0)
//	request_read_timeout             5 (not 0)
//	upstream_timeout                30 (not 0)
//	upstream_tls_failure             0 (not 60)
//	upstream_capacity_exceeded      30 (not 5)
//	idempotency_key_reused   409 conflict_error (not 400 invalid_request_error)
//	content_policy_violation       403 (not 400)
//
// A message or hint is never assembled by a business package. Callers pass
// only the interpolation arguments to Newf.
package apierr

import (
	"fmt"
	"strings"
)

// Type values. Exactly ten are allowed (PRD E.1 read-table rule 1).
const (
	TypeAuthentication     = "authentication_error"
	TypeAuthorization      = "authorization_error"
	TypeInvalidRequest     = "invalid_request_error"
	TypeBilling            = "billing_error"
	TypeRateLimit          = "rate_limit_exceeded"
	TypeConflict           = "conflict_error"
	TypeUpstream           = "upstream_error"
	TypeServiceUnavailable = "service_unavailable_error"
	TypeClient             = "client_error"
	TypeInternal           = "internal_error"
)

// Owner names who the response blames, in the vocabulary the caller-facing
// `hint` uses: "this is your problem" (BUSINESS) vs "this is ours"
// (PLATFORM/CONFIG) vs "the upstream is broken" (UPSTREAM).
//
// Owner is NOT the §9.3 availability predicate. That numerator is a fixed
// eight-code whitelist (PlatformAvailabilityCodes in availability.go); twelve
// specs are OwnerPlatform and four of them are deliberately outside it. Use
// IsPlatformAvailabilityCode anywhere availability is counted — filtering on
// Owner would inflate the platform's own unavailability with upstream
// credential faults (G26).
//
// The only producer of Owner is gw.usage.ownerForCode, which fills
// t_usage_record.fail_owner on inbound failures; no money path reads it.
type Owner string

const (
	OwnerPlatform Owner = "PLATFORM"
	OwnerUpstream Owner = "UPSTREAM"
	OwnerConfig   Owner = "CONFIG"
	OwnerBusiness Owner = "BUSINESS"
)

// RetryClass mirrors the "可重试" column of PRD E.1.
type RetryClass string

const (
	RetryNo            RetryClass = "no"
	RetryYes           RetryClass = "yes"
	RetryDuration      RetryClass = "duration"
	RetryNotApplicable RetryClass = "not_applicable"
)

// OptionalFields selects which optional body fields a code may carry.
type OptionalFields uint8

const (
	OptionalNone OptionalFields = iota
	OptionalRateLimit
	OptionalBilling
)

// The 76 codes. One constant per code; Codes() returns the same set in the
// order below. Adding or removing one requires updating codes_test.go's
// fixture and the generated markdown table in the same change.
const (
	CodeInvalidAPIKey          = "invalid_api_key"
	CodeInvalidCredentials     = "invalid_credentials"
	CodeSessionExpired         = "session_expired"
	CodeKeyExpired             = "key_expired"
	CodeKeyDisabled            = "key_disabled"
	CodeAccountDisabled        = "account_disabled"
	CodeAccountFrozen          = "account_frozen"
	CodeIPNotAllowed           = "ip_not_allowed"
	CodeConflictingAuthHeaders = "conflicting_auth_headers"

	CodeModelNotFound      = "model_not_found"
	CodeModelNotAvailable  = "model_not_available"
	CodeModelNotInGroup    = "model_not_in_group"
	CodeModelNotAuthorized = "model_not_authorized"

	CodeInvalidRequestBody    = "invalid_request_body"
	CodeInvalidQueryParameter = "invalid_query_parameter"
	CodeInvalidMessages       = "invalid_messages"
	CodeInvalidTemperature    = "invalid_temperature"
	CodeMaxTokensExceeded     = "max_tokens_exceeded"
	CodeInvalidTools          = "invalid_tools"
	CodeInvalidImageCount     = "invalid_image_count"
	CodeInvalidImageSize      = "invalid_image_size"
	CodeInvalidImageURL       = "invalid_image_url"
	CodeRequestTooLarge       = "request_too_large"
	CodeRequestReadTimeout    = "request_read_timeout"
	CodeUnknownEndpoint       = "unknown_endpoint"

	CodeInsufficientCredits         = "insufficient_credits"
	CodeAccountFrozenDueToOverdraft = "account_frozen_due_to_overdraft"
	CodePriceNotConfigured          = "price_not_configured"

	CodeRPMLimitExceeded         = "rpm_limit_exceeded"
	CodeTPMLimitExceeded         = "tpm_limit_exceeded"
	CodeConcurrencyLimitExceeded = "concurrency_limit_exceeded"
	CodeDailyQuotaExceeded       = "daily_quota_exceeded"
	CodeRateLimiterUnavailable   = "rate_limiter_unavailable"
	CodeAccountRPMExceeded       = "account_rpm_exceeded"

	CodeRequestInFlight             = "request_in_flight"
	CodeIdempotencyKeyReused        = "idempotency_key_reused"
	CodeIdempotentReplayUnsupported = "idempotent_replay_unsupported"
	CodeDuplicateRequestInFlight    = "duplicate_request_in_flight"
	CodeDuplicateRequestID          = "duplicate_request_id"

	CodeNoAvailableUpstream         = "no_available_upstream"
	CodeNoAvailableUpstreamForGroup = "no_available_upstream_for_group"
	CodeUpstreamCapacityExceeded    = "upstream_capacity_exceeded"
	CodeUpstreamTimeout             = "upstream_timeout"
	CodeUpstreamDNSFailure          = "upstream_dns_failure"
	CodeUpstreamTLSFailure          = "upstream_tls_failure"
	CodeUpstreamAuthFailed          = "upstream_auth_failed"
	CodeUpstreamModelNotFound       = "upstream_model_not_found"
	CodeUpstreamInvalidRequest      = "upstream_invalid_request"
	CodeUpstreamInvalidResponse     = "upstream_invalid_response"
	CodeUpstreamEmptyResponse       = "upstream_empty_response"
	CodeUpstreamKeyUnavailable      = "upstream_key_unavailable"
	CodeUpstreamAllFailed           = "upstream_all_failed"

	CodeClientAborted              = "client_aborted"
	CodeConvertFailed              = "convert_failed"
	CodeSettlementUnavailable      = "settlement_unavailable"
	CodeBackpressureStreamRejected = "backpressure_stream_rejected"
	CodeBackpressureRejected       = "backpressure_rejected"
	CodeConfigUnavailable          = "config_unavailable"
	CodeAuthUnavailable            = "auth_unavailable"
	CodeInternalError              = "internal_error"

	CodeKeyNotFound        = "key_not_found"
	CodeKeyLimitReached    = "key_limit_reached"
	CodeAccountExists      = "account_exists"
	CodeLoginLocked        = "login_locked"
	CodeAccountNotVerified = "account_not_verified"
	CodePermissionDenied   = "permission_denied"
	CodeChannelNotFound    = "channel_not_found"
	CodeChannelInUse       = "channel_in_use"
	CodeResourceNotFound   = "resource_not_found"
	CodeConfigItemNotFound = "config_item_not_found"
	CodeConfigValueInvalid = "config_value_invalid"

	CodeResourceStateConflict = "resource_state_conflict"
	CodeTrialAlreadyClaimed   = "trial_already_claimed"
	CodeInvoiceAlreadyIssued  = "invoice_already_issued"
	CodePriceConflict         = "price_conflict"

	CodeContentPolicyViolation = "content_policy_violation"
)

// Spec is one row of PRD E.1 plus the message and hint templates. Message
// templates use fmt verbs; Hint templates may contain {retry}, substituted
// with the effective retry_after_seconds.
type Spec struct {
	Code              string
	Type              string
	HTTPStatus        int
	RetryAfterSeconds int
	Retry             RetryClass
	Owner             Owner
	Stage             string
	Message           string
	Hint              string
	Optional          OptionalFields
}

// specs is the metadata table: exactly 76 rows.
var specs = []Spec{
	// E.1.1 鉴权与账号 (9)
	{
		Code: CodeInvalidAPIKey, Type: TypeAuthentication, HTTPStatus: 401,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P05",
		Message: "密钥无效",
		Hint:    "控制台 → 密钥管理页核对或重新生成",
	},
	{
		Code: CodeInvalidCredentials, Type: TypeAuthentication, HTTPStatus: 401,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "控制台登录",
		Message: "账号或密码不正确",
		Hint:    "核对账号与密码后重新登录；忘记密码走找回流程",
	},
	{
		Code: CodeSessionExpired, Type: TypeAuthentication, HTTPStatus: 401,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "控制台会话",
		Message: "登录状态已失效，请重新登录",
		Hint:    "重新登录控制台",
	},
	{
		Code: CodeKeyExpired, Type: TypeAuthentication, HTTPStatus: 401,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P05",
		Message: "密钥已于 %s 过期",
		Hint:    "控制台 → 密钥管理页延长有效期或新建密钥",
	},
	{
		Code: CodeKeyDisabled, Type: TypeAuthorization, HTTPStatus: 403,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P05",
		Message: "该密钥已停用",
		Hint:    "控制台 → 密钥管理页启用",
	},
	{
		Code: CodeAccountDisabled, Type: TypeAuthorization, HTTPStatus: 403,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P05",
		// T15 hard constraint 13: the customer-facing body never names the
		// disable category or the operator's reason. Those live in t_user.
		// disable_reason and are returned only by /api/admin/users.
		Message: "账号已被禁用，请联系客服",
		Hint:    "联系客服核实账号状态",
	},
	{
		Code: CodeAccountFrozen, Type: TypeAuthorization, HTTPStatus: 403,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P05",
		Message: "账号处于注销冷静期，调用已暂停",
		Hint:    "控制台 → 账号设置页撤销注销",
	},
	{
		Code: CodeIPNotAllowed, Type: TypeAuthorization, HTTPStatus: 403,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P06",
		Message: "来源 IP 不在该密钥白名单内",
		Hint:    "控制台 → 密钥设置页修改 IP 白名单",
	},
	{
		Code: CodeConflictingAuthHeaders, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P05",
		Message: "Authorization 与 x-api-key 同时存在且不一致",
		Hint:    "只保留一个认证头",
	},

	// E.1.2 模型可见性 (4)
	{
		Code: CodeModelNotFound, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P07",
		Message: "模型 %s 不在可用列表",
		Hint:    "调 /v1/models 取当前可用模型名",
	},
	{
		Code: CodeModelNotAvailable, Type: TypeUpstream, HTTPStatus: 503,
		RetryAfterSeconds: 30, Retry: RetryYes, Owner: OwnerBusiness, Stage: "P07",
		Message: "模型 %s 暂停服务，恢复上架后有公告",
		Hint:    "换用其他模型；若必须用该模型，按 {retry} 秒退避重试，恢复后有公告",
	},
	{
		Code: CodeModelNotInGroup, Type: TypeAuthorization, HTTPStatus: 403,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P07",
		Message: "模型 %s 不在当前价格分组的可用范围",
		Hint:    "控制台 → 提交分组切换申请",
	},
	{
		Code: CodeModelNotAuthorized, Type: TypeAuthorization, HTTPStatus: 403,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P07",
		Message: "该密钥未勾选模型 %s",
		Hint:    "控制台 → 密钥设置页勾选模型",
	},

	// E.1.3 请求校验 (12)
	{
		Code: CodeInvalidRequestBody, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P04",
		Message: "请求体参数有误：%s",
		Hint:    "按 message 中的字段路径修正请求体",
	},
	{
		Code: CodeInvalidQueryParameter, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "控制台接口",
		Message: "查询参数 %s 不合法",
		Hint:    "按 message 中的参数名修正查询条件",
	},
	{
		Code: CodeInvalidMessages, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P04",
		Message: "messages 字段格式不合法",
		Hint:    "文档站第 2 章 → messages 结构",
	},
	{
		Code: CodeInvalidTemperature, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P04",
		Message: "temperature 取值需在 0~2，当前 %v",
		Hint:    "把 temperature 改到 0~2",
	},
	{
		Code: CodeMaxTokensExceeded, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P04",
		Message: "本模型单次输出上限 %d，当前请求 %d",
		Hint:    "把 max_tokens 降到 message 给出的上限以内",
	},
	{
		Code: CodeInvalidTools, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P04",
		Message: "tools 字段格式不合法：%s",
		Hint:    "按字段路径修正 tools",
	},
	{
		Code: CodeInvalidImageCount, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P04",
		Message: "图像张数上限 4，当前请求 %d",
		Hint:    "把张数降到 4 以内",
	},
	{
		Code: CodeInvalidImageSize, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P04",
		Message: "不支持的尺寸 %s，支持列表：512x512 / 768x768 / 1024x1024 / 1024x1792 / 1792x1024",
		Hint:    "改用 message 列出的支持尺寸",
	},
	{
		Code: CodeInvalidImageURL, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P04",
		Message: "image_url 必须为 https:// 或 data:image/ 前缀",
		Hint:    "改用 https URL 或 data: URI",
	},
	{
		Code: CodeRequestTooLarge, Type: TypeInvalidRequest, HTTPStatus: 413,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P02",
		Message: "单请求体上限 10 MB",
		Hint:    "压缩或拆分请求体至 ≤10 MB",
	},
	{
		Code: CodeRequestReadTimeout, Type: TypeInvalidRequest, HTTPStatus: 408,
		RetryAfterSeconds: 5, Retry: RetryYes, Owner: OwnerBusiness, Stage: "P02",
		Message: "请求体读取超时，请检查客户端到网关的网络",
		Hint:    "按 {retry} 秒退避后重发；持续出现 → 检查客户端网络与 Content-Length",
	},
	{
		Code: CodeUnknownEndpoint, Type: TypeInvalidRequest, HTTPStatus: 404,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "兜底",
		Message: "请求的路径不存在",
		Hint:    "检查 base_url 是否多了或少了 /v1",
	},

	// E.1.4 计费与余额 (3)
	{
		Code: CodeInsufficientCredits, Type: TypeBilling, HTTPStatus: 402,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P09/P10",
		Message:  "本次请求预估需要 %s 积分，当前可用余额 %s（含透支上限 %s），差额 %s",
		Hint:     "控制台 → 充值中心",
		Optional: OptionalBilling,
	},
	{
		Code: CodeAccountFrozenDueToOverdraft, Type: TypeBilling, HTTPStatus: 402,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P09",
		Message:  "已透支 %s 积分、透支上限 %s，需充值 %s 以上才恢复",
		Hint:     "控制台 → 充值中心（充值后自动恢复）",
		Optional: OptionalBilling,
	},
	{
		Code: CodePriceNotConfigured, Type: TypeInternal, HTTPStatus: 500,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerPlatform, Stage: "P09",
		Message: "该模型价格未配置，平台侧故障",
		Hint:    "携 request_id 联系客服；不要充值、不要换 Key",
	},

	// E.1.5 限流 (5)
	{
		Code: CodeRPMLimitExceeded, Type: TypeRateLimit, HTTPStatus: 429,
		RetryAfterSeconds: 60, Retry: RetryDuration, Owner: OwnerBusiness, Stage: "P11",
		Message:  "本分钟请求数已达上限 %d，请 %d 秒后重试",
		Hint:     "按 {retry} 秒等待；持续触顶 → 控制台申请提额",
		Optional: OptionalRateLimit,
	},
	{
		Code: CodeTPMLimitExceeded, Type: TypeRateLimit, HTTPStatus: 429,
		RetryAfterSeconds: 60, Retry: RetryDuration, Owner: OwnerBusiness, Stage: "P11",
		Message:  "本分钟 token 用量已达上限 %d，请 %d 秒后重试",
		Hint:     "按 {retry} 秒等待；持续触顶 → 控制台申请提额",
		Optional: OptionalRateLimit,
	},
	{
		Code: CodeConcurrencyLimitExceeded, Type: TypeRateLimit, HTTPStatus: 429,
		RetryAfterSeconds: 3, Retry: RetryDuration, Owner: OwnerBusiness, Stage: "P11",
		Message:  "并发已达上限 %d，请 %d 秒后重试",
		Hint:     "按 3 秒退避重试；持续触顶 → 控制台申请提额",
		Optional: OptionalRateLimit,
	},
	{
		Code: CodeDailyQuotaExceeded, Type: TypeRateLimit, HTTPStatus: 429,
		RetryAfterSeconds: 86400, Retry: RetryDuration, Owner: OwnerBusiness, Stage: "P11",
		Message:  "账户日额度已用尽（今日已用 %d / 上限 %d），请次日 00:00 后重试",
		Hint:     "控制台 → 申请提额，或次日 00:00 后再试",
		Optional: OptionalRateLimit,
	},
	{
		Code: CodeRateLimiterUnavailable, Type: TypeServiceUnavailable, HTTPStatus: 503,
		RetryAfterSeconds: 5, Retry: RetryYes, Owner: OwnerPlatform, Stage: "P11",
		Message: "限流服务暂时不可用，请稍后重试",
		Hint:    "按 {retry} 秒退避重试；持续出现 → 携 request_id 联系客服。平台侧故障，不必调整你的请求频率",
	},
	// E.1.6 幂等 (5)
	{
		Code: CodeRequestInFlight, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 1, Retry: RetryDuration, Owner: OwnerBusiness, Stage: "P08",
		Message: "相同幂等键的首次请求仍在处理中",
		Hint:    "稍后用同一个 Idempotency-Key 重发以取回结果",
	},
	{
		Code: CodeIdempotencyKeyReused, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P08",
		Message: "幂等键已被另一请求体使用",
		Hint:    "换一个新的 Idempotency-Key 重发",
	},
	{
		Code: CodeIdempotentReplayUnsupported, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P08",
		Message: "该请求为流式，无法重放，请使用新请求发起",
		Hint:    "用新请求发起（流式不支持幂等重放）",
	},
	{
		Code: CodeDuplicateRequestInFlight, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 1, Retry: RetryDuration, Owner: OwnerBusiness, Stage: "P08",
		Message: "检测到 10 秒内完全相同的请求，本次未执行",
		Hint:    "等待首次请求的结果，勿重复提交",
	},
	{
		Code: CodeDuplicateRequestID, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "L3",
		Message: "X-Request-Id 已被复用",
		Hint:    "每次请求生成新的 X-Request-Id",
	},

	// E.1.7 上游与平台 (13)
	{
		Code: CodeNoAvailableUpstream, Type: TypeUpstream, HTTPStatus: 503,
		RetryAfterSeconds: 30, Retry: RetryYes, Owner: OwnerConfig, Stage: "P12/P13",
		Message: "当前无可用上游路由，已通知运营，恢复后有公告",
		Hint:    "携 request_id 联系运营核对渠道配置",
	},
	{
		Code: CodeNoAvailableUpstreamForGroup, Type: TypeUpstream, HTTPStatus: 503,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerConfig, Stage: "P12",
		Message: "当前分组无可用渠道",
		Hint:    "联系运营核对「分组可用渠道」配置",
	},
	{
		Code: CodeUpstreamCapacityExceeded, Type: TypeUpstream, HTTPStatus: 503,
		RetryAfterSeconds: 30, Retry: RetryYes, Owner: OwnerUpstream, Stage: "P16",
		Message: "平台上游容量暂时不足，请稍后重试",
		Hint:    "按 {retry} 秒退避重试",
	},
	{
		Code: CodeUpstreamTimeout, Type: TypeUpstream, HTTPStatus: 504,
		RetryAfterSeconds: 30, Retry: RetryYes, Owner: OwnerUpstream, Stage: "P15/P16",
		Message: "上游响应超时，已重试 %d 次",
		Hint:    "按 {retry} 秒退避重试；持续超时 → 携 request_id 联系客服",
	},
	{
		Code: CodeUpstreamDNSFailure, Type: TypeUpstream, HTTPStatus: 502,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerUpstream, Stage: "P15",
		Message: "上游服务暂时不可用",
		Hint:    "携 request_id 联系客服。不要重试",
	},
	{
		Code: CodeUpstreamTLSFailure, Type: TypeUpstream, HTTPStatus: 502,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerUpstream, Stage: "P15",
		Message: "上游服务暂时不可用",
		Hint:    "携 request_id 联系客服。不要重试",
	},
	{
		Code: CodeUpstreamAuthFailed, Type: TypeUpstream, HTTPStatus: 502,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerPlatform, Stage: "P16",
		Message: "平台上游凭据异常，已通知运维",
		Hint:    "携 request_id 联系客服（平台凭据问题）",
	},
	{
		Code: CodeUpstreamModelNotFound, Type: TypeUpstream, HTTPStatus: 502,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerPlatform, Stage: "P16",
		Message: "上游无该模型，平台模型映射配置错误",
		Hint:    "携 request_id 联系客服（模型映射配置）",
	},
	{
		Code: CodeUpstreamInvalidRequest, Type: TypeUpstream, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P16",
		Message: "请求参数不符合上游要求：%s",
		Hint:    "按 message 修正参数；文档站第 2 章",
	},
	{
		Code: CodeUpstreamInvalidResponse, Type: TypeUpstream, HTTPStatus: 502,
		RetryAfterSeconds: 10, Retry: RetryYes, Owner: OwnerUpstream, Stage: "P19",
		Message: "上游返回了无法解析的响应，已记录",
		Hint:    "按 10 秒退避重试",
	},
	{
		Code: CodeUpstreamEmptyResponse, Type: TypeUpstream, HTTPStatus: 502,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerUpstream, Stage: "P19",
		Message: "上游返回了空响应，本次不扣费",
		Hint:    "稍后重新发起（本次未扣费，无幂等冲突）",
	},
	{
		Code: CodeUpstreamKeyUnavailable, Type: TypeUpstream, HTTPStatus: 503,
		RetryAfterSeconds: 60, Retry: RetryYes, Owner: OwnerPlatform, Stage: "P15",
		Message: "上游凭据服务暂时不可用",
		Hint:    "按 {retry} 秒退避重试；持续 → 携 request_id 联系客服",
	},
	{
		Code: CodeUpstreamAllFailed, Type: TypeUpstream, HTTPStatus: 502,
		RetryAfterSeconds: 30, Retry: RetryYes, Owner: OwnerUpstream, Stage: "E-09",
		Message: "上游服务当前不可用，已重试 %d 次",
		Hint:    "按 {retry} 秒退避重试；持续 → 携 request_id 联系客服",
	},

	// E.1.8 生命周期与背压 (8)
	{
		Code: CodeClientAborted, Type: TypeClient, HTTPStatus: 499,
		RetryAfterSeconds: 0, Retry: RetryNotApplicable, Owner: OwnerBusiness, Stage: "内部归类",
		Message: "调用方已断开",
		Hint:    "—（连接已断，无响应可写）",
	},
	{
		Code: CodeConvertFailed, Type: TypeInternal, HTTPStatus: 500,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerPlatform, Stage: "P14/P20",
		Message: "协议转换失败，已记录",
		Hint:    "携 request_id 联系客服",
	},
	{
		Code: CodeSettlementUnavailable, Type: TypeServiceUnavailable, HTTPStatus: 503,
		RetryAfterSeconds: 5, Retry: RetryYes, Owner: OwnerPlatform, Stage: "P10",
		Message: "结算服务暂时不可用",
		Hint:    "按 {retry} 秒退避重试。不要以为是余额不足",
	},
	{
		Code: CodeBackpressureStreamRejected, Type: TypeServiceUnavailable, HTTPStatus: 503,
		RetryAfterSeconds: 5, Retry: RetryYes, Owner: OwnerPlatform, Stage: "背压 60% 档",
		Message: "本实例在途请求较多，暂不接受流式请求",
		Hint:    "改用非流式，或按 {retry} 秒退避重试",
	},
	{
		Code: CodeBackpressureRejected, Type: TypeServiceUnavailable, HTTPStatus: 503,
		RetryAfterSeconds: 2, Retry: RetryYes, Owner: OwnerPlatform, Stage: "背压 90% 档",
		Message: "本实例在途请求已满，暂不接受新请求",
		Hint:    "按 {retry} 秒退避重试。这不是你的请求太快，不必自查额度",
	},
	{
		Code: CodeConfigUnavailable, Type: TypeServiceUnavailable, HTTPStatus: 503,
		RetryAfterSeconds: 10, Retry: RetryYes, Owner: OwnerPlatform, Stage: "E-60",
		Message: "服务启动中，请稍后重试",
		Hint:    "按 {retry} 秒退避重试",
	},
	{
		Code: CodeAuthUnavailable, Type: TypeServiceUnavailable, HTTPStatus: 503,
		RetryAfterSeconds: 5, Retry: RetryYes, Owner: OwnerPlatform, Stage: "P05",
		Message: "鉴权服务暂时不可用",
		Hint:    "按 {retry} 秒退避重试。不要换 Key、不要重新生成",
	},
	{
		Code: CodeInternalError, Type: TypeInternal, HTTPStatus: 500,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerPlatform, Stage: "全局兜底",
		Message: "服务内部错误，请求 ID {request_id}",
		Hint:    "携 request_id 联系客服",
	},

	// E.1.9 控制面 (15; E-21 added account_exists, T11 added login_locked,
	// T12 added key_limit_reached, T15 added the four state/entitlement conflict
	// codes and resource_not_found)
	{
		Code: CodeKeyNotFound, Type: TypeInvalidRequest, HTTPStatus: 404,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "控制台接口",
		Message: "密钥不存在",
		Hint:    "控制台 → 密钥管理页刷新列表",
	},
	{
		Code: CodeKeyLimitReached, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "控制台接口",
		// The ceiling is interpolated from quota.free_max_active_keys, never
		// hard-coded: operations may raise it without a code change.
		Message: "免费档最多 %d 个 Key，升级套餐可扩容（当前已启用 %d 个，本次需新增 %d 个）",
		Hint:    "停用一个已有 Key 释放名额后重试；付费套餐不限 Key 数量，可在控制台 → 套餐页升级",
	},
	{
		Code: CodeAccountExists, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "控制台注册",
		Message: "该账号已注册",
		Hint:    "直接登录，或通过找回密码重置",
	},
	{
		Code: CodeLoginLocked, Type: TypeRateLimit, HTTPStatus: 429,
		// The default is the full account lock window; the call site always
		// overrides it with the remaining TTL of the dimension that locked.
		RetryAfterSeconds: 900, Retry: RetryDuration, Owner: OwnerBusiness, Stage: "控制台登录",
		// The message names the account, not the request rate: logging in again
		// with the correct password inside the window still fails.
		Message:  "登录失败次数过多，账号已临时锁定，请 %d 秒后重试",
		Hint:     "等待锁定结束后重试；忘记密码请使用找回密码（待开放）",
		Optional: OptionalRateLimit,
	},
	{
		Code: CodeAccountNotVerified, Type: TypeAuthorization, HTTPStatus: 403,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "控制台接口",
		Message: "账号未完成实名认证",
		Hint:    "控制台 → 账号设置页完成认证",
	},
	{
		Code: CodePermissionDenied, Type: TypeAuthorization, HTTPStatus: 403,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "运营后台",
		Message: "当前账号没有该操作的权限",
		Hint:    "联系管理员分配角色",
	},
	{
		Code: CodeChannelNotFound, Type: TypeInvalidRequest, HTTPStatus: 404,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "运营后台",
		Message: "渠道不存在或已被移除",
		Hint:    "刷新渠道列表",
	},
	{
		Code: CodeChannelInUse, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "运营后台",
		Message: "该渠道已被模型路由引用，不能直接删除",
		Hint:    "先解除引用或改为停用",
	},
	{
		Code: CodeResourceNotFound, Type: TypeInvalidRequest, HTTPStatus: 404,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "运营后台",
		// Generic on purpose: the console's detail/update paths address rows by
		// id, and a soft-deleted or mistyped id is the same condition. The
		// message must not promise which resource was being handled.
		Message: "资源不存在或已被删除",
		Hint:    "刷新列表后重试",
	},
	{
		Code: CodeConfigItemNotFound, Type: TypeInvalidRequest, HTTPStatus: 404,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "运营后台",
		Message: "配置项不存在",
		Hint:    "message 中点明是「全局未配置」还是「该作用域未覆盖」",
	},
	{
		Code: CodeConfigValueInvalid, Type: TypeInvalidRequest, HTTPStatus: 400,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "运营后台",
		Message: "配置值未通过校验：%s",
		Hint:    "按 message 修正取值",
	},
	{
		Code: CodeResourceStateConflict, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "控制台接口 / 运营后台",
		// Never a bare "conflict": the argument names the state that blocked
		// the action (当前状态为 CLOSED / 已有进行中的退款单 / ...).
		Message: "当前状态不允许该操作：%s",
		Hint:    "刷新后重试；若单据状态异常请联系客服",
	},
	{
		Code: CodeTrialAlreadyClaimed, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "控制台接口",
		// The four anti-abuse factors (device fingerprint, phone, IP segment,
		// account) are deliberately not told apart: naming the matched factor
		// would help an abuser probe which one to change.
		Message: "该账号或设备已领取过体验额度",
		Hint:    "每个设备、手机号与网络仅可领取一次；如有异议请联系客服",
	},
	{
		Code: CodeInvoiceAlreadyIssued, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "控制台接口",
		Message: "该订单已开具发票，不可重复申请",
		Hint:    "在发票记录中查看已开票据",
	},
	{
		Code: CodePriceConflict, Type: TypeConflict, HTTPStatus: 409,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "运营后台",
		// The argument names both sources (table + row id) so operations can
		// clear the duplicate instead of guessing which row is authoritative.
		Message: "该模型存在多处配置且价格不一致：%s",
		Hint:    "先清理冲突来源，再保存新单价",
	},

	// E.1.10 登记但不实现 (1)
	{
		Code: CodeAccountRPMExceeded, Type: TypeRateLimit, HTTPStatus: 429,
		RetryAfterSeconds: 60, Retry: RetryDuration, Owner: OwnerBusiness, Stage: "P11（V1.0 不实现）",
		Message:  "账户请求数已达上限 %d，请 %d 秒后重试",
		Hint:     "按 {retry} 秒等待（V1.0 不会出现该码）",
		Optional: OptionalRateLimit,
	},

	// E.1.11 内容安全 (1)
	{
		Code: CodeContentPolicyViolation, Type: TypeInvalidRequest, HTTPStatus: 403,
		RetryAfterSeconds: 0, Retry: RetryNo, Owner: OwnerBusiness, Stage: "P04 / §8.4",
		Message: "内容违反平台使用规范",
		Hint:    "修改请求内容后重新提交；对判定有异议走工单申诉",
	},
}

// codesInOrder is the canonical order: table order is the E.1 section order.
var codeIndex = func() map[string]Spec {
	idx := make(map[string]Spec, len(specs))
	for _, s := range specs {
		idx[s.Code] = s
	}
	return idx
}()

// Codes returns the 76 code strings in E.1 order.
func Codes() []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Code
	}
	return out
}

// Specs returns a copy of the metadata table.
func Specs() []Spec {
	out := make([]Spec, len(specs))
	copy(out, specs)
	return out
}

// Lookup returns the spec for a code.
func Lookup(code string) (Spec, bool) {
	s, ok := codeIndex[code]
	return s, ok
}

// MarkdownTable renders the metadata table for the docs site. The docs file
// itself lives outside this package per the T10 "internal/apierr only" scope.
func MarkdownTable() string {
	var b strings.Builder
	b.WriteString("| `code` | `type` | HTTP | `retry_after_seconds` | 可重试 | 归属 | 触发阶段 |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, s := range specs {
		fmt.Fprintf(&b, "| `%s` | `%s` | %d | %d | %s | %s | %s |\n",
			s.Code, s.Type, s.HTTPStatus, s.RetryAfterSeconds, s.Retry, s.Owner, s.Stage)
	}
	return b.String()
}

// MarkdownAvailabilitySection renders PRD §9.3's availability numerator as a
// doc section. Kept out of MarkdownTable() so the latter stays exactly the
// 76-row code table (TestCodeCountIs76 and TestCodeCountMatchesDocsTable count
// its rows). docs/error-codes.md is MarkdownTable() followed by this.
//
// It is generated rather than hand-written so the published table cannot
// disagree with the codes the alert rule is checked against (G26).
func MarkdownAvailabilitySection() string {
	var b strings.Builder
	b.WriteString("## 平台可用性白名单（PRD §9.3，8 条）\n\n")
	b.WriteString("网关月可用性的分子 = 下表 8 条码的请求数；分母 = 到达网关的全部请求\n")
	b.WriteString("（429 / 402 / 400 / 404 / 上游 4xx 全在）。真相源是 `internal/apierr/availability.go`\n")
	b.WriteString("的 `PlatformAvailabilityCodes()`，本表与 `internal/obs/alert/rules.yaml` 的\n")
	b.WriteString("`PlatformErrorRateHigh` 正则由 `TestPlatformErrorRateRuleMatchesWhitelist` 钉住。\n\n")
	b.WriteString("**不要用「归属 = PLATFORM」当白名单**：`OwnerPlatform` 有 12 条码，其中\n")
	b.WriteString("`price_not_configured` 与三个上游凭据码（`upstream_auth_failed` /\n")
	b.WriteString("`upstream_model_not_found` / `upstream_key_unavailable`）不在分子里——\n")
	b.WriteString("按 Owner 过滤会把上游故障算成平台故障（G26）。\n\n")
	b.WriteString("| `code` | 说明 |\n| --- | --- |\n")
	for _, code := range PlatformAvailabilityCodes() {
		stage := ""
		if spec, ok := Lookup(code); ok {
			stage = spec.Stage
		}
		fmt.Fprintf(&b, "| `%s` | 触发阶段 %s |\n", code, stage)
	}
	return b.String()
}

/**
 * Display labels for console enums. The backend already omits the internal
 * group (visibleGroup returns ""), so an unknown or empty code is rendered as
 * absent rather than guessed.
 */

const KEY_GROUP_LABEL: Record<string, string> = {
  standard: '标准',
  contract: '合约',
}

export function groupLabel(code: string | null | undefined): string | null {
  if (!code) return null
  return KEY_GROUP_LABEL[code] ?? code
}

const AUDIT_ACTION_LABEL: Record<string, string> = {
  CREATE: '创建',
  RENAME: '重命名',
  MODEL_SCOPE_CHANGE: '模型权限变更',
  IP_RULE_CHANGE: 'IP 白名单变更',
  EXPIRE_CHANGE: '有效期变更',
  ENABLE: '启用',
  DISABLE: '停用',
  ROTATE: '轮换',
  DELETE: '删除',
  // 复制密钥时后端写一条 VIEW_SECRET：把凭据交出去，本人应当能在日志里查到。
  VIEW_SECRET: '查看密钥',
}

export function auditActionLabel(action: string): string {
  return AUDIT_ACTION_LABEL[action] ?? action
}

const AUDIT_FIELD_LABEL: Record<string, string> = {
  key_name: '名称',
  key_prefix: '前缀',
  key_last4: '后四位',
  status: '状态',
  model_scope_mode: '可调用模型',
  model_ids: '模型编号',
  group_code: '分组',
  expire_at: '到期时间',
  ip_whitelist_mode: '来源地址限制',
  ip_allowlist: '允许的来源',
  revoked_at: '失效时间',
  new_key_id: '新密钥编号',
  new_key_prefix: '新密钥前缀',
  new_key_last4: '新密钥后四位',
  deleted_at: '删除时间',
}

export function auditFieldLabel(field: string): string {
  return AUDIT_FIELD_LABEL[field] ?? field
}

const ORDER_STATE_LABEL: Record<string, string> = {
  PENDING: '待支付',
  PAID: '已支付',
  CLOSED: '已关闭',
  REFUNDING: '退款中',
  REFUNDED: '已退款',
  PARTIAL_REFUNDED: '部分退款',
}

export function orderStateLabel(state: string): string {
  return ORDER_STATE_LABEL[state] ?? state
}

const REFUND_STATE_LABEL: Record<string, string> = {
  APPLIED: '已申请',
  APPROVED: '已通过',
  REJECTED: '已驳回',
  PAYING: '退款中',
  PAID: '已退款',
  FAILED: '退款失败',
}

export function refundStateLabel(state: string): string {
  return REFUND_STATE_LABEL[state] ?? state
}

const INVOICE_STATE_LABEL: Record<string, string> = {
  APPLIED: '已申请',
  APPROVED: '已通过',
  ISSUED: '已开具',
  REJECTED: '已驳回',
  VOIDED: '已作废',
}

export function invoiceStateLabel(state: string): string {
  return INVOICE_STATE_LABEL[state] ?? state
}

const MODEL_TYPE_LABEL: Record<string, string> = {
  TEXT: '文本',
  IMAGE: '图像',
  EMBEDDING: '向量',
}

export function modelTypeLabel(type: string): string {
  return MODEL_TYPE_LABEL[type] ?? type
}

/**
 * Protocol codes come from t_protocol seeds; the API exposes only the code, so
 * an unknown (newly added) protocol falls back to the code instead of being
 * guessed.
 */
const PROTOCOL_LABEL: Record<string, string> = {
  BAILIAN: '百炼',
  OPENAI_COMPAT: 'OpenAI 兼容',
  ANTHROPIC_COMPAT: 'Anthropic 兼容',
}

export function protocolLabel(code: string): string {
  return PROTOCOL_LABEL[code] ?? code
}

const PAY_CHANNEL_LABEL: Record<string, string> = {
  WECHAT: '微信支付',
  ALIPAY: '支付宝',
}

export function payChannelLabel(channel: string): string {
  return PAY_CHANNEL_LABEL[channel] ?? channel
}

const DIMENSION_LABEL: Record<string, string> = {
  model: '按模型',
  channel: '按渠道',
  day: '按日',
}

export function dimensionLabel(dimension: string): string {
  return DIMENSION_LABEL[dimension] ?? dimension
}

// ---------------------------------------------------------------------------
// Request logs. fail_stage / error_class / retry_decision are open strings in
// the API; unknown values fall back to the raw code rather than being guessed.
// ---------------------------------------------------------------------------

const LOG_RESULT_LABEL: Record<string, string> = {
  SUCCESS: '成功',
  PARTIAL: '部分成功',
  FAILED: '失败',
}

export function logResultLabel(result: string): string {
  return LOG_RESULT_LABEL[result] ?? result
}

const FAIL_STAGE_LABEL: Record<string, string> = {
  REQUEST: '请求接收',
  AUTH: '鉴权',
  RATE_LIMIT: '限流检查',
  ROUTE: '路由选路',
  UPSTREAM: '上游调用',
  CONVERT: '协议转换',
  SETTLE: '结算',
  CLIENT_ABORT: '客户端中断',
}

export function failStageLabel(stage: string | null | undefined): string | null {
  if (!stage) return null
  return FAIL_STAGE_LABEL[stage] ?? stage
}

const FAIL_OWNER_LABEL: Record<string, string> = {
  CLIENT: '客户端',
  PLATFORM: '平台',
  UPSTREAM: '上游',
}

export function failOwnerLabel(owner: string | null | undefined): string | null {
  if (!owner) return null
  return FAIL_OWNER_LABEL[owner] ?? owner
}

const ATTEMPT_RESULT_LABEL: Record<string, string> = {
  SUCCESS: '成功',
  FAILED: '失败',
  ABORTED: '中断',
}

export function attemptResultLabel(result: string): string {
  return ATTEMPT_RESULT_LABEL[result] ?? result
}

const ATTEMPT_PHASE_LABEL: Record<string, string> = {
  CONNECT: '建立连接',
  SEND: '发送请求',
  FIRST_BYTE: '等待首字节',
  STREAM_BODY: '接收流',
  COMPLETE: '完成',
}

export function attemptPhaseLabel(phase: string): string {
  return ATTEMPT_PHASE_LABEL[phase] ?? phase
}

const RETRY_DECISION_LABEL: Record<string, string> = {
  RETRY_NEXT_ROUTE: '切换路由重试',
  RETRY_SAME_ROUTE: '原路由重试',
  NO_RETRY: '不重试',
  RETRY_BLOCKED_STREAMED: '已出流不可重试',
}

export function retryDecisionLabel(decision: string | null | undefined): string | null {
  if (!decision) return null
  return RETRY_DECISION_LABEL[decision] ?? decision
}

// ---------------------------------------------------------------------------
// Tickets
// ---------------------------------------------------------------------------

const TICKET_STATE_LABEL: Record<string, string> = {
  OPEN: '待响应',
  RESPONDED: '已响应',
  CLOSED: '已关闭',
}

export function ticketStateLabel(state: string): string {
  return TICKET_STATE_LABEL[state] ?? state
}

const TICKET_TYPE_LABEL: Record<string, string> = {
  BILLING: '计费',
  TECH: '技术故障',
  CONTENT_APPEAL: '内容申诉',
  OTHER: '其他',
}

export function ticketTypeLabel(type: string): string {
  return TICKET_TYPE_LABEL[type] ?? type
}

const REPLY_AUTHOR_LABEL: Record<string, string> = {
  USER: '用户',
  OPS: '客服',
}

export function replyAuthorLabel(author: string): string {
  return REPLY_AUTHOR_LABEL[author] ?? author
}

/**
 * 操作日志里的枚举值翻译。后端存的是机器值，界面上不该出现 ENABLED / ALL / OFF
 * 这类词（需求方 2026-11-05 明确要求去英文技术词）。
 */
const AUDIT_VALUE_LABEL: Record<string, Record<string, string>> = {
  status: { ENABLED: '启用中', DISABLED: '已停用', REVOKED: '已失效' },
  model_scope_mode: { ALL: '全部已上架模型', SELECTED: '仅指定模型' },
  ip_whitelist_mode: { OFF: '不限制', ON: '仅允许名单内', ALLOWLIST: '仅允许名单内' },
}

export function auditFieldValueLabel(field: string, value: string): string {
  if (field === 'group_code') {
    // 分组是运营口径（标准/合约），直接复用同一份译文，避免日志里冒出 standard。
    return groupLabel(value) ?? value
  }
  return AUDIT_VALUE_LABEL[field]?.[value] ?? value
}

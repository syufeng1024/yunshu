import {
  keepPreviousData,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'
import { createSSEParser } from '@/lib/sseStream'
import type { SSEEvent } from '@/lib/sseStream'
import { ApiError, api } from './client'
import { CSRF_HEADER, getCsrfToken } from './csrf'
import type { components } from './types'

/**
 * Console contracts, transcribed from internal/control/{user,key,model}.
 * Field names are the JSON tags, not the Go names: they are what the API
 * actually returns and what docs/control-api.md documents.
 */

export interface Page<T> {
  items: T[]
  page: number
  page_size: number
  total: number
}

// ---------------------------------------------------------------------------
// Overview (GET /api/console/overview)
// ---------------------------------------------------------------------------

export interface OverviewBalance {
  micro: number
  credits: string
  cny: string
  recharge_micro: number
  bonus_micro: number
}

export interface OverviewToday {
  requests: number
  consumed_micro: number
  consumed_credits: string
  /** null when today has no billable attempts; render "—", never 0%. */
  success_rate_bp: number | null
}

export interface OverviewHour {
  hour: string
  requests: number
  consumed_micro: number
}

export interface OverviewPlan {
  code: string
  name: string
  policy_source: string
  trial_expire_at: string | null
}

export interface OverviewQuota {
  rpm: number
  tpm: number
  concurrency: number
  /** null = the plan has no daily ceiling. */
  daily_quota_micro: number | null
  used_today_micro: number
  remaining_micro: number | null
}

export type EtaReason = 'OK' | 'QUOTA_REACHED' | 'NO_DAILY_QUOTA' | 'NO_RECENT_USAGE' | string

export interface OverviewEta {
  reachable: boolean
  reason: EtaReason
  eta_at: string | null
  seconds_left: number | null
  burn_micro_per_hour: number
}

export interface ConsoleOverview {
  balance: OverviewBalance
  today: OverviewToday
  hourly: OverviewHour[]
  plan: OverviewPlan
  quota: OverviewQuota
  eta: OverviewEta
}

// ---------------------------------------------------------------------------
// Keys (GET/POST/PATCH/DELETE /api/console/keys)
// ---------------------------------------------------------------------------

export type KeyStatus = 'ENABLED' | 'DISABLED'
export type ModelScopeMode = 'ALL' | 'SELECTED'

export interface KeyListItem {
  id: number
  key_name: string
  /** prefix + "****" + last4, assembled server-side. */
  key_masked: string
  status: KeyStatus
  expired: boolean
  group_code?: string
  group_ratio_bp: number | null
  model_scope_mode: ModelScopeMode
  model_count: number
  ip_whitelist_mode: string
  ip_rule_count: number
  expire_at: string | null
  today_consumed_micro: number
  created_at: string
  last_used_at: string | null
}

export type KeyListResponse = Page<KeyListItem>

export interface CreatedKey {
  id: number
  key_name: string
  /** Plaintext. Present only in the create/rotate response. */
  key: string
  key_prefix: string
  key_last4: string
  key_masked: string
  model_scope_mode: ModelScopeMode
  model_ids: number[]
  group_code: string
  expire_at: string | null
  ip_allowlist: string[]
}

export interface CreateKeysResponse {
  items: CreatedKey[]
  count: number
  warning: string
}

export interface CreateKeysInput {
  name: string
  count: number
  model_scope_mode: ModelScopeMode
  /** Omitted entirely for ALL: the backend rejects ALL + model_ids. */
  model_ids?: number[]
  /** 不传表示永不过期（需求方 2026-11-05 裁定：有效期改为可选）。 */
  expire_at?: string
  /** 来源地址白名单已从界面撤下，保留字段仅为兼容旧调用。 */
  ip_allowlist?: string[]
}

/** PRD F6-5: one batch is 1–10 keys. The backend rejects anything outside. */
export const KEY_BATCH_MIN = 1
export const KEY_BATCH_MAX = 10

/**
 * Free-tier active-key ceiling (PRD F6-7). Mirrors the backend default of
 * quota.free_max_active_keys; the backend stays authoritative and returns
 * key_limit_reached (409) with the interpolated copy. This copy only drives
 * the pre-emptive grey-out and carries no number in user-visible text.
 */
export const FREE_PLAN_MAX_ACTIVE_KEYS = 3

export interface KeyListParams {
  page?: number
  pageSize?: number
  q?: string
  status?: KeyStatus
  /** Server-side order; the backend whitelists these three columns. */
  sort?: 'key_name' | 'last_used_at' | 'created_at'
  order?: 'asc' | 'desc'
}

export interface KeyAuditItem {
  action: string
  before: Record<string, unknown> | null
  after: Record<string, unknown> | null
  operator_id: string
  created_at: string
}

export type KeyAuditResponse = Page<KeyAuditItem>

export interface KeyModelRef {
  id: number
  model_name: string
  display_name: string
}

export interface KeyIPRule {
  cidr: string
  ip_family: number
  note?: string
}

export interface KeyRealtime {
  as_of: string
  available: boolean
  rpm_used: number | null
  rpm_limit: number
  tpm_used: number | null
  tpm_limit: number
  concurrency_used: number | null
  concurrency_limit: number
}

export interface KeyAccountQuota {
  used_today_micro: number
  daily_quota_micro: number | null
  remaining_micro: number | null
  plan_code: string
  plan_name: string
  policy_source: string
}

export interface KeyDetail {
  id: number
  key_name: string
  key_masked: string
  key_prefix: string
  status: KeyStatus
  expired: boolean
  group_code?: string
  group_ratio_bp: number | null
  group_note?: string
  model_scope_mode: ModelScopeMode
  models: KeyModelRef[]
  ip_whitelist_mode: string
  ip_allowlist: KeyIPRule[]
  expire_at: string | null
  remaining_days: number | null
  created_at: string
  last_used_at: string | null
  realtime: KeyRealtime
  account_quota: KeyAccountQuota
}

export interface UpdateKeyInput {
  model_scope_mode?: ModelScopeMode
  model_ids?: number[]
  status?: KeyStatus
}

export interface UpdateKeyResponse {
  changed: boolean
  key_name: string
  status: KeyStatus
  model_scope_mode: ModelScopeMode
  expire_at: string | null
  ip_whitelist_mode: string
  ip_allowlist: string[]
}

export interface RotateKeyResponse {
  id: number
  rotated_from_id: number
  key_name: string
  key: string
  key_prefix: string
  key_last4: string
  key_masked: string
  group_code: string
  expire_at: string | null
  warning: string
  revocation: string
}

export interface DeleteKeyResponse {
  id: number
  deleted: boolean
}

// ---------------------------------------------------------------------------
// Model catalog (GET /api/console/models)
// ---------------------------------------------------------------------------

export interface ConsoleModel {
  id: number
  model_name: string
  display_name: string
  model_type: string
  context_length: number
  protocol: string
  aliases: string[]
}

interface ConsoleModelsResponse extends Page<ConsoleModel> {
  items: ConsoleModel[]
}

export type ModelType = 'TEXT' | 'IMAGE' | 'EMBEDDING'

/** B-53: null means "not configured" and must render as "—", not "不支持". */
export interface ModelCapabilities {
  stream: boolean | null
  tool_call: boolean | null
  vision: boolean | null
  json_mode: boolean | null
}

/**
 * Standard price from t_price_rule. The catalog never carries the viewer's
 * group ratio (F1-5) and omits cache_* when cache billing is disabled.
 */
export interface ModelPricing {
  in_price_micro_per_k: number
  out_price_micro_per_k: number
  per_call_price_micro: number
  in_price_credits_per_k: string
  out_price_credits_per_k: string
  per_call_credits_per_call: string
  in_price_cny_per_k: string
  out_price_cny_per_k: string
  per_call_cny_per_call: string
  cache_billing_enabled: boolean
  cache_hit_ratio_bp?: number | null
  cache_write_ratio_bp?: number | null
  cache_hit_price_credits_per_k?: string | null
  cache_write_price_credits_per_k?: string | null
}

/** GET /api/console/models item. LISTED/DOMESTIC only, server-sorted. */
export interface CatalogModel extends ConsoleModel {
  capabilities: ModelCapabilities
  recommend_weight: number
  published_at: string | null
  /** null = the API flagged a missing price; render "—", never a fake zero. */
  pricing: ModelPricing | null
}

export type ModelCatalogResponse = Page<CatalogModel>

export interface ModelCatalogParams {
  page?: number
  pageSize?: number
  modelType?: ModelType
  protocol?: string
  q?: string
  /** Backend whitelist: model_name, context_length. Omitted = recommended order. */
  sort?: 'model_name' | 'context_length'
  order?: 'asc' | 'desc'
}

// ---------------------------------------------------------------------------
// Usage / bill / export (GET /api/console/usage, /bills; POST /usage/export)
// ---------------------------------------------------------------------------

export type UsageDimension = 'model' | 'channel' | 'day'

export interface UsageRow {
  dimension: string
  id: number
  name: string
  request_total: number
  request_success: number
  success_rate_bp: number
  tokens_in: number
  tokens_out: number
  consumed_micro: number
  consumed_recharge_micro: number
  consumed_bonus_micro: number
  amount_fen: number
  amount_yuan: string
}

export interface UsageFilterEcho {
  model_id: number | null
  channel_id: number | null
}

export interface UsageResponse {
  from: string
  to: string
  dimension: UsageDimension
  filter: UsageFilterEcho
  window_days: number
  /** Same definition and order as the CSV header (F9-3). */
  columns: string[]
  items: UsageRow[]
  totals: UsageRow
}

export interface UsageParams {
  /** YYYY-MM-DD or RFC3339. */
  from: string
  to: string
  dimension: UsageDimension
  modelId?: number
  channelId?: number
}

export interface BillResponse {
  month: string
  period_start: string
  period_end: string
  request_total: number
  request_success: number
  success_rate_bp: number
  consumed_micro: number
  consumed_recharge_micro: number
  consumed_bonus_micro: number
  amount_fen: number
  /** Backend-rendered with two decimals; never recompute from fen. */
  amount_yuan: string
}

export type ExportState = 'RUNNING' | 'DONE' | 'FAILED'

export interface ExportStartResponse {
  /** HMAC-signed task token. */
  task_id: string
  state: 'RUNNING'
}

export interface ExportStatusResponse {
  task_id: string
  state: ExportState
  /** Signed download path, present when state is DONE. */
  url?: string | null
  expires_at?: string | null
}

// ---------------------------------------------------------------------------
// Funds: plans / orders / redeem / refunds / invoices
// ---------------------------------------------------------------------------

export interface Plan {
  id: number
  code: string
  name: string
  price_fen: number
  price_yuan: number
  grant_recharge_micro: number
  grant_bonus_micro: number
  grant_recharge: number
  grant_bonus: number
  bonus_valid_days: number
  rpm: number
  tpm: number
  concurrency: number
  daily_quota_micro: number | null
  daily_quota: number | null
  permanent: boolean
  /** FREE_DEFAULT and TRIAL are granted, never sold. */
  purchasable: boolean
  sort_order: number
}

export interface PlanList {
  items: Plan[]
  default_tier: string
  default_tier_label: string
  trial_days: number
  bonus_expire_days: number
  micro_per_credit: number
}

export type OrderState =
  | 'PENDING'
  | 'PAID'
  | 'CLOSED'
  | 'REFUNDING'
  | 'REFUNDED'
  | 'PARTIAL_REFUNDED'

export type OrderStatusFilter = 'pending' | 'paid' | 'closed' | 'refunding' | 'refunded'

export interface Order {
  id: number
  order_no: string
  order_type: 'CUSTOM' | 'PLAN'
  plan_code?: string
  pay_amount_fen: number
  pay_amount_yuan: number
  recharge_micro: number
  bonus_micro: number
  recharge: number
  bonus: number
  pay_channel: 'WECHAT' | 'ALIPAY'
  state: OrderState
  paid_at: string | null
  expire_at: string
  created_at: string
}

export type OrderList = Page<Order>

export interface OrderListParams {
  status?: OrderStatusFilter
  q?: string
  page?: number
  pageSize?: number
}

export interface CreateOrderInput {
  order_type: 'CUSTOM' | 'PLAN'
  plan_id?: number
  /** Required for CUSTOM; omitted for PLAN so the server prices the tier. */
  pay_amount_fen?: number
  pay_channel: 'WECHAT' | 'ALIPAY'
}

export interface CreatedOrder {
  order: Order
  idempotent_replay: boolean
}

export interface RedeemResult {
  batch_name: string
  /** Exact credited amount. Always render the credit count from this field. */
  amount_micro: number
  /**
   * The same amount expressed in 元, not credits: the handler computes
   * `microToFen(amount_micro) / 100`. A 50 元 code carries amount 50 while it
   * credits 5 000 积分, so never format this as 积分 — use amount_micro with
   * formatCreditAmount().
   */
  amount: number
  credit_target: 'BONUS' | 'RECHARGE'
  expire_at: string
  balance_after_micro: number
}

export type RefundState = 'APPLIED' | 'APPROVED' | 'REJECTED' | 'PAYING' | 'PAID' | 'FAILED'
export type RefundStatusFilter =
  | 'applied'
  | 'approved'
  | 'rejected'
  | 'paying'
  | 'paid'
  | 'failed'

export interface Refund {
  id: number
  refund_no: string
  recharge_order_id: number
  apply_micro: number
  apply_credits: number
  refund_micro: number
  refund_credits: number
  refund_fen: number
  refund_yuan: number
  fee_fen: number
  fee_yuan: number
  state: RefundState
  reject_reason: string | null
  fail_reason: string | null
  pay_txn_id: string | null
  auditor_id: number | null
  audited_at: string | null
  paid_at: string | null
  created_at: string
}

export type RefundList = Page<Refund>

export interface RefundListParams {
  status?: RefundStatusFilter
  page?: number
  pageSize?: number
}

export interface CreatedRefund {
  refund: Refund
  idempotent_replay: boolean
}

export type InvoiceState = 'APPLIED' | 'APPROVED' | 'ISSUED' | 'REJECTED' | 'VOIDED'
export type InvoiceStatusFilter = 'applied' | 'approved' | 'issued' | 'rejected' | 'voided'

export interface Invoice {
  id: number
  apply_no: string
  recharge_order_id: number
  invoice_type: string
  title_type: 'PERSONAL' | 'COMPANY'
  title: string
  tax_no: string | null
  receive_email: string
  amount_fen: number
  amount_yuan: number
  tax_rate_bp: number
  state: InvoiceState
  reject_reason: string | null
  invoice_url: string | null
  auditor_id: number | null
  issued_at: string | null
  created_at: string
}

export type InvoiceList = Page<Invoice>

export interface InvoiceListParams {
  status?: InvoiceStatusFilter
  page?: number
  pageSize?: number
  /** Backend whitelist: issued_at, otherwise created_at. */
  sort?: 'created_at' | 'issued_at'
  order?: 'asc' | 'desc'
}

export interface CreateInvoiceInput {
  recharge_order_id: number
  title_type: 'PERSONAL' | 'COMPANY'
  title: string
  tax_no?: string
  receive_email: string
  /** Omitted = the remaining invoiceable amount, per the API contract. */
  amount_fen?: number
}

export interface CreatedInvoice {
  invoice: Invoice
  idempotent_replay: boolean
}

// ---------------------------------------------------------------------------
// Quota apply (POST /api/console/quota-applies)
// ---------------------------------------------------------------------------

export interface CreateQuotaApplyInput {
  target_quota_micro: number
  apply_reason: string
}

export interface QuotaApplyResponse {
  apply: {
    id: number
    apply_no: string
    state: string
  }
  idempotent_replay: boolean
}

// ---------------------------------------------------------------------------
// Plain calls
// ---------------------------------------------------------------------------

export function fetchOverview(): Promise<ConsoleOverview> {
  return api.get<ConsoleOverview>('/api/console/overview')
}

function keysQuery(params: KeyListParams): string {
  const search = new URLSearchParams()
  search.set('page', String(params.page ?? 1))
  search.set('page_size', String(params.pageSize ?? 20))
  const q = params.q?.trim()
  if (q) search.set('q', q)
  if (params.status) search.set('status', params.status)
  if (params.sort) search.set('sort', params.sort)
  if (params.order) search.set('order', params.order)
  return search.toString()
}

export function fetchKeys(params: KeyListParams): Promise<KeyListResponse> {
  return api.get<KeyListResponse>(`/api/console/keys?${keysQuery(params)}`)
}

export function fetchKeyDetail(id: string): Promise<KeyDetail> {
  return api.get<KeyDetail>(`/api/console/keys/${encodeURIComponent(id)}`)
}

export function fetchKeyAudit(id: string, page: number, pageSize = 20): Promise<KeyAuditResponse> {
  return api.get<KeyAuditResponse>(
    `/api/console/keys/${encodeURIComponent(id)}/audit?page=${page}&page_size=${pageSize}`,
  )
}

/**
 * Walks the catalog until every listed model is loaded. The wizard needs the
 * full set for the SELECTED scope; the API caps page_size at 100.
 */
export async function fetchAllModels(): Promise<ConsoleModel[]> {
  const pageSize = 100
  const out: ConsoleModel[] = []
  for (let page = 1; page <= 10; page += 1) {
    const response = await api.get<ConsoleModelsResponse>(
      `/api/console/models?page=${page}&page_size=${pageSize}`,
    )
    out.push(...response.items)
    if (response.items.length < pageSize || out.length >= response.total) break
  }
  return out
}

// ---------------------------------------------------------------------------
// Query hooks
// ---------------------------------------------------------------------------

/**
 * `poll: false` is for pages that only need the plan code (e.g. the keys page
 * decides whether the free-tier ceiling applies); they should not refresh the
 * whole overview every 15s.
 */
export function useConsoleOverview(options: { poll?: boolean } = {}) {
  return useQuery({
    queryKey: ['console', 'overview'],
    queryFn: fetchOverview,
    refetchInterval: options.poll === false ? false : 15_000,
  })
}

export function useKeys(params: KeyListParams) {
  return useQuery({
    queryKey: ['console', 'keys', 'list', params],
    queryFn: () => fetchKeys(params),
    placeholderData: keepPreviousData,
  })
}

/**
 * Active (enabled, not expired) key count for the free-tier ceiling. The
 * backend counts the same way; only the ceiling itself is not exposed, which
 * is why FREE_PLAN_MAX_ACTIVE_KEYS exists.
 */
export function useActiveKeyCount(enabled: boolean) {
  return useQuery({
    queryKey: ['console', 'keys', 'active-count'],
    enabled,
    queryFn: async () => {
      let page = 1
      let active = 0
      for (; page <= 10; page += 1) {
        const response = await fetchKeys({ status: 'ENABLED', page, pageSize: 100 })
        active += response.items.filter((item) => !item.expired).length
        if (response.items.length < 100 || page * 100 >= response.total) break
      }
      return active
    },
  })
}

export function useKeyDetail(id: string | undefined) {
  return useQuery({
    queryKey: ['console', 'keys', 'detail', id],
    queryFn: () => fetchKeyDetail(id as string),
    enabled: Boolean(id),
  })
}

export interface KeyUsagePoint {
  /** 东八区自然小时，`YYYY-MM-DD HH:00:00`。 */
  hour: string
  requests: number
  consumed_micro: number
}

export interface KeyUsage {
  key_id: number
  hours: number
  points: KeyUsagePoint[]
  total_requests: number
  total_consumed_micro: number
}

/** 密钥维度的用量趋势；账户级概览只给总量，看不出单把密钥的使用情况。 */
export function useKeyUsage(id: string | undefined, hours: number) {
  return useQuery({
    queryKey: ['console', 'keys', 'usage', id, hours],
    queryFn: () => api.get<KeyUsage>(`/api/console/keys/${id as string}/usage?hours=${hours}`),
    enabled: Boolean(id),
    placeholderData: keepPreviousData,
  })
}

export function useKeyAudit(id: string | undefined, page: number) {
  return useQuery({
    queryKey: ['console', 'keys', 'audit', id, page],
    queryFn: () => fetchKeyAudit(id as string, page),
    enabled: Boolean(id),
    placeholderData: keepPreviousData,
  })
}

export function useConsoleModels(enabled = true) {
  return useQuery({
    queryKey: ['console', 'models', 'all'],
    queryFn: fetchAllModels,
    enabled,
    staleTime: 5 * 60_000,
  })
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

function invalidateKeys(client: ReturnType<typeof useQueryClient>, id?: number | string) {
  void client.invalidateQueries({ queryKey: ['console', 'keys'] })
  void client.invalidateQueries({ queryKey: ['console', 'overview'] })
  if (id !== undefined) {
    void client.invalidateQueries({ queryKey: ['console', 'keys', 'detail', String(id)] })
  }
}

export function useCreateKeys() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateKeysInput) =>
      api.post<CreateKeysResponse>('/api/console/keys', input),
    onSuccess: () => invalidateKeys(client),
  })
}

export function useUpdateKey() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: UpdateKeyInput }) =>
      api.patch<UpdateKeyResponse>(`/api/console/keys/${encodeURIComponent(String(id))}`, input),
    onSuccess: (_response, variables) => invalidateKeys(client, variables.id),
  })
}

export function useRotateKey() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id: number | string) =>
      api.post<RotateKeyResponse>(`/api/console/keys/${encodeURIComponent(String(id))}/rotate`),
    onSuccess: (_response, id) => invalidateKeys(client, id),
  })
}

export function useDeleteKey() {
  const client = useQueryClient()
  return useMutation({
    // The backend requires an explicit confirm flag in the DELETE body.
    mutationFn: (id: number | string) =>
      api.delete<DeleteKeyResponse>(`/api/console/keys/${encodeURIComponent(String(id))}`, {
        confirm: true,
      }),
    onSuccess: (_response, id) => invalidateKeys(client, id),
  })
}

export function useCreateQuotaApply() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ input, idempotencyKey }: { input: CreateQuotaApplyInput; idempotencyKey: string }) =>
      api.post<QuotaApplyResponse>('/api/console/quota-applies', input, {
        headers: { 'Idempotency-Key': idempotencyKey },
      }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['console', 'quota-applies'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Model plaza calls
// ---------------------------------------------------------------------------

function modelCatalogQuery(params: ModelCatalogParams): string {
  const search = new URLSearchParams()
  search.set('page', String(params.page ?? 1))
  search.set('page_size', String(params.pageSize ?? 20))
  if (params.modelType) search.set('model_type', params.modelType)
  if (params.protocol) search.set('protocol', params.protocol)
  if (params.sort) search.set('sort', params.sort)
  if (params.order) search.set('order', params.order)
  const q = params.q?.trim()
  if (q) search.set('q', q)
  return search.toString()
}

export function fetchModelCatalog(params: ModelCatalogParams): Promise<ModelCatalogResponse> {
  return api.get<ModelCatalogResponse>(`/api/console/models?${modelCatalogQuery(params)}`)
}

export function useModelCatalog(params: ModelCatalogParams) {
  return useQuery({
    queryKey: ['console', 'models', 'catalog', params],
    queryFn: () => fetchModelCatalog(params),
    placeholderData: keepPreviousData,
  })
}

// ---------------------------------------------------------------------------
// Usage / bill / export calls
// ---------------------------------------------------------------------------

function usageQuery(params: UsageParams): string {
  const search = new URLSearchParams()
  search.set('from', params.from)
  search.set('to', params.to)
  search.set('dimension', params.dimension)
  if (params.modelId !== undefined) search.set('model_id', String(params.modelId))
  if (params.channelId !== undefined) search.set('channel_id', String(params.channelId))
  return search.toString()
}

export function fetchUsage(params: UsageParams): Promise<UsageResponse> {
  return api.get<UsageResponse>(`/api/console/usage?${usageQuery(params)}`)
}

export function fetchBill(month: string | undefined): Promise<BillResponse> {
  const suffix = month ? `?month=${encodeURIComponent(month)}` : ''
  return api.get<BillResponse>(`/api/console/bills${suffix}`)
}

export function startUsageExport(params: UsageParams): Promise<ExportStartResponse> {
  return api.post<ExportStartResponse>(`/api/console/usage/export?${usageQuery(params)}`)
}

export function fetchExportStatus(taskId: string): Promise<ExportStatusResponse> {
  return api.get<ExportStatusResponse>(`/api/console/exports/${encodeURIComponent(taskId)}`)
}

export function useUsage(params: UsageParams) {
  return useQuery({
    queryKey: ['console', 'usage', params],
    queryFn: () => fetchUsage(params),
    placeholderData: keepPreviousData,
  })
}

export function useBill(month: string) {
  return useQuery({
    queryKey: ['console', 'bills', month],
    queryFn: () => fetchBill(month),
    placeholderData: keepPreviousData,
  })
}

export function useStartUsageExport() {
  return useMutation({ mutationFn: (params: UsageParams) => startUsageExport(params) })
}

/**
 * Polls a signed export task token until it leaves RUNNING. A DONE response
 * carries the signed download URL; a missing/expired token answers 404 and the
 * page renders that message verbatim (the link lives for one hour).
 */
export function useExportStatus(taskId: string | null) {
  return useQuery({
    queryKey: ['console', 'usage', 'export', taskId],
    queryFn: () => fetchExportStatus(taskId as string),
    enabled: Boolean(taskId),
    retry: false,
    refetchInterval: (query) =>
      query.state.data?.state === 'RUNNING' ? 2_000 : false,
  })
}

// ---------------------------------------------------------------------------
// Fund calls
// ---------------------------------------------------------------------------

export function fetchPlans(): Promise<PlanList> {
  return api.get<PlanList>('/api/console/plans')
}

function ordersQuery(params: OrderListParams): string {
  const search = new URLSearchParams()
  search.set('page', String(params.page ?? 1))
  search.set('page_size', String(params.pageSize ?? 20))
  if (params.status) search.set('status', params.status)
  const q = params.q?.trim()
  if (q) search.set('q', q)
  return search.toString()
}

export function fetchOrders(params: OrderListParams): Promise<OrderList> {
  return api.get<OrderList>(`/api/console/orders?${ordersQuery(params)}`)
}

function refundsQuery(params: RefundListParams): string {
  const search = new URLSearchParams()
  search.set('page', String(params.page ?? 1))
  search.set('page_size', String(params.pageSize ?? 20))
  if (params.status) search.set('status', params.status)
  return search.toString()
}

export function fetchRefunds(params: RefundListParams): Promise<RefundList> {
  return api.get<RefundList>(`/api/console/refunds?${refundsQuery(params)}`)
}

function invoicesQuery(params: InvoiceListParams): string {
  const search = new URLSearchParams()
  search.set('page', String(params.page ?? 1))
  search.set('page_size', String(params.pageSize ?? 20))
  if (params.status) search.set('status', params.status)
  if (params.sort) search.set('sort', params.sort)
  if (params.order) search.set('order', params.order)
  return search.toString()
}

export function fetchInvoices(params: InvoiceListParams): Promise<InvoiceList> {
  return api.get<InvoiceList>(`/api/console/invoices?${invoicesQuery(params)}`)
}

export function usePlans() {
  return useQuery({
    queryKey: ['console', 'plans'],
    queryFn: fetchPlans,
    staleTime: 5 * 60_000,
  })
}

export function useOrders(params: OrderListParams) {
  return useQuery({
    queryKey: ['console', 'orders', 'list', params],
    queryFn: () => fetchOrders(params),
    placeholderData: keepPreviousData,
  })
}

export function useRefunds(params: RefundListParams) {
  return useQuery({
    queryKey: ['console', 'refunds', 'list', params],
    queryFn: () => fetchRefunds(params),
    placeholderData: keepPreviousData,
  })
}

export function useInvoices(params: InvoiceListParams) {
  return useQuery({
    queryKey: ['console', 'invoices', 'list', params],
    queryFn: () => fetchInvoices(params),
    placeholderData: keepPreviousData,
  })
}

/**
 * Order / refund / invoice creation share the backend's 10-second
 * Idempotency-Key window. The key is generated per submission, so a repeated
 * click cannot create two orders.
 */
export function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(36).slice(2)}`
}

export function useCreateOrder() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateOrderInput) =>
      api.post<CreatedOrder>('/api/console/orders', input, {
        headers: { 'Idempotency-Key': newIdempotencyKey() },
      }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['console', 'orders'] })
    },
  })
}

export function useRedeemCode() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (code: string) => api.post<RedeemResult>('/api/console/redeem', { code }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['console', 'overview'] })
    },
  })
}

export function useCreateRefund() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (rechargeOrderId: number) =>
      api.post<CreatedRefund>(
        '/api/console/refunds',
        { recharge_order_id: rechargeOrderId },
        { headers: { 'Idempotency-Key': newIdempotencyKey() } },
      ),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['console', 'refunds'] })
      void client.invalidateQueries({ queryKey: ['console', 'orders'] })
      void client.invalidateQueries({ queryKey: ['console', 'overview'] })
    },
  })
}

export function useCreateInvoice() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateInvoiceInput) =>
      api.post<CreatedInvoice>('/api/console/invoices', input, {
        headers: { 'Idempotency-Key': newIdempotencyKey() },
      }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['console', 'invoices'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Request logs (GET /api/console/logs*)
// ---------------------------------------------------------------------------

export type LogResult = 'SUCCESS' | 'PARTIAL' | 'FAILED'
/** The API's filter values, not the stored result enum. */
export type LogStateFilter = 'all' | 'success' | 'failed' | 'partial' | 'rate_limited'

export interface LogItem {
  request_id: string
  api_key_id: number
  api_key_name: string
  model_id: number | null
  model_name: string
  request_model: string
  inbound_protocol: string
  channel_id: number | null
  channel_name: string | null
  group_code: string
  is_stream: boolean
  http_status: number
  biz_code: string
  result: LogResult
  fail_stage: string | null
  fail_owner: string | null
  tokens_in: number
  tokens_out: number
  consumed_micro: number
  upstream_attempts: number
  ttft_ms: number | null
  total_ms: number
  upstream_ms: number
  client_ip: string
  client_aborted: boolean
  full_log_saved: boolean
  created_at: string
}

export interface LogListResponse extends Page<LogItem> {
  from: string
  to: string
  window_days: number
}

export interface LogListParams {
  from: string
  to: string
  state?: LogStateFilter
  /** Exact match against model_name_snap, per the backend. */
  model?: string
  page?: number
  pageSize?: number
}

/** Detail rows add settlement fields that the list omits. */
export interface LogDetailRequest extends LogItem {
  outbound_protocol: string | null
  prehold_micro: number
  consumed_recharge_micro: number
  consumed_bonus_micro: number
}

export interface LogAttempt {
  attempt_index: number
  result: string
  http_status: number | null
  phase: string
  channel_id: number
  channel_name: string
  protocol_code: string
  upstream_model: string
  connect_ms: number | null
  first_byte_ms: number | null
  total_ms: number
  tokens_in: number
  tokens_out: number
  is_retryable: boolean
  retry_decision: string | null
  error_class: string | null
  upstream_code: string | null
  upstream_msg: string | null
  created_at: string
}

export interface LogDetailResponse {
  request: LogDetailRequest
  /** The backend key is `timeline`, not `attempts`. */
  timeline: LogAttempt[]
}

export interface LogBodyResponse {
  request_id: string
  model_name: string
  created_at: string
  body: string
  redacted: boolean
}

function logsQuery(params: LogListParams): string {
  const search = new URLSearchParams()
  search.set('page', String(params.page ?? 1))
  search.set('page_size', String(params.pageSize ?? 20))
  search.set('from', params.from)
  search.set('to', params.to)
  if (params.state && params.state !== 'all') search.set('state', params.state)
  const model = params.model?.trim()
  if (model) search.set('model', model)
  search.set('sort', 'created_at')
  search.set('order', 'desc')
  return search.toString()
}

export function fetchLogs(params: LogListParams): Promise<LogListResponse> {
  return api.get<LogListResponse>(`/api/console/logs?${logsQuery(params)}`)
}

export function fetchLogDetail(requestId: string): Promise<LogDetailResponse> {
  return api.get<LogDetailResponse>(`/api/console/logs/${encodeURIComponent(requestId)}`)
}

export function fetchLogBody(requestId: string): Promise<LogBodyResponse> {
  return api.get<LogBodyResponse>(
    `/api/console/logs/${encodeURIComponent(requestId)}/body`,
  )
}

export function useLogs(params: LogListParams) {
  return useQuery({
    queryKey: ['console', 'logs', 'list', params],
    queryFn: () => fetchLogs(params),
    placeholderData: keepPreviousData,
  })
}

export function useLogDetail(requestId: string | null) {
  return useQuery({
    queryKey: ['console', 'logs', 'detail', requestId],
    queryFn: () => fetchLogDetail(requestId as string),
    enabled: Boolean(requestId),
  })
}

/**
 * The body is fetched only after the user asks for it: most requests never
 * store one, and the 404 copy is meaningful (see PRD F11-3).
 */
export function useLogBody(requestId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: ['console', 'logs', 'body', requestId],
    queryFn: () => fetchLogBody(requestId as string),
    enabled: enabled && Boolean(requestId),
    retry: false,
  })
}

// ---------------------------------------------------------------------------
// Tickets (GET/POST /api/console/tickets*)
// ---------------------------------------------------------------------------

export type TicketState = 'OPEN' | 'RESPONDED' | 'CLOSED'
export type TicketType = 'BILLING' | 'TECH' | 'CONTENT_APPEAL' | 'OTHER'

export interface TicketSLA {
  first_response_due_at: string
  first_responded_at: string | null
  responded: boolean
  overdue: boolean
  remaining_seconds: number
  overdue_seconds: number
  /** Backend-composed copy: 已响应（用时 xx）/ 剩余 xx / 已超时 xx. */
  display: string
}

export interface TicketReply {
  id: number
  author_type: 'USER' | 'OPS'
  content: string
  created_at: string
}

export interface Ticket {
  id: number
  ticket_no: string
  ticket_type: TicketType
  title: string
  description: string
  request_id: string | null
  policy_hit_id: number | null
  policy_rule_id: string | null
  policy_category: string | null
  state: TicketState
  first_response_due_at: string
  first_responded_at: string | null
  last_replied_at: string | null
  reopen_count: number
  created_at: string
  updated_at: string
  sla: TicketSLA
  /** Internal notes are filtered in SQL; this array never carries them. */
  replies: TicketReply[]
}

export interface TicketListItem {
  id: number
  ticket_no: string
  ticket_type: TicketType
  title: string
  state: TicketState
  first_response_due_at: string
  first_responded_at: string | null
  last_replied_at: string | null
  created_at: string
  sla: TicketSLA
}

export type TicketList = Page<TicketListItem>

export interface TicketListParams {
  state?: TicketState | 'all'
  type?: TicketType | 'all'
  page?: number
  pageSize?: number
  /** The list is always ordered by created_at; only the direction is settable. */
  order?: 'asc' | 'desc'
}

export interface CreateTicketInput {
  ticket_type: TicketType
  title: string
  description: string
  request_id?: string
}

export interface CreatedTicket {
  ticket: Ticket
  request_id_source: '' | 'USER' | 'POLICY_HIT'
  notice?: string
}

export interface RequestIDItem {
  request_id: string
  model_name: string
  http_status: number
  result: LogResult
  created_at: string
}

export interface RequestIDList extends Page<RequestIDItem> {
  /** Backend-computed 90-day window start; display it, never recompute. */
  since: string
}

export interface RequestIDParams {
  q?: string
  page?: number
  pageSize?: number
}

function ticketsQuery(params: TicketListParams): string {
  const search = new URLSearchParams()
  search.set('page', String(params.page ?? 1))
  search.set('page_size', String(params.pageSize ?? 20))
  if (params.state && params.state !== 'all') search.set('state', params.state)
  if (params.type && params.type !== 'all') search.set('type', params.type)
  search.set('sort', 'created_at')
  search.set('order', params.order ?? 'desc')
  return search.toString()
}

function requestIdsQuery(params: RequestIDParams): string {
  const search = new URLSearchParams()
  search.set('page', String(params.page ?? 1))
  search.set('page_size', String(params.pageSize ?? 20))
  const q = params.q?.trim()
  if (q) search.set('q', q)
  search.set('order', 'desc')
  return search.toString()
}

export function fetchTickets(params: TicketListParams): Promise<TicketList> {
  return api.get<TicketList>(`/api/console/tickets?${ticketsQuery(params)}`)
}

export function fetchTicket(id: number): Promise<Ticket> {
  return api.get<Ticket>(`/api/console/tickets/${id}`)
}

export function fetchRequestIds(params: RequestIDParams): Promise<RequestIDList> {
  return api.get<RequestIDList>(`/api/console/request-ids?${requestIdsQuery(params)}`)
}

export function useTickets(params: TicketListParams) {
  return useQuery({
    queryKey: ['console', 'tickets', 'list', params],
    queryFn: () => fetchTickets(params),
    placeholderData: keepPreviousData,
  })
}

export function useTicket(id: number | null) {
  return useQuery({
    queryKey: ['console', 'tickets', 'detail', id],
    queryFn: () => fetchTicket(id as number),
    enabled: id !== null,
  })
}

/**
 * The picker must not fetch until the dialog is open and the selected type
 * actually uses a linked request. The 90-day window is a backend predicate;
 * this hook never sends from/to.
 */
export function useRequestIds(params: RequestIDParams, enabled = true) {
  return useQuery({
    queryKey: ['console', 'request-ids', params],
    queryFn: () => fetchRequestIds(params),
    enabled,
    placeholderData: keepPreviousData,
  })
}

export function useCreateTicket() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateTicketInput) =>
      api.post<CreatedTicket>('/api/console/tickets', input),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['console', 'tickets'] })
    },
  })
}

export function useCloseTicket() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id: number) =>
      api.post<Ticket>(`/api/console/tickets/${id}/close`, {}),
    onSuccess: (ticket) => {
      client.setQueryData(['console', 'tickets', 'detail', ticket.id], ticket)
      void client.invalidateQueries({ queryKey: ['console', 'tickets', 'list'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Playground (POST /api/console/playground/chat)
// ---------------------------------------------------------------------------

const PLAYGROUND_PATH = '/api/console/playground/chat'
const PLAYGROUND_BASE_URL = (import.meta.env.VITE_API_BASE_URL ?? '').replace(/\/+$/, '')

export interface PlaygroundChatRequest {
  /** Outer field only. Never merged into `body`. */
  keyId: number
  /** Forwarded verbatim; the console does not add or rewrite fields. */
  body: Record<string, unknown>
  signal?: AbortSignal
  /** Receives each content delta as it arrives (the whole text at once when stream=false). */
  onDelta?: (text: string) => void
  /** Called once after a successful stream or JSON response. Not called on abort/error. */
  onDone?: () => void
}

export interface PlaygroundChatResult {
  /** `X-Credits-Remaining`: pre-deduction balance in credits, null when the header is absent. */
  creditsRemaining: number | null
  /** Parsed JSON payload for a non-streaming response; null for streams. */
  payload: unknown
  /** Concatenated assistant text actually received. */
  content: string
  streamed: boolean
}

/** ApiError carrying the response's `X-Credits-Remaining` (402 always has one). */
export interface PlaygroundChatError extends ApiError {
  creditsRemaining: number | null
}

type ErrorBody = components['schemas']['ErrorBody']
type ErrorEnvelope = components['schemas']['ErrorEnvelope']

function isAbortError(error: unknown): boolean {
  return (
    typeof error === 'object' &&
    error !== null &&
    (error as { name?: unknown }).name === 'AbortError'
  )
}

function playgroundNetworkError(): ApiError {
  return new ApiError({
    status: 0,
    type: 'network_error',
    code: 'network_error',
    message: '网络连接失败，请检查网络后重试。',
    hint: '确认网络可达后重新加载页面。',
  })
}

function readCreditsRemaining(response: Response): number | null {
  const raw = response.headers.get('X-Credits-Remaining')
  if (raw === null || raw.trim() === '') return null
  const value = Number(raw)
  return Number.isFinite(value) ? value : null
}

function parseJson(text: string): unknown {
  if (!text) return null
  try {
    return JSON.parse(text) as unknown
  } catch {
    return null
  }
}

function readErrorDetail(payload: unknown): ErrorBody | null {
  if (typeof payload !== 'object' || payload === null) return null
  const error = (payload as Partial<ErrorEnvelope>).error
  if (!error || typeof error.code !== 'string' || typeof error.message !== 'string') {
    return null
  }
  return error
}

function apiErrorFromDetail(
  detail: ErrorBody | null,
  status: number,
  creditsRemaining: number | null,
): PlaygroundChatError {
  const error = new ApiError({
    status,
    type: detail?.type ?? 'internal_error',
    code: detail?.code ?? `http_${status}`,
    message: detail?.message ?? (status >= 500 ? '服务暂时不可用，请稍后重试。' : `请求失败（HTTP ${status}）。`),
    hint: detail?.hint ?? '',
    requestId: detail?.request_id,
    retryAfterSeconds: detail?.retry_after_seconds,
  }) as PlaygroundChatError
  error.creditsRemaining = creditsRemaining
  return error
}

/** Reads X-Credits-Remaining off an error thrown by playgroundChat. */
export function playgroundCreditsRemaining(error: unknown): number | null {
  if (typeof error !== 'object' || error === null) return null
  const value = (error as { creditsRemaining?: unknown }).creditsRemaining
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

function contentOf(value: unknown): string {
  if (typeof value !== 'object' || value === null) return ''
  const content = (value as { content?: unknown }).content
  if (typeof content === 'string') return content
  // OpenAI-compatible upstreams normally return a string, but some emit an
  // array of text parts. Join the text parts rather than dropping the delta.
  if (Array.isArray(content)) {
    return content
      .map((part) => {
        if (typeof part === 'string') return part
        if (
          typeof part === 'object' &&
          part !== null &&
          typeof (part as { text?: unknown }).text === 'string'
        ) {
          return (part as { text: string }).text
        }
        return ''
      })
      .join('')
  }
  return ''
}

function firstChoiceContent(payload: unknown): string {
  if (typeof payload !== 'object' || payload === null) return ''
  const choices = (payload as { choices?: unknown }).choices
  if (!Array.isArray(choices) || choices.length === 0) return ''
  const choice = choices[0]
  if (typeof choice !== 'object' || choice === null) return ''
  // Streaming chunks carry `delta`; a non-streaming body carries `message`.
  return contentOf((choice as { delta?: unknown }).delta) || contentOf((choice as { message?: unknown }).message)
}

/**
 * Calls the T27 console proxy with the session cookie and CSRF header.
 *
 * The browser's one-shot event-source API cannot be used here: the endpoint is
 * POST, requires the CSRF double-submit header, and has to be abortable.
 * Reading the body through `fetch` + `ReadableStream` satisfies all three.
 */
export async function playgroundChat({
  keyId,
  body,
  signal,
  onDelta,
  onDone,
}: PlaygroundChatRequest): Promise<PlaygroundChatResult> {
  const headers = new Headers()
  headers.set('Content-Type', 'application/json')
  headers.set('Accept', body.stream === true ? 'text/event-stream' : 'application/json')
  const csrf = getCsrfToken()
  if (csrf) headers.set(CSRF_HEADER, csrf)

  let response: Response
  try {
    response = await fetch(`${PLAYGROUND_BASE_URL}${PLAYGROUND_PATH}`, {
      method: 'POST',
      headers,
      credentials: 'include',
      body: JSON.stringify({ key_id: keyId, body }),
      signal,
    })
  } catch (error) {
    if (isAbortError(error)) throw error
    throw playgroundNetworkError()
  }

  const creditsRemaining = readCreditsRemaining(response)

  if (!response.ok) {
    const detail = readErrorDetail(parseJson(await response.text()))
    throw apiErrorFromDetail(detail, response.status, creditsRemaining)
  }

  const streamed = (response.headers.get('Content-Type') ?? '').includes('text/event-stream')
  if (!streamed) {
    const payload = parseJson(await response.text())
    const text = firstChoiceContent(payload)
    if (text) onDelta?.(text)
    onDone?.()
    return { creditsRemaining, payload, content: text, streamed: false }
  }

  const reader = response.body?.getReader()
  if (!reader) throw playgroundNetworkError()

  const decoder = new TextDecoder()
  const parser = createSSEParser()
  let content = ''
  let finished = false

  const handleEvents = (events: SSEEvent[]) => {
    for (const event of events) {
      if (event.done) {
        finished = true
        return
      }
      const payload = parseJson(event.data)
      // gatewayd signals mid-stream failures as a `data: {"error":{...}}`
      // frame. Surface it instead of rendering an empty successful reply.
      const detail = readErrorDetail(payload)
      if (detail) throw apiErrorFromDetail(detail, response.status, creditsRemaining)
      const text = firstChoiceContent(payload)
      if (text) {
        content += text
        onDelta?.(text)
      }
    }
  }

  try {
    while (!finished) {
      const { done, value } = await reader.read()
      if (done) break
      // stream:true holds an incomplete multi-byte UTF-8 sequence until the
      // next chunk; without it every split Chinese character becomes U+FFFD.
      handleEvents(parser.push(decoder.decode(value, { stream: true })))
    }
    if (!finished) {
      handleEvents(parser.push(decoder.decode()))
      handleEvents(parser.flush())
    }
  } catch (error) {
    if (error instanceof ApiError || isAbortError(error)) throw error
    throw playgroundNetworkError()
  } finally {
    try {
      await reader.cancel()
    } catch {
      // The connection is already gone; releasing the lock is all that matters.
    }
    reader.releaseLock()
  }

  onDone?.()
  return { creditsRemaining, payload: null, content, streamed: true }
}

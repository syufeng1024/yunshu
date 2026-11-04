import {
  keepPreviousData,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'
import { api } from './client'
import type {
  OrderState,
  OrderStatusFilter,
  Page,
  TicketReply,
  TicketSLA,
  TicketState,
  TicketType,
} from './console'

/**
 * Operator console contracts, transcribed from the Go handlers in
 * internal/control/{ops,review,ticket,auditq}. Field names are the JSON tags,
 * not the Go names. The routes themselves are declared in
 * internal/control/server.go:246-330.
 *
 * Pagination always uses mw.ParsePage's vocabulary: page / page_size / sort /
 * order. Never limit / offset. A list endpoint only accepts the sort columns
 * written into its own ParsePage call; sending another value is a 400, so the
 * params types below narrow sort per resource.
 */

export type { OrderState, OrderStatusFilter, Page, TicketReply, TicketSLA, TicketState, TicketType }

export type SortOrder = 'asc' | 'desc'
export type Region = 'DOMESTIC' | 'OVERSEAS'
export type ScopeMode = 'ALL' | 'SELECTED'

interface PageParams {
  page?: number
  pageSize?: number
}

function pageQuery(params: PageParams): URLSearchParams {
  const search = new URLSearchParams()
  search.set('page', String(params.page ?? 1))
  search.set('page_size', String(params.pageSize ?? 20))
  return search
}

function sortedPageQuery(params: PageParams & { sort?: string; order?: SortOrder }): URLSearchParams {
  const search = pageQuery(params)
  if (params.sort) search.set('sort', params.sort)
  if (params.order) search.set('order', params.order)
  return search
}

// ---------------------------------------------------------------------------
// Overview (GET /api/admin/overview, T24)
// ---------------------------------------------------------------------------

export interface AdminOverviewToday {
  requests: number
  consumed_micro: number
  /** null when the internal availability denominator is zero; never 0 for "no data". */
  success_rate_bp: number | null
}

export interface AdminOverviewAlerts {
  dangling_prehold: number
  /** Open reconciliation differences in the last 30 days. */
  recon_diff: number
  sla_overdue: number
  /** Domestic, enabled, not deleted channels whose health_state is not CLOSED. */
  channel_abnormal: number
}

export interface AdminOverviewCounts {
  pending_reviews: number
  /** OPEN + RESPONDED, i.e. unresolved. */
  open_tickets: number
}

export interface AdminOverview {
  today: AdminOverviewToday
  alerts: AdminOverviewAlerts
  counts: AdminOverviewCounts
}

export function fetchAdminOverview(): Promise<AdminOverview> {
  return api.get<AdminOverview>('/api/admin/overview')
}

export function useAdminOverview() {
  return useQuery({
    queryKey: ['admin', 'overview'],
    queryFn: fetchAdminOverview,
  })
}

// ---------------------------------------------------------------------------
// Channels (internal/control/ops/channel.go)
// ---------------------------------------------------------------------------

export type ChannelHealthState = 'CLOSED' | 'OPEN' | 'HALF_OPEN' | 'AUTH_FAIL'
export type StreamUsageMode = 'NATIVE' | 'STREAM_OPTIONS' | 'ESTIMATE' | 'UNKNOWN'

/** channelView. There is deliberately no upstream key field: the mask is all the API returns. */
export interface Channel {
  id: number
  name: string
  protocol_id: number
  protocol: string
  region: Region
  base_url: string
  /** Server-composed mask (sk-****last4); the envelope never leaves the API. */
  upstream_key_mask: string
  key_rotation_at?: string | null
  group_scope_mode: ScopeMode
  dedicated_scope_mode: ScopeMode
  fallback_allowed: boolean
  dedicated_user_ids?: number[]
  weight: number
  priority: number
  timeout_connect_ms: number
  timeout_first_byte_ms: number
  timeout_total_ms: number
  retry_max: number
  stream_supported: boolean
  stream_usage_mode: StreamUsageMode
  upstream_model_name_map?: Record<string, string>
  health_state: ChannelHealthState | string
  /** Last-15-minute upstream attempt success rate in basis points; null = no attempts. */
  success_rate_bp: number | null
  enabled: boolean
  created_at: string
  updated_at: string
}

export type ChannelListResponse = Page<Channel>
export interface ChannelDetailResponse {
  channel: Channel
}

export interface ChannelListParams extends PageParams {
  q?: string
  region?: Region
  /** Backend accepts the strings "0" / "1", not booleans. */
  enabled?: '0' | '1'
  groupScopeMode?: ScopeMode
  sort?: 'created_at' | 'updated_at' | 'name' | 'weight' | 'priority'
  order?: SortOrder
}

/** channelCreateRequest / channelPatchRequest. Every write field is optional on PATCH. */
export interface ChannelWriteInput {
  name?: string
  protocol_id?: number
  region?: Region
  base_url?: string
  /**
   * Create requires it. On PATCH, omit the key entirely when the operator did
   * not click 「重新填写」: an empty string is a validation error, and the
   * browser must never re-send the stored secret.
   */
  upstream_key?: string
  group_scope_mode?: ScopeMode
  dedicated_scope_mode?: ScopeMode
  dedicated_user_ids?: number[]
  fallback_allowed?: boolean
  weight?: number
  priority?: number
  timeout_connect_ms?: number
  timeout_first_byte_ms?: number
  timeout_total_ms?: number
  retry_max?: number
  stream_supported?: boolean
  stream_usage_mode?: StreamUsageMode
  upstream_model_name_map?: Record<string, string>
  enabled?: boolean
}

export interface ChannelDeleteResponse {
  id: number
  deleted: boolean
}

function channelQuery(params: ChannelListParams): string {
  const search = sortedPageQuery(params)
  const q = params.q?.trim()
  if (q) search.set('q', q)
  if (params.region) search.set('region', params.region)
  if (params.enabled) search.set('enabled', params.enabled)
  if (params.groupScopeMode) search.set('group_scope_mode', params.groupScopeMode)
  return search.toString()
}

export function fetchChannels(params: ChannelListParams = {}): Promise<ChannelListResponse> {
  return api.get<ChannelListResponse>(`/api/admin/channels?${channelQuery(params)}`)
}

/**
 * Walks every page for the overview's health count. The endpoint has no
 * health_state filter, so counting only page 1 would under-report.
 */
export async function fetchAllChannels(): Promise<Channel[]> {
  const pageSize = 100
  const out: Channel[] = []
  for (let page = 1; page <= 10; page += 1) {
    const response = await fetchChannels({ page, pageSize })
    out.push(...response.items)
    if (response.items.length < pageSize || out.length >= response.total) break
  }
  return out
}

export function fetchChannel(id: number | string): Promise<ChannelDetailResponse> {
  return api.get<ChannelDetailResponse>(
    `/api/admin/channels/${encodeURIComponent(String(id))}`,
  )
}

export function createChannel(input: ChannelWriteInput): Promise<ChannelDetailResponse> {
  return api.post<ChannelDetailResponse>('/api/admin/channels', input)
}

export function updateChannel(
  id: number | string,
  input: ChannelWriteInput,
): Promise<ChannelDetailResponse> {
  return api.patch<ChannelDetailResponse>(
    `/api/admin/channels/${encodeURIComponent(String(id))}`,
    input,
  )
}

export function disableChannel(id: number | string): Promise<ChannelDetailResponse> {
  return api.post<ChannelDetailResponse>(
    `/api/admin/channels/${encodeURIComponent(String(id))}/disable`,
  )
}

export function deleteChannel(id: number | string): Promise<ChannelDeleteResponse> {
  return api.delete<ChannelDeleteResponse>(
    `/api/admin/channels/${encodeURIComponent(String(id))}`,
  )
}

export function useChannels(params: ChannelListParams = {}) {
  return useQuery({
    queryKey: ['admin', 'channels', 'list', params],
    queryFn: () => fetchChannels(params),
    placeholderData: keepPreviousData,
  })
}

export function useAllChannels(enabled = true) {
  return useQuery({
    queryKey: ['admin', 'channels', 'all'],
    queryFn: fetchAllChannels,
    enabled,
    staleTime: 60_000,
  })
}

export function useChannel(id: number | string | null | undefined) {
  return useQuery({
    queryKey: ['admin', 'channels', 'detail', String(id)],
    queryFn: () => fetchChannel(id as number | string),
    enabled: id !== null && id !== undefined,
  })
}

function invalidateChannels(client: ReturnType<typeof useQueryClient>) {
  void client.invalidateQueries({ queryKey: ['admin', 'channels'] })
}

export function useCreateChannel() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: ChannelWriteInput) => createChannel(input),
    onSuccess: () => invalidateChannels(client),
  })
}

export function useUpdateChannel() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: ChannelWriteInput }) =>
      updateChannel(id, input),
    onSuccess: () => invalidateChannels(client),
  })
}

export function useDisableChannel() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id: number | string) => disableChannel(id),
    onSuccess: () => invalidateChannels(client),
  })
}

export function useDeleteChannel() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id: number | string) => deleteChannel(id),
    onSuccess: () => invalidateChannels(client),
  })
}

// ---------------------------------------------------------------------------
// Models (internal/control/ops/model.go, alias.go)
// ---------------------------------------------------------------------------

export type ModelType = 'TEXT' | 'IMAGE' | 'EMBEDDING'
export type ListingState = 'LISTED' | 'CATALOG_ONLY' | 'DELISTED'

/** modelCapabilities: null means "not configured", which is not "unsupported". */
export interface ModelCapabilities {
  stream: boolean | null
  tool_call: boolean | null
  vision: boolean | null
  json_mode: boolean | null
}

/** modelPricingView. Read-only here; price writes live in PricingPage. */
export interface ModelPricingRule {
  price_rule_id: number
  version: number
  effective_from: string
  in_price_micro_per_k: number
  out_price_micro_per_k: number
  per_call_price_micro: number
  cache_hit_ratio_bp?: number | null
  cache_write_ratio_bp?: number | null
  long_ctx_threshold_tokens?: number | null
  long_ctx_in_price_micro_per_k?: number | null
  long_ctx_out_price_micro_per_k?: number | null
  cost_in_micro_per_k?: number | null
  cost_out_micro_per_k?: number | null
  cost_per_call_micro?: number | null
}

export interface AdminModel {
  id: number
  model_name: string
  display_name: string
  region: Region
  model_type: ModelType
  protocol_id: number
  protocol: string
  primary_channel_id?: number | null
  context_length: number
  capability_mask: number
  capabilities: ModelCapabilities
  listing_state: ListingState
  recommend_weight: number
  published_at?: string | null
  delisted_at?: string | null
  aliases: string[]
  /** null = no price rule is effective now; render "未配置", never a fake zero. */
  pricing: ModelPricingRule | null
  created_at: string
  updated_at: string
}

export type ModelListResponse = Page<AdminModel>
export interface ModelDetailResponse {
  model: AdminModel
}

export interface ModelListParams extends PageParams {
  q?: string
  modelType?: ModelType
  listingState?: ListingState
  region?: Region
  protocol?: string
  sort?: 'created_at' | 'updated_at' | 'model_name' | 'recommend_weight' | 'published_at'
  order?: SortOrder
}

/** modelCreateRequest. */
export interface ModelCreateInput {
  model_name: string
  display_name: string
  region: Region
  model_type: ModelType
  protocol_id: number
  primary_channel_id?: number
  context_length?: number
  listing_state?: ListingState
  recommend_weight?: number
  aliases?: string[]
  capabilities?: Record<string, boolean>
}

/** modelPatchRequest. model_name is the single source of truth for renames (F22-1). */
export interface ModelPatchInput {
  model_name?: string
  display_name?: string
  region?: Region
  model_type?: ModelType
  protocol_id?: number
  primary_channel_id?: number
  context_length?: number
  listing_state?: ListingState
  recommend_weight?: number
  aliases?: string[]
  capabilities?: Record<string, boolean>
  reason?: string
}

export interface ListingStateInput {
  listing_state: ListingState
  reason?: string
}

export interface RecommendWeightInput {
  recommend_weight: number
  reason?: string
}

export interface AliasView {
  alias_name: string
  source: string
  created_at: string
}

export interface AliasListResponse {
  model_id: number
  aliases: AliasView[]
}

export interface AliasDetailResponse {
  alias: AliasView
}

export interface AliasDeleteResponse {
  deleted: boolean
  alias_name: string
}

function modelQuery(params: ModelListParams): string {
  const search = sortedPageQuery(params)
  const q = params.q?.trim()
  if (q) search.set('q', q)
  if (params.modelType) search.set('model_type', params.modelType)
  if (params.listingState) search.set('listing_state', params.listingState)
  if (params.region) search.set('region', params.region)
  if (params.protocol) search.set('protocol', params.protocol)
  return search.toString()
}

export function fetchModels(params: ModelListParams = {}): Promise<ModelListResponse> {
  return api.get<ModelListResponse>(`/api/admin/models?${modelQuery(params)}`)
}

export function fetchModel(id: number | string): Promise<ModelDetailResponse> {
  return api.get<ModelDetailResponse>(`/api/admin/models/${encodeURIComponent(String(id))}`)
}

export function createModel(input: ModelCreateInput): Promise<ModelDetailResponse> {
  return api.post<ModelDetailResponse>('/api/admin/models', input)
}

export function updateModel(
  id: number | string,
  input: ModelPatchInput,
): Promise<ModelDetailResponse> {
  return api.patch<ModelDetailResponse>(
    `/api/admin/models/${encodeURIComponent(String(id))}`,
    input,
  )
}

export function setListingState(
  id: number | string,
  input: ListingStateInput,
): Promise<ModelDetailResponse> {
  return api.post<ModelDetailResponse>(
    `/api/admin/models/${encodeURIComponent(String(id))}/listing-state`,
    input,
  )
}

export function setRecommendWeight(
  id: number | string,
  input: RecommendWeightInput,
): Promise<ModelDetailResponse> {
  return api.post<ModelDetailResponse>(
    `/api/admin/models/${encodeURIComponent(String(id))}/recommend-weight`,
    input,
  )
}

export function fetchModelAliases(
  id: number | string,
): Promise<AliasListResponse> {
  return api.get<AliasListResponse>(
    `/api/admin/models/${encodeURIComponent(String(id))}/aliases`,
  )
}

export function createModelAlias(
  id: number | string,
  aliasName: string,
): Promise<AliasDetailResponse> {
  return api.post<AliasDetailResponse>(
    `/api/admin/models/${encodeURIComponent(String(id))}/aliases`,
    { alias_name: aliasName },
  )
}

export function deleteModelAlias(
  id: number | string,
  aliasName: string,
): Promise<AliasDeleteResponse> {
  return api.delete<AliasDeleteResponse>(
    `/api/admin/models/${encodeURIComponent(String(id))}/aliases/${encodeURIComponent(aliasName)}`,
  )
}

export function useModels(params: ModelListParams = {}) {
  return useQuery({
    queryKey: ['admin', 'models', 'list', params],
    queryFn: () => fetchModels(params),
    placeholderData: keepPreviousData,
  })
}

export function useModelAliases(id: number | string | null | undefined) {
  return useQuery({
    queryKey: ['admin', 'models', 'aliases', String(id)],
    queryFn: () => fetchModelAliases(id as number | string),
    enabled: id !== null && id !== undefined,
  })
}

function invalidateModels(client: ReturnType<typeof useQueryClient>, id?: number | string) {
  void client.invalidateQueries({ queryKey: ['admin', 'models'] })
  if (id !== undefined) {
    void client.invalidateQueries({ queryKey: ['admin', 'models', 'aliases', String(id)] })
  }
}

export function useCreateModel() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: ModelCreateInput) => createModel(input),
    onSuccess: (response) => invalidateModels(client, response.model.id),
  })
}

export function useUpdateModel() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: ModelPatchInput }) =>
      updateModel(id, input),
    onSuccess: (response) => invalidateModels(client, response.model.id),
  })
}

export function useSetListingState() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: ListingStateInput }) =>
      setListingState(id, input),
    onSuccess: (response) => invalidateModels(client, response.model.id),
  })
}

export function useSetRecommendWeight() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: RecommendWeightInput }) =>
      setRecommendWeight(id, input),
    onSuccess: (response) => invalidateModels(client, response.model.id),
  })
}

export function useCreateModelAlias() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, alias }: { id: number | string; alias: string }) =>
      createModelAlias(id, alias),
    onSuccess: (_response, variables) => invalidateModels(client, variables.id),
  })
}

export function useDeleteModelAlias() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, alias }: { id: number | string; alias: string }) =>
      deleteModelAlias(id, alias),
    onSuccess: (_response, variables) => invalidateModels(client, variables.id),
  })
}

// ---------------------------------------------------------------------------
// Prices (internal/control/ops/pricing.go)
// ---------------------------------------------------------------------------

/** priceRuleView. Amounts are integer micro-credits; costs may be null. */
/** priceWriteRequest.clear_fields whitelist (T24). */
export type PriceClearField =
  | 'cache_hit_ratio_bp'
  | 'cache_write_ratio_bp'
  | 'long_ctx_threshold_tokens'
  | 'long_ctx_in_price_micro_per_k'
  | 'long_ctx_out_price_micro_per_k'
  | 'cost_in_micro_per_k'
  | 'cost_out_micro_per_k'
  | 'cost_per_call_micro'
  | 'effective_to'

export interface AdminPriceRule {
  id: number
  scope_level: string
  model_id: number
  model_name: string
  model_type: ModelType
  in_price_micro_per_k: number
  out_price_micro_per_k: number
  per_call_price_micro: number
  cache_hit_ratio_bp?: number | null
  cache_write_ratio_bp?: number | null
  long_ctx_threshold_tokens?: number | null
  long_ctx_in_price_micro_per_k?: number | null
  long_ctx_out_price_micro_per_k?: number | null
  cost_in_micro_per_k?: number | null
  cost_out_micro_per_k?: number | null
  cost_per_call_micro?: number | null
  currency_cost: string
  version: number
  effective_from: string
  effective_to?: string | null
  enabled: boolean
  created_at: string
}

export type PriceListResponse = Page<AdminPriceRule>
export interface PriceDetailResponse {
  price: AdminPriceRule
}

export interface PriceListParams extends PageParams {
  modelId?: number
  modelType?: ModelType
  enabled?: '0' | '1'
  sort?: 'created_at' | 'effective_from' | 'version' | 'id'
  order?: SortOrder
}

/**
 * priceWriteRequest. PATCH rejects model_id (a rule cannot move between
 * models), so callers omit it there.
 */
export interface PriceWriteInput {
  model_id?: number
  in_price_micro_per_k?: number
  out_price_micro_per_k?: number
  per_call_price_micro?: number
  cache_hit_ratio_bp?: number | null
  cache_write_ratio_bp?: number | null
  long_ctx_threshold_tokens?: number | null
  long_ctx_in_price_micro_per_k?: number | null
  long_ctx_out_price_micro_per_k?: number | null
  cost_in_micro_per_k?: number | null
  cost_out_micro_per_k?: number | null
  cost_per_call_micro?: number | null
  currency_cost?: string
  /** RFC3339. A raise must be at least server-now + 7 days (F16-4). */
  effective_from?: string
  effective_to?: string | null
  enabled?: boolean
  /**
   * Explicit NULLs. A missing field means "leave alone", so clearing an existing
   * value requires this list. A field may not appear here and also carry a
   * value in the same request (backend 400).
   */
  clear_fields?: PriceClearField[]
  /** Required when a sell price has no matching upstream cost. */
  confirm_no_cost?: boolean
}

function priceQuery(params: PriceListParams): string {
  const search = sortedPageQuery(params)
  if (params.modelId !== undefined) search.set('model_id', String(params.modelId))
  if (params.modelType) search.set('model_type', params.modelType)
  if (params.enabled) search.set('enabled', params.enabled)
  return search.toString()
}

export function fetchPrices(params: PriceListParams = {}): Promise<PriceListResponse> {
  return api.get<PriceListResponse>(`/api/admin/prices?${priceQuery(params)}`)
}

export function createPrice(input: PriceWriteInput): Promise<PriceDetailResponse> {
  return api.post<PriceDetailResponse>('/api/admin/prices', input)
}

export function updatePrice(
  id: number | string,
  input: PriceWriteInput,
): Promise<PriceDetailResponse> {
  return api.patch<PriceDetailResponse>(
    `/api/admin/prices/${encodeURIComponent(String(id))}`,
    input,
  )
}

export function usePrices(params: PriceListParams = {}, enabled = true) {
  return useQuery({
    queryKey: ['admin', 'prices', 'list', params],
    queryFn: () => fetchPrices(params),
    enabled,
    placeholderData: keepPreviousData,
  })
}

export function useCreatePrice() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: PriceWriteInput) => createPrice(input),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['admin', 'prices'] })
      void client.invalidateQueries({ queryKey: ['admin', 'models'] })
    },
  })
}

export function useUpdatePrice() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: PriceWriteInput }) =>
      updatePrice(id, input),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['admin', 'prices'] })
      void client.invalidateQueries({ queryKey: ['admin', 'models'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Groups (internal/control/ops/group.go)
// ---------------------------------------------------------------------------

export interface PriceGroup {
  id: number
  code: string
  name: string
  ratio_bp: number
  model_scope_mode: ScopeMode
  channel_scope_mode: ScopeMode
  self_service: boolean
  min_margin_bp?: number | null
  remark: string
  enabled: boolean
  model_ids: number[]
  channel_ids: number[]
  created_at: string
  updated_at: string
}

export interface GroupListResponse {
  items: PriceGroup[]
  total: number
}

export interface GroupKeySync {
  keys: number
  ok: boolean
  note?: string
}

export interface GroupDetailResponse {
  group: PriceGroup
  key_sync: GroupKeySync
  /**
   * T31: in-scope models whose floor could not be checked because they have no
   * cost price. Present on a 200 after `confirm: true`; also mirrored on the
   * 400 body and read back through `ApiError.warnings`.
   */
  warnings?: string[]
}

/** groupPatchRequest. ratio may be the decimal ("0.85") or the stored basis points. */
export interface GroupPatchInput {
  name?: string
  ratio?: number | string
  ratio_bp?: number
  model_scope_mode?: ScopeMode
  channel_scope_mode?: ScopeMode
  self_service?: boolean
  min_margin_bp?: number
  remark?: string
  enabled?: boolean
  model_ids?: number[]
  channel_ids?: number[]
  /**
   * T31: mandatory `true` for internal saves, and for non-internal ratio
   * changes whose in-scope models have no cost price (the 400 carries
   * `warnings[]` naming them).
   */
  confirm?: boolean
  /** T31: operator's written reason; mandatory for internal saves (audited). */
  reason?: string
}

export function fetchGroups(): Promise<GroupListResponse> {
  return api.get<GroupListResponse>('/api/admin/groups')
}

export function updateGroup(
  id: number | string,
  input: GroupPatchInput,
): Promise<GroupDetailResponse> {
  return api.patch<GroupDetailResponse>(
    `/api/admin/groups/${encodeURIComponent(String(id))}`,
    input,
  )
}

export function useGroups() {
  return useQuery({
    queryKey: ['admin', 'groups'],
    queryFn: fetchGroups,
  })
}

export function useUpdateGroup() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: GroupPatchInput }) =>
      updateGroup(id, input),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['admin', 'groups'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Tool integrations (internal/control/ops/integration.go, T33)
// ---------------------------------------------------------------------------

/**
 * integrationView. There is no credential field by design: the table stores a
 * URL template only, and the browser never receives an upstream key (F4-3).
 */
export interface AdminIntegration {
  tool_key: string
  display_name: string
  enabled: boolean
  /** null = no template stored yet; the site renders no deep link. */
  deep_link_template: string | null
}

export interface IntegrationListResponse {
  items: AdminIntegration[]
  total: number
}

export interface IntegrationDetailResponse {
  integration: AdminIntegration
}

/**
 * integrationPatchRequest. Only these two fields are accepted; the five tools
 * are fixed (D-30 Q3). Sending an empty deep_link_template clears it, and the
 * backend refuses enabled=true while the stored template is null.
 */
export interface IntegrationPatchInput {
  enabled?: boolean
  deep_link_template?: string
}

export function fetchIntegrations(): Promise<IntegrationListResponse> {
  return api.get<IntegrationListResponse>('/api/admin/integrations')
}

export function updateIntegration(
  toolKey: string,
  input: IntegrationPatchInput,
): Promise<IntegrationDetailResponse> {
  return api.patch<IntegrationDetailResponse>(
    `/api/admin/integrations/${encodeURIComponent(toolKey)}`,
    input,
  )
}

/**
 * Disabled until the operator opens the tab: the list is only needed there,
 * and every other PricingPage test would otherwise fetch it.
 */
export function useIntegrations(enabled = true) {
  return useQuery({
    queryKey: ['admin', 'integrations'],
    queryFn: fetchIntegrations,
    enabled,
  })
}

export function useUpdateIntegration() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ toolKey, input }: { toolKey: string; input: IntegrationPatchInput }) =>
      updateIntegration(toolKey, input),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['admin', 'integrations'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Announcements (internal/control/ops/announcement.go)
// ---------------------------------------------------------------------------

export type AnnouncementState = 'DRAFT' | 'SCHEDULED' | 'PUBLISHED' | 'OFFLINE'
export type AnnouncementType = 'NOTICE' | 'MAINTENANCE' | 'PROMOTION' | 'PRICE_CHANGE'

export interface Announcement {
  id: number
  title: string
  content: string
  ann_type: AnnouncementType
  position_mask: number
  pinned: boolean
  effective_from: string
  effective_to?: string | null
  state: AnnouncementState
  /** Persisted t_announcement_model associations; always an array, never null. */
  model_ids: number[]
  created_by: number
  created_at: string
  updated_at: string
}

export type AnnouncementListResponse = Page<Announcement>
export interface AnnouncementDetailResponse {
  announcement: Announcement
}

export interface AnnouncementListParams extends PageParams {
  state?: AnnouncementState
  annType?: AnnouncementType
  pinned?: '0' | '1'
  sort?: 'created_at' | 'updated_at' | 'effective_from' | 'id'
  order?: SortOrder
}

/** announcementWriteRequest. model_ids is persisted and also narrows the promotion conflict check. */
export interface AnnouncementWriteInput {
  title?: string
  content?: string
  ann_type?: AnnouncementType
  position_mask?: number
  pinned?: boolean
  publish_at?: string
  /** Visibility start. Must not be earlier than publish_at (backend 400). */
  effective_from?: string
  effective_to?: string | null
  model_ids?: number[]
}

export interface AnnouncementPublishInput {
  publish_at?: string
}

function announcementQuery(params: AnnouncementListParams): string {
  const search = sortedPageQuery(params)
  if (params.state) search.set('state', params.state)
  if (params.annType) search.set('ann_type', params.annType)
  if (params.pinned) search.set('pinned', params.pinned)
  return search.toString()
}

export function fetchAnnouncements(
  params: AnnouncementListParams = {},
): Promise<AnnouncementListResponse> {
  return api.get<AnnouncementListResponse>(`/api/admin/announcements?${announcementQuery(params)}`)
}

export function createAnnouncement(
  input: AnnouncementWriteInput,
): Promise<AnnouncementDetailResponse> {
  return api.post<AnnouncementDetailResponse>('/api/admin/announcements', input)
}

export function updateAnnouncement(
  id: number | string,
  input: AnnouncementWriteInput,
): Promise<AnnouncementDetailResponse> {
  return api.patch<AnnouncementDetailResponse>(
    `/api/admin/announcements/${encodeURIComponent(String(id))}`,
    input,
  )
}

export function publishAnnouncement(
  id: number | string,
  input: AnnouncementPublishInput = {},
): Promise<AnnouncementDetailResponse> {
  return api.post<AnnouncementDetailResponse>(
    `/api/admin/announcements/${encodeURIComponent(String(id))}/publish`,
    input,
  )
}

/** OFFLINE is terminal for display; a second call answers 409. */
export function offlineAnnouncement(
  id: number | string,
): Promise<AnnouncementDetailResponse> {
  return api.post<AnnouncementDetailResponse>(
    `/api/admin/announcements/${encodeURIComponent(String(id))}/offline`,
  )
}

export function useAnnouncements(params: AnnouncementListParams = {}) {
  return useQuery({
    queryKey: ['admin', 'announcements', 'list', params],
    queryFn: () => fetchAnnouncements(params),
    placeholderData: keepPreviousData,
  })
}

function invalidateAnnouncements(client: ReturnType<typeof useQueryClient>) {
  void client.invalidateQueries({ queryKey: ['admin', 'announcements'] })
}

export function useCreateAnnouncement() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: AnnouncementWriteInput) => createAnnouncement(input),
    onSuccess: () => invalidateAnnouncements(client),
  })
}

export function useUpdateAnnouncement() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: AnnouncementWriteInput }) =>
      updateAnnouncement(id, input),
    onSuccess: () => invalidateAnnouncements(client),
  })
}

export function usePublishAnnouncement() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input?: AnnouncementPublishInput }) =>
      publishAnnouncement(id, input),
    onSuccess: () => invalidateAnnouncements(client),
  })
}

export function useOfflineAnnouncement() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id: number | string) => offlineAnnouncement(id),
    onSuccess: () => invalidateAnnouncements(client),
  })
}

// ---------------------------------------------------------------------------
// Users (internal/control/ops/users.go)
// ---------------------------------------------------------------------------

export type AdminUserStatus = 'NORMAL' | 'DISABLED' | 'FROZEN_OVERDRAFT'

export interface AdminUser {
  id: number
  user_no: string
  /** 自选登录用户名；未设置时后端省略该字段（内置超管只有它，没有邮箱/手机号）。 */
  username?: string
  email?: string
  phone?: string
  nickname: string
  role: string
  status: AdminUserStatus
  disable_source?: string
  /** Operator-only; the customer-facing account_disabled body never reads it. */
  disable_reason?: string
  disabled_at?: string | null
  disabled_by?: number | null
  risk_score: number
  plan_id?: number | null
  plan_code?: string
  plan_name?: string
  /** ENABLED and not deleted API keys; 0 is a real count, not "unknown". */
  key_count: number
  created_at: string
  updated_at: string
}

export interface CreditBalance {
  exists: boolean
  balance_recharge_micro: number
  balance_bonus_micro: number
  balance_micro: number
  frozen_refund_micro: number
  overdraft_limit_micro: number
  total_recharge_micro: number
  total_bonus_micro: number
  total_consumed_micro: number
  last_settled_at?: string | null
}

export type BalanceAdjustDirection = 'INCREASE' | 'DECREASE'
export type BalanceAdjustState = 'PENDING' | 'APPROVED' | 'REJECTED'

/**
 * User detail's most recent t_balance_adjust row. null means the user has never
 * had one (not "unknown").
 */
export interface BalanceAdjustSummary {
  id: number
  direction: BalanceAdjustDirection
  amount_micro: number
  reason: string
  state: BalanceAdjustState
  applicant_id: number
  decided_by?: number | null
  decided_at?: string | null
  created_at: string
}

export interface AdminUserDetailResponse {
  user: AdminUser
  balance: CreditBalance
  latest_balance_adjust: BalanceAdjustSummary | null
}

export interface BalanceAdjustInput {
  direction: BalanceAdjustDirection
  amount_micro: number
  /** At least 5 characters; the backend also caps it at 255. */
  reason: string
}

export interface BalanceAdjustResponse {
  balance_adjust: BalanceAdjustSummary & {
    user_id: number
    updated_at: string
  }
  review_task_id: number
}

export type AdminUserListResponse = Page<AdminUser>

export interface AdminUserListParams extends PageParams {
  email?: string
  phone?: string
  username?: string
  userNo?: string
  status?: AdminUserStatus
  sort?: 'created_at' | 'updated_at' | 'id' | 'risk_score' | 'user_no'
  order?: SortOrder
}

export interface UserDisableInput {
  reason: string
}

function userQuery(params: AdminUserListParams): string {
  const search = sortedPageQuery(params)
  const email = params.email?.trim()
  if (email) search.set('email', email)
  const phone = params.phone?.trim()
  if (phone) search.set('phone', phone)
  const username = params.username?.trim()
  if (username) search.set('username', username)
  const userNo = params.userNo?.trim()
  if (userNo) search.set('user_no', userNo)
  if (params.status) search.set('status', params.status)
  return search.toString()
}

export function fetchUsers(params: AdminUserListParams = {}): Promise<AdminUserListResponse> {
  return api.get<AdminUserListResponse>(`/api/admin/users?${userQuery(params)}`)
}

export function fetchUser(id: number | string): Promise<AdminUserDetailResponse> {
  return api.get<AdminUserDetailResponse>(`/api/admin/users/${encodeURIComponent(String(id))}`)
}

export function disableUser(
  id: number | string,
  input: UserDisableInput,
): Promise<AdminUserDetailResponse> {
  return api.post<AdminUserDetailResponse>(
    `/api/admin/users/${encodeURIComponent(String(id))}/disable`,
    input,
  )
}

/**
 * Submits a balance adjustment for review. The account is not changed here:
 * a different operator must approve it and only then does the ledger move.
 */
export function submitBalanceAdjust(
  id: number | string,
  input: BalanceAdjustInput,
): Promise<BalanceAdjustResponse> {
  return api.post<BalanceAdjustResponse>(
    `/api/admin/users/${encodeURIComponent(String(id))}/balance-adjust`,
    input,
  )
}

export function enableUser(id: number | string): Promise<AdminUserDetailResponse> {
  return api.post<AdminUserDetailResponse>(
    `/api/admin/users/${encodeURIComponent(String(id))}/enable`,
  )
}

export function useUsers(params: AdminUserListParams = {}) {
  return useQuery({
    queryKey: ['admin', 'users', 'list', params],
    queryFn: () => fetchUsers(params),
    placeholderData: keepPreviousData,
  })
}

export function useUser(id: number | string | null | undefined) {
  return useQuery({
    queryKey: ['admin', 'users', 'detail', String(id)],
    queryFn: () => fetchUser(id as number | string),
    enabled: id !== null && id !== undefined,
  })
}

function invalidateUsers(client: ReturnType<typeof useQueryClient>, id?: number | string) {
  void client.invalidateQueries({ queryKey: ['admin', 'users'] })
  if (id !== undefined) {
    void client.invalidateQueries({ queryKey: ['admin', 'users', 'detail', String(id)] })
  }
}

export function useDisableUser() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: UserDisableInput }) =>
      disableUser(id, input),
    onSuccess: (_response, variables) => invalidateUsers(client, variables.id),
  })
}

export function useEnableUser() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id: number | string) => enableUser(id),
    onSuccess: (_response, id) => invalidateUsers(client, id),
  })
}

export function useSubmitBalanceAdjust() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: BalanceAdjustInput }) =>
      submitBalanceAdjust(id, input),
    onSuccess: (_response, variables) => {
      invalidateUsers(client, variables.id)
      void client.invalidateQueries({ queryKey: ['admin', 'reviews'] })
    },
  })
}

/**
 * Operator provisioning (internal/control/ops/users_operator.go).
 *
 * Both endpoints are gated by admin.all server-side: only SUPER_ADMIN may open an
 * operator account or change anyone's role. Hiding the entry points is a
 * convenience, never the control.
 */

/** The t_user.role enum, spelled as the backend stores it. */
export type AdminUserRole = 'PERSONAL' | 'OPS' | 'FINANCE' | 'SUPER_ADMIN'

/** POST /api/admin/users refuses PERSONAL: customer accounts come from public registration. */
export type ProvisionableRole = Exclude<AdminUserRole, 'PERSONAL'>

export interface OperatorCreateInput {
  /** Email or phone; the server normalizes it with the registration rule. */
  identifier: string
  /** Initial password, 8..72 bytes. Never returned, audited or logged. */
  password: string
  nickname?: string
  role: ProvisionableRole
}

export interface UserRoleChangeInput {
  role: AdminUserRole
  /** Required; written to the audit row's reason column. */
  reason: string
}

export function createOperatorUser(input: OperatorCreateInput): Promise<AdminUserDetailResponse> {
  return api.post<AdminUserDetailResponse>('/api/admin/users', input)
}

export function changeUserRole(
  id: number | string,
  input: UserRoleChangeInput,
): Promise<AdminUserDetailResponse> {
  return api.post<AdminUserDetailResponse>(
    `/api/admin/users/${encodeURIComponent(String(id))}/role`,
    input,
  )
}

export function useCreateOperatorUser() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: OperatorCreateInput) => createOperatorUser(input),
    onSuccess: () => invalidateUsers(client),
  })
}

export function useChangeUserRole() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: UserRoleChangeInput }) =>
      changeUserRole(id, input),
    onSuccess: (_response, variables) => invalidateUsers(client, variables.id),
  })
}

// ---------------------------------------------------------------------------
// Reviews (internal/control/review)
// ---------------------------------------------------------------------------

export type ReviewState = 'PENDING' | 'APPROVED' | 'REJECTED'
export type ReviewBizType =
  | 'QUOTA_APPLY'
  | 'INVOICE_APPLY'
  | 'REFUND_APPLY'
  | 'REDEEM_BATCH'
  | 'BALANCE_ADJUST'

export interface ReviewItem {
  id: number
  biz_type: ReviewBizType
  biz_id: number
  biz_no?: string
  applicant_id: number
  applicant_name: string
  amount_micro?: number | null
  amount_fen?: number | null
  /** BALANCE_ADJUST only; the console renders the sign next to the amount. */
  direction?: BalanceAdjustDirection | null
  summary: string
  business_state?: string
  state: ReviewState
  assignee_id?: number | null
  decision_note?: string | null
  decided_at?: string | null
  created_at: string
  /** True when the referenced business row no longer exists; the list still answers 200. */
  anomaly: boolean
  anomaly_reason?: string | null
}

export type ReviewListResponse = Page<ReviewItem>
export interface ReviewDetailResponse {
  review: ReviewItem
}

/**
 * List accepts page/page_size only. The handler calls mw.ParsePage with no
 * allowed sort columns, so any sort= is a 400; the default order is
 * created_at ASC (F20-1) and cannot be overridden.
 */
export interface ReviewListParams extends PageParams {
  state?: ReviewState | 'ALL'
  bizType?: ReviewBizType
}

export interface ReviewDecisionInput {
  reason?: string
  /** Required by the backend when approving INVOICE_APPLY. */
  invoice_url?: string
}

function reviewQuery(params: ReviewListParams): string {
  const search = pageQuery(params)
  if (params.state) search.set('state', params.state)
  if (params.bizType) search.set('biz_type', params.bizType)
  return search.toString()
}

export function fetchReviews(params: ReviewListParams = {}): Promise<ReviewListResponse> {
  return api.get<ReviewListResponse>(`/api/admin/reviews?${reviewQuery(params)}`)
}

export function approveReview(
  id: number | string,
  input: ReviewDecisionInput = {},
): Promise<ReviewDetailResponse> {
  return api.post<ReviewDetailResponse>(
    `/api/admin/reviews/${encodeURIComponent(String(id))}/approve`,
    input,
  )
}

/** The reason is mandatory server-side. The browser's `required` is not a control. */
export function rejectReview(
  id: number | string,
  input: ReviewDecisionInput,
): Promise<ReviewDetailResponse> {
  return api.post<ReviewDetailResponse>(
    `/api/admin/reviews/${encodeURIComponent(String(id))}/reject`,
    input,
  )
}

export function useReviews(params: ReviewListParams = {}) {
  return useQuery({
    queryKey: ['admin', 'reviews', 'list', params],
    queryFn: () => fetchReviews(params),
    placeholderData: keepPreviousData,
  })
}

function invalidateReviews(client: ReturnType<typeof useQueryClient>) {
  void client.invalidateQueries({ queryKey: ['admin', 'reviews'] })
}

export function useApproveReview() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input?: ReviewDecisionInput }) =>
      approveReview(id, input),
    onSuccess: () => invalidateReviews(client),
  })
}

export function useRejectReview() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: ReviewDecisionInput }) =>
      rejectReview(id, input),
    onSuccess: () => invalidateReviews(client),
  })
}

// ---------------------------------------------------------------------------
// Admin tickets (internal/control/ticket)
// ---------------------------------------------------------------------------

export interface AdminTicketItem {
  id: number
  ticket_no: string
  user_id: number
  user_email: string
  ticket_type: TicketType
  title: string
  request_id: string | null
  policy_hit_id: number | null
  state: TicketState
  first_response_due_at: string
  first_responded_at: string | null
  last_replied_at: string | null
  created_at: string
  sla: TicketSLA
}

export type AdminTicketListResponse = Page<AdminTicketItem>

export interface AdminTicketListParams extends PageParams {
  state?: TicketState | 'ALL'
  type?: TicketType | 'ALL'
  /** true = the SLA alert zone only; false excludes it; omit for all. */
  overdue?: boolean
  q?: string
  sort?: 'created_at' | 'first_response_due_at'
  order?: SortOrder
}

export interface AdminTicketReplyInput {
  content: string
  /** Internal notes are never returned to the account (is_internal = 1). */
  is_internal?: boolean
}

export interface AdminTicketReplyResponse {
  reply: TicketReply
  ticket: AdminTicketItem
}

export interface AdminTicketStateInput {
  /** Only CLOSED is reachable; RESPONDED is written by a public reply. */
  state: 'CLOSED'
  reason?: string
}

function adminTicketQuery(params: AdminTicketListParams): string {
  const search = sortedPageQuery(params)
  if (params.state && params.state !== 'ALL') search.set('state', params.state)
  if (params.type && params.type !== 'ALL') search.set('type', params.type)
  if (params.overdue !== undefined) search.set('overdue', String(params.overdue))
  const q = params.q?.trim()
  if (q) search.set('q', q)
  return search.toString()
}

export function fetchAdminTickets(
  params: AdminTicketListParams = {},
): Promise<AdminTicketListResponse> {
  return api.get<AdminTicketListResponse>(`/api/admin/tickets?${adminTicketQuery(params)}`)
}

/**
 * There is no GET /api/admin/tickets/{id}: the operator surface is list +
 * reply + state only. The reply response carries the refreshed ticket row.
 */
export function replyAdminTicket(
  id: number | string,
  input: AdminTicketReplyInput,
): Promise<AdminTicketReplyResponse> {
  return api.post<AdminTicketReplyResponse>(
    `/api/admin/tickets/${encodeURIComponent(String(id))}/reply`,
    input,
  )
}

export function setAdminTicketState(
  id: number | string,
  input: AdminTicketStateInput,
): Promise<AdminTicketItem> {
  return api.post<AdminTicketItem>(
    `/api/admin/tickets/${encodeURIComponent(String(id))}/state`,
    input,
  )
}

export function useAdminTickets(params: AdminTicketListParams = {}) {
  return useQuery({
    queryKey: ['admin', 'tickets', 'list', params],
    queryFn: () => fetchAdminTickets(params),
    placeholderData: keepPreviousData,
  })
}

/** Operator reply shape: internal notes are visible to operators only. */
export interface AdminTicketReply {
  id: number
  author_type: 'USER' | 'OPS'
  content: string
  created_at: string
  is_internal: boolean
}

export interface AdminTicketDetail {
  id: number
  ticket_no: string
  user_id: number
  user_email: string
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
  replies: AdminTicketReply[]
}

export function fetchAdminTicketDetail(id: number | string): Promise<AdminTicketDetail> {
  return api.get<AdminTicketDetail>(
    `/api/admin/tickets/${encodeURIComponent(String(id))}`,
  )
}

export function useAdminTicketDetail(id: number | string | null | undefined) {
  return useQuery({
    queryKey: ['admin', 'tickets', 'detail', String(id)],
    queryFn: () => fetchAdminTicketDetail(id as number | string),
    enabled: id !== null && id !== undefined,
  })
}

function invalidateAdminTickets(client: ReturnType<typeof useQueryClient>) {
  void client.invalidateQueries({ queryKey: ['admin', 'tickets'] })
}

export function useReplyAdminTicket() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: AdminTicketReplyInput }) =>
      replyAdminTicket(id, input),
    onSuccess: () => invalidateAdminTickets(client),
  })
}

export function useSetAdminTicketState() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: AdminTicketStateInput }) =>
      setAdminTicketState(id, input),
    onSuccess: () => invalidateAdminTickets(client),
  })
}

// ---------------------------------------------------------------------------
// Audit trail (internal/control/auditq/list.go)
// ---------------------------------------------------------------------------

/** listItem. before/after are the stored JSON documents (objects or null). */
export interface AuditLogItem {
  id: number
  trace_id: string
  operator_id: string
  operator_role: string
  operator_ip: string
  module: string
  action: string
  target_type: string
  target_id: string
  before: unknown
  after: unknown
  reason?: string
  result: string
  created_at: string
}

export interface AuditLogListResponse extends Page<AuditLogItem> {
  /** Backend-computed window bounds; display them, never recompute. */
  from: string
  to: string
}

export interface AuditLogParams extends PageParams {
  module?: string
  action?: string
  result?: string
  operatorId?: number
  from?: string
  to?: string
  /** Exact target_type / target_id SQL filters (T24). Pass both for idx_aal_target. */
  targetType?: string
  targetId?: string | number
}

function auditQuery(params: AuditLogParams): string {
  const search = pageQuery(params)
  if (params.module) search.set('module', params.module)
  if (params.action) search.set('action', params.action)
  if (params.result) search.set('result', params.result)
  if (params.operatorId !== undefined) search.set('operator_id', String(params.operatorId))
  if (params.targetType) search.set('target_type', params.targetType)
  if (params.targetId !== undefined) search.set('target_id', String(params.targetId))
  if (params.from) search.set('from', params.from)
  if (params.to) search.set('to', params.to)
  // The endpoint is defined as newest-first and rejects order=asc.
  search.set('order', 'desc')
  return search.toString()
}

export function fetchAuditLogs(params: AuditLogParams = {}): Promise<AuditLogListResponse> {
  return api.get<AuditLogListResponse>(`/api/admin/audit-logs?${auditQuery(params)}`)
}

export function useAuditLogs(params: AuditLogParams = {}, enabled = true) {
  return useQuery({
    queryKey: ['admin', 'audit', params],
    queryFn: () => fetchAuditLogs(params),
    enabled,
    placeholderData: keepPreviousData,
  })
}

// ---------------------------------------------------------------------------
// Operator orders and manual payment confirmation (internal/control/fund/confirm.go)
// ---------------------------------------------------------------------------

/**
 * adminOrderView: the account's own order plus the identity finance needs to
 * match a bank statement to a customer.
 */
export interface AdminOrder {
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
  user_id: number
  user_email: string
  user_nickname: string
  /** Channel trade_no, or the bank statement number an operator typed. */
  pay_txn_id?: string
}

export type AdminOrderList = Page<AdminOrder>

/** `state=all` is the server's "no filter" value; the console shows it as 全部. */
export type AdminOrderStateFilter = 'ALL' | OrderStatusFilter

export interface AdminOrderListParams extends PageParams {
  state?: AdminOrderStateFilter
  q?: string
}

export interface ConfirmPaymentInput {
  /** Proof of payment: 6–64 of [A-Za-z0-9_-], required by the server. */
  bank_txn_no: string
  note?: string
}

export interface ConfirmPaymentResult {
  order: AdminOrder
  credited: boolean
  /** True when this statement number had already settled the order. */
  replayed: boolean
}

function adminOrderQuery(params: AdminOrderListParams): string {
  const search = sortedPageQuery(params)
  if (params.state && params.state !== 'ALL') search.set('state', params.state)
  if (params.q) search.set('q', params.q)
  return search.toString()
}

export function fetchAdminOrders(params: AdminOrderListParams = {}): Promise<AdminOrderList> {
  return api.get<AdminOrderList>(`/api/admin/orders?${adminOrderQuery(params)}`)
}

export function useAdminOrders(params: AdminOrderListParams = {}) {
  return useQuery({
    queryKey: ['admin', 'orders', 'list', params],
    queryFn: () => fetchAdminOrders(params),
    placeholderData: keepPreviousData,
  })
}

export function confirmOrderPayment(
  id: number | string,
  input: ConfirmPaymentInput,
): Promise<ConfirmPaymentResult> {
  return api.post<ConfirmPaymentResult>(
    `/api/admin/orders/${encodeURIComponent(String(id))}/confirm-payment`,
    input,
  )
}

export function useConfirmOrderPayment() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: ConfirmPaymentInput }) =>
      confirmOrderPayment(id, input),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['admin', 'orders'] })
      // The customer's own order list must show the new state too.
      void client.invalidateQueries({ queryKey: ['console', 'orders'] })
    },
  })
}

export interface CloseOrderInput {
  /** Why the order is voided; required, 2–100 characters. */
  reason: string
}

export interface CloseOrderResult {
  order: AdminOrder
  closed: boolean
  /** True when the order was already CLOSED; nothing was written. */
  replayed: boolean
}

/**
 * Voids a PENDING order. No money moves: a void writes one audit row and
 * nothing else, so this is the way out of the finance queue for an order the
 * customer never paid.
 */
export function closeAdminOrder(
  id: number | string,
  input: CloseOrderInput,
): Promise<CloseOrderResult> {
  return api.post<CloseOrderResult>(
    `/api/admin/orders/${encodeURIComponent(String(id))}/close`,
    input,
  )
}

export function useCloseAdminOrder() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: number | string; input: CloseOrderInput }) =>
      closeAdminOrder(id, input),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['admin', 'orders'] })
      void client.invalidateQueries({ queryKey: ['console', 'orders'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Redeem-code batches (internal/control/fund/redeem_batch.go)
// ---------------------------------------------------------------------------

export type RedeemCreditTarget = 'RECHARGE' | 'BONUS'

export interface RedeemBatch {
  id: number
  batch_name: string
  face_value_fen: number
  face_value_yuan: number
  quantity: number
  generated_count: number
  redeemed_count: number
  valid_from: string
  valid_to: string
  credit_target: RedeemCreditTarget
  created_by: number
  created_by_email?: string
  created_at: string
}

export type RedeemBatchList = Page<RedeemBatch>

export interface RedeemBatchListParams extends PageParams {}

export interface CreateRedeemBatchInput {
  batch_name: string
  face_value_fen: number
  /** 1–500 per call. */
  quantity: number
  credit_target: RedeemCreditTarget
  /** 1–3650 days. */
  valid_days: number
}

export interface CreateRedeemBatchResult {
  batch: RedeemBatch
  /** The one and only time the plaintext leaves the server. */
  codes: string[]
}

export function fetchRedeemBatches(
  params: RedeemBatchListParams = {},
): Promise<RedeemBatchList> {
  return api.get<RedeemBatchList>(`/api/admin/redeem-codes?${pageQuery(params)}`)
}

export function useRedeemBatches(params: RedeemBatchListParams = {}) {
  return useQuery({
    queryKey: ['admin', 'redeem-codes', 'list', params],
    queryFn: () => fetchRedeemBatches(params),
    placeholderData: keepPreviousData,
  })
}

export function createRedeemBatch(
  input: CreateRedeemBatchInput,
): Promise<CreateRedeemBatchResult> {
  return api.post<CreateRedeemBatchResult>('/api/admin/redeem-codes/batch', input)
}

export function useCreateRedeemBatch() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateRedeemBatchInput) => createRedeemBatch(input),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['admin', 'redeem-codes'] })
    },
  })
}

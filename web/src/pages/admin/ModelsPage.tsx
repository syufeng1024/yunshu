import { useEffect, useState } from 'react'
import { errorHint, errorMessage } from '@/api/client'
import {
  useCreateModel,
  useCreateModelAlias,
  useDeleteModelAlias,
  useModelAliases,
  useModels,
  useSetListingState,
  useSetRecommendWeight,
  useUpdateModel,
} from '@/api/admin'
import type {
  AdminModel,
  ListingState,
  ModelCreateInput,
  ModelPatchInput,
  ModelType,
  Region,
} from '@/api/admin'
import {
  Badge,
  Button,
  Drawer,
  Input,
  Modal,
  PageHeader,
  Select,
  Table,
  useToast,
} from '@/components'
import type { TableColumn } from '@/components'
import { AuditTrail, SourceRegionGuard, isSourceRegionBlocked } from '@/components/admin'
import { modelTypeLabel, protocolLabel } from '@/lib/consoleLabels'
import { formatInteger, formatTokens } from '@/lib/format'
import { useDebouncedValue } from '@/lib/hooks/useDebouncedValue'
import { usePageTitle } from '@/lib/usePageTitle'

const PAGE_SIZE = 20

/**
 * t_model.recommend_weight is MySQL `int unsigned` (TDS §t_model), so the
 * database itself refuses anything above 4294967295. The backend has no
 * business-layer ceiling, so this is the real upper bound the UI can enforce.
 */
const RECOMMEND_WEIGHT_MAX = 4_294_967_295

type TypeFilter = 'ALL' | ModelType
type RegionFilter = 'ALL' | Region
type ListingFilter = 'ALL' | ListingState

const LISTING_LABEL: Record<ListingState, string> = {
  LISTED: '已上架',
  CATALOG_ONLY: '仅目录可见',
  DELISTED: '已下架',
}

const CAPABILITY_LABELS = [
  { key: 'stream', label: '流式' },
  { key: 'tool_call', label: '工具调用' },
  { key: 'vision', label: '视觉' },
  { key: 'json_mode', label: 'JSON 模式' },
] as const

type CapabilityKey = (typeof CAPABILITY_LABELS)[number]['key']

interface ModelFormState {
  modelName: string
  displayName: string
  region: Region
  modelType: ModelType
  protocolId: string
  primaryChannelId: string
  contextLength: string
  maxOutputTokens: string
  capabilities: Record<CapabilityKey, boolean>
  aliasesText: string
}

const EMPTY_FORM: ModelFormState = {
  modelName: '',
  displayName: '',
  region: 'DOMESTIC',
  modelType: 'TEXT',
  protocolId: '',
  primaryChannelId: '',
  contextLength: '0',
  maxOutputTokens: '0',
  capabilities: { stream: false, tool_call: false, vision: false, json_mode: false },
  aliasesText: '',
}

function formFromModel(model: AdminModel): ModelFormState {
  return {
    modelName: model.model_name,
    displayName: model.display_name,
    region: model.region,
    modelType: model.model_type,
    protocolId: String(model.protocol_id),
    primaryChannelId:
      model.primary_channel_id === null || model.primary_channel_id === undefined
        ? ''
        : String(model.primary_channel_id),
    contextLength: String(model.context_length),
    maxOutputTokens: String(model.max_output_tokens ?? 0),
    capabilities: {
      stream: model.capabilities.stream === true,
      tool_call: model.capabilities.tool_call === true,
      vision: model.capabilities.vision === true,
      json_mode: model.capabilities.json_mode === true,
    },
    aliasesText: model.aliases.join('\n'),
  }
}

function parseRecommendWeight(raw: string): { ok: boolean; value: number | null } {
  const normalized = normalizeNumeric(raw).replace(/[，,\s]/g, '')
  if (!/^-?\d+$/.test(normalized)) return { ok: false, value: null }
  const value = Number(normalized)
  return Number.isSafeInteger(value) ? { ok: true, value } : { ok: false, value: null }
}

function normalizeNumeric(raw: string): string {
  return raw
    .replace(/[０-９]/g, (char) => String.fromCharCode(char.charCodeAt(0) - 0xfee0))
    .replace(/[，,\s]/g, '')
}

function parseInteger(raw: string): number | null {
  const normalized = normalizeNumeric(raw)
  if (!/^-?\d+$/.test(normalized)) return null
  const value = Number(normalized)
  return Number.isSafeInteger(value) ? value : null
}

function parseAliases(raw: string): string[] | null {
  const values = raw
    .split(/[\n,，]+/)
    .map((value) => value.trim())
    .filter(Boolean)
  if (values.length > 20) return null
  const out: string[] = []
  const seen = new Set<string>()
  for (const value of values) {
    if ([...value].length > 128) return null
    if (seen.has(value)) continue
    seen.add(value)
    out.push(value)
  }
  return out
}

function listingVariant(state: ListingState): 'success' | 'warning' | 'neutral' {
  if (state === 'LISTED') return 'success'
  if (state === 'CATALOG_ONLY') return 'warning'
  return 'neutral'
}

function CheckboxField({
  label,
  checked,
  onChange,
  disabled,
}: {
  label: string
  checked: boolean
  onChange: (checked: boolean) => void
  disabled?: boolean
}) {
  return (
    <label className="flex items-center gap-2 text-sm text-ink">
      <input
        type="checkbox"
        className="h-4 w-4 rounded border-border-strong text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
        checked={checked}
        disabled={disabled}
        onChange={(event) => onChange(event.target.checked)}
      />
      {label}
    </label>
  )
}

export default function AdminModelsPage() {
  usePageTitle('模型主数据')
  const toast = useToast()

  const [search, setSearch] = useState('')
  const [type, setType] = useState<TypeFilter>('ALL')
  const [region, setRegion] = useState<RegionFilter>('ALL')
  const [listing, setListing] = useState<ListingFilter>('ALL')
  const [page, setPage] = useState(1)
  const debouncedSearch = useDebouncedValue(search, 300)

  useEffect(() => {
    setPage(1)
  }, [debouncedSearch, type, region, listing])

  const list = useModels({
    page,
    pageSize: PAGE_SIZE,
    q: debouncedSearch || undefined,
    modelType: type === 'ALL' ? undefined : type,
    region: region === 'ALL' ? undefined : region,
    listingState: listing === 'ALL' ? undefined : listing,
    sort: 'updated_at',
    order: 'desc',
  })

  const [creating, setCreating] = useState(false)
  const [editTarget, setEditTarget] = useState<AdminModel | null>(null)
  const drawerOpen = creating || editTarget !== null

  const createModel = useCreateModel()
  const updateModel = useUpdateModel()
  const setModelListing = useSetListingState()
  const setRecommendWeight = useSetRecommendWeight()
  const createAlias = useCreateModelAlias()
  const deleteAlias = useDeleteModelAlias()
  const aliasesQuery = useModelAliases(editTarget?.id)

  const [form, setForm] = useState<ModelFormState>(EMPTY_FORM)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [saveError, setSaveError] = useState<string | null>(null)
  const [newAlias, setNewAlias] = useState('')
  const [aliasError, setAliasError] = useState<string | null>(null)

  const [weightTarget, setWeightTarget] = useState<AdminModel | null>(null)
  const [weightValue, setWeightValue] = useState('')
  const [weightError, setWeightError] = useState<string | null>(null)

  const [auditOpen, setAuditOpen] = useState(false)
  const [auditTarget, setAuditTarget] = useState<{ type?: string; id?: string }>({})
  const [auditRefreshKey, setAuditRefreshKey] = useState(0)

  const openAudit = (targetType?: string, targetId?: number | string) => {
    setAuditTarget({
      type: targetType,
      id: targetId === undefined ? undefined : String(targetId),
    })
    setAuditRefreshKey((value) => value + 1)
    setAuditOpen(true)
  }

  const closeDrawer = () => {
    if (createModel.isPending || updateModel.isPending) return
    setCreating(false)
    setEditTarget(null)
    setErrors({})
    setSaveError(null)
    setAliasError(null)
    setNewAlias('')
  }

  const openCreate = () => {
    setEditTarget(null)
    setForm(EMPTY_FORM)
    setErrors({})
    setCreating(true)
  }

  const openEdit = (model: AdminModel) => {
    setCreating(false)
    setForm(formFromModel(model))
    setErrors({})
    setSaveError(null)
    setAliasError(null)
    setNewAlias('')
    setEditTarget(model)
  }

  const updateField = <K extends keyof ModelFormState>(key: K, value: ModelFormState[K]) => {
    setForm((current) => ({ ...current, [key]: value }))
    setErrors((current) => {
      if (!current[key]) return current
      const next = { ...current }
      delete next[key]
      return next
    })
  }

  const validate = (): {
    errors: Record<string, string>
    createInput: ModelCreateInput | null
    patchInput: ModelPatchInput | null
  } => {
    const nextErrors: Record<string, string> = {}
    const modelName = form.modelName.trim()
    if (!modelName) nextErrors.modelName = '请填写模型名称。'
    else if ([...modelName].length > 128) nextErrors.modelName = '模型名称最多 128 个字符。'

    const displayName = form.displayName.trim()
    if (!displayName) nextErrors.displayName = '请填写展示名称。'
    else if ([...displayName].length > 128) nextErrors.displayName = '展示名称最多 128 个字符。'

    if (isSourceRegionBlocked(form.region)) {
      nextErrors.region = 'V1.0 仅支持境内模型，不能选择「境外」。'
    }

    const protocolID = parseInteger(form.protocolId)
    if (protocolID === null || protocolID <= 0) {
      nextErrors.protocolId = '请填写有效的协议编号（正整数）。'
    }

    const primaryRaw = form.primaryChannelId.trim()
    const parsedPrimary = primaryRaw ? parseInteger(primaryRaw) : undefined
    if (primaryRaw && (parsedPrimary === null || parsedPrimary === undefined || parsedPrimary <= 0)) {
      nextErrors.primaryChannelId = '主渠道编号需为正整数，留空表示不指定。'
    }
    const primaryChannelID =
      parsedPrimary !== null && parsedPrimary !== undefined && parsedPrimary > 0
        ? parsedPrimary
        : undefined

    const contextLength = parseInteger(form.contextLength)
    if (contextLength === null || contextLength < 0 || contextLength > 100_000_000) {
      nextErrors.contextLength = '上下文长度需为 0–100000000 的整数。'
    }

    // max_output_tokens: 0 = 未配置（网关按 min(context_length, 32768) 兜底）。
    // 与后端同一口径：context_length > 0 时不得越过它；= 0（未知）时只看正负。
    //
    // T48：两个字段是一对，改动方向有两个。上面只管「输出上限越过上下文」；
    // 反方向——把上下文改小到存量输出上限之下——同样会让库里那一行自相矛盾，
    // 而网关估算在 max_output_tokens > 0 时直接拿它当输出上限、不再裁剪到
    // context_length，prehold 就会按模型产不出的输出量预留额度（余额偏紧的
    // 用户可能被 402 误拦）。
    const maxOutputTokensRaw = form.maxOutputTokens.trim()
    const maxOutputTokens = maxOutputTokensRaw === '' ? 0 : parseInteger(maxOutputTokensRaw)
    // 存量输出上限：编辑态下就是这一行当前的值，新建态没有存量值。
    const storedMaxOutputTokens = creating ? 0 : (editTarget?.max_output_tokens ?? 0)
    // 输出上限输入框这次有没有被改动过（清空等于改成 0，算显式改动）。
    const outputCeilingTouched =
      creating || maxOutputTokensRaw !== String(storedMaxOutputTokens)

    if (maxOutputTokens === null || maxOutputTokens < 0 || maxOutputTokens > 100_000_000) {
      nextErrors.maxOutputTokens = '输出上限需为 0–100000000 的整数，0 表示未配置。'
    } else if (contextLength !== null && contextLength > 0 && maxOutputTokens > contextLength) {
      if (!outputCeilingTouched) {
        // 只改了上下文：错误挂在上下文长度上，与后端一致（本次提交的是它，
        // 存量上限这次没被修改）。挂在输出上限上会让运营去改一个自己没碰过
        // 的字段，而真正的动作是把两者一起改到一致。
        nextErrors.contextLength = `上下文长度不能小于当前输出上限（${storedMaxOutputTokens}）；如需下调，请同时把输出上限改到不超过 ${contextLength}。`
      } else {
        nextErrors.maxOutputTokens = `输出上限不能超过上下文长度（当前 ${contextLength}）。`
      }
    }

    const aliases = creating ? parseAliases(form.aliasesText) : []
    if (creating && aliases === null) {
      nextErrors.aliasesText = '别名最多 20 个，每个不超过 128 个字符。'
    }

    const capabilities: Record<string, boolean> = {
      stream: form.capabilities.stream,
      tool_call: form.capabilities.tool_call,
      vision: form.capabilities.vision,
      json_mode: form.capabilities.json_mode,
    }

    if (
      Object.keys(nextErrors).length > 0 ||
      protocolID === null ||
      contextLength === null ||
      maxOutputTokens === null ||
      (creating && aliases === null)
    ) {
      return { errors: nextErrors, createInput: null, patchInput: null }
    }

    if (creating) {
      const input: ModelCreateInput = {
        model_name: modelName,
        display_name: displayName,
        region: form.region,
        model_type: form.modelType,
        protocol_id: protocolID,
        context_length: contextLength,
        max_output_tokens: maxOutputTokens,
        // 表单没有上架状态控件（上/下架是列表里的独立操作），新建一律走后端默认值。
        listing_state: 'CATALOG_ONLY',
        // 仅在同时建默认路由时生效：模型不支持流式，默认路由也没有理由声明支持。
        stream_supported: form.capabilities.stream,
        capabilities,
        aliases: aliases as string[],
      }
      if (primaryChannelID !== undefined) input.primary_channel_id = primaryChannelID
      return { errors: nextErrors, createInput: input, patchInput: null }
    }

    const patch: ModelPatchInput = {
      model_name: modelName,
      display_name: displayName,
      region: form.region,
      model_type: form.modelType,
      protocol_id: protocolID,
      context_length: contextLength,
      max_output_tokens: maxOutputTokens,
      capabilities,
    }
    if (primaryChannelID !== undefined) patch.primary_channel_id = primaryChannelID
    return { errors: nextErrors, createInput: null, patchInput: patch }
  }

  const handleSave = async () => {
    const result = validate()
    setErrors(result.errors)
    if (creating ? !result.createInput : !result.patchInput) return
    setSaveError(null)
    try {
      const response = creating
        ? await createModel.mutateAsync(result.createInput as ModelCreateInput)
        : await updateModel.mutateAsync({
            id: editTarget?.id as number,
            input: result.patchInput as ModelPatchInput,
          })
      toast.success(creating ? '模型已创建' : '模型已更新')
      closeDrawer()
      openAudit('t_model', response.model.id)
    } catch (error) {
      const hint = errorHint(error)
      setSaveError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const handleListingChange = async (model: AdminModel, next: ListingState) => {
    try {
      await setModelListing.mutateAsync({ id: model.id, input: { listing_state: next } })
      const generatesDraft =
        (model.listing_state === 'LISTED' && next === 'CATALOG_ONLY') ||
        (model.listing_state === 'CATALOG_ONLY' && next === 'LISTED')
      if (generatesDraft) {
        // ops/model.go:649-683 creates the DRAFT for exactly these two moves.
        toast.success('已生成待发布公告（草稿）', '请到公告页审核后再发布。')
      } else {
        toast.success('上架状态已更新', '该状态转换不生成公告草稿。')
      }
      openAudit('t_model', model.id)
    } catch (error) {
      const hint = errorHint(error)
      toast.error(
        '上架状态更新失败',
        hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error),
      )
    }
  }

  const handleAddAlias = async () => {
    if (!editTarget) return
    const alias = newAlias.trim()
    if (!alias) {
      setAliasError('请填写要添加的别名。')
      return
    }
    if ([...alias].length > 128) {
      setAliasError('别名最多 128 个字符。')
      return
    }
    setAliasError(null)
    try {
      await createAlias.mutateAsync({ id: editTarget.id, alias })
      toast.success('别名已添加', alias)
      setNewAlias('')
      openAudit('t_model', editTarget.id)
    } catch (error) {
      const hint = errorHint(error)
      setAliasError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const handleDeleteAlias = async (alias: string) => {
    if (!editTarget) return
    setAliasError(null)
    try {
      await deleteAlias.mutateAsync({ id: editTarget.id, alias })
      toast.success('别名已删除', alias)
      openAudit('t_model', editTarget.id)
    } catch (error) {
      const hint = errorHint(error)
      setAliasError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const openWeight = (model: AdminModel) => {
    setWeightTarget(model)
    setWeightValue(String(model.recommend_weight))
    setWeightError(null)
  }

  const closeWeight = () => {
    if (setRecommendWeight.isPending) return
    setWeightTarget(null)
    setWeightError(null)
  }

  const handleWeightSave = async () => {
    if (!weightTarget) return
    if (!weightValue.trim()) {
      setWeightError('请填写推荐权重。')
      return
    }
    const parsed = parseRecommendWeight(weightValue)
    if (!parsed.ok || parsed.value === null) {
      setWeightError('推荐权重必须为整数。')
      return
    }
    if (parsed.value < 0) {
      setWeightError('推荐权重不能为负数。')
      return
    }
    if (parsed.value > RECOMMEND_WEIGHT_MAX) {
      setWeightError(
        `推荐权重不能超过 ${RECOMMEND_WEIGHT_MAX}（t_model.recommend_weight 为 int unsigned）。`,
      )
      return
    }
    if (weightTarget.listing_state !== 'LISTED' && parsed.value > 0) {
      setWeightError('仅已上架模型可设置非 0 推荐权重；当前状态为 ' + weightTarget.listing_state + '。')
      return
    }
    try {
      await setRecommendWeight.mutateAsync({
        id: weightTarget.id,
        input: { recommend_weight: parsed.value },
      })
      toast.success('推荐权重已更新')
      setWeightTarget(null)
      openAudit('t_model', weightTarget.id)
    } catch (error) {
      const hint = errorHint(error)
      setWeightError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const error = list.isError ? errorMessage(list.error) : null
  const hint = list.isError ? errorHint(list.error) : null

  const columns: TableColumn<AdminModel>[] = [
    {
      key: 'model',
      header: '模型',
      render: (model) => (
        <div className="space-y-0.5">
          <p className="font-medium text-ink">{model.display_name}</p>
          <code className="text-xs text-ink-secondary">{model.model_name}</code>
          {model.pricing === null ? (
            <Badge variant="warning">未配价</Badge>
          ) : null}
        </div>
      ),
    },
    {
      key: 'aliases',
      header: '别名',
      render: (model) =>
        model.aliases.length > 0 ? (
          <span className="text-xs text-ink-secondary">{model.aliases.join('、')}</span>
        ) : (
          <span className="text-xs text-ink-muted">—</span>
        ),
    },
    {
      key: 'type',
      header: '类型',
      render: (model) => <Badge variant="primary">{modelTypeLabel(model.model_type)}</Badge>,
    },
    {
      key: 'context',
      header: '上下文',
      align: 'right',
      render: (model) => (
        <span className="tabular-nums text-xs text-ink-secondary">
          {formatTokens(model.context_length)}
        </span>
      ),
    },
    {
      key: 'maxOutput',
      header: '输出上限',
      align: 'right',
      // 0 = 未配置：显示「—」，不冒充一个真实的 0 上限（网关会按
      // min(context_length, 32768) 兜底）。
      render: (model) => (
        <span className="tabular-nums text-xs text-ink-secondary">
          {model.max_output_tokens > 0 ? formatTokens(model.max_output_tokens) : '—'}
        </span>
      ),
    },
    {
      key: 'region',
      header: '来源地',
      render: (model) => (model.region === 'DOMESTIC' ? '境内' : '境外'),
    },
    {
      key: 'protocol',
      header: '协议',
      render: (model) => (
        <span className="text-xs text-ink-secondary">{protocolLabel(model.protocol)}</span>
      ),
    },
    {
      key: 'listing',
      header: '上架状态',
      render: (model) => (
        <Badge variant={listingVariant(model.listing_state)}>
          {LISTING_LABEL[model.listing_state]}
        </Badge>
      ),
    },
    {
      key: 'weight',
      header: '推荐权重',
      align: 'right',
      render: (model) => <span className="tabular-nums">{formatInteger(model.recommend_weight)}</span>,
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (model) => {
        const canList = model.listing_state !== 'LISTED'
        const unpriced = canList && model.pricing === null
        return (
          <div className="flex flex-col items-end gap-1">
            <div className="flex flex-wrap items-center justify-end gap-1">
              <Button variant="ghost" size="sm" onClick={() => openEdit(model)}>
                编辑
              </Button>
              <Button variant="ghost" size="sm" onClick={() => openWeight(model)}>
                推荐权重
              </Button>
              <Button
                variant="secondary"
                size="sm"
                loading={setModelListing.isPending}
                disabled={unpriced}
                title={unpriced ? '先配置价格才能上架' : undefined}
                onClick={() =>
                  void handleListingChange(
                    model,
                    model.listing_state === 'LISTED' ? 'CATALOG_ONLY' : 'LISTED',
                  )
                }
              >
                {model.listing_state === 'LISTED' ? '下架' : '上架'}
              </Button>
            </div>
            {unpriced ? (
              <span data-testid="pricing-required-hint" className="text-xs text-warning">
                先配置价格才能上架
              </span>
            ) : null}
          </div>
        )
      },
    },
  ]

  const blocked = isSourceRegionBlocked(form.region)
  const saving = createModel.isPending || updateModel.isPending
  const currentAliases = aliasesQuery.data?.aliases.map((item) => item.alias_name)
    ?? editTarget?.aliases
    ?? []

  return (
    <div className="space-y-5">
      <PageHeader
        title="模型主数据"
        description="模型名称、别名与上下架状态的唯一写入口；价格为只读引用。"
        actions={
          <>
            <Button variant="secondary" onClick={() => openAudit()}>
              审计记录
            </Button>
            <Button onClick={openCreate}>新建模型</Button>
          </>
        }
      />

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Input
          label="搜索模型"
          type="search"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="名称、展示名或别名片段"
        />
        <Select
          label="类型"
          value={type}
          onChange={(event) => setType(event.target.value as TypeFilter)}
          options={[
            { value: 'ALL', label: '全部类型' },
            { value: 'TEXT', label: modelTypeLabel('TEXT') },
            { value: 'IMAGE', label: modelTypeLabel('IMAGE') },
            { value: 'EMBEDDING', label: modelTypeLabel('EMBEDDING') },
          ]}
        />
        <Select
          label="来源地"
          value={region}
          onChange={(event) => setRegion(event.target.value as RegionFilter)}
          options={[
            { value: 'ALL', label: '全部来源地' },
            { value: 'DOMESTIC', label: '境内' },
            { value: 'OVERSEAS', label: '境外（历史数据）' },
          ]}
        />
        <Select
          label="上架状态"
          value={listing}
          onChange={(event) => setListing(event.target.value as ListingFilter)}
          options={[
            { value: 'ALL', label: '全部状态' },
            { value: 'LISTED', label: LISTING_LABEL.LISTED },
            { value: 'CATALOG_ONLY', label: LISTING_LABEL.CATALOG_ONLY },
            { value: 'DELISTED', label: LISTING_LABEL.DELISTED },
          ]}
        />
      </div>

      {hint ? (
        <p role="status" className="rounded-lg border border-danger bg-danger-soft px-4 py-2 text-xs text-danger">
          {hint}
        </p>
      ) : null}

      <Table
        ariaLabel="模型主数据列表"
        columns={columns}
        rows={list.data?.items ?? []}
        rowKey={(model) => String(model.id)}
        loading={list.isLoading}
        error={error}
        onRetry={() => void list.refetch()}
        emptyTitle="暂无模型"
        emptyDescription={
          debouncedSearch || type !== 'ALL' || region !== 'ALL' || listing !== 'ALL'
            ? '当前筛选条件没有匹配的模型。'
            : '创建模型后即可在定价页配置价格。'
        }
        pagination={
          list.data
            ? {
                page: list.data.page,
                pageSize: list.data.page_size,
                total: list.data.total,
                onPageChange: setPage,
              }
            : undefined
        }
      />

      <Drawer
        open={drawerOpen}
        onClose={closeDrawer}
        title={creating ? '新建模型' : '编辑模型'}
        description="模型名为价格表、模型广场与路由共同读取的唯一来源；改名请在此处完成。"
        size="xl"
        footer={
          <>
            <Button variant="secondary" onClick={closeDrawer} disabled={saving}>
              取消
            </Button>
            <Button onClick={() => void handleSave()} loading={saving} disabled={blocked}>
              保存
            </Button>
          </>
        }
      >
        <div className="space-y-5">
          {saveError ? (
            <p role="alert" className="rounded-lg border border-danger bg-danger-soft px-3 py-2 text-xs text-danger">
              {saveError}
            </p>
          ) : null}

          <div className="grid gap-4 sm:grid-cols-2">
            <Input
              label="模型名称"
              required
              value={form.modelName}
              error={errors.modelName}
              hint="对外调用时使用的模型名；改名会同步影响价格表与模型广场。"
              onChange={(event) => updateField('modelName', event.target.value)}
            />
            <Input
              label="展示名称"
              required
              value={form.displayName}
              error={errors.displayName}
              onChange={(event) => updateField('displayName', event.target.value)}
            />
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <Select
              label="模型类型"
              value={form.modelType}
              onChange={(event) => updateField('modelType', event.target.value as ModelType)}
              options={[
                { value: 'TEXT', label: modelTypeLabel('TEXT') },
                { value: 'IMAGE', label: modelTypeLabel('IMAGE') },
                { value: 'EMBEDDING', label: modelTypeLabel('EMBEDDING') },
              ]}
            />
            <Input
              label="协议编号"
              required
              inputMode="numeric"
              value={form.protocolId}
              error={errors.protocolId}
              hint={editTarget ? `当前协议代码：${protocolLabel(editTarget.protocol)}` : '填写 t_protocol.id'}
              onChange={(event) => updateField('protocolId', event.target.value)}
            />
          </div>

          <SourceRegionGuard
            value={form.region}
            onChange={(next) => updateField('region', next)}
            noun="模型"
            disabled={saving}
          />
          {errors.region ? <p className="text-xs text-danger">{errors.region}</p> : null}

          <div className="grid gap-4 sm:grid-cols-2">
            <Input
              label="上下文长度（tokens）"
              inputMode="numeric"
              value={form.contextLength}
              error={errors.contextLength}
              onChange={(event) => updateField('contextLength', event.target.value)}
            />
            <Input
              label="输出上限（tokens）"
              inputMode="numeric"
              value={form.maxOutputTokens}
              error={errors.maxOutputTokens}
              hint="0 或留空表示未配置，按 min(上下文长度, 32768) 兜底。"
              onChange={(event) => updateField('maxOutputTokens', event.target.value)}
            />
            <Input
              label="主渠道编号"
              inputMode="numeric"
              value={form.primaryChannelId}
              error={errors.primaryChannelId}
              hint="留空表示不指定；该渠道必须是境内渠道。"
              onChange={(event) => updateField('primaryChannelId', event.target.value)}
            />
          </div>

          <fieldset className="space-y-2">
            <legend className="text-sm font-medium text-ink">能力标签</legend>
            <div className="grid gap-2 sm:grid-cols-2">
              {CAPABILITY_LABELS.map((item) => (
                <CheckboxField
                  key={item.key}
                  label={item.label}
                  checked={form.capabilities[item.key]}
                  onChange={(checked) =>
                    updateField('capabilities', {
                      ...form.capabilities,
                      [item.key]: checked,
                    })
                  }
                />
              ))}
            </div>
            <p className="text-xs text-ink-muted">
              勾选表示支持；未勾选会显式写入“不支持”，与“后端未配置”不同。
            </p>
          </fieldset>

          {creating ? (
            <div className="space-y-1.5">
              <label className="block text-sm font-medium text-ink" htmlFor="model-aliases">
                别名
              </label>
              <textarea
                id="model-aliases"
                className="min-h-20 w-full rounded-lg border border-border-strong bg-surface px-3 py-2 font-mono text-xs text-ink placeholder:text-ink-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
                placeholder={'一行一个，或用逗号分隔'}
                value={form.aliasesText}
                onChange={(event) => updateField('aliasesText', event.target.value)}
              />
              {errors.aliasesText ? (
                <p className="text-xs text-danger">{errors.aliasesText}</p>
              ) : (
                <p className="text-xs text-ink-muted">别名最多 20 个，每个不超过 128 个字符。</p>
              )}
            </div>
          ) : (
            <div className="space-y-2 rounded-lg border border-border bg-surface-muted p-3">
              <p className="text-sm font-medium text-ink">别名</p>
              {currentAliases.length === 0 ? (
                <p className="text-xs text-ink-muted">暂未配置别名。</p>
              ) : (
                <ul className="flex flex-wrap gap-2">
                  {currentAliases.map((alias) => (
                    <li key={alias} className="inline-flex items-center gap-1 rounded-full border border-border-strong bg-surface px-2 py-0.5">
                      <code className="text-xs text-ink-secondary">{alias}</code>
                      <Button
                        variant="ghost"
                        size="sm"
                        className="h-6 px-1 text-danger"
                        onClick={() => void handleDeleteAlias(alias)}
                      >
                        删除
                      </Button>
                    </li>
                  ))}
                </ul>
              )}
              <div className="flex flex-wrap items-end gap-2">
                <Input
                  label="新增别名"
                  value={newAlias}
                  onChange={(event) => {
                    setNewAlias(event.target.value)
                    setAliasError(null)
                  }}
                  hint="别名不能与任何已有模型名重复。"
                />
                <Button
                  variant="secondary"
                  loading={createAlias.isPending}
                  onClick={() => void handleAddAlias()}
                >
                  添加别名
                </Button>
              </div>
              {aliasError ? (
                <p role="alert" className="text-xs text-danger">
                  {aliasError}
                </p>
              ) : null}
            </div>
          )}
        </div>
      </Drawer>

      <Modal
        open={weightTarget !== null}
        onClose={closeWeight}
        title="编辑推荐权重"
        description="0 表示不进推荐位；非 0 仅对已上架模型有效。"
        size="sm"
        footer={
          <>
            <Button variant="secondary" onClick={closeWeight} disabled={setRecommendWeight.isPending}>
              取消
            </Button>
            <Button onClick={() => void handleWeightSave()} loading={setRecommendWeight.isPending}>
              保存
            </Button>
          </>
        }
      >
        {weightTarget ? (
          <div className="space-y-3">
            <p className="text-sm text-ink-secondary">
              模型：{weightTarget.display_name}（{weightTarget.model_name}） · 当前状态：
              {LISTING_LABEL[weightTarget.listing_state]}
            </p>
            <Input
              label="推荐权重"
              inputMode="numeric"
              data-testid="recommend-weight-input"
              value={weightValue}
              error={weightError ?? undefined}
              hint={`${RECOMMEND_WEIGHT_MAX} 为 int unsigned 上限；0 表示不进推荐位。`}
              onChange={(event) => {
                setWeightValue(event.target.value)
                setWeightError(null)
              }}
            />
          </div>
        ) : null}
      </Modal>

      <AuditTrail
        open={auditOpen}
        onClose={() => setAuditOpen(false)}
        module="MODEL"
        targetType={auditTarget.type}
        targetId={auditTarget.id}
        refreshKey={auditRefreshKey}
      />
    </div>
  )
}

import { useState } from 'react'
import { ApiError, errorHint, errorMessage } from '@/api/client'
import {
  useChannel,
  useChannels,
  useCreateChannel,
  useDeleteChannel,
  useDisableChannel,
  useUpdateChannel,
} from '@/api/admin'
import type {
  Channel,
  ChannelWriteInput,
  Region,
  ScopeMode,
  StreamUsageMode,
} from '@/api/admin'
import {
  Badge,
  Button,
  Drawer,
  EmptyState,
  ErrorState,
  Input,
  PageHeader,
  Select,
  Table,
  useToast,
} from '@/components'
import type { TableColumn } from '@/components'
import {
  AuditTrail,
  ModelMapPicker,
  SourceRegionGuard,
  isSourceRegionBlocked,
} from '@/components/admin'
import { ConfirmDialog } from '@/components/console'
import { protocolLabel } from '@/lib/consoleLabels'
import { formatBasisPoints, formatDateTime } from '@/lib/format'
import { usePageTitle } from '@/lib/usePageTitle'

const PAGE_SIZE = 20

type EnabledFilter = 'ALL' | '0' | '1'
type RegionFilter = 'ALL' | Region

const HEALTH_LABEL: Record<string, string> = {
  CLOSED: '正常',
  OPEN: '熔断',
  HALF_OPEN: '半开',
  AUTH_FAIL: '鉴权失败',
}

const SCOPE_OPTIONS = [
  { value: 'ALL', label: '全部分组' },
  { value: 'SELECTED', label: '指定分组' },
]

// 用量口径原来是「NATIVE（上游返回用量）」这种「枚举 + 括号解释」的写法，
// 运营看到的第一眼是英文。只留中文解释，枚举交给接口，界面上不出现代码值。
const STREAM_USAGE_OPTIONS = [
  { value: 'NATIVE', label: '上游返回的用量' },
  { value: 'STREAM_OPTIONS', label: '从返回内容里读取用量' },
  { value: 'ESTIMATE', label: '本地按字数估算' },
  { value: 'UNKNOWN', label: '未知（不做限制）' },
]

interface ChannelFormState {
  name: string
  region: Region
  protocolId: string
  baseUrl: string
  upstreamKey: string
  groupScopeMode: ScopeMode
  dedicatedScopeMode: ScopeMode
  dedicatedUserIds: string
  fallbackAllowed: boolean
  weight: string
  priority: string
  timeoutConnectMs: string
  timeoutFirstByteMs: string
  timeoutTotalMs: string
  retryMax: string
  streamSupported: boolean
  streamUsageMode: StreamUsageMode
  modelMapText: string
}

const EMPTY_FORM: ChannelFormState = {
  name: '',
  region: 'DOMESTIC',
  protocolId: '',
  baseUrl: '',
  upstreamKey: '',
  groupScopeMode: 'ALL',
  dedicatedScopeMode: 'ALL',
  dedicatedUserIds: '',
  fallbackAllowed: true,
  weight: '100',
  priority: '100',
  timeoutConnectMs: '3000',
  timeoutFirstByteMs: '60000',
  timeoutTotalMs: '120000',
  retryMax: '2',
  streamSupported: true,
  streamUsageMode: 'UNKNOWN',
  modelMapText: '',
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

function parseUserIDs(raw: string): number[] | null {
  const normalized = raw
    .replace(/[０-９]/g, (char) => String.fromCharCode(char.charCodeAt(0) - 0xfee0))
    .split(/[,，\s]+/)
    .map((value) => value.trim())
    .filter(Boolean)
  if (normalized.length > 1_000) return null
  const out: number[] = []
  const seen = new Set<number>()
  for (const value of normalized) {
    if (!/^\d+$/.test(value)) return null
    const id = Number(value)
    if (!Number.isSafeInteger(id) || id <= 0) return null
    if (seen.has(id)) continue
    seen.add(id)
    out.push(id)
  }
  return out
}

/**
 * 把「平台模型=上游模型」写进映射文本：同名覆盖、保持其余行与原顺序。
 *
 * 选择器（ModelMapPicker）给的是两个确定的字符串，所以这里不需要像 parseModelMap
 * 那样做严格校验；它只负责把一行落进文本，剩下的交给保存前的统一校验。
 */
export function upsertModelMapLine(raw: string, platformModel: string, upstreamModel: string): string {
  const lines = raw
    .split(/\r?\n/)
    .map((line) => line.replace(/＝/g, '=').trim())
    .filter((line) => line !== '')
  const out: string[] = []
  let replaced = false
  for (const line of lines) {
    const index = line.indexOf('=')
    const key = index > 0 ? line.slice(0, index).trim() : line
    if (key === platformModel) {
      if (!replaced) {
        out.push(`${platformModel}=${upstreamModel}`)
        replaced = true
      }
      continue
    }
    // 顺手把「键 = 值」这种带空格的写法归一，避免写回后同一份映射有两种排版。
    out.push(index > 0 ? `${key}=${line.slice(index + 1).trim()}` : line)
  }
  if (!replaced) out.push(`${platformModel}=${upstreamModel}`)
  return out.join('\n')
}

function parseModelMap(raw: string): Record<string, string> | null {
  const lines = raw.split(/\r?\n/)
  const out: Record<string, string> = {}
  let count = 0
  for (const line of lines) {
    const normalized = line.replace(/＝/g, '=').trim()
    if (!normalized) continue
    const index = normalized.indexOf('=')
    if (index <= 0) return null
    const key = normalized.slice(0, index).trim()
    const value = normalized.slice(index + 1).trim()
    if (!key || !value || [...key].length > 128 || [...value].length > 128) return null
    out[key] = value
    count += 1
    if (count > 200) return null
  }
  return out
}

function serializeModelMap(map: Record<string, string> | undefined): string {
  if (!map) return ''
  return Object.entries(map)
    .map(([key, value]) => `${key}=${value}`)
    .join('\n')
}

function formFromChannel(channel: Channel): ChannelFormState {
  return {
    name: channel.name,
    region: channel.region,
    protocolId: String(channel.protocol_id),
    baseUrl: channel.base_url,
    upstreamKey: '',
    groupScopeMode: channel.group_scope_mode,
    dedicatedScopeMode: channel.dedicated_scope_mode,
    dedicatedUserIds: (channel.dedicated_user_ids ?? []).join(', '),
    fallbackAllowed: channel.fallback_allowed,
    weight: String(channel.weight),
    priority: String(channel.priority),
    timeoutConnectMs: String(channel.timeout_connect_ms),
    timeoutFirstByteMs: String(channel.timeout_first_byte_ms),
    timeoutTotalMs: String(channel.timeout_total_ms),
    retryMax: String(channel.retry_max),
    streamSupported: channel.stream_supported,
    streamUsageMode: channel.stream_usage_mode,
    modelMapText: serializeModelMap(channel.upstream_model_name_map),
  }
}

function validateForm(
  form: ChannelFormState,
  mode: 'create' | 'edit',
  keyTouched: boolean,
): { errors: Record<string, string>; payload: ChannelWriteInput | null } {
  const errors: Record<string, string> = {}
  const name = form.name.trim()
  if (!name) errors.name = '请填写渠道名称。'
  else if ([...name].length > 64) errors.name = '渠道名称最多 64 个字符。'

  if (isSourceRegionBlocked(form.region)) {
    errors.region = 'V1.0 仅支持境内渠道，不能选择「境外」。'
  }

  const protocolID = parseInteger(form.protocolId)
  if (protocolID === null || protocolID <= 0) errors.protocolId = '请填写有效的协议编号（正整数）。'

  const baseURL = form.baseUrl.trim()
  if (!baseURL) {
    errors.baseUrl = '请填写接入地址。'
  } else {
    try {
      const parsed = new URL(baseURL)
      if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') {
        errors.baseUrl = '接入地址只支持 http 或 https。'
      }
    } catch {
      errors.baseUrl = '接入地址不是合法地址。'
    }
  }

  const upstreamKey = form.upstreamKey.trim()
  if (mode === 'create' && !upstreamKey) {
    errors.upstreamKey = '新建渠道必须填写上游密钥。'
  } else if (mode === 'edit' && keyTouched && !upstreamKey) {
    errors.upstreamKey = '已选择重新填写，请填写新的上游密钥；取消后保留原密钥。'
  } else if (upstreamKey.length > 512) {
    errors.upstreamKey = '上游密钥最多 512 个字符。'
  }

  const weight = parseInteger(form.weight)
  if (weight === null || weight < 0 || weight > 1_000_000) {
    errors.weight = '权重需为 0–1000000 的整数。'
  }
  const priority = parseInteger(form.priority)
  if (priority === null || priority < -32768 || priority > 32767) {
    errors.priority = '优先级需为 -32768–32767 的整数。'
  }
  const connect = parseInteger(form.timeoutConnectMs)
  if (connect === null || connect <= 0 || connect > 600_000) {
    errors.timeoutConnectMs = '连接超时需为 1–600000 毫秒。'
  }
  const firstByte = parseInteger(form.timeoutFirstByteMs)
  if (firstByte === null || firstByte <= 0 || firstByte > 600_000) {
    errors.timeoutFirstByteMs = '首字节超时需为 1–600000 毫秒。'
  }
  const total = parseInteger(form.timeoutTotalMs)
  if (total === null || total <= 0 || total > 3_600_000) {
    errors.timeoutTotalMs = '总超时需为 1–3600000 毫秒。'
  }
  const retry = parseInteger(form.retryMax)
  if (retry === null || retry < 0 || retry > 2) {
    errors.retryMax = '重试次数需为 0–2 的整数。'
  }

  let dedicatedUserIDs: number[] | null = []
  if (form.dedicatedScopeMode === 'SELECTED') {
    dedicatedUserIDs = parseUserIDs(form.dedicatedUserIds)
    if (dedicatedUserIDs === null) {
      errors.dedicatedUserIds = '专属名单需为逗号分隔的正整数用户编号，最多 1000 个。'
    }
  }

  const modelMap = parseModelMap(form.modelMapText)
  if (modelMap === null) {
    errors.modelMapText = '每行格式为「模型名=上游模型名」，键值均不超过 128 个字符，最多 200 行。'
  }

  const hasErrors = Object.keys(errors).length > 0
  if (hasErrors || protocolID === null || weight === null || priority === null || connect === null ||
    firstByte === null || total === null || retry === null || dedicatedUserIDs === null || modelMap === null) {
    return { errors, payload: null }
  }

  const payload: ChannelWriteInput = {
    name,
    protocol_id: protocolID,
    region: form.region,
    base_url: baseURL,
    group_scope_mode: form.groupScopeMode,
    dedicated_scope_mode: form.dedicatedScopeMode,
    fallback_allowed: form.fallbackAllowed,
    weight,
    priority,
    timeout_connect_ms: connect,
    timeout_first_byte_ms: firstByte,
    timeout_total_ms: total,
    retry_max: retry,
    stream_supported: form.streamSupported,
    stream_usage_mode: form.streamUsageMode,
    upstream_model_name_map: modelMap,
  }
  if (form.dedicatedScopeMode === 'SELECTED') {
    payload.dedicated_user_ids = dedicatedUserIDs
  }
  if (mode === 'create' || (keyTouched && upstreamKey)) {
    payload.upstream_key = upstreamKey
  }
  return { errors, payload }
}

function readableChannelError(error: unknown): string {
  if (error instanceof ApiError && error.code === 'channel_in_use') {
    const hint = errorHint(error)
    return `仍有路由引用该渠道，请先解除引用后再删除。${hint ? `（${hint}）` : ''}`
  }
  const hint = errorHint(error)
  return hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error)
}

function CheckboxField({
  label,
  description,
  checked,
  onChange,
  disabled,
}: {
  label: string
  description?: string
  checked: boolean
  onChange: (checked: boolean) => void
  disabled?: boolean
}) {
  return (
    <label className="flex items-start gap-2 text-sm text-ink">
      <input
        type="checkbox"
        className="mt-0.5 h-4 w-4 rounded border-border-strong text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
        checked={checked}
        disabled={disabled}
        onChange={(event) => onChange(event.target.checked)}
      />
      <span>
        {label}
        {description ? <span className="block text-xs text-ink-muted">{description}</span> : null}
      </span>
    </label>
  )
}

export default function ChannelsPage() {
  usePageTitle('渠道管理')
  const toast = useToast()

  const [enabledFilter, setEnabledFilter] = useState<EnabledFilter>('ALL')
  const [regionFilter, setRegionFilter] = useState<RegionFilter>('ALL')
  const [page, setPage] = useState(1)

  const list = useChannels({
    page,
    pageSize: PAGE_SIZE,
    enabled: enabledFilter === 'ALL' ? undefined : enabledFilter,
    region: regionFilter === 'ALL' ? undefined : regionFilter,
    sort: 'updated_at',
    order: 'desc',
  })

  const [creating, setCreating] = useState(false)
  const [editTarget, setEditTarget] = useState<Channel | null>(null)
  const drawerOpen = creating || editTarget !== null

  const detail = useChannel(editTarget?.id)
  const createChannel = useCreateChannel()
  const updateChannel = useUpdateChannel()
  const disableChannel = useDisableChannel()
  const deleteChannel = useDeleteChannel()

  const [form, setForm] = useState<ChannelFormState>(EMPTY_FORM)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [upstreamKeyTouched, setUpstreamKeyTouched] = useState(false)
  // 上游模型选择器：只在编辑已有渠道时可用（需要渠道 id 才能用它自己的密钥去查）。
  const [modelPickerOpen, setModelPickerOpen] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)

  const [disableTarget, setDisableTarget] = useState<Channel | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Channel | null>(null)
  const [confirmError, setConfirmError] = useState<string | null>(null)

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

  const loadedChannel = detail.data?.channel

  const resetDrawerState = () => {
    setCreating(false)
    setEditTarget(null)
    setErrors({})
    setSaveError(null)
    setUpstreamKeyTouched(false)
  }

  const closeDrawer = () => {
    if (createChannel.isPending || updateChannel.isPending) return
    resetDrawerState()
  }

  // 选择器选完只改表单，不直接落库：运营仍然要过一遍保存，写入才会进审计。
  const applyModelMap = (platformModel: string, upstreamModel: string) => {
    updateField('modelMapText', upsertModelMapLine(form.modelMapText, platformModel, upstreamModel))
    setErrors((previous) => {
      if (previous.modelMapText === undefined) return previous
      const next = { ...previous }
      delete next.modelMapText
      return next
    })
  }

  const openCreate = () => {
    setEditTarget(null)
    setForm(EMPTY_FORM)
    setErrors({})
    setUpstreamKeyTouched(false)
    setSaveError(null)
    setCreating(true)
  }

  const openEdit = (channel: Channel) => {
    setCreating(false)
    // Seed from the table row (already refreshed after the previous write).
    // The detail request below only proves the row still exists and supplies
    // the mask; it must not overwrite the form after a refetch.
    setForm(formFromChannel(channel))
    setErrors({})
    setUpstreamKeyTouched(false)
    setSaveError(null)
    setEditTarget(channel)
  }

  const updateField = <K extends keyof ChannelFormState>(key: K, value: ChannelFormState[K]) => {
    setForm((current) => ({ ...current, [key]: value }))
    setErrors((current) => {
      if (!current[key]) return current
      const next = { ...current }
      delete next[key]
      return next
    })
  }

  const handleSave = async () => {
    const mode = creating ? 'create' : 'edit'
    if (mode === 'edit' && !editTarget) return
    const result = validateForm(form, mode, upstreamKeyTouched)
    setErrors(result.errors)
    if (!result.payload) return
    setSaveError(null)
    try {
      const response =
        mode === 'create'
          ? await createChannel.mutateAsync(result.payload)
          : await updateChannel.mutateAsync({
              id: editTarget?.id as number,
              input: result.payload,
            })
      toast.success(mode === 'create' ? '渠道已创建' : '渠道已更新')
      resetDrawerState()
      openAudit('t_channel', response.channel.id)
    } catch (error) {
      setSaveError(readableChannelError(error))
    }
  }

  const handleDisable = async () => {
    if (!disableTarget) return
    setConfirmError(null)
    try {
      await disableChannel.mutateAsync(disableTarget.id)
      toast.success('渠道已停用')
      openAudit('t_channel', disableTarget.id)
      setDisableTarget(null)
    } catch (error) {
      setConfirmError(readableChannelError(error))
    }
  }

  const handleDelete = async () => {
    if (!deleteTarget) return
    setConfirmError(null)
    try {
      await deleteChannel.mutateAsync(deleteTarget.id)
      toast.success('渠道已删除')
      openAudit('t_channel', deleteTarget.id)
      setDeleteTarget(null)
    } catch (error) {
      setConfirmError(readableChannelError(error))
    }
  }

  const error = list.isError ? errorMessage(list.error) : null
  const hint = list.isError ? errorHint(list.error) : null

  const columns: TableColumn<Channel>[] = [
      {
        key: 'name',
        header: '名称',
        render: (row) => (
          <div className="space-y-0.5">
            <p className="font-medium text-ink">{row.name}</p>
            </div>
        ),
      },
      {
        key: 'region',
        header: '来源地',
        render: (row) => (row.region === 'DOMESTIC' ? '境内' : '境外'),
      },
      {
        key: 'protocol',
        header: '协议',
        render: (row) => (
          <span className="text-xs text-ink-secondary">{protocolLabel(row.protocol)}</span>
        ),
      },
      {
        key: 'weight',
        header: '权重',
        align: 'right',
        render: (row) => <span className="tabular-nums">{row.weight}</span>,
      },
      {
        key: 'status',
        header: '状态',
        render: (row) => (
          <div className="flex flex-wrap gap-1">
            <Badge variant={row.enabled ? 'success' : 'neutral'} dot>
              {row.enabled ? '启用' : '已停用'}
            </Badge>
            <Badge variant={row.health_state === 'CLOSED' ? 'success' : 'warning'}>
              {HEALTH_LABEL[row.health_state] ?? row.health_state}
            </Badge>
          </div>
        ),
      },
      {
        key: 'success_rate',
        header: '成功率',
        align: 'right',
        render: (row) =>
          row.success_rate_bp === null ? (
            <span className="text-xs text-ink-muted" title="近 15 分钟无上游尝试">
              —
            </span>
          ) : (
            <span className="tabular-nums text-xs text-ink-secondary">
              {formatBasisPoints(row.success_rate_bp)}
            </span>
          ),
      },
      {
        key: 'updated_at',
        header: '更新时间',
        render: (row) => (
          <span className="text-xs text-ink-secondary">{formatDateTime(row.updated_at)}</span>
        ),
      },
      {
        key: 'actions',
        header: '操作',
        align: 'right',
        render: (row) => (
          <div className="flex flex-wrap items-center justify-end gap-1">
            <Button variant="ghost" size="sm" onClick={() => openEdit(row)}>
              编辑
            </Button>
            {row.enabled ? (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => {
                  setConfirmError(null)
                  setDisableTarget(row)
                }}
              >
                停用
              </Button>
            ) : null}
            <Button
              variant="ghost"
              size="sm"
              className="text-danger"
              onClick={() => {
                setConfirmError(null)
                setDeleteTarget(row)
              }}
            >
              删除
            </Button>
          </div>
        ),
      },
  ]

  const missing =
    detail.error instanceof ApiError && detail.error.code === 'channel_not_found'

  if (missing) {
    return (
      <div className="space-y-5">
        <PageHeader title="渠道管理" description="上游渠道、来源地、协议、权重与分组范围。" />
        <EmptyState
          title="渠道不存在或已被移除"
          description="该渠道已被删除或不在当前账号的数据范围内。请返回列表确认最新状态。"
          action={
            <Button
              size="sm"
              onClick={() => {
                setEditTarget(null)
                setCreating(false)
                void list.refetch()
              }}
            >
              返回渠道列表
            </Button>
          }
        />
      </div>
    )
  }

  const blocked = isSourceRegionBlocked(form.region)
  const saving = createChannel.isPending || updateChannel.isPending
  const detailLoading = !creating && detail.isLoading && !loadedChannel
  const detailError = !creating && detail.isError && !missing ? errorMessage(detail.error) : null
  const detailHint = !creating && detail.isError && !missing ? errorHint(detail.error) : null

  return (
    <div className="space-y-5">
      <PageHeader
        title="渠道管理"
        description="来源地仅支持境内；上游密钥保存后只显示掩码，不再展示明文。"
        actions={
          <>
            <Button variant="secondary" onClick={() => openAudit()}>
              审计记录
            </Button>
            <Button onClick={openCreate}>新建渠道</Button>
          </>
        }
      />

      <div className="grid gap-3 sm:grid-cols-[12rem_12rem]">
        <Select
          label="状态"
          value={enabledFilter}
          onChange={(event) => {
            setEnabledFilter(event.target.value as EnabledFilter)
            setPage(1)
          }}
          options={[
            { value: 'ALL', label: '全部状态' },
            { value: '1', label: '启用' },
            { value: '0', label: '已停用' },
          ]}
        />
        <Select
          label="来源地"
          value={regionFilter}
          onChange={(event) => {
            setRegionFilter(event.target.value as RegionFilter)
            setPage(1)
          }}
          options={[
            { value: 'ALL', label: '全部来源地' },
            { value: 'DOMESTIC', label: '境内' },
            { value: 'OVERSEAS', label: '境外（历史数据）' },
          ]}
        />
      </div>

      {hint ? (
        <p role="status" className="rounded-lg border border-danger bg-danger-soft px-4 py-2 text-xs text-danger">
          {hint}
        </p>
      ) : null}

      <Table
        ariaLabel="渠道列表"
        columns={columns}
        rows={list.data?.items ?? []}
        rowKey={(row) => String(row.id)}
        loading={list.isLoading}
        error={error}
        onRetry={() => void list.refetch()}
        emptyTitle="暂无渠道"
        emptyDescription={
          enabledFilter !== 'ALL' || regionFilter !== 'ALL'
            ? '当前筛选条件没有匹配的渠道。'
            : '创建第一条上游渠道后即可配置路由。'
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
        title={creating ? '新建渠道' : '编辑渠道'}
        description="写操作成功后会自动打开审计记录，可查看变更前后值。"
        size="xl"
        footer={
          <>
            <Button variant="secondary" onClick={closeDrawer} disabled={saving}>
              取消
            </Button>
            <Button
              onClick={() => void handleSave()}
              loading={saving}
              disabled={blocked || detailLoading || Boolean(detailError)}
            >
              保存
            </Button>
          </>
        }
      >
        {detailLoading ? (
          <p className="text-sm text-ink-secondary">正在读取渠道详情…</p>
        ) : detailError ? (
          <ErrorState message={detailError} hint={detailHint} onRetry={() => void detail.refetch()} />
        ) : (
          <div className="space-y-5">
            {saveError ? (
              <p role="alert" className="rounded-lg border border-danger bg-danger-soft px-3 py-2 text-xs text-danger">
                {saveError}
              </p>
            ) : null}

            <div className="grid gap-4 sm:grid-cols-2">
              <Input
                label="渠道名称"
                required
                value={form.name}
                error={errors.name}
                onChange={(event) => updateField('name', event.target.value)}
              />
              <Input
                label="协议编号"
                required
                inputMode="numeric"
                value={form.protocolId}
                error={errors.protocolId}
                hint={
                  loadedChannel
                    ? `当前协议：${protocolLabel(loadedChannel.protocol)}`
                    : '填写平台登记的协议编号，可向技术同事索取'
                }
                onChange={(event) => updateField('protocolId', event.target.value)}
              />
            </div>

            <SourceRegionGuard
              value={form.region}
              onChange={(region) => updateField('region', region)}
              disabled={saving}
            />
            {errors.region ? <p className="text-xs text-danger">{errors.region}</p> : null}

            <Input
              label="接入地址"
              required
              value={form.baseUrl}
              error={errors.baseUrl}
              hint="只接受 https 地址；http 仅限本机或内网地址。"
              onChange={(event) => updateField('baseUrl', event.target.value)}
            />

            {creating ? (
              <Input
                label="上游密钥"
                required
                type="password"
                autoComplete="new-password"
                value={form.upstreamKey}
                error={errors.upstreamKey}
                hint="保存后只显示掩码，无法再次查看明文。"
                onChange={(event) => updateField('upstreamKey', event.target.value)}
              />
            ) : (
              <div className="space-y-2 rounded-lg border border-border bg-surface-muted p-3">
                <p className="text-sm font-medium text-ink">上游密钥</p>
                {upstreamKeyTouched ? (
                  <>
                    <Input
                      label="新的上游密钥"
                      type="password"
                      autoComplete="new-password"
                      value={form.upstreamKey}
                      error={errors.upstreamKey}
                      hint="填写后保存会覆盖原密钥并轮换；留空不会提交该字段。"
                      onChange={(event) => updateField('upstreamKey', event.target.value)}
                    />
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => {
                        setUpstreamKeyTouched(false)
                        updateField('upstreamKey', '')
                      }}
                    >
                      取消重新填写
                    </Button>
                  </>
                ) : (
                  <div className="flex flex-wrap items-center gap-2">
                    <code
                      data-testid="upstream-key-mask"
                      className="font-mono text-xs text-ink-secondary"
                    >
                      {loadedChannel?.upstream_key_mask ?? editTarget?.upstream_key_mask ?? '—'}
                    </code>
                    <Button
                      variant="secondary"
                      size="sm"
                      onClick={() => {
                        setUpstreamKeyTouched(true)
                        updateField('upstreamKey', '')
                      }}
                    >
                      重新填写
                    </Button>
                  </div>
                )}
              </div>
            )}

            <div className="grid gap-4 sm:grid-cols-3">
              <Input
                label="权重"
                inputMode="numeric"
                value={form.weight}
                error={errors.weight}
                onChange={(event) => updateField('weight', event.target.value)}
              />
              <Input
                label="优先级"
                inputMode="numeric"
                value={form.priority}
                error={errors.priority}
                onChange={(event) => updateField('priority', event.target.value)}
              />
              <Input
                label="重试次数"
                inputMode="numeric"
                value={form.retryMax}
                error={errors.retryMax}
                hint="0–2"
                onChange={(event) => updateField('retryMax', event.target.value)}
              />
            </div>

            <div className="grid gap-4 sm:grid-cols-3">
              <Input
                label="连接超时（毫秒）"
                inputMode="numeric"
                value={form.timeoutConnectMs}
                error={errors.timeoutConnectMs}
                onChange={(event) => updateField('timeoutConnectMs', event.target.value)}
              />
              <Input
                label="首字节超时（毫秒）"
                inputMode="numeric"
                value={form.timeoutFirstByteMs}
                error={errors.timeoutFirstByteMs}
                onChange={(event) => updateField('timeoutFirstByteMs', event.target.value)}
              />
              <Input
                label="总超时（毫秒）"
                inputMode="numeric"
                value={form.timeoutTotalMs}
                error={errors.timeoutTotalMs}
                onChange={(event) => updateField('timeoutTotalMs', event.target.value)}
              />
            </div>

            <div className="grid gap-4 sm:grid-cols-2">
              <Select
                label="允许分组范围"
                value={form.groupScopeMode}
                onChange={(event) => updateField('groupScopeMode', event.target.value as ScopeMode)}
                options={SCOPE_OPTIONS}
              />
              <Select
                label="专属名单范围"
                value={form.dedicatedScopeMode}
                onChange={(event) =>
                  updateField('dedicatedScopeMode', event.target.value as ScopeMode)
                }
                options={SCOPE_OPTIONS}
              />
            </div>

            {form.dedicatedScopeMode === 'SELECTED' ? (
              <Input
                label="专属用户编号"
                value={form.dedicatedUserIds}
                error={errors.dedicatedUserIds}
                hint="用逗号分隔的用户编号，最多 1000 个。"
                onChange={(event) => updateField('dedicatedUserIds', event.target.value)}
              />
            ) : null}

            <div className="grid gap-4 sm:grid-cols-2">
              <CheckboxField
                label="支持流式"
                checked={form.streamSupported}
                onChange={(checked) => updateField('streamSupported', checked)}
              />
              <Select
                label="流式用量模式"
                value={form.streamUsageMode}
                onChange={(event) =>
                  updateField('streamUsageMode', event.target.value as StreamUsageMode)
                }
                options={STREAM_USAGE_OPTIONS}
              />
            </div>

            <CheckboxField
              label="允许故障转移"
              description="关闭后该渠道失败时不会切换到其他渠道。"
              checked={form.fallbackAllowed}
              onChange={(checked) => updateField('fallbackAllowed', checked)}
            />

            <div className="space-y-1.5">
              <div className="flex items-center justify-between gap-2">
                <label
                  className="block text-sm font-medium text-ink"
                  htmlFor="channel-model-map"
                >
                  上游模型名覆盖（高级）
                </label>
                {/* 上游模型名不要手打：厂商改目录时手打的名字不会报错，只会在调用时 502。 */}
                <Button
                  type="button"
                  size="sm"
                  variant="secondary"
                  disabled={editTarget === null}
                  title={
                    editTarget === null
                      ? '先保存渠道，才能用它自己的密钥向上游查询'
                      : undefined
                  }
                  onClick={() => setModelPickerOpen(true)}
                >
                  从上游获取模型名
                </Button>
              </div>
              {/*
                这一栏不是必填项，也不该是默认步骤：模型名与厂商官方名称一致时，
                真正决定发什么名字的是路由上的 upstream_model_name（每条模型 × 渠道
                一份），这里只是「同一个平台名要发成别的上游名」时的覆盖层。
                折叠起来是为了别让运营以为「不填就跑不起来」。
              */}
              <details
                className="rounded-lg border border-border px-3 py-2"
                open={errors.modelMapText ? true : undefined}
              >
                <summary className="cursor-pointer text-xs text-ink-secondary">
                  名称与上游官方一致时无需填写；确需覆盖时点开（留空即用官方名）
                </summary>
                <div className="mt-2 space-y-1.5">
                  <textarea
                    id="channel-model-map"
                    className="min-h-20 w-full rounded-lg border border-border-strong bg-surface px-3 py-2 font-mono text-xs text-ink placeholder:text-ink-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
                    placeholder={'每行一条，例如：\nqwen-max=qwen-max-latest'}
                    value={form.modelMapText}
                    onChange={(event) => updateField('modelMapText', event.target.value)}
                  />
                  {errors.modelMapText ? (
                    <p className="text-xs text-danger">{errors.modelMapText}</p>
                  ) : (
                    <p className="text-xs text-ink-muted">
                      每行「平台模型名=上游模型名」，作用于本渠道全部模型；支持全角「＝」，空行忽略。
                    </p>
                  )}
                </div>
              </details>
            </div>
          </div>
        )}
      </Drawer>

      <ModelMapPicker
        open={modelPickerOpen}
        channelId={editTarget === null ? null : editTarget.id}
        onClose={() => setModelPickerOpen(false)}
        onApply={applyModelMap}
      />

      <ConfirmDialog
        open={disableTarget !== null}
        title="停用渠道"
        description={
          disableTarget
            ? `停用后「${disableTarget.name}」不再参与路由，可随时重新启用。确认停用？`
            : undefined
        }
        confirmLabel="确认停用"
        loading={disableChannel.isPending}
        error={confirmError}
        onConfirm={() => void handleDisable()}
        onCancel={() => {
          if (disableChannel.isPending) return
          setDisableTarget(null)
          setConfirmError(null)
        }}
      />

      <ConfirmDialog
        open={deleteTarget !== null}
        title="删除渠道"
        description={
          deleteTarget
            ? `删除后「${deleteTarget.name}」不再出现在列表且无法恢复；被路由引用的渠道会被后端拒绝删除。确认删除？`
            : undefined
        }
        confirmLabel="确认删除"
        loading={deleteChannel.isPending}
        error={confirmError}
        onConfirm={() => void handleDelete()}
        onCancel={() => {
          if (deleteChannel.isPending) return
          setDeleteTarget(null)
          setConfirmError(null)
        }}
      />

      <AuditTrail
        open={auditOpen}
        onClose={() => setAuditOpen(false)}
        module="CHANNEL"
        targetType={auditTarget.type}
        targetId={auditTarget.id}
        refreshKey={auditRefreshKey}
      />
    </div>
  )
}

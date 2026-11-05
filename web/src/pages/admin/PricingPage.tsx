import { useEffect, useState } from 'react'
import { ApiError, errorHint, errorMessage } from '@/api/client'
import {
  useCreatePrice,
  useGroups,
  useIntegrations,
  useModels,
  usePrices,
  useUpdateGroup,
  useUpdateIntegration,
  useUpdatePrice,
} from '@/api/admin'
import type {
  AdminIntegration,
  AdminPriceRule,
  GroupKeySync,
  ModelType,
  PriceClearField,
  PriceGroup,
  PriceWriteInput,
} from '@/api/admin'
import { cn } from '@/lib/cn'
import {
  Badge,
  Button,
  Drawer,
  ErrorState,
  Input,
  Modal,
  PageHeader,
  Select,
  Table,
  Tabs,
  useToast,
} from '@/components'
import type { TableColumn } from '@/components'
import { AuditTrail, EffectiveTimePicker, FloorCheckHint } from '@/components/admin'
import {
  earliestEffectiveAt,
  isEffectiveTimeTooEarly,
  toLocalDateTimeValue,
} from '@/components/admin'
import { modelTypeLabel } from '@/lib/consoleLabels'
import {
  formatCreditAmount,
  formatDateTime,
  formatInteger,
  formatPricePerK,
  formatRatioBp,
} from '@/lib/format'
import { usePageTitle } from '@/lib/usePageTitle'
import {
  activePriceRule,
  findPriceConflicts,
  resolvePriceDirection,
} from './pricingUtils'
import type { PriceDirection, PriceSnapshot } from './pricingUtils'

const PAGE_SIZE = 20
const CACHE_RATIO_MAX_BP = 100_000
const NO_STANDARD_RATIO_BP = 10_000

type TypeFilter = 'ALL' | ModelType
type EnabledFilter = 'ALL' | '0' | '1'
type ScopeLabel = 'ALL' | 'SELECTED'

interface PriceFormState {
  modelId: string
  modelType: ModelType
  inPrice: string
  outPrice: string
  perCall: string
  cacheHit: string
  cacheWrite: string
  longThreshold: string
  longIn: string
  longOut: string
  costIn: string
  costOut: string
  costCall: string
  currency: string
  effectiveFrom: string
  effectiveTo: string
  enabled: boolean
  confirmNoCost: boolean
  /** Optional columns the operator explicitly wants set to NULL. */
  clearFields: PriceClearField[]
}

const LONG_CTX_CLEAR_FIELDS: PriceClearField[] = [
  'long_ctx_threshold_tokens',
  'long_ctx_in_price_micro_per_k',
  'long_ctx_out_price_micro_per_k',
]

const CLEAR_FIELD_BY_FORM_KEY: Partial<Record<keyof PriceFormState, PriceClearField>> = {
  cacheHit: 'cache_hit_ratio_bp',
  cacheWrite: 'cache_write_ratio_bp',
  costIn: 'cost_in_micro_per_k',
  costOut: 'cost_out_micro_per_k',
  costCall: 'cost_per_call_micro',
  effectiveTo: 'effective_to',
}

interface ParsedNumber {
  ok: boolean
  value: number | null
}

function normalizeDigits(raw: string): string {
  return raw
    .replace(/[０-９]/g, (char) => String.fromCharCode(char.charCodeAt(0) - 0xfee0))
    .replace(/[，,\s]/g, '')
}

function parseOptionalInteger(raw: string): ParsedNumber {
  const trimmed = raw.trim()
  if (!trimmed) return { ok: true, value: null }
  const normalized = normalizeDigits(trimmed)
  if (!/^\d+$/.test(normalized)) return { ok: false, value: null }
  const value = Number(normalized)
  return Number.isSafeInteger(value) ? { ok: true, value } : { ok: false, value: null }
}

function parsePositiveInteger(raw: string): number | null {
  const parsed = parseOptionalInteger(raw)
  return parsed.ok && parsed.value !== null && parsed.value > 0 ? parsed.value : null
}

function optionalNumber(value: number | null | undefined): string {
  return value === null || value === undefined ? '' : String(value)
}

function effectiveFromForEdit(rule: EffectiveTimeSource): string {
  const now = new Date()
  const from = new Date(rule.effective_from)
  if (Number.isNaN(from.getTime()) || from.getTime() < now.getTime()) {
    return now.toISOString()
  }
  return from.toISOString()
}

interface EffectiveTimeSource {
  effective_from: string
}

function emptyForm(): PriceFormState {
  return {
    modelId: '',
    modelType: 'TEXT',
    inPrice: '0',
    outPrice: '0',
    perCall: '0',
    cacheHit: '',
    cacheWrite: '',
    longThreshold: '',
    longIn: '',
    longOut: '',
    costIn: '',
    costOut: '',
    costCall: '',
    currency: 'CNY',
    effectiveFrom: new Date().toISOString(),
    effectiveTo: '',
    enabled: true,
    confirmNoCost: false,
    clearFields: [],
  }
}

function formFromRule(rule: AdminPriceRule): PriceFormState {
  return {
    modelId: String(rule.model_id),
    modelType: rule.model_type,
    inPrice: String(rule.in_price_micro_per_k),
    outPrice: String(rule.out_price_micro_per_k),
    perCall: String(rule.per_call_price_micro),
    cacheHit: optionalNumber(rule.cache_hit_ratio_bp),
    cacheWrite: optionalNumber(rule.cache_write_ratio_bp),
    longThreshold: optionalNumber(rule.long_ctx_threshold_tokens),
    longIn: optionalNumber(rule.long_ctx_in_price_micro_per_k),
    longOut: optionalNumber(rule.long_ctx_out_price_micro_per_k),
    costIn: optionalNumber(rule.cost_in_micro_per_k),
    costOut: optionalNumber(rule.cost_out_micro_per_k),
    costCall: optionalNumber(rule.cost_per_call_micro),
    currency: rule.currency_cost,
    effectiveFrom: effectiveFromForEdit(rule),
    effectiveTo: rule.effective_to ?? '',
    enabled: rule.enabled,
    confirmNoCost: false,
    clearFields: [],
  }
}

function snapshotFromForm(form: PriceFormState): PriceSnapshot {
  return {
    in_price_micro_per_k: parseOptionalInteger(form.inPrice).value ?? 0,
    out_price_micro_per_k: parseOptionalInteger(form.outPrice).value ?? 0,
    per_call_price_micro: parseOptionalInteger(form.perCall).value ?? 0,
    long_ctx_in_price_micro_per_k: parseOptionalInteger(form.longIn).value,
    long_ctx_out_price_micro_per_k: parseOptionalInteger(form.longOut).value,
  }
}

function relevantCostFields(type: ModelType): Array<keyof PriceFormState> {
  if (type === 'IMAGE') return ['costCall']
  if (type === 'EMBEDDING') return ['costIn']
  return ['costIn', 'costOut']
}

function validatePriceForm(
  form: PriceFormState,
  creating: boolean,
  direction: PriceDirection,
): { errors: Record<string, string>; payload: PriceWriteInput | null } {
  const errors: Record<string, string> = {}

  const modelId = parsePositiveInteger(form.modelId)
  if (creating && modelId === null) {
    errors.modelId = '请选择模型。'
  }

  const inPrice = parseOptionalInteger(form.inPrice)
  const outPrice = parseOptionalInteger(form.outPrice)
  const perCall = parseOptionalInteger(form.perCall)
  if (!inPrice.ok) errors.inPrice = '输入单价必须为非负整数。'
  if (!outPrice.ok) errors.outPrice = '输出单价必须为非负整数。'
  if (!perCall.ok) errors.perCall = '按次单价必须为非负整数。'

  const requireGreaterThanZero = (
    key: 'inPrice' | 'outPrice' | 'perCall',
    parsed: ParsedNumber,
    label: string,
  ) => {
    if (!parsed.ok) {
      errors[key] = `${label}必须为非负整数。`
    } else if (parsed.value === null || parsed.value <= 0) {
      errors[key] = `${label}必须大于 0。`
    }
  }

  if (form.modelType === 'TEXT') {
    requireGreaterThanZero('inPrice', inPrice, '输入单价')
    requireGreaterThanZero('outPrice', outPrice, '输出单价')
    if (perCall.ok && perCall.value !== null && perCall.value !== 0) {
      errors.perCall = '文本模型的按次单价必须为 0。'
    }
  } else if (form.modelType === 'IMAGE') {
    requireGreaterThanZero('perCall', perCall, '按次单价')
    if (inPrice.ok && inPrice.value !== null && inPrice.value !== 0) {
      errors.inPrice = '图像模型的输入单价必须为 0。'
    }
    if (outPrice.ok && outPrice.value !== null && outPrice.value !== 0) {
      errors.outPrice = '图像模型的输出单价必须为 0。'
    }
  } else {
    requireGreaterThanZero('inPrice', inPrice, '输入单价')
    if (outPrice.ok && outPrice.value !== null && outPrice.value !== 0) {
      errors.outPrice = '向量模型的输出单价必须为 0。'
    }
    if (perCall.ok && perCall.value !== null && perCall.value !== 0) {
      errors.perCall = '向量模型的按次单价必须为 0。'
    }
  }

  const cacheHit = parseOptionalInteger(form.cacheHit)
  if (!cacheHit.ok) errors.cacheHit = '缓存命中倍率必须为非负整数。'
  else if (cacheHit.value !== null && cacheHit.value > CACHE_RATIO_MAX_BP) {
    errors.cacheHit = `缓存命中倍率不能超过 ${CACHE_RATIO_MAX_BP}。`
  }
  const cacheWrite = parseOptionalInteger(form.cacheWrite)
  if (!cacheWrite.ok) errors.cacheWrite = '缓存写入倍率必须为非负整数。'
  else if (cacheWrite.value !== null && cacheWrite.value > CACHE_RATIO_MAX_BP) {
    errors.cacheWrite = `缓存写入倍率不能超过 ${CACHE_RATIO_MAX_BP}。`
  }

  const costIn = parseOptionalInteger(form.costIn)
  const costOut = parseOptionalInteger(form.costOut)
  const costCall = parseOptionalInteger(form.costCall)
  const validateCost = (key: 'costIn' | 'costOut' | 'costCall', parsed: ParsedNumber) => {
    if (!parsed.ok) {
      errors[key] = '成本价必须为非负整数。'
    } else if (parsed.value !== null && parsed.value <= 0) {
      errors[key] = '成本价必须大于 0；不填表示无成本价。'
    }
  }
  validateCost('costIn', costIn)
  validateCost('costOut', costOut)
  validateCost('costCall', costCall)

  const longThreshold = parseOptionalInteger(form.longThreshold)
  const longIn = parseOptionalInteger(form.longIn)
  const longOut = parseOptionalInteger(form.longOut)
  const longRawValues = [form.longThreshold, form.longIn, form.longOut].map((value) =>
    value.trim(),
  )
  const longFilled = longRawValues.filter(Boolean).length
  if (longFilled > 0 && longFilled < 3) {
    if (!longRawValues[0]) errors.longThreshold = '阶梯三项必须同时填写或全部留空。'
    if (!longRawValues[1]) errors.longIn = '阶梯三项必须同时填写或全部留空。'
    if (!longRawValues[2]) errors.longOut = '阶梯三项必须同时填写或全部留空。'
  }
  if (longFilled === 3) {
    if (!longThreshold.ok) errors.longThreshold = '长上下文阈值必须为正整数。'
    else if (longThreshold.value === null || longThreshold.value <= 0) {
      errors.longThreshold = '长上下文阈值必须大于 0。'
    }
    if (!longIn.ok) errors.longIn = '阶梯输入价必须为正整数。'
    else if (longIn.value === null || longIn.value <= 0) {
      errors.longIn = '阶梯输入价必须大于 0。'
    }
    if (!longOut.ok) errors.longOut = '阶梯输出价必须为正整数。'
    else if (longOut.value === null || longOut.value <= 0) {
      errors.longOut = '阶梯输出价必须大于 0。'
    }
  }

  const currency = form.currency.trim().toUpperCase()
  if (currency !== 'CNY') {
    errors.currency = '当前仅支持 CNY。'
  }

  const effectiveFrom = new Date(form.effectiveFrom)
  if (!form.effectiveFrom || Number.isNaN(effectiveFrom.getTime())) {
    errors.effectiveFrom = '请选择生效时间。'
  } else if (direction === 'raise' && isEffectiveTimeTooEarly(form.effectiveFrom, 'raise')) {
    errors.effectiveFrom = `涨价需提前 7 天公示，请选择 ${earliestEffectiveAt(
      'raise',
    ).toLocaleString('zh-CN')} 之后的时间。`
  }

  const effectiveTo = form.effectiveTo ? new Date(form.effectiveTo) : null
  if (effectiveTo && Number.isNaN(effectiveTo.getTime())) {
    errors.effectiveTo = '失效时间格式不正确。'
  } else if (effectiveTo && !Number.isNaN(effectiveFrom.getTime()) && effectiveTo.getTime() <= effectiveFrom.getTime()) {
    errors.effectiveTo = '失效时间必须晚于生效时间。'
  }

  const missingCost = relevantCostFields(form.modelType).some(
    (key) => String(form[key]).trim() === '',
  )
  if (missingCost && !form.confirmNoCost) {
    errors.confirmNoCost = '缺少成本价，需勾选确认按无成本价保存（不参与毛利地板校验）。'
  }

  if (Object.keys(errors).length > 0) {
    return { errors, payload: null }
  }

  const payload: PriceWriteInput = {
    in_price_micro_per_k: inPrice.value ?? 0,
    out_price_micro_per_k: outPrice.value ?? 0,
    per_call_price_micro: perCall.value ?? 0,
    currency_cost: currency,
    effective_from: effectiveFrom.toISOString(),
    enabled: form.enabled,
  }
  const cleared = new Set(form.clearFields)
  if (creating && modelId !== null) payload.model_id = modelId
  // A cleared field must be absent from the body; it travels in clear_fields
  // instead. Sending both is a backend 400.
  if (!cleared.has('cache_hit_ratio_bp') && cacheHit.value !== null) {
    payload.cache_hit_ratio_bp = cacheHit.value
  }
  if (!cleared.has('cache_write_ratio_bp') && cacheWrite.value !== null) {
    payload.cache_write_ratio_bp = cacheWrite.value
  }
  if (!cleared.has('cost_in_micro_per_k') && costIn.value !== null) {
    payload.cost_in_micro_per_k = costIn.value
  }
  if (!cleared.has('cost_out_micro_per_k') && costOut.value !== null) {
    payload.cost_out_micro_per_k = costOut.value
  }
  if (!cleared.has('cost_per_call_micro') && costCall.value !== null) {
    payload.cost_per_call_micro = costCall.value
  }
  if (longFilled === 3 && !cleared.has('long_ctx_threshold_tokens')) {
    payload.long_ctx_threshold_tokens = longThreshold.value
    payload.long_ctx_in_price_micro_per_k = longIn.value
    payload.long_ctx_out_price_micro_per_k = longOut.value
  }
  if (!cleared.has('effective_to') && effectiveTo) {
    payload.effective_to = effectiveTo.toISOString()
  }
  if (cleared.size > 0) payload.clear_fields = [...cleared]
  if (missingCost && form.confirmNoCost) payload.confirm_no_cost = true

  return { errors, payload }
}

function scopeLabel(mode: ScopeLabel): string {
  return mode === 'ALL' ? '全部分组' : '指定分组'
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

function ClearToggle({
  checked,
  onChange,
  label = '清除该字段',
}: {
  checked: boolean
  onChange: (checked: boolean) => void
  label?: string
}) {
  return (
    <label className="flex items-center gap-1.5 text-xs text-ink-secondary">
      <input
        type="checkbox"
        className="h-3.5 w-3.5 rounded border-border-strong text-danger focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
        checked={checked}
        onChange={(event) => onChange(event.target.checked)}
      />
      {label}
    </label>
  )
}

interface IntegrationDraft {
  enabled: boolean
  template: string
}

/** Minimal labelled switch: no shared Switch component exists in components/. */
function ToolSwitch({
  checked,
  label,
  onChange,
  disabled,
}: {
  checked: boolean
  label: string
  onChange: (checked: boolean) => void
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cn(
        'relative inline-flex h-5 w-9 items-center rounded-full transition-colors duration-fast focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-60',
        checked ? 'bg-primary' : 'bg-border-strong',
      )}
    >
      <span
        className={cn(
          'inline-block h-4 w-4 rounded-full bg-surface shadow transition-transform duration-fast',
          checked ? 'translate-x-4' : 'translate-x-0.5',
        )}
      />
    </button>
  )
}

export default function AdminPricingPage() {
  usePageTitle('倍率与定价')
  const toast = useToast()
  // Controlled so the integrations query stays disabled until its tab opens.
  const [activeTab, setActiveTab] = useState('prices')

  // -------------------------------------------------------------------------
  // price table filters
  // -------------------------------------------------------------------------
  const [modelIdFilter, setModelIdFilter] = useState('')
  const [typeFilter, setTypeFilter] = useState<TypeFilter>('ALL')
  const [enabledFilter, setEnabledFilter] = useState<EnabledFilter>('ALL')
  const [page, setPage] = useState(1)

  const modelIdParam = parsePositiveInteger(modelIdFilter)
  const priceList = usePrices({
    page,
    pageSize: PAGE_SIZE,
    modelId: modelIdParam ?? undefined,
    modelType: typeFilter === 'ALL' ? undefined : typeFilter,
    enabled: enabledFilter === 'ALL' ? undefined : enabledFilter,
    sort: 'effective_from',
    order: 'desc',
  })

  const groups = useGroups()
  const standardGroup = groups.data?.items.find((group) => group.code === 'standard')
  const standardRatioBp = standardGroup?.ratio_bp ?? NO_STANDARD_RATIO_BP

  const allModels = useModels({ page: 1, pageSize: 100 })

  const createPrice = useCreatePrice()
  const updatePrice = useUpdatePrice()
  const updateGroup = useUpdateGroup()

  // -------------------------------------------------------------------------
  // tool integrations (third tab)
  // -------------------------------------------------------------------------
  const integrations = useIntegrations(activeTab === 'integrations')
  const updateIntegration = useUpdateIntegration()
  const [integrationDrafts, setIntegrationDrafts] = useState<Record<string, IntegrationDraft>>({})
  const [integrationErrors, setIntegrationErrors] = useState<Record<string, string>>({})
  const [integrationConfirm, setIntegrationConfirm] = useState<AdminIntegration | null>(null)

  // Seed once per tool. Re-seeding on every refetch would overwrite an unsaved
  // edit whenever a background refetch lands.
  useEffect(() => {
    const items = integrations.data?.items
    if (!items) return
    setIntegrationDrafts((current) => {
      let changed = false
      const next = { ...current }
      for (const item of items) {
        if (next[item.tool_key]) continue
        next[item.tool_key] = {
          enabled: item.enabled,
          template: item.deep_link_template ?? '',
        }
        changed = true
      }
      return changed ? next : current
    })
  }, [integrations.data])

  const integrationDraftOf = (item: AdminIntegration): IntegrationDraft =>
    integrationDrafts[item.tool_key] ?? {
      enabled: item.enabled,
      template: item.deep_link_template ?? '',
    }

  const updateIntegrationDraft = (
    item: AdminIntegration,
    patch: Partial<IntegrationDraft>,
  ) => {
    setIntegrationDrafts((current) => ({
      ...current,
      [item.tool_key]: {
        ...(current[item.tool_key] ?? {
          enabled: item.enabled,
          template: item.deep_link_template ?? '',
        }),
        ...patch,
      },
    }))
    setIntegrationErrors((current) => {
      if (!current[item.tool_key]) return current
      const next = { ...current }
      delete next[item.tool_key]
      return next
    })
  }

  const requestIntegrationSave = (item: AdminIntegration) => {
    const draft = integrationDraftOf(item)
    // Same rule as the backend: enabling without a stored template would make
    // the site render an empty deep link.
    if (draft.enabled && draft.template.trim() === '') {
      setIntegrationErrors((current) => ({
        ...current,
        [item.tool_key]: `开启工具 ${item.tool_key} 前必须先配置深链模板`,
      }))
      return
    }
    setIntegrationConfirm(item)
  }

  const confirmIntegrationSave = async () => {
    const item = integrationConfirm
    if (!item) return
    const draft = integrationDraftOf(item)
    setIntegrationErrors((current) => {
      if (!current[item.tool_key]) return current
      const next = { ...current }
      delete next[item.tool_key]
      return next
    })
    try {
      const response = await updateIntegration.mutateAsync({
        toolKey: item.tool_key,
        input: {
          enabled: draft.enabled,
          deep_link_template: draft.template.trim(),
        },
      })
      setIntegrationDrafts((current) => ({
        ...current,
        [item.tool_key]: {
          enabled: response.integration.enabled,
          template: response.integration.deep_link_template ?? '',
        },
      }))
      setIntegrationConfirm(null)
      toast.success('工具集成已更新', item.display_name)
    } catch (error) {
      setIntegrationConfirm(null)
      // Render the backend message verbatim: it names the offending placeholder.
      const message = errorMessage(error)
      setIntegrationErrors((current) => ({ ...current, [item.tool_key]: message }))
      toast.error('工具集成保存失败', message)
    }
  }

  const [creating, setCreating] = useState(false)
  const [editingRule, setEditingRule] = useState<AdminPriceRule | null>(null)
  const priceDrawerOpen = creating || editingRule !== null

  const [form, setForm] = useState<PriceFormState>(emptyForm)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [saveError, setSaveError] = useState<string | null>(null)
  const [saveHint, setSaveHint] = useState<string | null>(null)
  const [highlightId, setHighlightId] = useState<number | null>(null)

  const [auditOpen, setAuditOpen] = useState(false)
  const [auditModule, setAuditModule] = useState<'PRICE' | 'GROUP'>('PRICE')
  const [auditTarget, setAuditTarget] = useState<{ type?: string; id?: string }>({})
  const [auditRefreshKey, setAuditRefreshKey] = useState(0)

  const openAudit = (
    module: 'PRICE' | 'GROUP',
    targetType?: string,
    targetId?: number | string,
  ) => {
    setAuditModule(module)
    setAuditTarget({
      type: targetType,
      id: targetId === undefined ? undefined : String(targetId),
    })
    setAuditRefreshKey((value) => value + 1)
    setAuditOpen(true)
  }

  const rulesModelId = creating
    ? parsePositiveInteger(form.modelId)
    : editingRule?.model_id ?? modelIdParam
  const rulesQuery = usePrices(
    { modelId: rulesModelId ?? undefined, page: 1, pageSize: 100 },
    rulesModelId !== null,
  )
  const modelRules = (rulesQuery.data?.items ?? []).filter(
    (rule) => rule.model_id === rulesModelId,
  )
  const activeRule = activePriceRule(modelRules)
  const direction: PriceDirection = resolvePriceDirection(activeRule, snapshotFromForm(form))
  const conflicts = findPriceConflicts(modelRules)

  const resetPriceDrawer = () => {
    setCreating(false)
    setEditingRule(null)
    setErrors({})
    setSaveError(null)
    setSaveHint(null)
  }

  const closePriceDrawer = () => {
    if (createPrice.isPending || updatePrice.isPending) return
    resetPriceDrawer()
  }

  const openCreate = () => {
    setEditingRule(null)
    setForm(emptyForm())
    setErrors({})
    setSaveError(null)
    setSaveHint(null)
    setCreating(true)
  }

  const openEdit = (rule: AdminPriceRule) => {
    setCreating(false)
    setForm(formFromRule(rule))
    setErrors({})
    setSaveError(null)
    setSaveHint(null)
    setEditingRule(rule)
  }

  const updateField = <K extends keyof PriceFormState>(key: K, value: PriceFormState[K]) => {
    setForm((current) => {
      const next: PriceFormState = { ...current, [key]: value }
      // Typing into a field un-clears it: the body must never carry a value and
      // the same field in clear_fields (the backend answers 400).
      const clearField = CLEAR_FIELD_BY_FORM_KEY[key]
      if (clearField) {
        next.clearFields = next.clearFields.filter((item) => item !== clearField)
      }
      if (key === 'longThreshold' || key === 'longIn' || key === 'longOut') {
        next.clearFields = next.clearFields.filter(
          (item) => !LONG_CTX_CLEAR_FIELDS.includes(item),
        )
      }
      return next
    })
    setErrors((current) => {
      if (!current[key]) return current
      const next = { ...current }
      delete next[key]
      return next
    })
  }

  const isCleared = (field: PriceClearField) => form.clearFields.includes(field)

  /**
   * Toggles an explicit NULL. The long-context tier is all-or-nothing on the
   * backend, so its three columns share one checkbox and move together.
   */
  const toggleClear = (field: PriceClearField, checked: boolean) => {
    const longField = LONG_CTX_CLEAR_FIELDS.includes(field)
    setForm((current) => {
      const next = new Set(current.clearFields)
      if (longField) {
        for (const item of LONG_CTX_CLEAR_FIELDS) {
          if (checked) next.add(item)
          else next.delete(item)
        }
      } else if (checked) {
        next.add(field)
      } else {
        next.delete(field)
      }
      const updated: PriceFormState = { ...current, clearFields: [...next] }
      if (checked) {
        switch (field) {
          case 'cache_hit_ratio_bp':
            updated.cacheHit = ''
            break
          case 'cache_write_ratio_bp':
            updated.cacheWrite = ''
            break
          case 'cost_in_micro_per_k':
            updated.costIn = ''
            break
          case 'cost_out_micro_per_k':
            updated.costOut = ''
            break
          case 'cost_per_call_micro':
            updated.costCall = ''
            break
          case 'effective_to':
            updated.effectiveTo = ''
            break
          default:
            if (longField) {
              updated.longThreshold = ''
              updated.longIn = ''
              updated.longOut = ''
            }
        }
      }
      return updated
    })
    setErrors((current) => {
      const next = { ...current }
      delete next.effectiveTo
      return next
    })
  }

  const handlePriceSave = async () => {
    if (!creating && !editingRule) return
    const result = validatePriceForm(form, creating, direction)
    setErrors(result.errors)
    setSaveError(null)
    setSaveHint(null)
    if (!result.payload) return
    try {
      const response = creating
        ? await createPrice.mutateAsync(result.payload)
        : await updatePrice.mutateAsync({
            id: editingRule?.id as number,
            input: result.payload,
          })
      toast.success(creating ? '价格规则已创建' : '价格规则已更新')
      resetPriceDrawer()
      openAudit('PRICE', 't_price_rule', response.price.id)
    } catch (error) {
      // A 409 price_conflict message already names both t_price_rule rows.
      // Render it verbatim; do not parse it for jump targets.
      setSaveError(errorMessage(error))
      setSaveHint(errorHint(error))
    }
  }

  const jumpToRule = (id: number) => {
    setHighlightId(id)
    const node = document.querySelector(`[data-testid="price-row-${id}"]`)
    if (node && typeof node.scrollIntoView === 'function') {
      node.scrollIntoView({ behavior: 'smooth', block: 'center' })
    }
    window.setTimeout(() => setHighlightId(null), 2_000)
  }

  // -------------------------------------------------------------------------
  // groups
  // -------------------------------------------------------------------------
  const [groupTarget, setGroupTarget] = useState<PriceGroup | null>(null)
  const [ratioBp, setRatioBp] = useState('')
  const [groupRemark, setGroupRemark] = useState('')
  const [groupError, setGroupError] = useState<string | null>(null)
  const [groupSync, setGroupSync] = useState<GroupKeySync | null>(null)
  // T31: warnings come back on a 200 (saved, some models unchecked) and are
  // mirrored on the 400 that demands confirm=true. Kept separate from
  // groupError so the two states never overwrite each other.
  const [groupWarnings, setGroupWarnings] = useState<string[]>([])
  const [groupNeedsConfirm, setGroupNeedsConfirm] = useState(false)
  const [internalConfirmOpen, setInternalConfirmOpen] = useState(false)
  const [confirmReason, setConfirmReason] = useState('')
  const [confirmReasonError, setConfirmReasonError] = useState<string | null>(null)

  const openGroup = (group: PriceGroup) => {
    setGroupTarget(group)
    setRatioBp(String(group.ratio_bp))
    setGroupRemark(group.remark)
    setGroupError(null)
    setGroupSync(null)
    setGroupWarnings([])
    setGroupNeedsConfirm(false)
    setInternalConfirmOpen(false)
    setConfirmReason('')
    setConfirmReasonError(null)
  }

  const closeGroup = () => {
    if (updateGroup.isPending) return
    setGroupTarget(null)
    setGroupError(null)
    setGroupSync(null)
    setGroupWarnings([])
    setGroupNeedsConfirm(false)
    setInternalConfirmOpen(false)
    setConfirmReason('')
    setConfirmReasonError(null)
  }

  /** Returns the validated ratio, or null after writing the field error. */
  const validateGroupRatio = (): { internal: boolean; ratio: number } | null => {
    if (!groupTarget) return null
    const internal = groupTarget.code === 'internal'
    const parsed = parseOptionalInteger(ratioBp)
    if (internal) {
      if (parsed.value !== 0) {
        setGroupError('internal 分组的倍率只能是 0（语义为不计费）。')
        return null
      }
      if (!groupRemark.trim()) {
        setGroupError('internal 分组的备注不能为空：需写明「不计费」语义。')
        return null
      }
      return { internal, ratio: 0 }
    }
    if (!parsed.ok || parsed.value === null || parsed.value <= 0) {
      setGroupError('分组倍率必须为大于 0 的整数（万分比）。')
      return null
    }
    if (parsed.value > 10_000) {
      setGroupError('分组倍率不能大于 10000（即 1.00）。')
      return null
    }
    return { internal, ratio: parsed.value }
  }

  const submitGroup = async (options: { confirm?: boolean; reason?: string } = {}) => {
    if (!groupTarget) return
    const validated = validateGroupRatio()
    if (!validated) return
    const { internal, ratio } = validated
    setGroupError(null)
    try {
      const response = await updateGroup.mutateAsync({
        id: groupTarget.id,
        input: internal
          ? {
              ratio_bp: 0,
              remark: groupRemark.trim(),
              // T31: internal always carries both; the backend rejects either
              // one missing, and the reason lands in t_admin_audit_log.reason.
              confirm: true,
              reason: (options.reason ?? '').trim(),
            }
          : {
              ratio_bp: ratio,
              ...(options.confirm ? { confirm: true } : {}),
            },
      })
      const warnings = response.warnings ?? []
      setGroupWarnings(warnings)
      setGroupNeedsConfirm(false)
      setGroupSync(response.key_sync)
      setInternalConfirmOpen(false)
      toast.success('分组倍率已更新')
      openAudit('GROUP', 't_price_group', groupTarget.id)
    } catch (error) {
      // The 400 names every model whose floor could not be checked. Show the
      // list verbatim and offer the same save with confirm=true; the operator
      // does not retype the form.
      const warnings = error instanceof ApiError ? error.warnings : []
      const message = errorMessage(error)
      if (!internal && warnings.length > 0 && message.includes('无成本价')) {
        setGroupWarnings(warnings)
        setGroupNeedsConfirm(true)
        setGroupError(message)
        return
      }
      setGroupWarnings([])
      setGroupNeedsConfirm(false)
      const hint = errorHint(error)
      setGroupError(hint ? `${message}（${hint}）` : message)
    }
  }

  const handleGroupSave = () => {
    if (!groupTarget) return
    setGroupError(null)
    const validated = validateGroupRatio()
    if (!validated) return
    if (validated.internal) {
      // T31 D-28 ②: no internal save without an explicit, audited reason.
      setConfirmReason('')
      setConfirmReasonError(null)
      setInternalConfirmOpen(true)
      return
    }
    void submitGroup()
  }

  const confirmInternalSave = () => {
    if (!confirmReason.trim()) {
      setConfirmReasonError('必须填写操作理由，审计日志会记录该内容。')
      return
    }
    void submitGroup({ confirm: true, reason: confirmReason.trim() })
  }

  // -------------------------------------------------------------------------
  // rendering
  // -------------------------------------------------------------------------
  const priceError = priceList.isError ? errorMessage(priceList.error) : null
  const priceHint = priceList.isError ? errorHint(priceList.error) : null

  const priceColumns: TableColumn<AdminPriceRule>[] = [
    {
      key: 'model',
      header: '模型',
      render: (rule) => (
        <div
          data-testid={`price-row-${rule.id}`}
          className="space-y-0.5"
        >
          <p className="font-medium text-ink">{rule.model_name}</p>
          <p className="text-xs text-ink-muted">
            #{rule.id} · {modelTypeLabel(rule.model_type)}
          </p>
        </div>
      ),
    },
    {
      key: 'type',
      header: '类型',
      render: (rule) => <Badge variant="primary">{modelTypeLabel(rule.model_type)}</Badge>,
    },
    {
      key: 'standard_price',
      header: '当前标准价',
      render: (rule) => (
        <div className="space-y-0.5 text-xs">
          <p className="tabular-nums text-ink">输入 {formatPricePerK(rule.in_price_micro_per_k)}</p>
          <p className="tabular-nums text-ink">输出 {formatPricePerK(rule.out_price_micro_per_k)}</p>
          <p className="tabular-nums text-ink-muted">
            按次 {formatCreditAmount(rule.per_call_price_micro)} 积分/次
          </p>
        </div>
      ),
    },
    {
      key: 'cost',
      header: '成本价（运营可见）',
      render: (rule) => (
        <div className="space-y-0.5 text-xs text-ink-secondary">
          <p>
            输入 {rule.cost_in_micro_per_k === null || rule.cost_in_micro_per_k === undefined
              ? '—'
              : formatPricePerK(rule.cost_in_micro_per_k)}
          </p>
          <p>
            输出 {rule.cost_out_micro_per_k === null || rule.cost_out_micro_per_k === undefined
              ? '—'
              : formatPricePerK(rule.cost_out_micro_per_k)}
          </p>
          <p>
            按次 {rule.cost_per_call_micro === null || rule.cost_per_call_micro === undefined
              ? '—'
              : `${formatCreditAmount(rule.cost_per_call_micro)} 积分/次`}
          </p>
        </div>
      ),
    },
    {
      key: 'version',
      header: '版本',
      align: 'right',
      render: (rule) => <span className="tabular-nums">v{rule.version}</span>,
    },
    {
      key: 'effective_from',
      header: '生效起',
      render: (rule) => (
        <span className="text-xs text-ink-secondary">{formatDateTime(rule.effective_from)}</span>
      ),
    },
    {
      key: 'effective_to',
      header: '生效止',
      render: (rule) =>
        rule.effective_to ? (
          <span className="text-xs text-ink-secondary">{formatDateTime(rule.effective_to)}</span>
        ) : (
          <span className="text-xs text-ink-muted">不限</span>
        ),
    },
    {
      key: 'enabled',
      header: '状态',
      render: (rule) => (
        <Badge variant={rule.enabled ? 'success' : 'neutral'} dot>
          {rule.enabled ? '启用' : '停用'}
        </Badge>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (rule) => (
        <Button variant="ghost" size="sm" onClick={() => openEdit(rule)}>
          编辑
        </Button>
      ),
    },
  ]

  const groupColumns: TableColumn<PriceGroup>[] = [
    {
      key: 'code',
      header: '分组',
      render: (group) => (
        <div className="space-y-0.5">
          <p className="font-medium text-ink">{group.name}</p>
          <code className="text-xs text-ink-secondary">{group.code}</code>
        </div>
      ),
    },
    {
      key: 'ratio',
      header: '倍率',
      render: (group) =>
        group.ratio_bp === 0 ? (
          <div className="space-y-0.5">
            <Badge variant="info">不计费</Badge>
            <p className="text-xs text-ink-muted">倍率 0 表示不参与结算，并非折扣。</p>
          </div>
        ) : (
          <div className="space-y-0.5">
            <span className="tabular-nums text-ink">{formatRatioBp(group.ratio_bp)}</span>
            <p className="text-xs text-ink-muted">万分比 {group.ratio_bp}</p>
          </div>
        ),
    },
    {
      key: 'scope',
      header: '范围',
      render: (group) => (
        <span className="text-xs text-ink-secondary">
          模型：{scopeLabel(group.model_scope_mode)} · 渠道：{scopeLabel(group.channel_scope_mode)}
        </span>
      ),
    },
    {
      key: 'self_service',
      header: '自助开通',
      render: (group) => (group.self_service ? '是' : '否'),
    },
    {
      key: 'enabled',
      header: '状态',
      render: (group) => (
        <Badge variant={group.enabled ? 'success' : 'neutral'} dot>
          {group.enabled ? '启用' : '停用'}
        </Badge>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (group) => (
        <Button variant="ghost" size="sm" onClick={() => openGroup(group)}>
          编辑倍率
        </Button>
      ),
    },
  ]

  const integrationColumns: TableColumn<AdminIntegration>[] = [
    {
      key: 'tool',
      header: '工具',
      render: (item) => (
        <div className="space-y-0.5">
          <p className="font-medium text-ink">{item.display_name}</p>
          <code className="text-xs text-ink-secondary">{item.tool_key}</code>
        </div>
      ),
    },
    {
      key: 'enabled',
      header: '启用',
      render: (item) => {
        const draft = integrationDraftOf(item)
        return (
          <ToolSwitch
            checked={draft.enabled}
            label={`启用 ${item.display_name}`}
            disabled={updateIntegration.isPending}
            onChange={(checked) => updateIntegrationDraft(item, { enabled: checked })}
          />
        )
      },
    },
    {
      key: 'template',
      header: '深链模板',
      render: (item) => {
        const draft = integrationDraftOf(item)
        const error = integrationErrors[item.tool_key]
        return (
          <div className="max-w-xl space-y-1">
            <Input
              aria-label={`深链模板 ${item.display_name}`}
              value={draft.template}
              placeholder={'https://example.com/import?key={key}&model={model}'}
              onChange={(event) =>
                updateIntegrationDraft(item, { template: event.target.value })
              }
            />
            {error ? (
              <p
                role="alert"
                data-testid={`integration-error-${item.tool_key}`}
                className="rounded-lg border border-danger bg-danger-soft px-3 py-2 text-xs text-danger"
              >
                {error}
              </p>
            ) : null}
          </div>
        )
      },
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (item) => (
        <Button
          size="sm"
          aria-label={`保存 ${item.display_name}`}
          disabled={updateIntegration.isPending}
          onClick={() => requestIntegrationSave(item)}
        >
          保存
        </Button>
      ),
    },
  ]

  const groupInternal = groupTarget?.code === 'internal'
  const savingPrice = createPrice.isPending || updatePrice.isPending
  const missingCostPreview = relevantCostFields(form.modelType).some(
    (key) => String(form[key]).trim() === '',
  )
  const floorPrice =
    form.modelType === 'IMAGE'
      ? parseOptionalInteger(form.perCall).value ?? 0
      : parseOptionalInteger(form.inPrice).value ?? 0

  return (
    <div className="space-y-5">
      <PageHeader
        title="倍率与定价"
        description="模型单价写入后即时刷新价格快照；涨价需 7 天公示，毛利地板由后端复核。"
      />

      <Tabs
        ariaLabel="定价分区"
        value={activeTab}
        onChange={setActiveTab}
        items={[
          {
            id: 'prices',
            label: '模型单价',
            content: (
              <div className="space-y-4">
                <div className="flex flex-wrap items-end justify-between gap-3">
                  <div className="grid gap-3 sm:grid-cols-3">
                    <Input
                      label="模型编号"
                      inputMode="numeric"
                      value={modelIdFilter}
                      placeholder="按模型编号筛选"
                      error={
                        modelIdFilter.trim() && modelIdParam === null
                          ? '模型编号需为正整数。'
                          : undefined
                      }
                      onChange={(event) => {
                        setModelIdFilter(event.target.value)
                        setPage(1)
                      }}
                    />
                    <Select
                      label="模型类型"
                      value={typeFilter}
                      onChange={(event) => {
                        setTypeFilter(event.target.value as TypeFilter)
                        setPage(1)
                      }}
                      options={[
                        { value: 'ALL', label: '全部类型' },
                        { value: 'TEXT', label: modelTypeLabel('TEXT') },
                        { value: 'IMAGE', label: modelTypeLabel('IMAGE') },
                        { value: 'EMBEDDING', label: modelTypeLabel('EMBEDDING') },
                      ]}
                    />
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
                        { value: '0', label: '停用' },
                      ]}
                    />
                  </div>
                  <div className="flex flex-wrap gap-2">
                    <Button
                      variant="secondary"
                      onClick={() => openAudit('PRICE')}
                    >
                      审计记录
                    </Button>
                    <Button onClick={openCreate}>新建价格</Button>
                  </div>
                </div>

                {priceHint ? (
                  <p role="status" className="rounded-lg border border-danger bg-danger-soft px-4 py-2 text-xs text-danger">
                    {priceHint}
                  </p>
                ) : null}

                {conflicts.length > 0 ? (
                  <div
                    data-testid="price-conflict-banner"
                    role="alert"
                    className="space-y-2 rounded-xl border border-danger bg-danger-soft p-4"
                  >
                    <p className="text-sm font-semibold text-danger">
                      同一模型存在时间窗重叠且价格不一致的规则
                    </p>
                    <p className="text-xs text-ink-secondary">
                      判定依据：两张规则均启用、生效窗口重叠、价格 signature（含成本与缓存维度）不同。后端保存时会返回 409 price_conflict。
                    </p>
                    <ul className="space-y-2">
                      {conflicts.map((pair) => (
                        <li
                          key={`${pair.first.id}-${pair.second.id}`}
                          className="rounded-lg border border-danger bg-surface p-3 text-xs"
                        >
                          <div className="flex flex-wrap items-center gap-2">
                            <span className="font-medium text-ink">
                              t_price_rule#{pair.first.id}
                            </span>
                            <span className="text-ink-secondary">
                              {formatDateTime(pair.first.effective_from)} ~{' '}
                              {pair.first.effective_to
                                ? formatDateTime(pair.first.effective_to)
                                : '不限'}
                            </span>
                            <Button
                              variant="secondary"
                              size="sm"
                              onClick={() => jumpToRule(pair.first.id)}
                            >
                              跳到该行
                            </Button>
                          </div>
                          <div className="mt-2 flex flex-wrap items-center gap-2">
                            <span className="font-medium text-ink">
                              t_price_rule#{pair.second.id}
                            </span>
                            <span className="text-ink-secondary">
                              {formatDateTime(pair.second.effective_from)} ~{' '}
                              {pair.second.effective_to
                                ? formatDateTime(pair.second.effective_to)
                                : '不限'}
                            </span>
                            <Button
                              variant="secondary"
                              size="sm"
                              onClick={() => jumpToRule(pair.second.id)}
                            >
                              跳到该行
                            </Button>
                          </div>
                        </li>
                      ))}
                    </ul>
                  </div>
                ) : null}

                <Table
                  ariaLabel="模型价格规则列表"
                  columns={priceColumns}
                  rows={priceList.data?.items ?? []}
                  rowKey={(rule) => String(rule.id)}
                  loading={priceList.isLoading}
                  error={priceError}
                  onRetry={() => void priceList.refetch()}
                  rowClassName={(rule) =>
                    rule.id === highlightId ? 'bg-warning-soft' : undefined
                  }
                  emptyTitle="暂无价格规则"
                  emptyDescription={
                    modelIdFilter || typeFilter !== 'ALL' || enabledFilter !== 'ALL'
                      ? '当前筛选条件没有匹配的价格规则。'
                      : '为模型创建第一条生效价格后才能上架。'
                  }
                  pagination={
                    priceList.data
                      ? {
                          page: priceList.data.page,
                          pageSize: priceList.data.page_size,
                          total: priceList.data.total,
                          onPageChange: setPage,
                        }
                      : undefined
                  }
                />
              </div>
            ),
          },
          {
            id: 'groups',
            label: '分组倍率',
            content: (
              <div className="space-y-4">
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <p className="text-xs text-ink-secondary">
                    倍率以万分比存储，8500 表示 0.85；internal 分组固定为 0，语义为不计费。
                  </p>
                  <Button variant="secondary" onClick={() => openAudit('GROUP')}>
                    审计记录
                  </Button>
                </div>
                {groups.isError ? (
                  <ErrorState
                    message={errorMessage(groups.error)}
                    hint={errorHint(groups.error)}
                    onRetry={() => void groups.refetch()}
                  />
                ) : (
                  <Table
                    ariaLabel="分组倍率列表"
                    columns={groupColumns}
                    rows={groups.data?.items ?? []}
                    rowKey={(group) => String(group.id)}
                    loading={groups.isLoading}
                    emptyTitle="暂无分组"
                    emptyDescription="后端尚未返回任何价格分组。"
                  />
                )}
              </div>
            ),
          },
          {
            id: 'integrations',
            label: '工具集成',
            content: (
              <div className="space-y-4">
                <p className="text-xs text-ink-secondary">
                  5 个工具由产品固定，运营只能修改开关与深链模板；模板保存在后端，公开端点只返回已启用项。
                </p>
                {integrations.isError ? (
                  <ErrorState
                    message={errorMessage(integrations.error)}
                    hint={errorHint(integrations.error)}
                    onRetry={() => void integrations.refetch()}
                  />
                ) : (
                  <Table
                    ariaLabel="工具集成列表"
                    columns={integrationColumns}
                    rows={integrations.data?.items ?? []}
                    rowKey={(item) => item.tool_key}
                    loading={integrations.isLoading}
                    emptyTitle="暂无工具"
                    emptyDescription="后端尚未返回任何工具集成配置。"
                  />
                )}
                <p className="text-xs text-ink-muted">
                  深链模板只支持 {'{key}'} / {'{base_url}'} / {'{model}'} 三个占位符，其它占位符后端会拒绝保存；模板由后端持有，浏览器不会拿到真实 Key。开启前必须先保存非空模板。
                </p>
              </div>
            ),
          },
        ]}
      />

      <Drawer
        open={priceDrawerOpen}
        onClose={closePriceDrawer}
        title={creating ? '新建价格规则' : '编辑价格规则'}
        description="价格单位为微积分；所有价格与成本字段保存前按模型类型校验。"
        size="xl"
        footer={
          <>
            <Button variant="secondary" onClick={closePriceDrawer} disabled={savingPrice}>
              取消
            </Button>
            <Button onClick={() => void handlePriceSave()} loading={savingPrice}>
              保存
            </Button>
          </>
        }
      >
        <div className="space-y-5">
          {saveError ? (
            <div role="alert" className="space-y-1 rounded-lg border border-danger bg-danger-soft px-3 py-2">
              <p className="text-xs text-danger">{saveError}</p>
              {saveHint ? <p className="text-xs text-ink-muted">{saveHint}</p> : null}
            </div>
          ) : null}

          {creating ? (
            <Select
              label="模型"
              required
              value={form.modelId}
              error={errors.modelId}
              placeholder="请选择模型"
              onChange={(event) => {
                const selected = allModels.data?.items.find(
                  (model) => String(model.id) === event.target.value,
                )
                updateField('modelId', event.target.value)
                if (selected) updateField('modelType', selected.model_type)
              }}
              options={(allModels.data?.items ?? []).map((model) => ({
                value: String(model.id),
                label: `${model.display_name}（${model.model_name}）`,
              }))}
            />
          ) : (
            <p className="text-sm text-ink-secondary">
              模型：<span className="font-medium text-ink">{editingRule?.model_name}</span>
              （#{editingRule?.model_id} · {modelTypeLabel(form.modelType)}）
            </p>
          )}

          <div className="grid gap-4 sm:grid-cols-3">
            <Input
              label="输入单价（微积分/千 token）"
              inputMode="numeric"
              value={form.inPrice}
              error={errors.inPrice}
              onChange={(event) => updateField('inPrice', event.target.value)}
            />
            <Input
              label="输出单价（微积分/千 token）"
              inputMode="numeric"
              value={form.outPrice}
              error={errors.outPrice}
              onChange={(event) => updateField('outPrice', event.target.value)}
            />
            <Input
              label="按次单价（微积分/次）"
              inputMode="numeric"
              value={form.perCall}
              error={errors.perCall}
              onChange={(event) => updateField('perCall', event.target.value)}
            />
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1">
              <Input
                label="缓存命中倍率（万分比）"
                inputMode="numeric"
                value={form.cacheHit}
                error={errors.cacheHit}
                hint="0 表示不打折；留空 = 不修改，勾选「清除」才会置空。"
                onChange={(event) => updateField('cacheHit', event.target.value)}
              />
              {!creating ? (
                <ClearToggle
                  label="清除缓存命中倍率"
                  checked={isCleared('cache_hit_ratio_bp')}
                  onChange={(checked) => toggleClear('cache_hit_ratio_bp', checked)}
                />
              ) : null}
            </div>
            <div className="space-y-1">
              <Input
                label="缓存写入倍率（万分比）"
                inputMode="numeric"
                value={form.cacheWrite}
                error={errors.cacheWrite}
                hint="0 表示不适用；留空 = 不修改，勾选「清除」才会置空。"
                onChange={(event) => updateField('cacheWrite', event.target.value)}
              />
              {!creating ? (
                <ClearToggle
                  label="清除缓存写入倍率"
                  checked={isCleared('cache_write_ratio_bp')}
                  onChange={(checked) => toggleClear('cache_write_ratio_bp', checked)}
                />
              ) : null}
            </div>
          </div>

          <fieldset className="space-y-3">
            <legend className="text-sm font-medium text-ink">长上下文阶梯（三项同时填写或全部留空）</legend>
            {!creating ? (
              <ClearToggle
                label="清除长上下文阶梯（三项一起置空）"
                checked={isCleared('long_ctx_threshold_tokens')}
                onChange={(checked) =>
                  toggleClear('long_ctx_threshold_tokens', checked)
                }
              />
            ) : null}
            <div className="grid gap-4 sm:grid-cols-3">
              <Input
                label="阈值（tokens）"
                inputMode="numeric"
                value={form.longThreshold}
                error={errors.longThreshold}
                onChange={(event) => updateField('longThreshold', event.target.value)}
              />
              <Input
                label="阶梯输入价（微积分/千 token）"
                inputMode="numeric"
                value={form.longIn}
                error={errors.longIn}
                onChange={(event) => updateField('longIn', event.target.value)}
              />
              <Input
                label="阶梯输出价（微积分/千 token）"
                inputMode="numeric"
                value={form.longOut}
                error={errors.longOut}
                onChange={(event) => updateField('longOut', event.target.value)}
              />
            </div>
          </fieldset>

          <fieldset className="space-y-3">
            <legend className="text-sm font-medium text-ink">上游成本价（仅运营可见）</legend>
            <div className="grid gap-4 sm:grid-cols-3">
              <div className="space-y-1">
                <Input
                  label="输入成本价"
                  inputMode="numeric"
                  value={form.costIn}
                  error={errors.costIn}
                  onChange={(event) => updateField('costIn', event.target.value)}
                />
                {!creating ? (
                  <ClearToggle
                    label="清除输入成本价"
                    checked={isCleared('cost_in_micro_per_k')}
                    onChange={(checked) => toggleClear('cost_in_micro_per_k', checked)}
                  />
                ) : null}
              </div>
              <div className="space-y-1">
                <Input
                  label="输出成本价"
                  inputMode="numeric"
                  value={form.costOut}
                  error={errors.costOut}
                  onChange={(event) => updateField('costOut', event.target.value)}
                />
                {!creating ? (
                  <ClearToggle
                    label="清除输出成本价"
                    checked={isCleared('cost_out_micro_per_k')}
                    onChange={(checked) => toggleClear('cost_out_micro_per_k', checked)}
                  />
                ) : null}
              </div>
              <div className="space-y-1">
                <Input
                  label="按次成本价"
                  inputMode="numeric"
                  value={form.costCall}
                  error={errors.costCall}
                  onChange={(event) => updateField('costCall', event.target.value)}
                />
                {!creating ? (
                  <ClearToggle
                    label="清除按次成本价"
                    checked={isCleared('cost_per_call_micro')}
                    onChange={(checked) => toggleClear('cost_per_call_micro', checked)}
                  />
                ) : null}
              </div>
            </div>
            <p className="text-xs text-ink-muted">
              留空 = 不修改；勾选「清除」会把后端已有值置空。清除成本价仍需确认按无成本价保存。
            </p>
            {missingCostPreview ? (
              <>
                <CheckboxField
                  label="确认按无成本价保存（不参与毛利地板校验）"
                  description="不计费需要单独勾选确认，这条确认会写入审计记录。"
                  checked={form.confirmNoCost}
                  onChange={(checked) => updateField('confirmNoCost', checked)}
                />
                {errors.confirmNoCost ? (
                  <p className="text-xs text-danger">{errors.confirmNoCost}</p>
                ) : null}
              </>
            ) : null}
          </fieldset>

          <div className="grid gap-4 sm:grid-cols-2">
            <Input
              label="成本币种"
              value={form.currency}
              error={errors.currency}
              hint="当前只支持人民币。"
              onChange={(event) => updateField('currency', event.target.value)}
            />
            <div className="space-y-1">
              <Input
                label="生效止（可选）"
                type="datetime-local"
                value={
                  form.effectiveTo ? toLocalDateTimeValue(new Date(form.effectiveTo)) : ''
                }
                error={errors.effectiveTo}
                hint="留空 = 不修改；勾选「清除」表示长期有效。"
                onChange={(event) => {
                  const value = event.target.value
                  updateField('effectiveTo', value ? new Date(value).toISOString() : '')
                }}
              />
              {!creating ? (
                <ClearToggle
                  label="清除生效止"
                  checked={isCleared('effective_to')}
                  onChange={(checked) => toggleClear('effective_to', checked)}
                />
              ) : null}
            </div>
          </div>

          <EffectiveTimePicker
            direction={direction}
            value={form.effectiveFrom}
            onChange={(iso) => updateField('effectiveFrom', iso)}
          />
          {errors.effectiveFrom ? (
            <p className="text-xs text-danger">{errors.effectiveFrom}</p>
          ) : null}
          {!creating && editingRule ? (
            <p className="text-xs text-ink-muted">
              原规则生效起：{formatDateTime(editingRule.effective_from)}。编辑历史规则时默认从当前时间重新生效，因为后端会把过去的 effective_from 钳制为服务器当前时间。
            </p>
          ) : null}

          <div className="space-y-3 rounded-lg border border-border bg-surface-muted p-3">
            <p className="text-sm font-medium text-ink">毛利地板校验（标准分组）</p>
            {standardGroup ? (
              <p className="text-xs text-ink-muted">
                标准分组 {standardGroup.code} 倍率 {formatRatioBp(standardGroup.ratio_bp)}
                （{standardGroup.ratio_bp} 万分比）
              </p>
            ) : (
              <p className="text-xs text-warning">
                {groups.isError
                  ? '标准分组加载失败，按标准倍率 1.00 估算。'
                  : '未找到 standard 分组，按标准倍率 1.00 估算。'}
              </p>
            )}
            {form.modelType === 'IMAGE' ? (
              <p className="text-xs text-ink-muted">
                图像模型按按次价格与按次成本校验；组件参数使用 per_call 与 cost_per_call。
              </p>
            ) : null}
            {floorPrice > 0 ? (
              <FloorCheckHint
                priceMicroPerK={floorPrice}
                groupRatioBp={standardRatioBp}
                costMicroPerK={
                  form.modelType === 'IMAGE'
                    ? parseOptionalInteger(form.costCall).value
                    : parseOptionalInteger(form.costIn).value
                }
                unit={form.modelType === 'IMAGE' ? '积分/次' : '积分/千 token'}
              />
            ) : (
              <p className="text-xs text-ink-muted">填写单价后显示毛利地板校验结果。</p>
            )}
          </div>

          <CheckboxField
            label="启用该价格规则"
            checked={form.enabled}
            onChange={(checked) => updateField('enabled', checked)}
          />
        </div>
      </Drawer>

      <Drawer
        open={groupTarget !== null}
        onClose={closeGroup}
        title="编辑分组倍率"
        description="保存后会把新倍率同步到该分组下所有密钥；同步结果显示在下方。"
        size="md"
        footer={
          <>
            <Button variant="secondary" onClick={closeGroup} disabled={updateGroup.isPending}>
              取消
            </Button>
            <Button onClick={() => void handleGroupSave()} loading={updateGroup.isPending}>
              保存
            </Button>
          </>
        }
      >
        {groupTarget ? (
          <div className="space-y-4">
            <p className="text-sm text-ink-secondary">
              分组：<span className="font-medium text-ink">{groupTarget.name}</span>
              （{groupTarget.code}）
            </p>
            <Input
              label="倍率（万分比）"
              inputMode="numeric"
              value={ratioBp}
              disabled={groupInternal}
              hint={
                groupInternal
                  ? '内部渠道固定为 0：表示不计费。'
                  : '8500 表示 0.85；必须大于 0 且不超过 10000。'
              }
              onChange={(event) => setRatioBp(event.target.value)}
            />
            {groupInternal ? (
              <Input
                label="备注（内部渠道必填）"
                value={groupRemark}
                onChange={(event) => setGroupRemark(event.target.value)}
                hint="需写明不计费语义，后端会拒绝空备注。"
              />
            ) : null}
            {groupError ? (
              <p
                role="alert"
                data-testid="group-save-error"
                className="rounded-lg border border-danger bg-danger-soft px-3 py-2 text-xs text-danger"
              >
                {groupError}
              </p>
            ) : null}
            {groupNeedsConfirm ? (
              <div
                role="alert"
                data-testid="group-warning-confirm"
                className="space-y-2 rounded-lg border border-warning bg-warning-soft px-3 py-2 text-xs text-warning"
              >
                <p className="font-medium">以下模型缺成本价，毛利地板无法校验：</p>
                <ul
                  data-testid="group-warning-list"
                  className="list-disc space-y-0.5 pl-5 text-ink-secondary"
                >
                  {groupWarnings.map((warning) => (
                    <li key={warning}>{warning}</li>
                  ))}
                </ul>
                <Button
                  size="sm"
                  variant="danger"
                  loading={updateGroup.isPending}
                  aria-label="仍要保存分组倍率"
                  onClick={() => void submitGroup({ confirm: true })}
                >
                  仍要保存
                </Button>
              </div>
            ) : groupWarnings.length > 0 ? (
              <div
                data-testid="group-save-warnings"
                className="space-y-1 rounded-lg border border-warning bg-warning-soft px-3 py-2 text-xs text-warning"
              >
                <p className="font-medium">已保存，但以下模型的毛利地板未校验：</p>
                <ul className="list-disc space-y-0.5 pl-5 text-ink-secondary">
                  {groupWarnings.map((warning) => (
                    <li key={warning}>{warning}</li>
                  ))}
                </ul>
              </div>
            ) : null}
            {groupSync ? (
              <div className="space-y-1 rounded-lg border border-border bg-surface-muted p-3 text-xs">
                <p className="font-medium text-ink">密钥同步结果</p>
                <p className="text-ink-secondary">
                  影响密钥数：{formatInteger(groupSync.keys)} · 结果：
                  {groupSync.ok ? '成功' : '失败'}（后端异步写穿）
                </p>
                {groupSync.note ? <p className="text-ink-muted">{groupSync.note}</p> : null}
                {groupSync.keys === 0 && groupSync.ok ? (
                  <p className="text-ink-muted">该分组下没有需要同步的密钥。</p>
                ) : null}
              </div>
            ) : null}
          </div>
        ) : null}
      </Drawer>

      <Modal
        open={internalConfirmOpen}
        onClose={() => {
          if (updateGroup.isPending) return
          setInternalConfirmOpen(false)
        }}
        title="确认修改内部分组"
        description="内部渠道的倍率 0 表示不计费。保存须二次确认并填写理由，理由会写入审计日志。"
        size="sm"
        footer={
          <>
            <Button
              variant="secondary"
              onClick={() => setInternalConfirmOpen(false)}
              disabled={updateGroup.isPending}
            >
              取消
            </Button>
            <Button
              onClick={confirmInternalSave}
              loading={updateGroup.isPending}
              disabled={!confirmReason.trim()}
              aria-label="确认保存内部分组"
            >
              确认保存
            </Button>
          </>
        }
      >
        <Input
          label="操作理由（必填）"
          required
          value={confirmReason}
          error={confirmReasonError ?? undefined}
          onChange={(event) => {
            setConfirmReason(event.target.value)
            setConfirmReasonError(null)
          }}
          hint="例：内部压测临时关闭计费，工单 OPS-123。理由为空时无法提交。"
        />
      </Modal>

      <Modal
        open={integrationConfirm !== null}
        onClose={() => {
          if (updateIntegration.isPending) return
          setIntegrationConfirm(null)
        }}
        title="确认保存工具集成"
        description="保存后立即影响站点展示，并写入审计日志。"
        size="sm"
        footer={
          <>
            <Button
              variant="secondary"
              onClick={() => setIntegrationConfirm(null)}
              disabled={updateIntegration.isPending}
            >
              取消
            </Button>
            <Button
              onClick={() => void confirmIntegrationSave()}
              loading={updateIntegration.isPending}
              aria-label="确认保存工具集成"
            >
              确认保存
            </Button>
          </>
        }
      >
        {integrationConfirm ? (
          <div className="space-y-1 text-sm text-ink-secondary">
            <p>
              工具：
              <span className="font-medium text-ink">{integrationConfirm.display_name}</span>
              （<code className="text-xs">{integrationConfirm.tool_key}</code>）
            </p>
            <p>开关：{integrationDraftOf(integrationConfirm).enabled ? '开启' : '关闭'}</p>
            <p className="break-all text-xs text-ink-muted">
              模板：
              {integrationDraftOf(integrationConfirm).template.trim() || '（空，保存后清除）'}
            </p>
          </div>
        ) : null}
      </Modal>

      <AuditTrail
        open={auditOpen}
        onClose={() => setAuditOpen(false)}
        module={auditModule}
        targetType={auditTarget.type}
        targetId={auditTarget.id}
        refreshKey={auditRefreshKey}
      />
    </div>
  )
}

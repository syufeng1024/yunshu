import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { errorHint, errorMessage } from '@/api/client'
import { useAdminConfig, useUpdateAdminConfig } from '@/api/admin'
import type { ConfigItem, ConfigUpdateResponse } from '@/api/admin'
import {
  Badge,
  Button,
  ErrorState,
  Icon,
  PageHeader,
  Select,
  Skeleton,
  useToast,
} from '@/components'
import { configFieldMeta } from '@/lib/configKeys'
import { CONFIG_NAV, CONFIG_NAV_ITEMS, CONFIG_NAV_ITEM_OF_KEY, firstItemWithKeys } from '@/lib/configNav'
import { cn } from '@/lib/cn'
import { usePageTitle } from '@/lib/usePageTitle'

/**
 * 「系统管理」= 45 个配置键的在线维护页（对齐 new-api 的
 * /system-settings/site/system-info 形态：左侧页内分组导航 + 右侧表单，
 * 右上角「重置 / 保存更改」）。
 *
 * 三条设计上的硬约束：
 *
 * 1. **控件类型来自服务端**。首屏用 `value` 的 JSON 类型决定是单行输入、
 *    数字输入、开关还是 JSON 文本框，前端不维护「键 → 类型」表——键表会变，
 *    硬编码必然漂移。
 * 2. **只提交改动过的键**。与加载时的原始值深比较，没改就禁用保存，绝不发
 *    空 PUT（服务端也拒收空 `values`）。
 * 3. **不伪造后端没有的能力**。`runtime_writable === false` 的键在
 *    `PUT /api/admin/config` 上会被 400 `config_value_invalid` 拒绝（只有 12
 *    个键在运行时白名单内），所以这 33 个键一律**只读呈现**：`readonly` 的
 *    输入框 / 文本框（值仍可选中复制，核对日志与文档时用得上）、`disabled` 的
 *    开关，配「需重启生效」Badge。不给"能按但按了存不进去"的假开关，也不做
 *    "编辑完再拦下来"的二次伤害——只读就不会产生脏值，`canSave` 也就不会被
 *    锁定键影响（33/45 个键误碰一个就把 12 个可写键一起锁死，比只读糟得多）。
 */

// ---------------------------------------------------------------------------
// 编辑器状态
// ---------------------------------------------------------------------------

/** 由 `value` 的 JSON 类型推导出的控件类型。 */
type EditorKind = 'string' | 'number' | 'boolean' | 'json'

interface DraftField {
  key: string
  kind: EditorKind
  /** 加载时的原始值，用于深比较判断「改没改」以及「重置」。 */
  original: unknown
  /** string / number / json 的编辑态文本。 */
  text: string
  /** boolean 的编辑态。 */
  checked: boolean
  /** 本地格式错误（如 JSON 非法）；null 表示当前文本可解析。 */
  error: string | null
}

type ParsedDraft = { ok: true; value: unknown } | { ok: false; error: string }

/**
 * 值 → 控件类型。
 *
 * 字符串里带换行的（配置表里目前没有，但不要假设永远没有）走 JSON 编辑器：
 * `<input>` 会把 CR/LF 从 value 里吃掉，用文本框读只能静默改写用户的值。
 */
function editorKindOf(value: unknown): EditorKind {
  if (typeof value === 'boolean') return 'boolean'
  if (typeof value === 'number') return 'number'
  if (typeof value === 'string') return value.includes('\n') ? 'json' : 'string'
  return 'json'
}

function formatJson(value: unknown): string {
  const pretty = JSON.stringify(value, null, 2)
  return pretty === undefined ? String(value) : pretty
}

function buildField(item: ConfigItem): DraftField {
  const kind = editorKindOf(item.value)
  return {
    key: item.key,
    kind,
    original: item.value,
    text:
      kind === 'boolean'
        ? ''
        : kind === 'json'
          ? formatJson(item.value)
          : String(item.value),
    checked: kind === 'boolean' ? item.value === true : false,
    error: null,
  }
}

function buildDrafts(items: readonly ConfigItem[]): Record<string, DraftField> {
  const drafts: Record<string, DraftField> = {}
  for (const item of items) drafts[item.key] = buildField(item)
  return drafts
}

/** 编辑态 → 待提交的值。数字保持 number，开关保持 boolean，对象保持原结构。 */
function parseDraft(field: DraftField): ParsedDraft {
  switch (field.kind) {
    case 'boolean':
      return { ok: true, value: field.checked }
    case 'string':
      return { ok: true, value: field.text }
    case 'number': {
      const text = field.text.trim()
      if (text === '') return { ok: false, error: '请填写数字' }
      const parsed = Number(text)
      if (!Number.isFinite(parsed)) return { ok: false, error: '请填写合法的数字' }
      return { ok: true, value: parsed }
    }
    case 'json':
    default:
      try {
        return { ok: true, value: JSON.parse(field.text) as unknown }
      } catch {
        return { ok: false, error: '格式不正确：请检查引号、逗号与括号是否配对' }
      }
  }
}

/** 深度按值比较，用于「改没改」。JSON 值只可能是 null/bool/number/string/array/object。 */
function sameValue(left: unknown, right: unknown): boolean {
  if (left === right) return true
  if (left === null || right === null) return false
  if (typeof left !== typeof right) return false
  if (Array.isArray(left) || Array.isArray(right)) {
    if (!Array.isArray(left) || !Array.isArray(right)) return false
    if (left.length !== right.length) return false
    return left.every((item, index) => sameValue(item, right[index]))
  }
  if (typeof left === 'object' && typeof right === 'object') {
    const leftRecord = left as Record<string, unknown>
    const rightRecord = right as Record<string, unknown>
    const leftKeys = Object.keys(leftRecord)
    if (leftKeys.length !== Object.keys(rightRecord).length) return false
    return leftKeys.every(
      (key) => key in rightRecord && sameValue(leftRecord[key], rightRecord[key]),
    )
  }
  return false
}

function isDirtyField(field: DraftField): boolean {
  const parsed = parseDraft(field)
  if (!parsed.ok) return true
  return !sameValue(parsed.value, field.original)
}

// ---------------------------------------------------------------------------
// 分组
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 单字段
// ---------------------------------------------------------------------------

const CONTROL_BASE =
  'h-10 w-full rounded-lg border bg-surface px-3 text-sm text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary placeholder:text-ink-muted'

const JSON_TEXTAREA_BASE =
  'w-full rounded-lg border bg-surface px-3 py-2 font-mono text-xs leading-relaxed text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary'

interface ConfigFieldProps {
  item: ConfigItem
  field: DraftField
  onDraftChange: (key: string, next: DraftField) => void
}

function ConfigField({ item, field, onDraftChange }: ConfigFieldProps) {
  const meta = configFieldMeta(item.key)
  const fieldId = `config-field-${item.key}`
  const labelId = `${fieldId}-label`
  const hintId = `${fieldId}-hint`
  const errorId = `${fieldId}-error`
  const locked = !item.runtime_writable
  const dirty = isDirtyField(field)
  const describedBy = field.error !== null ? errorId : hintId
  // 只读控件用 muted 底色：既然改不了，就别让它看起来像能改的输入框。
  const controlClass = cn(
    CONTROL_BASE,
    field.error !== null ? 'border-danger' : 'border-border-strong',
    locked && 'bg-surface-muted text-ink-secondary',
  )

  const handleText = (text: string) => {
    const next: DraftField = { ...field, text }
    const parsed = parseDraft(next)
    onDraftChange(field.key, { ...next, error: parsed.ok ? null : parsed.error })
  }

  let control: ReactNode
  if (field.kind === 'boolean') {
    control = (
      <button
        type="button"
        id={fieldId}
        role="switch"
        aria-checked={field.checked}
        aria-labelledby={labelId}
        aria-describedby={describedBy}
        // 开关是按钮，没有 readOnly：只能 disabled + aria-readonly，让读屏读成「只读」而不是「坏掉的开关」。
        disabled={locked}
        aria-readonly={locked ? true : undefined}
        title={locked ? '只读：该键需改配置文件并重启进程后才生效' : undefined}
        onClick={() => onDraftChange(field.key, { ...field, checked: !field.checked, error: null })}
        className={cn(
          'inline-flex h-10 items-center gap-2 rounded-lg border px-3 text-sm font-medium transition-colors duration-fast focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary',
          field.checked
            ? 'border-primary-ring bg-primary-soft text-primary-strong'
            : 'border-border-strong bg-surface text-ink-secondary',
          locked && 'cursor-not-allowed bg-surface-muted opacity-80',
        )}
      >
        <span
          aria-hidden="true"
          className={cn(
            'h-2 w-2 rounded-full',
            field.checked ? 'bg-primary' : 'bg-border-strong',
          )}
        />
        {field.checked ? '已开启' : '已关闭'}
      </button>
    )
  } else if (field.kind === 'json') {
    control = (
      <textarea
        id={fieldId}
        rows={Math.min(12, Math.max(3, field.text.split('\n').length))}
        value={field.text}
        spellCheck={false}
        // readOnly 而不是 disabled：只读文本框仍可聚焦、选中、复制，核对 JSON 时要能复制。
        readOnly={locked}
        onChange={(event) => handleText(event.target.value)}
        aria-invalid={field.error !== null ? true : undefined}
        aria-describedby={describedBy}
        className={cn(
          JSON_TEXTAREA_BASE,
          field.error !== null ? 'border-danger' : 'border-border-strong',
          locked && 'bg-surface-muted text-ink-secondary',
        )}
      />
    )
  } else {
    control = (
      <input
        id={fieldId}
        type={field.kind === 'number' ? 'number' : 'text'}
        step={field.kind === 'number' ? 'any' : undefined}
        inputMode={field.kind === 'number' ? 'decimal' : undefined}
        value={field.text}
        readOnly={locked}
        onChange={(event) => handleText(event.target.value)}
        aria-invalid={field.error !== null ? true : undefined}
        aria-describedby={describedBy}
        className={controlClass}
      />
    )
  }

  return (
    <div className="grid gap-3 border-t border-border py-4 first:border-t-0 first:pt-0 lg:grid-cols-[minmax(0,20rem)_minmax(0,1fr)] lg:gap-6">
      <div className="space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <label
            id={labelId}
            htmlFor={field.kind === 'boolean' ? undefined : fieldId}
            className="text-sm font-medium text-ink"
          >
            {meta.label}
          </label>
          {locked ? <Badge variant="warning">需重启生效</Badge> : null}
          {item.source === 'DEFAULT' ? <Badge variant="neutral">默认值</Badge> : null}
          {dirty ? (
            <span className="text-xs text-primary">未保存</span>
          ) : null}
        </div>
        {/*
          键名保留给技术同事对照配置文件用，但必须自证身份：只挂一串 billing.xxx
          在运营眼里就是「日志词」。前缀放在 <code> 外面，键名本身仍是纯文本，
          复制、对照配置都不受影响。
        */}
        <p className="flex flex-wrap items-baseline gap-1 text-xs text-ink-muted">
          <span>技术标识</span>
          <code className="font-mono" title="配置项的技术标识，报给技术同事时用这一串">
            {item.key}
          </code>
        </p>
        <p id={hintId} className="text-xs text-ink-secondary">
          {meta.description}
        </p>
      </div>
      <div className="space-y-1.5">
        {control}
        {field.error !== null ? (
          <p id={errorId} role="alert" className="text-xs text-danger">
            {field.error}
          </p>
        ) : null}
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// 页面
// ---------------------------------------------------------------------------

export default function AdminSystemPage() {
  usePageTitle('系统管理')
  const configQuery = useAdminConfig()
  const updateMutation = useUpdateAdminConfig()
  const toast = useToast()

  const items = configQuery.data?.items
  const [drafts, setDrafts] = useState<Record<string, DraftField> | null>(null)
  const [saveError, setSaveError] = useState<{ message: string; hint: string | null } | null>(null)
  const [lastResult, setLastResult] = useState<ConfigUpdateResponse | null>(null)
  // 当前选中的设置项。null 表示「还没点过」，此时用第一个**有数据**的项兜底：
  // 一进来就落到空页会让人以为页面坏了，落到没有数据的项则是一个空表单。
  const [activeItemId, setActiveItemId] = useState<string | null>(null)
  // 折叠状态按组记录。默认全展开（与参考站一致），点组标题才收起。
  const [collapsedGroupIds, setCollapsedGroupIds] = useState<readonly string[]>([])

  // 首屏把接口返回值拍平成编辑态。只在还没有编辑态时执行：后台自动重取
  // （窗口重新聚焦等）不应该把运营正在改的内容冲掉。
  useEffect(() => {
    if (items === undefined) return
    setDrafts((current) => current ?? buildDrafts(items))
  }, [items])

  const itemByKey = useMemo(() => {
    const map = new Map<string, ConfigItem>()
    for (const item of items ?? []) map.set(item.key, item)
    return map
  }, [items])

  const firstAvailableItem = useMemo(() => {
    const present = new Set((items ?? []).map((item) => item.key))
    return (
      CONFIG_NAV_ITEMS.find((item) => item.keys.some((key) => present.has(key))) ??
      firstItemWithKeys()
    )
  }, [items])

  const activeItem =
    (activeItemId === null
      ? undefined
      : CONFIG_NAV_ITEMS.find((item) => item.id === activeItemId)) ?? firstAvailableItem

  const fields = useMemo(
    () => (drafts === null ? [] : Object.values(drafts)),
    [drafts],
  )

  const dirtyKeys = useMemo(
    () => fields.filter((field) => isDirtyField(field)).map((field) => field.key),
    [fields],
  )

  /** 设置项 id → 该项下未保存的键数。菜单上直接标出来，省得「改了没存」被忘在别的页。 */
  const dirtyCountByItem = useMemo(() => {
    const counts = new Map<string, number>()
    for (const key of dirtyKeys) {
      const itemId = CONFIG_NAV_ITEM_OF_KEY.get(key)
      if (itemId !== undefined) counts.set(itemId, (counts.get(itemId) ?? 0) + 1)
    }
    return counts
  }, [dirtyKeys])

  const invalidKeys = useMemo(
    () => fields.filter((field) => field.error !== null).map((field) => field.key),
    [fields],
  )

  /**
   * 真正要提交的内容：有改动、可解析、且在运行时白名单内。
   *
   * 锁定键在 UI 上是只读的，本来就产生不了脏值；这里再过滤一次是给「后端哪天
   * 放宽/收紧白名单」留的保险——payload 的合法性由调用点自己保证，不靠 UI。
   */
  const pendingValues = useMemo(() => {
    const values: Record<string, unknown> = {}
    for (const field of fields) {
      if (field.error !== null) continue
      const parsed = parseDraft(field)
      if (!parsed.ok) continue
      if (sameValue(parsed.value, field.original)) continue
      if (itemByKey.get(field.key)?.runtime_writable !== true) continue
      values[field.key] = parsed.value
    }
    return values
  }, [fields, itemByKey])

  const saving = updateMutation.isPending
  const writableCount = useMemo(
    () => (items ?? []).filter((item) => item.runtime_writable).length,
    [items],
  )
  const defaultCount = useMemo(
    () => (items ?? []).filter((item) => item.source === 'DEFAULT').length,
    [items],
  )

  /**
   * 保存的前置条件只有两条：有可提交的改动、且没有解析失败的字段。
   * 锁定键不参与判断——它们是只读的，脏不了，也就不该连坐可写键。
   */
  const canSave =
    Object.keys(pendingValues).length > 0 && invalidKeys.length === 0 && !saving

  const handleDraftChange = useCallback((key: string, next: DraftField) => {
    setDrafts((current) => (current === null ? current : { ...current, [key]: next }))
  }, [])

  /** 重置：丢弃全部未保存改动，回到「上次加载值」。 */
  const handleReset = useCallback(() => {
    if (items === undefined) return
    setDrafts(buildDrafts(items))
    setSaveError(null)
  }, [items])

  const handleSave = async () => {
    if (!canSave) return
    setSaveError(null)
    try {
      const result = await updateMutation.mutateAsync(pendingValues)
      setLastResult(result)
      toast.success(
        '配置已保存',
        `版本 v${result.version} · 发布 ${result.publish_id} · 改动 ${result.changed_keys.length} 个键`,
      )
      // 重新拉取：source / runtime_writable / version 只有后端知道，不能本地猜。
      const fresh = await configQuery.refetch()
      if (fresh.data !== undefined) setDrafts(buildDrafts(fresh.data.items))
    } catch (error) {
      // 保留用户输入：只记录错误，不碰 drafts。
      setSaveError({ message: errorMessage(error), hint: errorHint(error) })
      toast.error('保存失败', errorMessage(error))
    }
  }

  // 吸顶页头的实测高度：左侧菜单要钉在它下沿，否则菜单会钻到页头底下。
  const headerRef = useRef<HTMLDivElement | null>(null)
  const [headerHeight, setHeaderHeight] = useState(72)

  useEffect(() => {
    const element = headerRef.current
    if (element === null || typeof ResizeObserver === 'undefined') return
    // 量到 0 就别采信（布局还没算出来的那一帧，jsdom 里也永远是 0）。
    const measure = () => {
      const height = element.offsetHeight
      if (height > 0) setHeaderHeight(height)
    }
    const observer = new ResizeObserver(measure)
    observer.observe(element)
    measure()
    return () => observer.disconnect()
  }, [])

  const toggleGroup = (groupId: string) => {
    setCollapsedGroupIds((current) =>
      current.includes(groupId)
        ? current.filter((id) => id !== groupId)
        : [...current, groupId],
    )
  }

  return (
    <div className="space-y-6">
      {/*
        操作区随页面滚动固定：改到页面下半截时不必回到顶部找保存。
      */}
      <div ref={headerRef} className="sticky top-0 z-10 bg-canvas">
        <PageHeader
          title="系统管理"
          description="按「站点 / 计费 / 路由 / 安全 / 运维」分组的配置入口。改动按整体版本发布，网关与控制面在发布后热加载。"
          actions={
            <>
              <Button
                variant="secondary"
                onClick={handleReset}
                disabled={dirtyKeys.length === 0 || saving}
              >
                重置
              </Button>
              <Button onClick={handleSave} disabled={!canSave} loading={saving}>
                保存更改
              </Button>
            </>
          }
        />
      </div>

      {configQuery.isError ? (
        <ErrorState
          title="配置加载失败"
          message={errorMessage(configQuery.error)}
          hint={errorHint(configQuery.error)}
          onRetry={() => {
            void configQuery.refetch()
          }}
        />
      ) : drafts === null || items === undefined ? (
        <div className="space-y-3">
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-32 w-full" />
          <Skeleton className="h-32 w-full" />
        </div>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant="neutral">共 {items.length} 个键</Badge>
            <Badge variant="primary">可在线修改 {writableCount} 个</Badge>
            <Badge variant="warning">需重启生效 {items.length - writableCount} 个</Badge>
            {defaultCount > 0 ? (
              <Badge variant="neutral">默认值 {defaultCount} 个</Badge>
            ) : null}
          </div>

          <p className="text-xs text-ink-secondary">
            标注「需重启生效」的键为只读：它们不在运行时写入白名单内（后端只接受白名单里的键），
            页面展示其当前生效值供核对，修改需要改配置文件并重启对应进程，无法在线发布。
            「默认值」标记表示该键在库里还没有行，页面显示的是注册表默认值。
          </p>

          {saveError !== null ? (
            <ErrorState
              compact
              title="保存失败"
              message={saveError.message}
              hint={saveError.hint}
            />
          ) : null}

          {invalidKeys.length > 0 ? (
            <p role="alert" className="text-xs text-danger">
              有 {invalidKeys.length} 个字段格式不合法，修正后才能保存。
            </p>
          ) : null}

          {lastResult !== null ? (
            <p className="text-xs text-ink-secondary">
              已发布版本 <span className="font-medium text-ink">v{lastResult.version}</span>
              <span aria-hidden="true" className="mx-1.5 text-border-strong">
                ·
              </span>
              发布 ID <span className="font-mono text-ink">{lastResult.publish_id}</span>
              <span aria-hidden="true" className="mx-1.5 text-border-strong">
                ·
              </span>
              改动 {lastResult.changed_keys.length} 个键：
              <span className="font-mono">{lastResult.changed_keys.join('、')}</span>
            </p>
          ) : null}

          <div className="flex flex-col gap-6 md:flex-row md:items-start">
            {/* 窄屏：树形菜单换成「分组 / 设置项」下拉，避免横向挤爆。 */}
            <div className="md:hidden">
              <Select
                label="跳到设置"
                value={activeItem.id}
                onChange={(event) => setActiveItemId(event.target.value)}
                options={CONFIG_NAV_ITEMS.map((item) => ({
                  value: item.id,
                  label: `${item.groupTitle} / ${item.title}`,
                }))}
              />
            </div>

            <nav
              aria-label="设置菜单"
              className="hidden md:sticky md:block md:max-h-[calc(100vh-12rem)] md:w-56 md:shrink-0 md:overflow-y-auto md:pr-1"
              style={{ top: headerHeight + 8 }}
            >
              {CONFIG_NAV.map((group) => {
                const collapsed = collapsedGroupIds.includes(group.id)
                return (
                  <div key={group.id} className="mb-1">
                    <button
                      type="button"
                      onClick={() => toggleGroup(group.id)}
                      aria-expanded={!collapsed}
                      className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-xs font-medium text-ink-muted transition-colors duration-fast hover:bg-surface-muted hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
                    >
                      <Icon name={group.icon} className="h-4 w-4" />
                      <span className="flex-1 text-left">{group.title}</span>
                      <Icon
                        name="chevron-down"
                        className={cn(
                          'h-3.5 w-3.5 transition-transform',
                          collapsed && '-rotate-90',
                        )}
                      />
                    </button>
                    {collapsed ? null : (
                      <ul className="mb-1 space-y-0.5 border-l border-border pl-2">
                        {group.items.map((item) => {
                          const active = item.id === activeItem.id
                          const dirtyCount = dirtyCountByItem.get(item.id) ?? 0
                          return (
                            <li key={item.id}>
                              <button
                                type="button"
                                onClick={() => setActiveItemId(item.id)}
                                aria-current={active ? 'page' : undefined}
                                aria-label={item.title}
                                className={cn(
                                  'flex w-full items-center justify-between gap-2 rounded-lg px-3 py-1.5 text-left text-sm transition-colors duration-fast focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary',
                                  active
                                    ? 'bg-surface-muted font-medium text-ink'
                                    : 'text-ink-secondary hover:bg-surface-muted hover:text-ink',
                                )}
                              >
                                <span className="truncate">{item.title}</span>
                                {dirtyCount > 0 ? (
                                  <span className="shrink-0 rounded-full bg-primary-soft px-1.5 text-xs text-primary-strong">
                                    {dirtyCount}
                                  </span>
                                ) : (
                                  <span className="shrink-0 text-xs text-ink-muted">
                                    {item.keys.length > 0 ? item.keys.length : '—'}
                                  </span>
                                )}
                              </button>
                            </li>
                          )
                        })}
                      </ul>
                    )}
                  </div>
                )
              })}
            </nav>

            <div className="min-w-0 flex-1">
              <section
                aria-labelledby="config-item-title"
                className="rounded-xl border border-border bg-surface p-5 shadow-card"
              >
                <div className="flex flex-wrap items-start justify-between gap-2 border-b border-border pb-3">
                  <div className="space-y-0.5">
                    <h2 id="config-item-title" className="text-base font-semibold text-ink">
                      {activeItem.title}
                    </h2>
                    <p className="text-xs text-ink-secondary">{activeItem.description}</p>
                  </div>
                  <Badge variant="neutral">{activeItem.groupTitle}</Badge>
                </div>

                {activeItem.keys.length === 0 ? (
                  // 空项照实说清楚「为什么是空的」，比放一个假表单有用：
                  // 运营至少知道该去找谁，而不是以为页面坏了。
                  <div className="mt-4 rounded-lg border border-dashed border-border-strong bg-surface-muted px-4 py-8 text-center">
                    <p className="text-sm font-medium text-ink">这一项当前没有可在线修改的配置</p>
                    <p className="mx-auto mt-1 max-w-md text-xs text-ink-secondary">
                      相关能力由部署配置或代码固定，需要调整请联系技术同事。其余设置项不受影响。
                    </p>
                  </div>
                ) : (
                  <div className="pt-3">
                    {activeItem.keys.map((key) => {
                      const field = drafts[key]
                      const item = itemByKey.get(key)
                      if (field === undefined || item === undefined) return null
                      return (
                        <ConfigField
                          key={key}
                          item={item}
                          field={field}
                          onDraftChange={handleDraftChange}
                        />
                      )
                    })}
                  </div>
                )}
              </section>
            </div>
          </div>
        </>
      )}
    </div>
  )
}

import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from '@/api/client'
import type { ConfigItem, ConfigListResponse } from '@/api/admin'
import { CONFIG_FIELD_META } from '@/lib/configKeys'
import { renderWithProviders } from '@/test/renderConsole'
import AdminSystemPage from './SystemPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPut = vi.mocked(api.put)

/**
 * 两档 fixture。
 *
 * 页面有 45 个受控字段，`render()` 一次要建 ~45 组 label/input/描述/Badge。
 * 20 条用例每条都渲全量的话单文件 10s+，并行跑时 worker 会被拖垮 —— 实测过
 * 「整份文件不被收集但仍 exit 0」（BACKLOG G19 的表现形式）。所以：
 *
 * - `FULL`（45 键，逐行抄自 `docs/config-keys.md`）：只给**必须依赖全量**的用例，
 *   即分组结构、分组归属、页内导航条数、12/33 可写计数、45 条中文文案。
 * - `SMALL`（7 键）：其余全部用例。它仍然覆盖四种值类型（string / number /
 *   boolean / object）、可写与锁定两类、以及 source=DEFAULT，所以保存语义、
 *   payload 精确断言、只读语义、默认值标记的判定力没有下降——只是字段少了。
 *
 * 断言的**判定力不允许降级**：payload 仍然是 `toHaveBeenCalledWith(...)` 的
 * 精确 `toEqual` 语义，不是「包含」。
 */
interface FixtureRow {
  key: string
  namespace: string
  value: unknown
  writable?: boolean
  source?: ConfigItem['source']
}

/** 小 fixture：7 键、3 个命名空间，四类型 + 可写/锁定 + 一条 DEFAULT 齐全。 */
const SMALL: readonly FixtureRow[] = [
  // 锁定 · string
  { key: 'billing.refund_policy', namespace: 'PRICING', value: 'by_order' },
  // 锁定 · number
  { key: 'billing.prehold_ratio', namespace: 'PRICING', value: 1.2 },
  // 锁定 · number · 库里没有行
  { key: 'billing.prestimate_safety_bp', namespace: 'PRICING', value: 11000, source: 'DEFAULT' },
  // 锁定 · object
  {
    key: 'billing.estimate_language_coef',
    namespace: 'PRICING',
    value: { zh: 1, en: 1, code: 1, mixed: 1 },
  },
  // 锁定 · boolean
  { key: 'pricing.cache_billing_enabled', namespace: 'PRICING', value: false },
  // 可写 · string
  { key: 'quota.reset_tz', namespace: 'RATELIMIT', value: 'Asia/Shanghai', writable: true },
  // 可写 · number
  { key: 'gw.max_inflight', namespace: 'UPSTREAM', value: 8000, writable: true },
]

/** 完整 fixture：45 键，逐行抄自 `docs/config-keys.md`（键 / 命名空间 / 默认值 / 白名单）。 */
const FULL: readonly FixtureRow[] = [
  // PRICING 21：billing.* 10 + pricing.* 9 + plan.* 2
  { key: 'billing.deduct_policy', namespace: 'PRICING', value: 'fifo_expiry', writable: true },
  { key: 'billing.refund_policy', namespace: 'PRICING', value: 'by_order' },
  { key: 'billing.overdraft_limit_default', namespace: 'PRICING', value: 50 },
  {
    key: 'billing.estimate_language_coef',
    namespace: 'PRICING',
    value: { zh: 1, en: 1, code: 1, mixed: 1 },
  },
  { key: 'billing.prehold_ratio', namespace: 'PRICING', value: 1.2 },
  { key: 'billing.prehold_max_ratio', namespace: 'PRICING', value: 0.5, writable: true },
  {
    key: 'billing.settle_deadline_ms',
    namespace: 'PRICING',
    value: { non_stream_ms: 5000, stream_ms: 10000 },
  },
  { key: 'billing.bonus_expire_days', namespace: 'PRICING', value: 90 },
  { key: 'billing.long_ctx_prehold_guard_bp', namespace: 'PRICING', value: 9000 },
  {
    key: 'billing.prestimate_safety_bp',
    namespace: 'PRICING',
    value: 11000,
    source: 'DEFAULT',
  },
  { key: 'pricing.cost_source', namespace: 'PRICING', value: 'manual' },
  { key: 'pricing.tier_chain', namespace: 'PRICING', value: 'model>channel' },
  { key: 'pricing.agent_markup', namespace: 'PRICING', value: 'off' },
  { key: 'pricing.cache_billing_enabled', namespace: 'PRICING', value: false },
  { key: 'pricing.min_margin_bp', namespace: 'PRICING', value: 2500 },
  { key: 'pricing.plan_min_margin_bp', namespace: 'PRICING', value: 1000 },
  { key: 'pricing.max_bonus_ratio_bp', namespace: 'PRICING', value: 3000 },
  { key: 'pricing.tax_rate_multiplier', namespace: 'PRICING', value: 1 },
  { key: 'pricing.group_switch_mode', namespace: 'PRICING', value: 'ops_only' },
  { key: 'plan.default_tier', namespace: 'PRICING', value: 'free' },
  { key: 'plan.trial_days', namespace: 'PRICING', value: 7 },

  // RATELIMIT 6
  { key: 'quota.daily_meter', namespace: 'RATELIMIT', value: 'prehold', writable: true },
  { key: 'quota.key_daily_mode', namespace: 'RATELIMIT', value: 'none', writable: true },
  { key: 'quota.reset_scope', namespace: 'RATELIMIT', value: 'account', writable: true },
  { key: 'quota.reset_tz', namespace: 'RATELIMIT', value: 'Asia/Shanghai', writable: true },
  {
    key: 'quota.rate_limit_order',
    namespace: 'RATELIMIT',
    value: 'concurrency>rpm>tpm>daily',
    writable: true,
  },
  { key: 'quota.free_max_active_keys', namespace: 'RATELIMIT', value: 3 },

  // UPSTREAM 9
  { key: 'gw.max_inflight', namespace: 'UPSTREAM', value: 8000, writable: true },
  { key: 'gw.drain_deadline_ms', namespace: 'UPSTREAM', value: 120000, writable: true },
  { key: 'gw.upstream_timeout_ms', namespace: 'UPSTREAM', value: 50000, writable: true },
  { key: 'gw.client_timeout_budget_ms', namespace: 'UPSTREAM', value: 120000, writable: true },
  { key: 'gw.retry_max', namespace: 'UPSTREAM', value: 1 },
  { key: 'gw.stream_first_byte_ms', namespace: 'UPSTREAM', value: 30000 },
  { key: 'gw.stream_total_ms', namespace: 'UPSTREAM', value: 600000 },
  { key: 'gw.settle_reserve_ms', namespace: 'UPSTREAM', value: 15000 },
  {
    key: 'gw.retry_threshold_ms',
    namespace: 'UPSTREAM',
    value: { unary: 65000, stream: 45000 },
  },

  // FEATURE 8：log.* 4 + idem.* 3 + site.* 1
  { key: 'log.body_mode', namespace: 'FEATURE', value: 'meta_only' },
  { key: 'log.volume_basis', namespace: 'FEATURE', value: 'meta' },
  { key: 'log.retention_days', namespace: 'FEATURE', value: 180, writable: true },
  { key: 'log.same_instance', namespace: 'FEATURE', value: true },
  { key: 'idem.duplicate_window_seconds', namespace: 'FEATURE', value: 10 },
  { key: 'idem.lease_ttl_seconds', namespace: 'FEATURE', value: 15 },
  { key: 'idem.lease_renew_seconds', namespace: 'FEATURE', value: 5 },
  { key: 'site.recommend_limit', namespace: 'FEATURE', value: 8 },

  // UNASSIGNED 1
  { key: 'quota.overdraft_warn_days', namespace: 'UNASSIGNED', value: 7 },
]

const GROUP_TITLES = ['计费与配额', '限流与额度', '网关与超时', '功能开关', '系统行为'] as const
const NAMESPACES = ['PRICING', 'RATELIMIT', 'UPSTREAM', 'FEATURE', 'UNASSIGNED'] as const

/** 服务端「当前值」：PUT 之后会被覆盖，用来模拟真实的重新拉取。 */
let storedOverrides: Record<string, unknown> = {}

function buildList(rows: readonly FixtureRow[]): ConfigListResponse {
  const items: ConfigItem[] = rows.map((row) => {
    const overridden = row.key in storedOverrides
    return {
      key: row.key,
      namespace: row.namespace,
      value: overridden ? storedOverrides[row.key] : row.value,
      runtime_writable: row.writable === true,
      version: overridden ? 2 : 1,
      source: overridden ? 'ROW' : row.source ?? 'ROW',
    }
  })
  return { items, total: items.length }
}

beforeEach(() => {
  storedOverrides = {}
  mockedGet.mockReset()
  mockedPut.mockReset()
})

afterEach(() => {
  cleanup()
})

/** 默认用小 fixture；需要全量结构的用例显式传 `FULL`。 */
async function renderPage(rows: readonly FixtureRow[] = SMALL) {
  mockedGet.mockImplementation(() => Promise.resolve(buildList(rows)))
  const view = renderWithProviders(<AdminSystemPage />)
  const firstGroup = rows[0]?.namespace
  const title = GROUP_TITLES[NAMESPACES.indexOf(firstGroup as (typeof NAMESPACES)[number])]
  await screen.findByRole('heading', { level: 2, name: title ?? GROUP_TITLES[0] })
  return view
}

function sectionOf(index: number): HTMLElement {
  const headings = screen.getAllByRole('heading', { level: 2 })
  const heading = headings[index]
  const section = heading?.closest('section')
  if (section === null || section === undefined) {
    throw new Error(`第 ${index + 1} 个分组标题不在 <section> 里，页面结构变了`)
  }
  return section
}

/** 分组内渲染出来的键名（键名用 <code> 呈现，便于与说明文字区分）。 */
function keysIn(section: HTMLElement): string[] {
  return Array.from(section.querySelectorAll('code')).map((node) => node.textContent ?? '')
}

function expectedKeys(namespace: string): string[] {
  return FULL.filter((row) => row.namespace === namespace).map((row) => row.key)
}

const saveButton = () => screen.getByRole('button', { name: '保存更改' })

// ---------------------------------------------------------------------------
// 全量结构（唯一使用 FULL 的三条 + 一条纯数据用例）
// ---------------------------------------------------------------------------

describe('系统管理页：分组结构（全量 45 键）', () => {
  it('5 个分组标题按序渲染，45 键逐组归属，页内导航可切换当前项', async () => {
    const user = userEvent.setup()
    await renderPage(FULL)

    const headings = screen.getAllByRole('heading', { level: 2 })
    expect(headings.map((heading) => heading.textContent)).toEqual([...GROUP_TITLES])

    const perSection = GROUP_TITLES.map((_, index) => keysIn(sectionOf(index)))
    expect(perSection.map((keys) => keys.length)).toEqual([21, 6, 9, 8, 1])
    expect(perSection.flat()).toHaveLength(45)

    NAMESPACES.forEach((namespace, index) => {
      // 顺序敏感：页面按服务端返回顺序渲染组内键，fixture 也按同一份文档顺序写。
      expect(perSection[index]).toEqual(expectedKeys(namespace))
    })

    // 反向证明：45 个键一个不少、一个不多。
    expect(perSection.flat().sort()).toEqual(FULL.map((row) => row.key).sort())

    // 页内锚点导航：5 个分组按钮、条数与分组一致、当前项带 aria-current，点击可切换。
    // （与上面的结构断言共用同一次 FULL 渲染，避免为同一份数据再渲 45 个字段。）
    const nav = screen.getByRole('navigation', { name: '配置分组' })
    const navItems = within(nav).getAllByRole('button')
    expect(navItems.map((item) => item.textContent)).toEqual([
      '计费与配额21',
      '限流与额度6',
      '网关与超时9',
      '功能开关8',
      '系统行为1',
    ])
    expect(navItems[0]).toHaveAttribute('aria-current', 'true')

    await user.click(navItems[2])

    expect(navItems[2]).toHaveAttribute('aria-current', 'true')
    expect(navItems[0]).not.toHaveAttribute('aria-current')
    // 窄屏下拉与导航共用同一份选中态。
    expect(screen.getByLabelText('跳到分组')).toHaveValue('upstream')
  })

  it('45 个键全部有中文标签与说明，没有漏登记的键', () => {
    // 纯数据断言，不需要渲染页面（原先挂着一次 45 字段 render，纯属浪费）。
    const missing = FULL.map((row) => row.key).filter(
      (key) => CONFIG_FIELD_META[key] === undefined,
    )

    expect(missing).toEqual([])
    expect(Object.keys(CONFIG_FIELD_META)).toHaveLength(45)
    for (const key of Object.keys(CONFIG_FIELD_META)) {
      const meta = CONFIG_FIELD_META[key]
      expect(meta).toBeDefined()
      // 标签不能等于键名（那就是没翻译），说明也不能是空串。
      expect(meta?.label).not.toBe(key)
      expect((meta?.description ?? '').length).toBeGreaterThan(10)
    }
  })

  it('12 个可写键全部可编辑、33 个锁定键全部只读，且只读键带「需重启生效」标记', async () => {
    await renderPage(FULL)

    const writable = FULL.filter((row) => row.writable === true).length
    const locked = FULL.length - writable

    // 页面上所有「填值」类控件（输入框 / 数字框 / 多行文本 / 开关）。窄屏分组下拉
    // 是 combobox，不在这三类里，不会被误算。
    const controls = [
      ...screen.getAllByRole('textbox'),
      ...screen.getAllByRole('spinbutton'),
      ...screen.getAllByRole('switch'),
    ]
    const isEditable = (control: HTMLElement): boolean => {
      const element = control as HTMLInputElement | HTMLTextAreaElement | HTMLButtonElement
      const readOnly = 'readOnly' in element ? element.readOnly : false
      return readOnly !== true && element.disabled !== true
    }

    expect(controls).toHaveLength(FULL.length)
    expect(controls.filter(isEditable)).toHaveLength(writable)
    expect(controls.filter((control) => !isEditable(control))).toHaveLength(locked)

    // 顺带钉住基线：文档口径是 12 可写 / 33 只读，改 fixture 时别把这条衬托掉。
    expect([writable, locked]).toEqual([12, 33])

    // 每个锁定键都有 Badge，可写键没有；DEFAULT 只有一个。
    expect(screen.getAllByText('需重启生效')).toHaveLength(33)
    expect(screen.getAllByText('默认值')).toHaveLength(1)

    // 计费与配额 21 键里只有 billing.deduct_policy 与 billing.prehold_max_ratio 在运行时
    // 白名单内，其余 19 个只读。
    expect(
      within(sectionOf(0)).getByText(
        '本组 19 个键为只读：需改配置文件并重启进程，无法在线发布。',
      ),
    ).toBeInTheDocument()
  })
})

// ---------------------------------------------------------------------------
// 控件按值类型自适应（小 fixture）
// ---------------------------------------------------------------------------

describe('系统管理页：控件按值类型自适应', () => {
  it('string 渲染成单行输入框，值与服务端一致', async () => {
    await renderPage()

    // 可写键：可编辑。
    const input = screen.getByLabelText('日额度重置时区')
    expect(input.tagName).toBe('INPUT')
    expect(input).toHaveAttribute('type', 'text')
    expect(input).toHaveValue('Asia/Shanghai')
    expect(input).toHaveProperty('readOnly', false)

    // 锁定键：同一种控件，但只读（值仍可见、可复制）。
    const locked = screen.getByLabelText('退款策略')
    expect(locked).toHaveAttribute('type', 'text')
    expect(locked).toHaveValue('by_order')
    expect(locked).toHaveProperty('readOnly', true)
  })

  it('number 渲染成数字输入框，浮点与整数都原样保留', async () => {
    await renderPage()

    const ratio = screen.getByLabelText('预扣比例')
    expect(ratio).toHaveAttribute('type', 'number')
    expect(ratio).toHaveValue(1.2)

    const inflight = screen.getByLabelText('单实例并发上限')
    expect(inflight).toHaveAttribute('type', 'number')
    expect(inflight).toHaveValue(8000)
  })

  it('boolean 渲染成开关，aria-checked 反映当前值；只读时 disabled + aria-readonly', async () => {
    await renderPage()

    const toggle = screen.getByRole('switch', { name: '缓存计价开关' })
    expect(toggle).toHaveAttribute('aria-checked', 'false')
    expect(toggle).toHaveTextContent('已关闭')

    // 全库两个 bool 都不在运行时白名单内，所以页面上的开关必须是按下无效的只读态，
    // 而不是"能按但按了存不进去"的假开关。
    expect(toggle).toBeDisabled()
    expect(toggle).toHaveAttribute('aria-readonly', 'true')
  })

  it('object 渲染成格式化好的多行 JSON 文本框（只读，仍可选中复制）', async () => {
    await renderPage()

    const editor = screen.getByLabelText('语言预估系数')
    expect(editor.tagName).toBe('TEXTAREA')
    // 格式化：带缩进与换行，不是一行挤在一起。
    const formatted = JSON.stringify({ zh: 1, en: 1, code: 1, mixed: 1 }, null, 2)
    expect(editor).toHaveValue(formatted)
    expect(formatted).toContain('\n')
    expect(editor).toHaveProperty('readOnly', true)
  })
})

// ---------------------------------------------------------------------------
// 可写性与默认值标记（小 fixture）
// ---------------------------------------------------------------------------

describe('系统管理页：可写性与默认值标记', () => {
  it('source=DEFAULT 的键带「默认值」标记，说明该键在库里还没有行', async () => {
    await renderPage()

    const field = screen.getByLabelText('预估安全余量').closest('div.grid')
    expect(field).not.toBeNull()
    expect(within(field as HTMLElement).getByText('默认值')).toBeInTheDocument()

    // 库里已有行的键不应该带这个标记。
    const stored = screen.getByLabelText('预扣比例').closest('div.grid')
    expect(within(stored as HTMLElement).queryByText('默认值')).not.toBeInTheDocument()
  })
})

// ---------------------------------------------------------------------------
// 保存语义（小 fixture）
// ---------------------------------------------------------------------------

describe('系统管理页：保存语义', () => {
  it('没有改动时「保存更改」与「重置」都禁用', async () => {
    await renderPage()

    expect(saveButton()).toBeDisabled()
    expect(screen.getByRole('button', { name: '重置' })).toBeDisabled()
    expect(mockedPut).not.toHaveBeenCalled()
  })

  it('只改一个键后保存，PUT 的 payload 只包含这个键', async () => {
    const user = userEvent.setup()
    mockedPut.mockImplementation((_path: string, body?: unknown) => {
      const values = (body as { values: Record<string, unknown> }).values
      storedOverrides = { ...storedOverrides, ...values }
      return Promise.resolve({
        version: 2,
        publish_id: 'pub-9f2c',
        namespaces: ['UPSTREAM'],
        changed_keys: Object.keys(values),
      })
    })
    await renderPage()

    // 用 change 一次性置值（不逐键 type）：本用例断言的是"值进了 draft 并进了 payload"，
    // 不是逐键输入的健壮性，省掉每键一个 act tick 的开销。
    fireEvent.change(screen.getByLabelText('单实例并发上限'), { target: { value: '9000' } })

    expect(saveButton()).toBeEnabled()
    await user.click(saveButton())

    await waitFor(() => {
      expect(mockedPut).toHaveBeenCalledTimes(1)
    })
    // 断言的是 payload 的内容，而不是「有调用」：另外 6 个键（含 2 个可写键）一个都不能带上去。
    expect(mockedPut).toHaveBeenCalledWith('/api/admin/config', {
      values: { 'gw.max_inflight': 9000 },
    })

    // 保存结果里的版本 / 发布 ID / 改动数量要展示出来，运营才知道发布了什么。
    const publishLine = await screen.findByText(/已发布版本/)
    expect(publishLine).toHaveTextContent('v2')
    expect(publishLine).toHaveTextContent('pub-9f2c')
    expect(publishLine).toHaveTextContent('改动 1 个键：gw.max_inflight')
  })

  it('数字保持 number 类型写回，不会被转成字符串', async () => {
    const user = userEvent.setup()
    mockedPut.mockResolvedValue({
      version: 3,
      publish_id: 'pub-num',
      namespaces: ['UPSTREAM'],
      changed_keys: ['gw.max_inflight'],
    })
    await renderPage()

    fireEvent.change(screen.getByLabelText('单实例并发上限'), { target: { value: '12.5' } })
    await user.click(saveButton())

    await waitFor(() => {
      expect(mockedPut).toHaveBeenCalledWith('/api/admin/config', {
        values: { 'gw.max_inflight': 12.5 },
      })
    })
  })

  it('保存失败时保留用户输入，并展示 message 与 hint', async () => {
    const user = userEvent.setup()
    mockedPut.mockRejectedValue(
      new ApiError({
        status: 400,
        code: 'config_value_invalid',
        type: 'invalid_request',
        message: 'gw.max_inflight 超出允许范围',
        hint: '并发上限必须在 1~100000 之间',
      }),
    )
    await renderPage()

    fireEvent.change(screen.getByLabelText('单实例并发上限'), { target: { value: '999999' } })
    await user.click(saveButton())

    expect(await screen.findByText('gw.max_inflight 超出允许范围')).toBeInTheDocument()
    expect(screen.getByText('并发上限必须在 1~100000 之间')).toBeInTheDocument()
    // 关键：失败不清空输入。
    expect(screen.getByLabelText('单实例并发上限')).toHaveValue(999999)
    expect(saveButton()).toBeEnabled()
  })

  it('锁定键是只读的：既改不动，也不会让保存变成可点', async () => {
    const user = userEvent.setup()
    await renderPage()

    const locked = screen.getByLabelText('预扣比例')
    expect(locked).toHaveProperty('readOnly', true)

    // 往只读输入框里打字不产生脏值，保存仍然是禁用的、且 0 次 PUT。
    await user.type(locked, '1.5')
    expect(locked).toHaveValue(1.2)
    expect(saveButton()).toBeDisabled()
    expect(mockedPut).not.toHaveBeenCalled()
  })

  it('可写键的改动照常保存，只读键不参与 payload', async () => {
    const user = userEvent.setup()
    mockedPut.mockResolvedValue({
      version: 4,
      publish_id: 'pub-mixed',
      namespaces: ['UPSTREAM'],
      changed_keys: ['gw.max_inflight'],
    })
    await renderPage()

    // 试着改一个只读键（无效），再正常改一个可写键。
    await user.type(screen.getByLabelText('预扣比例'), '1.5')
    fireEvent.change(screen.getByLabelText('单实例并发上限'), { target: { value: '9000' } })

    expect(saveButton()).toBeEnabled()
    await user.click(saveButton())

    await waitFor(() => {
      expect(mockedPut).toHaveBeenCalledTimes(1)
    })
    // payload 里既没有只读键，也没有未被改动的键（含另一个可写键 quota.reset_tz）。
    expect(mockedPut).toHaveBeenCalledWith('/api/admin/config', {
      values: { 'gw.max_inflight': 9000 },
    })

    // 保存后重新拉取，可写键回到基线；只读键本来就是服务端值。
    await waitFor(() => {
      expect(screen.getByLabelText('单实例并发上限')).toHaveValue(8000)
    })
    expect(screen.getByLabelText('预扣比例')).toHaveValue(1.2)
  })

  it('重置丢弃未保存改动，回到上次加载值', async () => {
    const user = userEvent.setup()
    await renderPage()

    const input = screen.getByLabelText('单实例并发上限')
    fireEvent.change(input, { target: { value: '9000' } })
    expect(input).toHaveValue(9000)

    await user.click(screen.getByRole('button', { name: '重置' }))

    expect(screen.getByLabelText('单实例并发上限')).toHaveValue(8000)
    expect(saveButton()).toBeDisabled()
  })

  it('加载失败时展示错误态与重试，不渲染空表单', async () => {
    mockedGet.mockRejectedValue(
      new ApiError({
        status: 500,
        code: 'internal_error',
        type: 'internal_error',
        message: '配置读取失败',
        hint: '',
      }),
    )

    renderWithProviders(<AdminSystemPage />)

    expect(await screen.findByText('配置读取失败')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
    expect(screen.queryAllByRole('heading', { level: 2 })).toHaveLength(0)
  })
})

/**
 * 可写 object 键的 JSON 校验（合成 fixture）。
 *
 * 现实数据里 3 个 object 键（`billing.estimate_language_coef`、
 * `billing.settle_deadline_ms`、`gw.retry_threshold_ms`）全是
 * `runtime_writable=false`，所以在真实数据上它们只读、走不到"非法 JSON 禁用保存"
 * 这条路径。但校验代码必须继续存在——后端以后可能放开某个 object 键（12 键白名单
 * 不是永恒的），那时这条路径就是唯一的防线。因此这里把同一个键标成可写来验证它，
 * 而不是因为"现实数据里没有"就把用例删掉。
 */
describe('系统管理页：可写 object 键的 JSON 校验（合成 fixture）', () => {
  const OBJECT_VALUE = { zh: 1, en: 1, code: 1, mixed: 1 }
  const WRITABLE_OBJECT: readonly FixtureRow[] = [
    {
      key: 'billing.estimate_language_coef',
      namespace: 'PRICING',
      value: OBJECT_VALUE,
      writable: true,
    },
    { key: 'gw.max_inflight', namespace: 'UPSTREAM', value: 8000, writable: true },
  ]

  it('非法 JSON 时红框提示并禁用保存；改成合法 JSON 后按对象原样提交', async () => {
    const user = userEvent.setup()
    mockedPut.mockResolvedValue({
      version: 2,
      publish_id: 'pub-object',
      namespaces: ['PRICING'],
      changed_keys: ['billing.estimate_language_coef'],
    })

    await renderPage(WRITABLE_OBJECT)

    const editor = screen.getByLabelText('语言预估系数')
    expect(editor.tagName).toBe('TEXTAREA')
    expect(editor).toHaveProperty('readOnly', false)

    // 少一个右括号：JSON.parse 在真实浏览器里同样会抛。
    fireEvent.change(editor, { target: { value: '{"zh":1' } })

    expect(screen.getByText(/JSON 格式不合法/)).toBeInTheDocument()
    expect(editor).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByText(/个字段格式不合法，修正后才能保存/)).toBeInTheDocument()
    expect(saveButton()).toBeDisabled()

    // 补成合法 JSON（且值有改动）→ 错误消失、保存可点。
    fireEvent.change(editor, { target: { value: '{"zh": 2, "en": 1, "code": 1, "mixed": 1}' } })
    expect(screen.queryByText(/JSON 格式不合法/)).not.toBeInTheDocument()
    expect(editor).not.toHaveAttribute('aria-invalid')

    expect(saveButton()).toBeEnabled()
    await user.click(saveButton())

    await waitFor(() => {
      expect(mockedPut).toHaveBeenCalledWith('/api/admin/config', {
        values: { 'billing.estimate_language_coef': { zh: 2, en: 1, code: 1, mixed: 1 } },
      })
    })
  })
})

import { cleanup, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from '@/api/client'
import type { AdminIntegration, AdminPriceRule, PriceGroup } from '@/api/admin'
import { earliestEffectiveAt, toLocalDateTimeValue } from '@/components/admin'
import { renderWithProviders } from '@/test/renderConsole'
import AdminPricingPage from './PricingPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPatch = vi.mocked(api.patch)

function priceRule(overrides: Partial<AdminPriceRule> = {}): AdminPriceRule {
  return {
    id: 701,
    scope_level: 'MODEL',
    model_id: 11,
    model_name: 'qwen-max',
    model_type: 'TEXT',
    in_price_micro_per_k: 1000,
    out_price_micro_per_k: 3000,
    per_call_price_micro: 0,
    cost_in_micro_per_k: 800,
    cost_out_micro_per_k: 2400,
    currency_cost: 'CNY',
    version: 3,
    effective_from: '2026-10-01T00:00:00Z',
    effective_to: null,
    enabled: true,
    created_at: '2026-09-20T02:00:00Z',
    ...overrides,
  }
}

/**
 * 「在售中」的价格必须有**未过期**的 effective_to。原来写死 `2026-11-05T00:00:00Z`，
 * 到这一天（本地 08:00 之后）夹具就变成"已过期"，页面判定没有生效中的价格：
 * 涨价方向退化成降价（生效时间 min 取当天）、新价又被要求填生效时间——两条用例
 * 于是同时变红。夹具里的时间一律相对当前时刻生成，别再写死日期。
 */
function futureIso(days: number): string {
  return new Date(Date.now() + days * 24 * 60 * 60 * 1000).toISOString()
}

const active = priceRule({ effective_to: futureIso(30) })

const groups: PriceGroup[] = [
  {
    id: 21,
    code: 'internal',
    name: '内部调用',
    ratio_bp: 0,
    model_scope_mode: 'ALL',
    channel_scope_mode: 'ALL',
    self_service: false,
    remark: '不计费',
    enabled: true,
    model_ids: [],
    channel_ids: [],
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  },
  {
    id: 22,
    code: 'standard',
    name: '标准分组',
    ratio_bp: 8500,
    model_scope_mode: 'ALL',
    channel_scope_mode: 'ALL',
    self_service: true,
    remark: '',
    enabled: true,
    model_ids: [],
    channel_ids: [],
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  },
]

const models = [
  {
    id: 11,
    model_name: 'qwen-max',
    display_name: 'Qwen Max',
    region: 'DOMESTIC' as const,
    model_type: 'TEXT' as const,
    protocol_id: 1,
    protocol: 'BAILIAN',
    context_length: 32768,
    capability_mask: 0,
    capabilities: { stream: null, tool_call: null, vision: null, json_mode: null },
    listing_state: 'LISTED' as const,
    recommend_weight: 100,
    aliases: [],
    pricing: null,
    created_at: '2026-07-01T02:00:00Z',
    updated_at: '2026-10-01T08:00:00Z',
  },
]

const integrations: AdminIntegration[] = [
  {
    tool_key: 'cursor',
    display_name: 'Cursor',
    enabled: true,
    deep_link_template: 'cursor://anysphere.cursor-deeplink/mcp/install?name=yunshu&config={key}',
  },
  { tool_key: 'claude-code', display_name: 'Claude Code', enabled: false, deep_link_template: null },
  { tool_key: 'cherry-studio', display_name: 'Cherry Studio', enabled: false, deep_link_template: null },
  { tool_key: 'nextchat', display_name: 'NextChat', enabled: false, deep_link_template: null },
  { tool_key: 'openai-sdk', display_name: 'OpenAI SDK', enabled: false, deep_link_template: null },
]

const emptyAudit = {
  items: [],
  page: 1,
  page_size: 50,
  total: 0,
  from: '2026-10-21T00:00:00Z',
  to: '2026-10-28T00:00:00Z',
}

function installMock(rules: AdminPriceRule[] = [active]) {
  mockedGet.mockImplementation((path: string) => {
    if (path.startsWith('/api/admin/prices?')) {
      if (path.includes('model_id=11')) {
        return Promise.resolve({
          items: rules,
          page: 1,
          page_size: 100,
          total: rules.length,
        })
      }
      return Promise.resolve({
        items: [active],
        page: 1,
        page_size: 20,
        total: 1,
      })
    }
    if (path === '/api/admin/groups') return Promise.resolve({ items: groups, total: groups.length })
    if (path === '/api/admin/integrations') {
      return Promise.resolve({ items: integrations, total: integrations.length })
    }
    if (path.startsWith('/api/admin/models?')) {
      return Promise.resolve({ items: models, page: 1, page_size: 100, total: models.length })
    }
    if (path.startsWith('/api/admin/audit-logs?')) return Promise.resolve(emptyAudit)
    return Promise.reject(new Error(`unexpected ${path}`))
  })
  mockedPatch.mockResolvedValue({ price: active })
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPatch.mockReset()
})

afterEach(cleanup)

async function openGroupEditor(code: 'internal' | 'standard') {
  const user = userEvent.setup()
  renderWithProviders(<AdminPricingPage />)
  await screen.findByText('qwen-max')
  await user.click(screen.getByRole('tab', { name: '分组倍率' }))
  const name = code === 'internal' ? '内部调用' : '标准分组'
  const cell = await screen.findByText(name)
  const row = cell.closest('tr')
  if (!row) throw new Error(`未找到分组行：${name}`)
  await user.click(within(row).getByRole('button', { name: '编辑倍率' }))
  await screen.findByText('编辑分组倍率')
  return user
}

async function openPriceEditor() {
  const user = userEvent.setup()
  renderWithProviders(<AdminPricingPage />)
  await screen.findByText('qwen-max')
  await user.click(screen.getByRole('button', { name: '编辑' }))
  await screen.findByLabelText(/输入单价/)
  return user
}

describe('PricingPage', () => {
  it('PricingPage：价格填 0 或负数在字段上提示', async () => {
    installMock()
    const user = await openPriceEditor()

    const input = screen.getByLabelText(/输入单价/)
    await user.clear(input)
    await user.type(input, '0')
    await user.click(screen.getByRole('button', { name: '保存' }))

    expect(await screen.findByText('输入单价必须大于 0。')).toBeInTheDocument()
    expect(mockedPatch).not.toHaveBeenCalled()

    await user.clear(input)
    await user.type(input, '-1')
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByText('输入单价必须为非负整数。')).toBeInTheDocument()
    expect(mockedPatch).not.toHaveBeenCalled()
  })

  it('PricingPage：涨价时生效时间最小 today+7', async () => {
    installMock()
    const user = await openPriceEditor()
    const input = screen.getByLabelText(/输入单价/)
    const raiseDate = toLocalDateTimeValue(earliestEffectiveAt('raise')).slice(0, 10)
    const today = toLocalDateTimeValue(earliestEffectiveAt('cut')).slice(0, 10)

    await user.clear(input)
    await user.type(input, '1200')
    await waitFor(() => {
      const time = screen.getByTestId('effective-time-input') as HTMLInputElement
      expect(time.min.startsWith(raiseDate)).toBe(true)
    })

    await user.clear(input)
    await user.type(input, '900')
    await waitFor(() => {
      const time = screen.getByTestId('effective-time-input') as HTMLInputElement
      expect(time.min.startsWith(today)).toBe(true)
    })
  })

  it('PricingPage：地板不满足显示差额', async () => {
    installMock()
    await openPriceEditor()

    const difference = await screen.findByTestId('floor-check-difference')
    expect(difference).toHaveTextContent('差额 217 积分/千 token')
  })

  it('PricingPage：两处配价冲突指出两份来源', async () => {
    installMock([
      priceRule({ id: 701 }),
      priceRule({ id: 702, in_price_micro_per_k: 1200, version: 4 }),
    ])
    await openPriceEditor()

    const banner = await screen.findByTestId('price-conflict-banner')
    expect(within(banner).getByText('t_price_rule#701')).toBeInTheDocument()
    expect(within(banner).getByText('t_price_rule#702')).toBeInTheDocument()
    expect(within(banner).getAllByRole('button', { name: '跳到该行' })).toHaveLength(2)
  })

  it('PricingPage：勾选清除后 body 不含该字段且 clear_fields 含该键', async () => {
    installMock([
      priceRule({
        cache_hit_ratio_bp: 8500,
        effective_to: futureIso(30),
      }),
    ])
    const user = await openPriceEditor()
    await user.click(screen.getByLabelText('清除缓存命中倍率'))
    await user.click(screen.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(mockedPatch).toHaveBeenCalled())
    const body = (mockedPatch.mock.calls.at(-1)?.[1] ?? {}) as Record<string, unknown>
    expect(body).not.toHaveProperty('cache_hit_ratio_bp')
    expect(body.clear_fields).toContain('cache_hit_ratio_bp')
  })

  it('PricingPage：internal 分组显示不计费', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<AdminPricingPage />)
    await screen.findByText('qwen-max')

    await user.click(screen.getByRole('tab', { name: '分组倍率' }))

    expect(await screen.findByText('不计费')).toBeInTheDocument()
    expect(screen.queryByText('免费')).not.toBeInTheDocument()
    expect(screen.queryByText('0 折')).not.toBeInTheDocument()
    expect(screen.queryByText('100 折')).not.toBeInTheDocument()
  })

  it('PricingPage：internal 保存弹二次确认，理由为空时确认钮禁用', async () => {
    installMock()
    const user = await openGroupEditor('internal')

    await user.click(screen.getByRole('button', { name: '保存' }))
    const dialog = await screen.findByRole('dialog', { name: '确认修改内部分组' })
    const confirm = within(dialog).getByRole('button', { name: '确认保存内部分组' })
    expect(confirm).toBeDisabled()
    expect(mockedPatch).not.toHaveBeenCalled()

    await user.type(within(dialog).getByLabelText(/操作理由/), '内部压测，工单 OPS-123')
    expect(confirm).toBeEnabled()
    await user.click(confirm)

    await waitFor(() => expect(mockedPatch).toHaveBeenCalledTimes(1))
    const body = (mockedPatch.mock.calls.at(-1)?.[1] ?? {}) as Record<string, unknown>
    expect(body).toMatchObject({
      ratio_bp: 0,
      confirm: true,
      reason: '内部压测，工单 OPS-123',
    })
  })

  it('PricingPage：缺成本价 400 展示 warnings，仍要保存带 confirm 重发', async () => {
    installMock()
    const user = await openGroupEditor('standard')
    const warning = '模型 qwen-max 缺成本价，地板未校验'
    mockedPatch.mockRejectedValueOnce(
      new ApiError({
        status: 400,
        code: 'invalid_request_body',
        type: 'invalid_request_body',
        message: '分组范围内存在无成本价的模型，毛利地板无法全部校验；确认继续请传 confirm=true',
        hint: '',
        warnings: [warning],
      }),
    )

    const ratio = screen.getByLabelText(/倍率（万分比）/)
    await user.clear(ratio)
    await user.type(ratio, '9000')
    await user.click(screen.getByRole('button', { name: '保存' }))

    const list = await screen.findByTestId('group-warning-list')
    expect(within(list).getByText(warning)).toBeInTheDocument()

    mockedPatch.mockResolvedValueOnce({
      group: groups[1],
      key_sync: { keys: 0, ok: true },
    })
    await user.click(screen.getByRole('button', { name: '仍要保存分组倍率' }))

    await waitFor(() => expect(mockedPatch).toHaveBeenCalledTimes(2))
    const body = (mockedPatch.mock.calls.at(-1)?.[1] ?? {}) as Record<string, unknown>
    expect(body).toEqual({ ratio_bp: 9000, confirm: true })
  })

  it('PricingPage：地板拒绝时原样展示后端消息（含差额与模型）', async () => {
    installMock()
    const user = await openGroupEditor('standard')
    const message =
      '分组 contract（倍率 6000）下 标准单价×倍率 = 18000 微积分/千 token，低于成本 20000 × 4/3 = 26667（18000 < 26667），差额 8667；涉及模型：qwen-max'
    mockedPatch.mockRejectedValueOnce(
      new ApiError({
        status: 400,
        code: 'invalid_request_body',
        type: 'invalid_request_body',
        message,
        hint: '',
      }),
    )

    const ratio = screen.getByLabelText(/倍率（万分比）/)
    await user.clear(ratio)
    await user.type(ratio, '6000')
    await user.click(screen.getByRole('button', { name: '保存' }))

    const alert = await screen.findByTestId('group-save-error')
    expect(alert).toHaveTextContent(message)
    expect(alert).toHaveTextContent('差额 8667')
    expect(alert).toHaveTextContent('涉及模型：qwen-max')
    expect(screen.queryByTestId('group-warning-confirm')).not.toBeInTheDocument()
  })

  async function openIntegrations() {
    const user = userEvent.setup()
    renderWithProviders(<AdminPricingPage />)
    await screen.findByText('qwen-max')
    await user.click(screen.getByRole('tab', { name: '工具集成' }))
    await screen.findByText('Cursor')
    return user
  }

  it('PricingPage：工具集成 5 行渲染且开关反映 enabled', async () => {
    installMock()
    await openIntegrations()

    const switches = screen.getAllByRole('switch')
    expect(switches).toHaveLength(5)
    expect(switches.map((node) => node.getAttribute('aria-checked'))).toEqual([
      'true',
      'false',
      'false',
      'false',
      'false',
    ])
    for (const name of ['Cursor', 'Claude Code', 'Cherry Studio', 'NextChat', 'OpenAI SDK']) {
      expect(screen.getByText(name)).toBeInTheDocument()
    }
    expect(screen.getByText(/只支持 \{key\} \/ \{base_url\} \/ \{model\}/)).toBeInTheDocument()
  })

  it('PricingPage：开启但模板为空时前端拦下且不发请求', async () => {
    installMock()
    const user = await openIntegrations()

    const row = screen.getByText('Claude Code').closest('tr')
    if (!row) throw new Error('未找到 Claude Code 行')
    const toggle = within(row).getByRole('switch', { name: '启用 Claude Code' })
    expect(toggle).toHaveAttribute('aria-checked', 'false')
    await user.click(toggle)
    expect(toggle).toHaveAttribute('aria-checked', 'true')

    await user.click(within(row).getByRole('button', { name: '保存 Claude Code' }))

    const alert = await screen.findByTestId('integration-error-claude-code')
    expect(alert).toHaveTextContent('开启工具 claude-code 前必须先配置深链模板')
    expect(screen.queryByRole('dialog', { name: '确认保存工具集成' })).not.toBeInTheDocument()
    expect(mockedPatch).not.toHaveBeenCalled()
  })

  it('PricingPage：后端 400 原样展示（含被点名的 {token}）', async () => {
    installMock()
    const message = '深链模板包含未允许的占位符 {token}；只支持 {key} / {base_url} / {model}'
    mockedPatch.mockRejectedValueOnce(
      new ApiError({
        status: 400,
        code: 'invalid_request_body',
        type: 'invalid_request_body',
        message,
        hint: '',
      }),
    )
    const user = await openIntegrations()

    const row = screen.getByText('Cursor').closest('tr')
    if (!row) throw new Error('未找到 Cursor 行')
    const input = within(row).getByLabelText('深链模板 Cursor')
    await user.clear(input)
    // userEvent treats `{` as a key descriptor; `{{` types a literal brace.
    await user.type(input, 'cursor://x?key={{token}')
    await user.click(within(row).getByRole('button', { name: '保存 Cursor' }))

    const dialog = await screen.findByRole('dialog', { name: '确认保存工具集成' })
    await user.click(within(dialog).getByRole('button', { name: '确认保存工具集成' }))

    const alert = await screen.findByTestId('integration-error-cursor')
    expect(alert).toHaveTextContent(message)
    expect(alert).toHaveTextContent('{token}')

    await waitFor(() => expect(mockedPatch).toHaveBeenCalledTimes(1))
    const sent = (mockedPatch.mock.calls.at(-1)?.[1] ?? {}) as Record<string, unknown>
    expect(sent.deep_link_template).toBe('cursor://x?key={token}')
  })
})

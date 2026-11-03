import { cleanup, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from '@/api/client'
import type { Invoice, Order, Plan, PlanList, Refund } from '@/api/console'
import { renderWithProviders } from '@/test/renderConsole'
import RechargePage from './RechargePage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPost = vi.mocked(api.post)

function plan(overrides: Partial<Plan> = {}): Plan {
  return {
    id: 2,
    code: 'STANDARD',
    name: '标准版',
    price_fen: 9900,
    price_yuan: 99,
    grant_recharge_micro: 99_000_000,
    grant_bonus_micro: 20_000_000,
    grant_recharge: 9900,
    grant_bonus: 2000,
    bonus_valid_days: 90,
    rpm: 300,
    tpm: 630_000,
    concurrency: 110,
    daily_quota_micro: 50_000_000,
    daily_quota: 5000,
    permanent: true,
    purchasable: true,
    sort_order: 1,
    ...overrides,
  }
}

const plans: PlanList = {
  items: [
    plan({
      id: 1,
      code: 'TRIAL',
      name: '体验版',
      price_fen: 0,
      price_yuan: 0,
      grant_recharge: 0,
      grant_bonus: 7000,
      purchasable: false,
      sort_order: 0,
    }),
    plan(),
    plan({ id: 3, code: 'PRO', name: '专业版', sort_order: 2 }),
    plan({ id: 4, code: 'ENTERPRISE', name: '企业版', sort_order: 3 }),
    plan({
      id: 5,
      code: 'FREE_DEFAULT',
      name: '默认档',
      price_fen: 0,
      price_yuan: 0,
      grant_recharge: 0,
      grant_bonus: 0,
      purchasable: false,
      sort_order: 4,
    }),
  ],
  default_tier: 'FREE_DEFAULT',
  default_tier_label: 'FREE_DEFAULT',
  trial_days: 7,
  bonus_expire_days: 90,
  micro_per_credit: 10_000,
}

function order(overrides: Partial<Order> = {}): Order {
  return {
    id: 1,
    order_no: '20261027120000abcdefghijklmn',
    order_type: 'PLAN',
    plan_code: 'STANDARD',
    pay_amount_fen: 9900,
    pay_amount_yuan: 99,
    recharge_micro: 99_000_000,
    bonus_micro: 20_000_000,
    recharge: 9900,
    bonus: 2000,
    pay_channel: 'WECHAT',
    state: 'PAID',
    paid_at: '2026-10-27T04:00:00Z',
    expire_at: '2026-10-27T04:30:00Z',
    created_at: '2026-10-27T04:00:00Z',
    ...overrides,
  }
}

function list<T>(items: T[]) {
  return { items, page: 1, page_size: 20, total: items.length }
}

interface MockOptions {
  orders?: Order[]
  refunds?: Refund[]
  invoices?: Invoice[]
}

function installMock(options: MockOptions = {}) {
  const orders = options.orders ?? [order()]
  mockedGet.mockImplementation((path: string) => {
    if (path === '/api/console/plans') return Promise.resolve(plans)
    if (path.startsWith('/api/console/orders?')) return Promise.resolve(list(orders))
    if (path.startsWith('/api/console/refunds?')) return Promise.resolve(list(options.refunds ?? []))
    if (path.startsWith('/api/console/invoices?')) {
      return Promise.resolve(list(options.invoices ?? []))
    }
    return Promise.reject(new Error(`unexpected ${path}`))
  })
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPost.mockReset()
})

/**
 * 自动选中档位发生在 plans 到达之后的 effect 里，而档位按钮在首帧就已渲染。
 * 因此 `findByRole` 一 resolve 就同步断言 `aria-pressed` 是竞态：满负载并行跑
 * 时 effect 还没跑完就断言，测试会随机红。统一等 UI 落定再断言。
 */
async function expectTierPressed(
  name: string,
  pressed: 'true' | 'false' = 'true',
): Promise<void> {
  await waitFor(
    () => {
      expect(screen.getByRole('button', { name })).toHaveAttribute('aria-pressed', pressed)
    },
    { timeout: 3_000 },
  )
}

afterEach(cleanup)

describe('RechargePage', () => {
  it('pre-selects the first purchasable tier and disables the default/trial cards', async () => {
    installMock()
    renderWithProviders(<RechargePage />)

    await screen.findByRole('button', { name: '选择套餐 标准版' })
    await expectTierPressed('选择套餐 标准版')
    expect(screen.getByRole('button', { name: '选择套餐 默认档' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '选择套餐 体验版' })).toBeDisabled()
    expect(screen.getAllByText('不可购买')).toHaveLength(2)
    // No ?plan= means no deep-link notice either.
    expect(screen.queryByTestId('plan-notice')).not.toBeInTheDocument()
  })

  it('F2-1: ?plan=Pro pre-selects the matching tier case-insensitively', async () => {
    installMock()
    renderWithProviders(<RechargePage />, { route: '/console/recharge?plan=Pro' })

    await screen.findByRole('button', { name: '选择套餐 专业版' })
    await expectTierPressed('选择套餐 专业版')
    await expectTierPressed('选择套餐 标准版', 'false')
    expect(screen.queryByTestId('plan-notice')).not.toBeInTheDocument()
  })

  it('F2-1: an unknown ?plan= falls back to the first purchasable tier and says so', async () => {
    installMock()
    renderWithProviders(<RechargePage />, { route: '/console/recharge?plan=nope' })

    await screen.findByRole('button', { name: '选择套餐 标准版' })
    await expectTierPressed('选择套餐 标准版')
    expect(await screen.findByTestId('plan-notice')).toHaveTextContent(
      '未找到目标档位「nope」，已为你选择第一个可购买档位。',
    )
  })

  it('F2-1: a non-purchasable ?plan= is never pre-selected and reports why', async () => {
    installMock()
    renderWithProviders(<RechargePage />, { route: '/console/recharge?plan=free_default' })

    await screen.findByRole('button', { name: '选择套餐 标准版' })
    await expectTierPressed('选择套餐 标准版')
    await expectTierPressed('选择套餐 默认档', 'false')
    expect(await screen.findByTestId('plan-notice')).toHaveTextContent(
      '目标档位 FREE_DEFAULT 不可直接购买，已为你选择第一个可购买档位。',
    )
  })

  it('F10-4: order status and order-number search are sent together as an intersection', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<RechargePage />)

    await user.click(screen.getByRole('tab', { name: '订单' }))
    await user.click(screen.getByRole('tab', { name: '订单' }))
    await screen.findByText('20261027120000abcdefghijklmn')
    await user.type(screen.getByLabelText('按订单号搜索'), '202610')
    await user.selectOptions(screen.getByLabelText('订单状态'), 'pending')

    await waitFor(
      () => {
        const calls = mockedGet.mock.calls
          .map((call) => call[0] as string)
          .filter((path) => path.startsWith('/api/console/orders?'))
        const params = new URL(calls.at(-1) ?? '', 'http://localhost').searchParams
        expect(params.get('q')).toBe('202610')
        expect(params.get('status')).toBe('pending')
      },
      { timeout: 3_000 },
    )
  })

  it('F10-2: a created order stays 待支付 and never announces an arrived balance', async () => {
    installMock({ orders: [] })
    mockedPost.mockResolvedValue({
      order: order({ state: 'PENDING', paid_at: null }),
      idempotent_replay: false,
    })
    const user = userEvent.setup()
    renderWithProviders(<RechargePage />)

    const buy = await screen.findByRole('button', { name: '立即购买' })
    expect(buy).toBeEnabled()
    await user.click(buy)

    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: '创建订单' }))

    expect(await within(dialog).findByText('待支付')).toBeInTheDocument()
    expect(within(dialog).queryByText(/已到账/)).not.toBeInTheDocument()
    expect(screen.queryByText(/积分已到账/)).not.toBeInTheDocument()

    const [path, body] = mockedPost.mock.calls[0]
    expect(path).toBe('/api/console/orders')
    expect(body).toMatchObject({
      order_type: 'PLAN',
      plan_id: 2,
      pay_channel: 'WECHAT',
    })
    expect(body).not.toHaveProperty('pay_amount_fen')
  })

  it('F10-3: a re-used redemption code shows the backend reason and hint', async () => {
    installMock()
    mockedPost.mockRejectedValue(
      new ApiError({
        status: 400,
        type: 'invalid_request_error',
        code: 'invalid_request_body',
        message: '兑换码已被使用',
        hint: '每个兑换码仅可使用一次',
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<RechargePage />)

    await user.click(screen.getByRole('tab', { name: '订单' }))
    await user.click(screen.getByRole('tab', { name: '订单' }))
    await screen.findByText('20261027120000abcdefghijklmn')
    await user.click(screen.getByRole('tab', { name: '购买' }))
    await user.click(screen.getByRole('tab', { name: '购买' }))
    await user.type(screen.getByLabelText('兑换码'), 'ABC123XYZ')
    await user.click(screen.getByRole('button', { name: '兑换' }))

    expect(await screen.findByText('兑换码已被使用')).toBeInTheDocument()
    expect(screen.getByText('每个兑换码仅可使用一次')).toBeInTheDocument()
  })

  it('B-44: a 50 元 voucher shows 5,000 积分 — the 元 figure must never be formatted as credits', async () => {
    installMock()
    // The real handler answers amount = microToFen(amount_micro) ÷ 100, i.e. 元:
    // a 50 元 code carries amount 50 while it credits 5 000 积分. Rendering
    // `amount` understated the arrival by 100×.
    mockedPost.mockResolvedValue({
      batch_name: '2026 双十一 · 50 元券',
      amount_micro: 50_000_000,
      amount: 50,
      credit_target: 'BONUS',
      expire_at: '2027-01-28T07:41:32.079Z',
      balance_after_micro: 878_564_765,
    })
    const user = userEvent.setup()
    renderWithProviders(<RechargePage />)

    await user.click(screen.getByRole('tab', { name: '订单' }))
    await user.click(screen.getByRole('tab', { name: '订单' }))
    await screen.findByText('20261027120000abcdefghijklmn')
    await user.click(screen.getByRole('tab', { name: '购买' }))
    await user.click(screen.getByRole('tab', { name: '购买' }))
    await user.type(screen.getByLabelText('兑换码'), 'ABC123XYZ')
    await user.click(screen.getByRole('button', { name: '兑换' }))

    expect(await screen.findByText('兑换成功：2026 双十一 · 50 元券')).toBeInTheDocument()
    expect(screen.getByText('到账 5,000 积分')).toBeInTheDocument()
    // The regression: 50 is the 元 amount, not the credit count.
    expect(screen.queryByText('到账 50 积分')).not.toBeInTheDocument()
    expect(screen.getByText(/当前余额 87856\.4765 积分/)).toBeInTheDocument()
  })

  it('shows the backend resource_state_conflict reason when a refund is rejected', async () => {
    installMock()
    mockedPost.mockRejectedValue(
      new ApiError({
        status: 409,
        type: 'conflict',
        code: 'resource_state_conflict',
        message: '该账号已有进行中的退款单，请等待处理完成后再申请',
        hint: '',
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<RechargePage />)

    await user.click(screen.getByRole('tab', { name: '订单' }))
    await user.click(screen.getByRole('tab', { name: '订单' }))
    await screen.findByText('20261027120000abcdefghijklmn')
    await user.click(screen.getByRole('button', { name: '退款' }))

    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: '提交退款申请' }))

    expect(
      await within(dialog).findByText('该账号已有进行中的退款单，请等待处理完成后再申请'),
    ).toBeInTheDocument()
    expect(mockedPost.mock.calls[0][1]).toMatchObject({ recharge_order_id: 1 })
  })

  it('shows real-name guidance instead of a red error for 403 account_not_verified', async () => {
    installMock()
    mockedPost.mockRejectedValue(
      new ApiError({
        status: 403,
        type: 'authorization_error',
        code: 'account_not_verified',
        message: '申请开票需先完成实名认证',
        hint: '完成实名认证后可申请开票',
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<RechargePage />)

    await user.click(screen.getByRole('tab', { name: '订单' }))
    await user.click(screen.getByRole('tab', { name: '订单' }))
    await screen.findByText('20261027120000abcdefghijklmn')
    await user.click(screen.getByRole('button', { name: '开票' }))

    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText(/发票抬头/), '云枢科技')
    await user.type(within(dialog).getByLabelText(/接收邮箱/), 'billing@example.com')
    await user.click(within(dialog).getByRole('button', { name: '提交开票申请' }))

    expect(await within(dialog).findByText('该账号未完成实名认证')).toBeInTheDocument()
    expect(within(dialog).getByText('申请开票需先完成实名认证')).toBeInTheDocument()
    expect(within(dialog).getByText('完成实名认证后可申请开票')).toBeInTheDocument()
    expect(within(dialog).getByRole('link', { name: '提交工单' })).toBeInTheDocument()
    // Guidance is a status message, not an error alert.
    expect(within(dialog).queryByRole('alert')).not.toBeInTheDocument()

    const [, body] = mockedPost.mock.calls[0]
    expect(body).toMatchObject({
      recharge_order_id: 1,
      title_type: 'PERSONAL',
      title: '云枢科技',
      receive_email: 'billing@example.com',
    })
    expect(body).not.toHaveProperty('tax_no')
    expect(body).not.toHaveProperty('amount_fen')
  })
})

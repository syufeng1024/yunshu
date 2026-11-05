import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import type {
  ConsoleModel,
  ConsoleOverview,
  KeyListItem,
  KeyListResponse,
} from '@/api/console'
import { renderWithProviders } from '@/test/renderConsole'
import KeysPage from './KeysPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPost = vi.mocked(api.post)
const mockedPatch = vi.mocked(api.patch)

function key(overrides: Partial<KeyListItem> = {}): KeyListItem {
  return {
    id: 1,
    key_name: 'prod-key',
    key_masked: 'sk-abc123****wxyz',
    status: 'ENABLED',
    expired: false,
    group_code: 'standard',
    group_ratio_bp: 10_000,
    model_scope_mode: 'ALL',
    model_count: 0,
    ip_whitelist_mode: 'OFF',
    ip_rule_count: 0,
    expire_at: '2030-01-01T00:00:00Z',
    today_consumed_micro: 12_340_000,
    created_at: '2026-10-01T00:00:00Z',
    last_used_at: null,
    ...overrides,
  }
}

function page(items: KeyListItem[]): KeyListResponse {
  return { items, page: 1, page_size: 20, total: items.length }
}

const overview: ConsoleOverview = {
  balance: { micro: 0, credits: '0', cny: '0.00', recharge_micro: 0, bonus_micro: 0 },
  today: { requests: 0, consumed_micro: 0, consumed_credits: '0', success_rate_bp: null },
  hourly: [],
  plan: { code: 'STANDARD', name: '标准版', policy_source: 'PLAN', trial_expire_at: null },
  quota: {
    rpm: 60,
    tpm: 100_000,
    concurrency: 5,
    daily_quota_micro: 50_000_000,
    used_today_micro: 0,
    remaining_micro: 50_000_000,
  },
  eta: {
    reachable: false,
    reason: 'NO_RECENT_USAGE',
    eta_at: null,
    seconds_left: null,
    burn_micro_per_hour: 0,
  },
}

const models: ConsoleModel[] = [
  {
    id: 11,
    model_name: 'text-large',
    display_name: 'Text Large',
    model_type: 'TEXT',
    context_length: 128_000,
    protocol: 'OPENAI',
    aliases: [],
  },
]

interface MockOptions {
  planCode?: string
  activeItems?: KeyListItem[]
  listItems?: KeyListItem[]
}

function installMock(options: MockOptions = {}) {
  const listItems = options.listItems ?? [key()]
  const activeItems = options.activeItems ?? listItems
  const plan = { ...overview.plan, code: options.planCode ?? 'STANDARD' }

  mockedGet.mockImplementation((path: string) => {
    if (path === '/api/console/overview') {
      return Promise.resolve({ ...overview, plan })
    }
    if (path.startsWith('/api/console/keys?')) {
      const params = new URL(path, 'http://localhost').searchParams
      if (params.get('status') === 'ENABLED' && params.get('page_size') === '100') {
        return Promise.resolve(page(activeItems))
      }
      return Promise.resolve(page(listItems))
    }
    if (path.startsWith('/api/console/models')) {
      return Promise.resolve({ items: models, page: 1, page_size: 100, total: models.length })
    }
    return Promise.reject(new Error(`unexpected ${path}`))
  })
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPost.mockReset()
  mockedPatch.mockReset()
})

afterEach(cleanup)

describe('KeysPage', () => {
  it('sorts by the clicked column and asks the server for that order', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<KeysPage />)
    await screen.findByText('prod-key')

    // Default order is created_at DESC (matches the backend default); clicking
    // the header flips the direction and must reach the API, not sort one page
    // locally — local sorting would only reorder the 20 visible rows.
    await user.click(screen.getByRole('button', { name: '按 创建时间 排序' }))

    await waitFor(() => {
      const last = mockedGet.mock.calls
        .map((call) => String(call[0]))
        .filter((path) => path.startsWith('/api/console/keys?'))
        .at(-1)
      expect(last).toContain('sort=created_at')
      expect(last).toContain('order=asc')
    })
  })

  it('changes the page size through the pagination switcher', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<KeysPage />)
    await screen.findByText('prod-key')

    await user.selectOptions(screen.getByLabelText('每页条数'), '50')

    await waitFor(() => {
      const last = mockedGet.mock.calls
        .map((call) => String(call[0]))
        .filter((path) => path.startsWith('/api/console/keys?'))
        .at(-1)
      expect(last).toContain('page_size=50')
    })
  })

  it('sorts by the clicked column and asks the server for that order', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<KeysPage />)
    await screen.findByText('prod-key')

    // Default order is created_at DESC (matches the backend default); clicking
    // the header flips the direction and must reach the API, not sort one page
    // locally — local sorting would only reorder the 20 visible rows.
    await user.click(screen.getByRole('button', { name: '按 创建时间 排序' }))

    await waitFor(() => {
      const last = mockedGet.mock.calls
        .map((call) => String(call[0]))
        .filter((path) => path.startsWith('/api/console/keys?'))
        .at(-1)
      expect(last).toContain('sort=created_at')
      expect(last).toContain('order=asc')
    })
  })

  it('changes the page size through the pagination switcher', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<KeysPage />)
    await screen.findByText('prod-key')

    await user.selectOptions(screen.getByLabelText('每页条数'), '50')

    await waitFor(() => {
      const last = mockedGet.mock.calls
        .map((call) => String(call[0]))
        .filter((path) => path.startsWith('/api/console/keys?'))
        .at(-1)
      expect(last).toContain('page_size=50')
    })
  })

  it('A3 / F6-1: sends name search and status filter together so the API returns the intersection', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<KeysPage />)
    await screen.findByText('prod-key')

    await user.type(screen.getByLabelText(/按名称搜索/), 'prod')
    await user.selectOptions(screen.getByLabelText('状态'), 'DISABLED')

    await waitFor(
      () => {
        const calls = mockedGet.mock.calls
          .map((call) => call[0])
          .filter(
            (path) =>
              path.startsWith('/api/console/keys?') &&
              !(path.includes('status=ENABLED') && path.includes('page_size=100')),
          )
        const last = calls[calls.length - 1] ?? ''
        const params = new URL(last, 'http://localhost').searchParams
        expect(params.get('q')).toBe('prod')
        expect(params.get('status')).toBe('DISABLED')
      },
      { timeout: 3_000 },
    )
  })

  it('A6 / F6-4: cancelling the disable confirmation leaves the status unchanged', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<KeysPage />)
    await screen.findByText('prod-key')

    await user.click(screen.getByRole('button', { name: '停用' }))
    expect(screen.getByText('停用密钥')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '取消' }))

    expect(mockedPatch).not.toHaveBeenCalled()
    const table = screen.getByRole('table', { name: '密钥列表' })
    expect(within(table).getByText('启用')).toBeInTheDocument()
    expect(screen.queryByText('停用密钥')).not.toBeInTheDocument()
  })

  it('A9 / F6-7: greys out the create button when the free tier is at its active-key ceiling', async () => {
    const three = [key(), key({ id: 2, key_name: 'b' }), key({ id: 3, key_name: 'c' })]
    installMock({ planCode: 'FREE_DEFAULT', activeItems: three, listItems: three })
    renderWithProviders(<KeysPage />)

    const create = await screen.findByRole('button', { name: '新建密钥' })
    await waitFor(() => expect(create).toBeDisabled())
    expect(screen.getByText(/已达免费档活跃 Key 上限/)).toBeInTheDocument()
  })

  it('F6-5: 创建密钥拒绝 1–10 之外的数量，且不发出请求', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<KeysPage />)
    await screen.findByText('prod-key')

    await user.click(screen.getByRole('button', { name: '新建密钥' }))
    await user.type(screen.getByLabelText(/密钥名称/), 'prod-key')
    const count = screen.getByLabelText(/创建数量/)
    await user.clear(count)
    await user.type(count, '11')
    await user.click(screen.getByRole('button', { name: '生成密钥' }))

    expect(await screen.findByText(/创建数量需为 1–10 的整数/)).toBeInTheDocument()
    expect(mockedPost).not.toHaveBeenCalled()
  })

  it('A5 / F6-3: the plaintext is shown once, and only the masked prefix remains after closing', async () => {
    installMock()
    mockedPost.mockResolvedValue({
      items: [
        {
          id: 99,
          key_name: 'prod-key',
          key: 'sk-plain-secret-123456',
          key_prefix: 'sk-plain',
          key_last4: '3456',
          key_masked: 'sk-plain****3456',
          model_scope_mode: 'ALL',
          model_ids: [],
          group_code: 'standard',
          expire_at: '2030-01-01T00:00:00Z',
          ip_allowlist: [],
        },
      ],
      count: 1,
      warning: '完整密钥仅本次返回，关闭后无法再次查看',
    })

    const user = userEvent.setup()
    renderWithProviders(<KeysPage />)
    await screen.findByText('prod-key')

    await user.click(screen.getByRole('button', { name: '新建密钥' }))
    await user.type(screen.getByLabelText(/密钥名称/), 'prod-key')
    fireEvent.change(screen.getByLabelText(/到期时间/), {
      target: { value: '2030-01-01T10:00' },
    })
    await user.click(screen.getByRole('button', { name: '生成密钥' }))

    expect(await screen.findByTestId('revealed-key-99')).toHaveTextContent('sk-plain-secret-123456')

    const body = mockedPost.mock.calls[0][1] as Record<string, unknown>
    expect(body).not.toHaveProperty('group_code')
    expect(body.model_scope_mode).toBe('ALL')
    expect(body).not.toHaveProperty('model_ids')

    await user.click(screen.getByRole('button', { name: '我已保存，关闭' }))

    await waitFor(() =>
      expect(screen.queryByText('sk-plain-secret-123456')).not.toBeInTheDocument(),
    )
    expect(screen.getByText('sk-abc123****wxyz')).toBeInTheDocument()
  })
})

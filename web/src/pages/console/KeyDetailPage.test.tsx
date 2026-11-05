import { cleanup, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from '@/api/client'
import type { KeyAuditResponse, KeyDetail } from '@/api/console'
import { renderWithProviders } from '@/test/renderConsole'
import KeyDetailPage from './KeyDetailPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPatch = vi.mocked(api.patch)

const detail: KeyDetail = {
  id: 7,
  key_name: 'prod-key',
  key_masked: 'sk-abc123****wxyz',
  key_prefix: 'sk-abc123',
  status: 'ENABLED',
  expired: false,
  group_code: 'standard',
  group_ratio_bp: 12_000,
  model_scope_mode: 'SELECTED',
  models: [{ id: 11, model_name: 'text-large', display_name: 'Text Large' }],
  ip_whitelist_mode: 'ON',
  ip_allowlist: [{ cidr: '203.0.113.0/24', ip_family: 4 }],
  expire_at: '2030-01-01T00:00:00Z',
  remaining_days: 1000,
  created_at: '2026-10-01T00:00:00Z',
  last_used_at: null,
  realtime: {
    as_of: '2026-10-27T10:00:00Z',
    available: true,
    rpm_used: 12,
    rpm_limit: 60,
    tpm_used: 1200,
    tpm_limit: 100_000,
    concurrency_used: 1,
    concurrency_limit: 5,
  },
  account_quota: {
    used_today_micro: 1_000_000,
    daily_quota_micro: 50_000_000,
    remaining_micro: 49_000_000,
    plan_code: 'STANDARD',
    plan_name: '标准版',
    policy_source: 'PLAN',
  },
}

const audit: KeyAuditResponse = {
  items: [
    {
      action: 'MODEL_SCOPE_CHANGE',
      before: { model_scope_mode: 'ALL', model_ids: [] },
      after: { model_scope_mode: 'SELECTED', model_ids: [11] },
      operator_id: '42',
      created_at: '2026-10-20T00:00:00Z',
    },
  ],
  page: 1,
  page_size: 20,
  total: 1,
}

const models = [
  {
    id: 11,
    model_name: 'text-large',
    display_name: 'Text Large',
    model_type: 'TEXT',
    context_length: 128_000,
    protocol: 'OPENAI',
    aliases: [],
  },
  {
    id: 12,
    model_name: 'text-small',
    display_name: 'Text Small',
    model_type: 'TEXT',
    context_length: 32_000,
    protocol: 'OPENAI',
    aliases: [],
  },
]

function installMock(data: KeyDetail = detail) {
  mockedGet.mockImplementation((path: string) => {
    if (path.startsWith('/api/console/keys/7/audit')) return Promise.resolve(audit)
    if (path.startsWith('/api/console/keys/7')) return Promise.resolve(data)
    if (path.startsWith('/api/console/models')) {
      return Promise.resolve({ items: models, page: 1, page_size: 100, total: models.length })
    }
    return Promise.reject(new Error(`unexpected ${path}`))
  })
}

function renderDetail() {
  return renderWithProviders(<KeyDetailPage />, {
    route: '/console/keys/7',
    path: '/console/keys/:id',
  })
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPatch.mockReset()
})

afterEach(cleanup)

describe('KeyDetailPage', () => {
  it('F7-1: a key that is not owned renders the server error without leaking existence', async () => {
    mockedGet.mockRejectedValue(
      new ApiError({
        status: 404,
        type: 'invalid_request_error',
        code: 'key_not_found',
        message: '密钥不存在',
        hint: '控制台 → 密钥管理页刷新列表',
      }),
    )
    renderDetail()

    expect(await screen.findByText('无法加载密钥')).toBeInTheDocument()
    expect(screen.getByText('密钥不存在')).toBeInTheDocument()
    expect(screen.queryByText('prod-key')).not.toBeInTheDocument()
  })

  it('F6-6 / F7-4: the group is read-only and has no upgrade, downgrade or switch control', async () => {
    installMock()
    renderDetail()

    await screen.findByText('prod-key')
    expect(screen.getByText('只读')).toBeInTheDocument()
    expect(screen.getByText('标准')).toBeInTheDocument()
    expect(screen.queryByText(/升档|降档|切换分组|变更分组/)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /分组/ })).not.toBeInTheDocument()
  })

  it('F7-2: polls realtime limits and shows the masked key only', async () => {
    installMock()
    renderDetail()

    await screen.findByText('prod-key')
    // 说明文案讲的是「数值怎么读」，不再是实现细节（每 8 秒刷新一次）。
    expect(screen.getByText(/已用 \/ 上限/)).toBeInTheDocument()
    expect(screen.getByText('sk-abc123****wxyz')).toBeInTheDocument()
    // 12/60 is the initial detail payload; the poll replaces it with the same
    // numbers in this mock. No "--" appears because the data is available.
    expect(screen.getByText('12')).toBeInTheDocument()
    expect(screen.getByText(/\/ 60/)).toBeInTheDocument()
    expect(screen.queryByText('--')).not.toBeInTheDocument()
  })

  it('F7-3: editing model permissions requires confirmation and the audit log keeps before/after', async () => {
    installMock()
    const user = userEvent.setup()
    renderDetail()

    await screen.findByText('prod-key')
    // The audit table shows the previous change with both sides.
    expect(await screen.findByText('模型权限变更')).toBeInTheDocument()
    expect(screen.getByText('变更前')).toBeInTheDocument()
    expect(screen.getByText('变更后')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '编辑模型权限' }))

    const second = await screen.findByRole('checkbox', { name: /Text Small/ })
    await user.click(second)
    await user.click(screen.getByRole('button', { name: '保存' }))

    expect(screen.getByText('确认修改模型权限')).toBeInTheDocument()
    expect(mockedPatch).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: '确认保存' }))

    await waitFor(() => {
      expect(mockedPatch).toHaveBeenCalledWith('/api/console/keys/7', {
        model_scope_mode: 'SELECTED',
        model_ids: [11, 12],
      })
    })
  })
})

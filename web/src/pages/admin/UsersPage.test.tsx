import { cleanup, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import type { AdminUser, AdminUserDetailResponse } from '@/api/admin'
import { ToastViewport } from '@/components'
import { useAuthStore } from '@/stores/auth'
import { useToastStore } from '@/stores/toast'
import { renderWithProviders } from '@/test/renderConsole'
import AdminUsersPage, { detectUserSearchField } from './UsersPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPost = vi.mocked(api.post)

const demoUser: AdminUser = {
  id: 501,
  user_no: 'U202600501',
  email: 'ops-demo@example.com',
  phone: '13800138000',
  nickname: '演示账号',
  role: 'PERSONAL',
  status: 'NORMAL',
  risk_score: 12,
  plan_id: 2,
  plan_code: 'STANDARD',
  plan_name: '标准版',
  key_count: 2,
  created_at: '2026-03-01T02:00:00Z',
  updated_at: '2026-10-01T02:00:00Z',
}

const detail: AdminUserDetailResponse = {
  user: demoUser,
  balance: {
    exists: true,
    balance_recharge_micro: 10_000_000,
    balance_bonus_micro: 5_000_000,
    balance_micro: 15_000_000,
    frozen_refund_micro: 0,
    overdraft_limit_micro: 0,
    total_recharge_micro: 10_000_000,
    total_bonus_micro: 5_000_000,
    total_consumed_micro: 0,
    last_settled_at: null,
  },
  latest_balance_adjust: {
    id: 9001,
    direction: 'INCREASE',
    amount_micro: 50_000_000,
    reason: '上游故障补偿',
    state: 'PENDING',
    applicant_id: 9001,
    decided_by: null,
    decided_at: null,
    created_at: '2026-10-27T02:00:00Z',
  },
}

const emptyAudit = {
  items: [],
  page: 1,
  page_size: 50,
  total: 0,
  from: '2026-10-21T00:00:00Z',
  to: '2026-10-28T00:00:00Z',
}

function installMock() {
  mockedGet.mockImplementation((path: string) => {
    if (path.startsWith('/api/admin/users?')) {
      return Promise.resolve({ items: [demoUser], page: 1, page_size: 20, total: 1 })
    }
    if (path === '/api/admin/users/501') return Promise.resolve(detail)
    if (path.startsWith('/api/admin/audit-logs?')) return Promise.resolve(emptyAudit)
    return Promise.reject(new Error(`unexpected ${path}`))
  })
}

beforeEach(() => {
  mockedGet.mockReset()
  mockedPost.mockReset()
  useToastStore.setState({ toasts: [] })
})

afterEach(() => {
  cleanup()
  // The provisioning entry points are role-gated in the UI; a leaked session
  // would make the "not visible" case below pass for the wrong reason.
  useAuthStore.setState({ status: 'anonymous', user: null, error: null, hint: null })
})

const superAdminUser = {
  id: '2',
  email: 'admin@example.com',
  display_name: '超级管理员',
  role: 'SUPER_ADMIN' as const,
  status: 'NORMAL' as const,
  csrf_token: 'test-csrf',
}

const opsUser = {
  ...superAdminUser,
  id: '3',
  email: 'ops@example.com',
  display_name: '运营甲',
  role: 'OPS' as const,
}

describe('UsersPage', () => {
  it('用户名字段按 username 查询，用户号仍按 user_no', () => {
    // 自动判定顺序决定了内置超管能不能被搜到：admin 没有邮箱/手机号，
    // 如果被当成 user_no 查询，结果永远是空列表。
    expect(detectUserSearchField('admin')).toBe('username')
    expect(detectUserSearchField('ops.zhang')).toBe('username')
    expect(detectUserSearchField('U20260301001')).toBe('user_no')
    expect(detectUserSearchField('ops-demo@example.com')).toBe('email')
    expect(detectUserSearchField('13800138000')).toBe('phone')
  })

  it('列表展示 username，且不把空邮箱/手机号渲染成两行破折号', async () => {
    const usernameOnly: AdminUser = {
      ...demoUser,
      id: 777,
      user_no: 'U202600777',
      username: 'admin',
      email: undefined,
      phone: undefined,
      nickname: '超级管理员',
      role: 'SUPER_ADMIN',
    }
    mockedGet.mockImplementation((path: string) => {
      if (path.startsWith('/api/admin/users?')) {
        return Promise.resolve({ items: [usernameOnly], page: 1, page_size: 20, total: 1 })
      }
      return Promise.resolve(emptyAudit)
    })
    renderWithProviders(<AdminUsersPage />)

    expect(await screen.findByText('@admin')).toBeInTheDocument()
    // 该行只有用户名，不应出现两个占位破折号淹没它。
    expect(screen.queryAllByText('—')).toHaveLength(0)
  })

  it('用户名字段按 username 查询，用户号仍按 user_no', () => {
    // 自动判定顺序决定了内置超管能不能被搜到：admin 没有邮箱/手机号，
    // 如果被当成 user_no 查询，结果永远是空列表。
    expect(detectUserSearchField('admin')).toBe('username')
    expect(detectUserSearchField('ops.zhang')).toBe('username')
    expect(detectUserSearchField('U20260301001')).toBe('user_no')
    expect(detectUserSearchField('ops-demo@example.com')).toBe('email')
    expect(detectUserSearchField('13800138000')).toBe('phone')
  })

  it('列表展示 username，且不把空邮箱/手机号渲染成两行破折号', async () => {
    const usernameOnly: AdminUser = {
      ...demoUser,
      id: 777,
      user_no: 'U202600777',
      username: 'admin',
      email: undefined,
      phone: undefined,
      nickname: '超级管理员',
      role: 'SUPER_ADMIN',
    }
    mockedGet.mockImplementation((path: string) => {
      if (path.startsWith('/api/admin/users?')) {
        return Promise.resolve({ items: [usernameOnly], page: 1, page_size: 20, total: 1 })
      }
      return Promise.resolve(emptyAudit)
    })
    renderWithProviders(<AdminUsersPage />)

    expect(await screen.findByText('@admin')).toBeInTheDocument()
    // 该行只有用户名，不应出现两个占位破折号淹没它。
    expect(screen.queryAllByText('—')).toHaveLength(0)
  })

  it('UsersPage：搜索按字段发出对应参数', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<AdminUsersPage />)
    await screen.findByText('演示账号')

    await user.selectOptions(screen.getByLabelText('搜索字段'), 'phone')
    await user.type(screen.getByLabelText('搜索用户'), '13800138000')

    await waitFor(
      () => {
        const calls = mockedGet.mock.calls
          .map((call) => call[0])
          .filter((path) => path.startsWith('/api/admin/users?'))
        const last = calls[calls.length - 1] ?? ''
        const params = new URL(last, 'http://localhost').searchParams
        expect(params.get('phone')).toBe('13800138000')
      },
      { timeout: 3_000 },
    )

    const calls = mockedGet.mock.calls
      .map((call) => call[0])
      .filter((path) => path.startsWith('/api/admin/users?'))
    const params = new URL(calls[calls.length - 1] ?? '', 'http://localhost').searchParams
    expect(params.has('q')).toBe(false)
  })

  it('UsersPage：余额调整可提交并展示最近一张调整单', async () => {
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<AdminUsersPage />)
    await screen.findByText('演示账号')

    await user.click(screen.getByRole('button', { name: '详情' }))
    await screen.findByText('总余额')

    expect(screen.getByRole('button', { name: '调整余额' })).toBeEnabled()
    expect(screen.queryByText(/后端暂未提供/)).not.toBeInTheDocument()
    expect(screen.getByText('2 个活跃 Key')).toBeInTheDocument()
    expect(screen.getByText('待审核')).toBeInTheDocument()
    expect(screen.getByText('上游故障补偿')).toBeInTheDocument()
  })

  it('UsersPage：余额调整提交后提示已提交审核', async () => {
    installMock()
    mockedPost.mockResolvedValue({
      balance_adjust: {
        id: 9002,
        user_id: 501,
        direction: 'INCREASE',
        amount_micro: 5_000_000,
        reason: '客服补偿积分',
        state: 'PENDING',
        applicant_id: 9001,
        created_at: '2026-10-28T02:00:00Z',
        updated_at: '2026-10-28T02:00:00Z',
      },
      review_task_id: 61,
    })
    const user = userEvent.setup()
    renderWithProviders(
      <>
        <AdminUsersPage />
        <ToastViewport />
      </>,
    )
    await screen.findByText('演示账号')

    await user.click(screen.getByRole('button', { name: '详情' }))
    await screen.findByText('总余额')
    await user.click(screen.getByRole('button', { name: '调整余额' }))
    await user.type(screen.getByLabelText(/金额（微积分/), '5000000')
    await user.type(screen.getByLabelText(/调整理由/), '客服补偿积分')
    await user.click(screen.getByRole('button', { name: '提交审核' }))

    expect(await screen.findByText('已提交审核')).toBeInTheDocument()
    const [path, body] = mockedPost.mock.calls.at(-1) ?? []
    expect(path).toBe('/api/admin/users/501/balance-adjust')
    expect(body).toMatchObject({
      direction: 'INCREASE',
      amount_micro: 5_000_000,
      reason: '客服补偿积分',
    })
  })

  it('UsersPage：超级管理员可新建运营账号，角色下拉不含个人用户', async () => {
    useAuthStore.setState({ status: 'authenticated', user: superAdminUser, error: null, hint: null })
    installMock()
    mockedPost.mockResolvedValue({
      user: { ...demoUser, id: 777, role: 'OPS', nickname: '新运营' },
      balance: detail.balance,
      latest_balance_adjust: null,
    })
    const user = userEvent.setup()
    renderWithProviders(
      <>
        <AdminUsersPage />
        <ToastViewport />
      </>,
    )
    await screen.findByText('演示账号')

    await user.click(screen.getByRole('button', { name: '新建运营账号' }))
    const roleSelect = await screen.findByLabelText('角色')
    // PERSONAL is not offerable: a customer account has to go through public
    // registration, and the backend answers 400 for PERSONAL anyway.
    const optionLabels = within(roleSelect)
      .getAllByRole('option')
      .map((option) => option.textContent)
    expect(optionLabels).toEqual(['运营', '财务', '超级管理员'])
    expect(optionLabels).not.toContain('个人用户')

    await user.type(screen.getByLabelText(/账号（邮箱/), 'new-ops@example.com')
    await user.type(screen.getByLabelText(/初始密码/), 'Ops#2026-Pass')
    await user.type(screen.getByLabelText(/^昵称/), '新运营')
    await user.click(screen.getByRole('button', { name: '创建账号' }))

    expect(await screen.findByText('运营账号已创建')).toBeInTheDocument()
    const [path, body] = mockedPost.mock.calls.at(-1) ?? []
    expect(path).toBe('/api/admin/users')
    expect(body).toMatchObject({
      identifier: 'new-ops@example.com',
      password: 'Ops#2026-Pass',
      nickname: '新运营',
      role: 'OPS',
    })
  })

  it('UsersPage：非超级管理员看不到新建入口与角色变更', async () => {
    useAuthStore.setState({ status: 'authenticated', user: opsUser, error: null, hint: null })
    installMock()
    const user = userEvent.setup()
    renderWithProviders(<AdminUsersPage />)
    await screen.findByText('演示账号')

    expect(screen.queryByRole('button', { name: '新建运营账号' })).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '详情' }))
    await screen.findByText('总余额')
    expect(screen.queryByText('变更角色')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '提交角色变更' })).not.toBeInTheDocument()
  })

  it('UsersPage：变更角色校验理由并提交目标角色', async () => {
    useAuthStore.setState({ status: 'authenticated', user: superAdminUser, error: null, hint: null })
    installMock()
    mockedPost.mockResolvedValue({
      user: { ...demoUser, role: 'OPS' },
      balance: detail.balance,
      latest_balance_adjust: null,
    })
    const user = userEvent.setup()
    renderWithProviders(
      <>
        <AdminUsersPage />
        <ToastViewport />
      </>,
    )
    await screen.findByText('演示账号')

    await user.click(screen.getByRole('button', { name: '详情' }))
    await screen.findByText('变更角色')
    await user.selectOptions(screen.getByLabelText('目标角色'), 'OPS')
    await user.click(screen.getByRole('button', { name: '提交角色变更' }))
    // The reason is mandatory: nothing is sent without it.
    expect(await screen.findByText('请填写变更理由，会写入审计。')).toBeInTheDocument()
    expect(mockedPost).not.toHaveBeenCalled()

    await user.type(screen.getByLabelText(/变更理由/), '转入运营团队')
    await user.click(screen.getByRole('button', { name: '提交角色变更' }))

    expect(await screen.findByText('角色已变更')).toBeInTheDocument()
    const [path, body] = mockedPost.mock.calls.at(-1) ?? []
    expect(path).toBe('/api/admin/users/501/role')
    expect(body).toMatchObject({ role: 'OPS', reason: '转入运营团队' })
  })
})

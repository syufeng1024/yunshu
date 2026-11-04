import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import AccountPage from './AccountPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return { ...actual, api: { ...actual.api, post: vi.fn(), patch: vi.fn() } }
})

beforeEach(() => {
  useAuthStore.setState({
    status: 'authenticated',
    user: {
      id: '7',
      email: 'demo@yunshu.test',
      display_name: '演示账号',
      role: 'PERSONAL',
      status: 'NORMAL',
      csrf_token: 'csrf',
    },
    error: null,
    hint: null,
  })
  vi.clearAllMocks()
})

function renderPage() {
  return render(
    <MemoryRouter initialEntries={['/console/account']}>
      <AccountPage />
    </MemoryRouter>,
  )
}

describe('AccountPage', () => {
  it('shows the account identity and both self-service actions', () => {
    renderPage()

    expect(screen.getByText('demo@yunshu.test')).toBeInTheDocument()
    expect(screen.getByText('个人用户')).toBeInTheDocument()
    expect(screen.getByText('正常')).toBeInTheDocument()
    // The two things that had no entry point in the console before: change the
    // password, and close the account.
    expect(screen.getByRole('button', { name: '更新密码' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '我要注销账号' })).toBeInTheDocument()
  })

  it('rejects a short or mismatched new password without calling the API', async () => {
    const user = userEvent.setup()
    const post = vi.mocked(api.post)
    renderPage()

    // 按 label 取，不按 DOM 顺序：资料区新增「登录用户名」后，索引会整体错位，
    // 而按 label 取能同时钉住字段的可访问名。
    const current = screen.getByLabelText(/当前密码/)
    const next = screen.getByLabelText(/^新密码/)
    const confirm = screen.getByLabelText(/确认新密码/)
    await user.type(current, 'old-password')
    await user.type(next, 'short')
    await user.type(confirm, 'short')
    await user.click(screen.getByRole('button', { name: '更新密码' }))
    expect(screen.getByText('新密码至少 8 位。')).toBeInTheDocument()

    await user.clear(next)
    await user.clear(confirm)
    await user.type(next, 'Str0ng-Passw0rd!')
    await user.type(confirm, 'Str0ng-Passw0rd?')
    await user.click(screen.getByRole('button', { name: '更新密码' }))
    expect(screen.getByText('两次输入的新密码不一致。')).toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()
  })

  it('saves the nickname and refreshes the session user', async () => {
    const user = userEvent.setup()
    const patch = vi.mocked(api.patch)
    patch.mockResolvedValue({
      user: {
        id: '7',
        email: 'demo@yunshu.test',
        display_name: '小明',
        role: 'PERSONAL',
        status: 'NORMAL',
        username: '',
        password_warning: false,
        csrf_token: 'csrf',
      },
    })

    renderPage()
    const input = screen.getByLabelText('显示名称')
    await user.clear(input)
    await user.type(input, '小明')
    await user.click(screen.getByRole('button', { name: '保存资料' }))

    await waitFor(() =>
      expect(patch).toHaveBeenCalledWith('/api/v1/auth/profile', { nickname: '小明' }),
    )
    // The sidebar/header read from the store, so a stale store would show the
    // old name right after a successful save.
    expect(useAuthStore.getState().user?.display_name).toBe('小明')
    expect(await screen.findByText('已保存')).toBeInTheDocument()
  })

  it('sends username only after the field was edited', async () => {
    const user = userEvent.setup()
    const patch = vi.mocked(api.patch)
    patch.mockResolvedValue({
      user: {
        id: '7',
        email: 'demo@yunshu.test',
        display_name: '演示账号',
        role: 'PERSONAL',
        status: 'NORMAL',
        username: 'demo',
        password_warning: false,
        csrf_token: 'csrf',
      },
    })

    renderPage()
    await user.type(screen.getByLabelText('登录用户名'), 'demo')
    await user.click(screen.getByRole('button', { name: '保存资料' }))

    // 缺省 username 表示"不改"，只有真正编辑过才放进请求体；否则每次保存昵称
    // 都会把用户名当成"清空"提交。
    await waitFor(() =>
      expect(patch).toHaveBeenCalledWith('/api/v1/auth/profile', {
        nickname: '演示账号',
        username: 'demo',
      }),
    )
    expect(useAuthStore.getState().user?.username).toBe('demo')
  })

  it('allows clearing the username (empty string is a real request)', async () => {
    const user = userEvent.setup()
    const patch = vi.mocked(api.patch)
    useAuthStore.setState({
      status: 'authenticated',
      user: {
        id: '7',
        email: 'demo@yunshu.test',
        display_name: '演示账号',
        role: 'PERSONAL',
        status: 'NORMAL',
        username: 'demo',
        password_warning: false,
        csrf_token: 'csrf',
      },
      error: null,
      hint: null,
    })
    patch.mockResolvedValue({
      user: {
        id: '7',
        email: 'demo@yunshu.test',
        display_name: '演示账号',
        role: 'PERSONAL',
        status: 'NORMAL',
        username: '',
        password_warning: false,
        csrf_token: 'csrf',
      },
    })

    renderPage()
    await user.clear(screen.getByLabelText('登录用户名'))
    await user.click(screen.getByRole('button', { name: '保存资料' }))

    await waitFor(() =>
      expect(patch).toHaveBeenCalledWith('/api/v1/auth/profile', {
        nickname: '演示账号',
        username: '',
      }),
    )
    expect(useAuthStore.getState().user?.username).toBe('')
  })

  it('requires the full e-mail before closing the account', async () => {
    const user = userEvent.setup()
    const post = vi.mocked(api.post)
    renderPage()

    await user.click(screen.getByRole('button', { name: '我要注销账号' }))
    await user.type(screen.getByPlaceholderText('demo@yunshu.test'), 'demo@wrong.test')
    await user.click(screen.getByRole('button', { name: '确认注销' }))

    expect(screen.getByText('请输入完整的登录邮箱以确认。')).toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()
  })
})

import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { bindEmail, sendBindEmailCode } from '@/api/auth'
import { api } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import AccountPage from './AccountPage'

vi.mock('@/api/auth', () => ({
  sendResetCode: vi.fn(),
  resetPassword: vi.fn(),
  sendBindEmailCode: vi.fn(),
  bindEmail: vi.fn(),
}))

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

  it('lists the three consequences of cancellation', () => {
    renderPage()
    expect(screen.getByText(/该手机号会被立即释放，可被他人重新注册/)).toBeInTheDocument()
    expect(screen.getByText(/90 天内该号码不能再领取体验版积分/)).toBeInTheDocument()
    expect(screen.getByText(/账号立即停用、无法登录/)).toBeInTheDocument()
  })

  // 手机号账号的 email 为空：旧实现在这里恒成立地比对空串，输入框空着就通过。
  // 现在 mode 由服务端下发，label / placeholder / 校验都按后 4 位走。
  it('asks a phone account for the last four digits and sends them', async () => {
    const user = userEvent.setup()
    const post = vi.mocked(api.post)
    post.mockResolvedValue(undefined as never)
    useAuthStore.setState({
      status: 'authenticated',
      user: {
        id: '8',
        email: '',
        display_name: '138****8000',
        role: 'PERSONAL',
        status: 'NORMAL',
        username: '',
        login_kind: 'phone_suffix',
        password_warning: false,
        csrf_token: 'csrf',
      },
      error: null,
      hint: null,
    })

    renderPage()
    await user.click(screen.getByRole('button', { name: '我要注销账号' }))

    const input = screen.getByLabelText('输入手机号后 4 位以确认')
    expect(input).toHaveAttribute('placeholder', '手机号后 4 位')

    // 非 4 位数字：本地拦下，不发请求。
    await user.type(input, '12')
    await user.click(screen.getByRole('button', { name: '确认注销' }))
    expect(screen.getByText('请输入手机号后 4 位（4 个数字）以确认。')).toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()

    await user.clear(input)
    await user.type(input, '8000')
    await user.click(screen.getByRole('button', { name: '确认注销' }))

    await waitFor(() =>
      expect(post).toHaveBeenCalledWith('/api/v1/auth/cancel', { confirm: '8000' }),
    )
  })

  it('does not accept an empty confirmation for a phone account', async () => {
    const user = userEvent.setup()
    const post = vi.mocked(api.post)
    useAuthStore.setState({
      status: 'authenticated',
      user: {
        id: '9',
        email: '',
        display_name: '139****1234',
        role: 'PERSONAL',
        status: 'NORMAL',
        username: '',
        login_kind: 'phone_suffix',
        password_warning: false,
        csrf_token: 'csrf',
      },
      error: null,
      hint: null,
    })

    renderPage()
    await user.click(screen.getByRole('button', { name: '我要注销账号' }))
    await user.click(screen.getByRole('button', { name: '确认注销' }))

    expect(screen.getByText('请输入手机号后 4 位以确认。')).toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()
  })

  it('binds a login mailbox for an account that has none', async () => {
    const user = userEvent.setup()
    const mockedSend = vi.mocked(sendBindEmailCode)
    const mockedBind = vi.mocked(bindEmail)
    mockedSend.mockResolvedValue({ channel: 'email', expires_in: 600, resend_after: 60 })
    mockedBind.mockResolvedValue({
      user: {
        id: '7',
        email: 'me@example.com',
        display_name: '演示账号',
        role: 'PERSONAL',
        status: 'NORMAL',
        username: '',
        password_warning: false,
      },
    })
    // 账号没有邮箱：内置超管与纯手机号账号就是这个形态。
    useAuthStore.setState({
      status: 'authenticated',
      user: {
        id: '7',
        email: '',
        display_name: '演示账号',
        role: 'SUPER_ADMIN',
        status: 'NORMAL',
        username: 'admin',
        password_warning: true,
        csrf_token: 'csrf',
      },
      error: null,
      hint: null,
    })

    renderPage()
    expect(screen.getByText(/当前账号没有登录邮箱/)).toBeInTheDocument()

    await user.type(screen.getByLabelText(/登录邮箱/), 'me@example.com')
    await user.click(screen.getByRole('button', { name: '发送验证码' }))
    await waitFor(() => expect(mockedSend).toHaveBeenCalledWith('me@example.com'))
    expect(await screen.findByText(/验证码已发送到 me@example.com/)).toBeInTheDocument()

    await user.type(screen.getByLabelText(/验证码/), '123456')
    await user.click(screen.getByRole('button', { name: '确认绑定' }))

    await waitFor(() =>
      expect(mockedBind).toHaveBeenCalledWith({ email: 'me@example.com', code: '123456' }),
    )
    // 绑定成功后：store 已更新，卡片改为脱敏展示（不新增接口）。
    await waitFor(() => expect(useAuthStore.getState().user?.email).toBe('me@example.com'))
    expect(await screen.findByText('m***@example.com')).toBeInTheDocument()
    expect(screen.getByText(/更换登录邮箱请联系客服提交工单/)).toBeInTheDocument()
    // 会话不该被吊销：状态仍是已登录，csrf_token 也还在。
    expect(useAuthStore.getState().status).toBe('authenticated')
    expect(useAuthStore.getState().user?.csrf_token).toBe('csrf')
  })

  it('stops a malformed bind code before it reaches the server', async () => {
    const user = userEvent.setup()
    const mockedSend = vi.mocked(sendBindEmailCode)
    const mockedBind = vi.mocked(bindEmail)
    mockedSend.mockResolvedValue({ channel: 'email', expires_in: 600, resend_after: 60 })
    useAuthStore.setState({
      status: 'authenticated',
      user: {
        id: '7',
        email: '',
        display_name: '演示账号',
        role: 'PERSONAL',
        status: 'NORMAL',
        csrf_token: 'csrf',
      },
      error: null,
      hint: null,
    })

    renderPage()
    await user.type(screen.getByLabelText(/登录邮箱/), 'me@example.com')
    await user.click(screen.getByRole('button', { name: '发送验证码' }))
    await screen.findByLabelText(/验证码/)

    await user.type(screen.getByLabelText(/验证码/), '1234')
    await user.click(screen.getByRole('button', { name: '确认绑定' }))
    expect(screen.getByText('验证码是 6 位数字，请检查是否输完整。')).toBeInTheDocument()
    expect(mockedBind).not.toHaveBeenCalled()
  })
})

import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { sendResetCode } from '@/api/auth'
import { ApiError } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import ResetPasswordPage from './ResetPasswordPage'

vi.mock('@/api/auth', () => ({
  sendResetCode: vi.fn(),
  resetPassword: vi.fn(),
  sendBindEmailCode: vi.fn(),
  bindEmail: vi.fn(),
}))

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return { ...actual, api: { ...actual.api, post: vi.fn() } }
})

const mockedSendResetCode = vi.mocked(sendResetCode)

/** Reports the current location so the redirect assertion can read it. */
function LocationProbe() {
  const location = useLocation()
  return <div data-testid="location">{`${location.pathname}${location.search}`}</div>
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={['/reset-password']}>
      <Routes>
        <Route path="/reset-password" element={<ResetPasswordPage />} />
        <Route path="/login" element={<div>login page</div>} />
      </Routes>
      <LocationProbe />
    </MemoryRouter>,
  )
}

beforeEach(() => {
  useAuthStore.setState({ status: 'anonymous', user: null, error: null, hint: null })
})

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('ResetPasswordPage', () => {
  it('walks identify → verify → done and hands the identifier back to /login', async () => {
    const { resetPassword } = await import('@/api/auth')
    const mockedReset = vi.mocked(resetPassword)
    mockedSendResetCode.mockResolvedValue({ channel: 'email', expires_in: 600, resend_after: 45 })
    mockedReset.mockResolvedValue(undefined)

    const user = userEvent.setup()
    renderPage()

    // State 1: one identifier field, nothing else.
    const account = screen.getByLabelText(/邮箱或手机号/)
    expect(account).toHaveAttribute('autocomplete', 'username')
    await user.type(account, 'someone@example.com')
    await user.click(screen.getByRole('button', { name: '发送验证码' }))

    expect(mockedSendResetCode).toHaveBeenCalledWith('someone@example.com')
    // 文案只用用户自己的输入值，不显示服务端返回的任何账号标识。
    expect(
      await screen.findByText('如果 someone@example.com 已注册，验证码已发送。'),
    ).toBeInTheDocument()

    // 倒计时取响应里的 resend_after（45），不是写死的 60。
    expect(screen.getByRole('button', { name: '45s 后重发' })).toBeDisabled()

    // State 2: code + new password + confirm.
    const code = screen.getByLabelText(/验证码/)
    expect(code).toHaveAttribute('autocomplete', 'one-time-code')
    const next = screen.getByLabelText(/^新密码/)
    const confirm = screen.getByLabelText(/确认新密码/)
    expect(next).toHaveAttribute('autocomplete', 'new-password')
    expect(confirm).toHaveAttribute('autocomplete', 'new-password')
    await user.type(code, '123456')
    await user.type(next, 'Str0ng-Passw0rd!')
    await user.type(confirm, 'Str0ng-Passw0rd!')
    await user.click(screen.getByRole('button', { name: '重置密码' }))

    await waitFor(() =>
      expect(mockedReset).toHaveBeenCalledWith({
        identifier: 'someone@example.com',
        code: '123456',
        new_password: 'Str0ng-Passw0rd!',
      }),
    )

    // State 3: role=status 提示 + 回到登录页并预填 identifier。
    const done = await screen.findByRole('status')
    expect(done).toHaveTextContent('密码已重置，请用新密码登录')
    await user.click(screen.getByRole('button', { name: '去登录' }))
    await waitFor(() =>
      expect(screen.getByTestId('location')).toHaveTextContent(
        '/login?identifier=someone%40example.com',
      ),
    )
  })

  it('stops a phone number locally while the SMS channel is off', async () => {
    const user = userEvent.setup()
    renderPage()

    await user.type(screen.getByLabelText(/邮箱或手机号/), '13800138000')
    await user.click(screen.getByRole('button', { name: '发送验证码' }))

    // 与服务端 hint 同文案：本地拦截时用户读到的句子与 400 时一致。
    expect(screen.getByText(/短信验证码暂未开放，请使用邮箱/)).toBeInTheDocument()
    expect(mockedSendResetCode).not.toHaveBeenCalled()
  })

  it('validates the code format and the password before calling the server', async () => {
    const { resetPassword } = await import('@/api/auth')
    const mockedReset = vi.mocked(resetPassword)
    mockedSendResetCode.mockResolvedValue({ channel: 'email', expires_in: 600, resend_after: 60 })

    const user = userEvent.setup()
    renderPage()
    await user.type(screen.getByLabelText(/邮箱或手机号/), 'someone@example.com')
    await user.click(screen.getByRole('button', { name: '发送验证码' }))
    await screen.findByLabelText(/验证码/)

    // 5 位码：服务端会计一次错误尝试，本地就必须拦住。
    await user.type(screen.getByLabelText(/验证码/), '12345')
    await user.type(screen.getByLabelText(/^新密码/), 'Str0ng-Passw0rd!')
    await user.type(screen.getByLabelText(/确认新密码/), 'Str0ng-Passw0rd!')
    await user.click(screen.getByRole('button', { name: '重置密码' }))
    expect(screen.getByText('验证码是 6 位数字，请检查是否输完整。')).toBeInTheDocument()

    // 短口令：服务端返回 400 invalid_request_body，本地先拦一次。
    await user.clear(screen.getByLabelText(/验证码/))
    await user.type(screen.getByLabelText(/验证码/), '123456')
    await user.clear(screen.getByLabelText(/^新密码/))
    await user.type(screen.getByLabelText(/^新密码/), 'short')
    await user.click(screen.getByRole('button', { name: '重置密码' }))
    expect(screen.getByText('新密码至少 8 位。')).toBeInTheDocument()

    // 两次不一致。
    await user.clear(screen.getByLabelText(/^新密码/))
    await user.type(screen.getByLabelText(/^新密码/), 'Str0ng-Passw0rd!')
    await user.clear(screen.getByLabelText(/确认新密码/))
    await user.type(screen.getByLabelText(/确认新密码/), 'Str0ng-Passw0rd?')
    await user.click(screen.getByRole('button', { name: '重置密码' }))
    expect(screen.getByText('两次输入的新密码不一致。')).toBeInTheDocument()
    expect(mockedReset).not.toHaveBeenCalled()
  })

  it('renders the server message and hint when the code is rejected', async () => {
    const { resetPassword } = await import('@/api/auth')
    const mockedReset = vi.mocked(resetPassword)
    mockedSendResetCode.mockResolvedValue({ channel: 'email', expires_in: 600, resend_after: 60 })
    mockedReset.mockRejectedValue(
      new ApiError({
        status: 401,
        code: 'invalid_credentials',
        type: 'authentication_error',
        message: '验证码不正确或已过期',
        hint: '请确认 6 位数字是否输完整；已过期就重新获取',
      }),
    )

    const user = userEvent.setup()
    renderPage()
    await user.type(screen.getByLabelText(/邮箱或手机号/), 'someone@example.com')
    await user.click(screen.getByRole('button', { name: '发送验证码' }))
    await screen.findByLabelText(/验证码/)
    await user.type(screen.getByLabelText(/验证码/), '000000')
    await user.type(screen.getByLabelText(/^新密码/), 'Str0ng-Passw0rd!')
    await user.type(screen.getByLabelText(/确认新密码/), 'Str0ng-Passw0rd!')
    await user.click(screen.getByRole('button', { name: '重置密码' }))

    expect(await screen.findByText('验证码不正确或已过期')).toBeInTheDocument()
    expect(screen.getByText('请确认 6 位数字是否输完整；已过期就重新获取')).toBeInTheDocument()
  })

  it('resets the local session so the shell cannot keep showing a dead login', async () => {
    const { resetPassword } = await import('@/api/auth')
    vi.mocked(resetPassword).mockResolvedValue(undefined)
    mockedSendResetCode.mockResolvedValue({ channel: 'email', expires_in: 600, resend_after: 60 })

    // 已登录用户走找回（「已登录但忘了旧密码」）：服务端会把当前会话一起吊销，
    // 所以本地登录态也必须复位。
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
    })

    const user = userEvent.setup()
    renderPage()
    await user.type(screen.getByLabelText(/邮箱或手机号/), 'demo@yunshu.test')
    await user.click(screen.getByRole('button', { name: '发送验证码' }))
    await screen.findByLabelText(/验证码/)
    await user.type(screen.getByLabelText(/验证码/), '123456')
    await user.type(screen.getByLabelText(/^新密码/), 'Str0ng-Passw0rd!')
    await user.type(screen.getByLabelText(/确认新密码/), 'Str0ng-Passw0rd!')
    await user.click(screen.getByRole('button', { name: '重置密码' }))

    await waitFor(() => expect(useAuthStore.getState().status).toBe('anonymous'))
    expect(useAuthStore.getState().user).toBeNull()
  })
})

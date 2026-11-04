import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import { usePublicPlans } from '@/api/public'
import { fixtureResponses } from '@/fixtures/public'
import { useAuthStore } from '@/stores/auth'
import LoginPage, { loginChannelCopy, registerChannelCopy } from './LoginPage'

vi.mock('@/api/public', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/public')>()
  return { ...actual, usePublicPlans: vi.fn() }
})

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return { ...actual, api: { ...actual.api, post: vi.fn() } }
})

/** Reports the current location, so a redirect assertion can read it. */
function LocationProbe() {
  const location = useLocation()
  return <div data-testid="location">{`${location.pathname}${location.search}`}</div>
}

const plans = fixtureResponses['/api/public/plans'].data
const mockedPlans = vi.mocked(usePublicPlans)

beforeEach(() => {
  useAuthStore.setState({ status: 'anonymous', user: null, error: null, hint: null })
  mockedPlans.mockReturnValue({
    data: { data: plans },
    isLoading: false,
    isError: false,
    error: null,
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof usePublicPlans>)
})

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('LoginPage', () => {
  it('A22: uses active-claim wording and never says registration grants cash', () => {
    render(
      <MemoryRouter initialEntries={['/login?tab=register']}>
        <LoginPage />
      </MemoryRouter>,
    )

    expect(screen.getByRole('button', { name: '主动领取体验积分' })).toBeInTheDocument()
    // 领取入口仍是「主动领取」，但文案站在用户视角：正向说明注册后如何领到，
    // 不再对用户讲「注册不会自动发放」这类内部口径。
    expect(screen.getByText(/注册后可在控制台主动领取/)).toBeInTheDocument()
    expect(screen.queryByText(/注册即送/)).not.toBeInTheDocument()
    // 公开页面不出现内部口径：角色/跳转规则由服务端决定，用户不需要在登录页读到。
    expect(screen.queryByText(/服务端返回/)).not.toBeInTheDocument()
    expect(screen.queryByText(/运营后台/)).not.toBeInTheDocument()
    expect(screen.queryByText('登录后跳转')).not.toBeInTheDocument()
  })

  it('renders the agreement checkbox and the plan marker from the query', () => {
    render(
      <MemoryRouter initialEntries={['/login?tab=register&plan=STANDARD']}>
        <LoginPage />
      </MemoryRouter>,
    )
    expect(screen.getByRole('checkbox')).toBeInTheDocument()
    expect(screen.getByText('已选择套餐：标准版')).toBeInTheDocument()
  })

  it('turns the claim entry into an explicit post-registration instruction', async () => {
    const user = userEvent.setup()
    render(
      <MemoryRouter initialEntries={['/login?tab=register']}>
        <LoginPage />
      </MemoryRouter>,
    )

    await user.click(screen.getByRole('button', { name: '主动领取体验积分' }))
    expect(
      screen.getByText(/完成注册后，请在控制台主动领取体验积分/),
    ).toBeInTheDocument()
  })

  it('requests an e-mail code and never calls the API for a phone number', async () => {
    const user = userEvent.setup()
    const post = vi.mocked(api.post)
    post.mockResolvedValue({ channel: 'email', expires_in: 600, resend_after: 60 })

    render(
      <MemoryRouter initialEntries={['/login?tab=register']}>
        <LoginPage />
      </MemoryRouter>,
    )

    const account = screen.getByPlaceholderText('you@example.com')
    const send = screen.getByRole('button', { name: '获取验证码' })

    // SMS is billed per message and has no provider: the click must stop at the
    // client, and say why, instead of looking like a delivery failure.
    await user.type(account, '13800138000')
    await user.click(send)
    expect(screen.getByText('短信验证码暂未开放，请使用邮箱。')).toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()

    await user.clear(account)
    await user.type(account, 'someone@example.com')
    await user.click(send)
    expect(post).toHaveBeenCalledWith('/api/v1/auth/verification-code', {
      identifier: 'someone@example.com',
      purpose: 'register',
    })
    expect(screen.getByRole('button', { name: /后重发/ })).toBeDisabled()
  })

  // Reported bug: a five-digit code was sent to the server, came back as
  // "账号或密码不正确", and burned one of the five attempts. An incomplete code
  // must be caught locally.
  it('stops an incomplete verification code before it reaches the server', async () => {
    const user = userEvent.setup()
    const post = vi.mocked(api.post)
    const { container } = render(
      <MemoryRouter initialEntries={['/login?tab=register']}>
        <LoginPage />
      </MemoryRouter>,
    )

    const inputs = container.querySelectorAll('input')
    await user.type(inputs[0], 'someone@example.com')
    await user.type(inputs[1], '12345')
    await user.type(inputs[2], 'Str0ng-Passw0rd!')
    await user.click(screen.getByRole('button', { name: '注册并进入控制台' }))

    expect(screen.getByText('验证码是 6 位数字，请检查是否输完整。')).toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()
  })

  // The reference site asks for consent on the login page as well; a missing
  // checkbox there was the original complaint.
  it('blocks sign-in until the agreement is accepted', async () => {
    const user = userEvent.setup()
    const post = vi.mocked(api.post)
    const { container } = render(
      <MemoryRouter initialEntries={['/login']}>
        <LoginPage />
      </MemoryRouter>,
    )

    const inputs = container.querySelectorAll('input')
    await user.type(inputs[0], 'someone@example.com')
    await user.type(inputs[1], 'Str0ng-Passw0rd!')
    await user.click(screen.getByRole('button', { name: '登录' }))

    expect(screen.getByText('请先勾选并同意用户协议与隐私政策。')).toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()

    // Both agreements must be real links on the login page too.
    expect(screen.getByRole('link', { name: '《用户协议》' })).toHaveAttribute('href', '/terms')
    expect(screen.getByRole('link', { name: '《隐私政策》' })).toHaveAttribute('href', '/privacy')
  })

  it('catches a password mismatch before registering', async () => {
    const user = userEvent.setup()
    const post = vi.mocked(api.post)
    const { container } = render(
      <MemoryRouter initialEntries={['/login?tab=register']}>
        <LoginPage />
      </MemoryRouter>,
    )

    const inputs = container.querySelectorAll('input')
    await user.type(inputs[0], 'someone@example.com')
    await user.type(inputs[1], '123456')
    await user.type(inputs[2], 'Str0ng-Passw0rd!')
    await user.type(inputs[3], 'Str0ng-Passw0rd?')
    await user.click(screen.getByRole('button', { name: '注册并进入控制台' }))

    expect(screen.getByText('两次输入的密码不一致。')).toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()
  })

  // P0-5③/F2: 登录页脚与注册表单的渠道说明都由 VITE_AUTH_SMS_ENABLED 决定。
  // 渲染断言只覆盖当前构建（默认 false）；两条分支由下面的纯函数断言补齐，
  // 因为 import.meta.env 在模块加载时就被内联，无法逐用例翻转。
  it('states the real channel availability on both forms', () => {
    render(
      <MemoryRouter initialEntries={['/login?tab=register']}>
        <LoginPage />
      </MemoryRouter>,
    )
    const smsEnabled = import.meta.env.VITE_AUTH_SMS_ENABLED === 'true'
    // 注册页签的渠道说明与登录页脚必须同源，不能一处改了另一处还写着「未开放」。
    expect(screen.getByText(registerChannelCopy(smsEnabled))).toBeInTheDocument()
    if (!smsEnabled) {
      expect(screen.getByText('注册只支持邮箱；短信验证码暂未开放。')).toBeInTheDocument()
    }
  })

  it('has one source of truth for the SMS availability copy', () => {
    // false：off 部署（默认）——承诺「注册只支持邮箱」，与实际 400 一致。
    expect(registerChannelCopy(false)).toBe('注册只支持邮箱；短信验证码暂未开放。')
    expect(loginChannelCopy(false)).toBe('支持邮箱、手机号或用户名登录；短信验证码未开放。')
    // true：provider 部署——页脚与注册页都必须翻过来，否则前端会说谎。
    expect(registerChannelCopy(true)).toBe('注册支持邮箱与手机号；短信验证码已开放')
    expect(loginChannelCopy(true)).toBe('支持邮箱、手机号或用户名登录。')
    // 两个开关只能有两态：不允许出现「注册说已开放、页脚说未开放」的组合。
    expect(registerChannelCopy(true)).not.toBe(registerChannelCopy(false))
    expect(loginChannelCopy(true)).not.toBe(loginChannelCopy(false))
  })

  // P0-5②: 「忘记密码」不再是占位面板的开关，而是一个真链接。
  it('links the "forgot password" entry to the recovery page', async () => {
    const user = userEvent.setup()
    render(
      <MemoryRouter initialEntries={['/login']}>
        <LoginPage />
        <LocationProbe />
      </MemoryRouter>,
    )

    const link = screen.getByRole('link', { name: '忘记密码' })
    expect(link).toHaveAttribute('href', '/reset-password')
    // 占位面板整块删除：页面上不该再有任何「暂未开放」的重置按钮。
    expect(screen.queryByText(/发送重置链接/)).not.toBeInTheDocument()
    expect(screen.queryByText(/密码重置功能暂未开放/)).not.toBeInTheDocument()

    await user.click(link)
    expect(screen.getByTestId('location')).toHaveTextContent('/reset-password')
  })

  // P0-4⑤: 找回密码成功后跳回登录页并预填 identifier。
  it('prefills the account field from the identifier query parameter', () => {
    render(
      <MemoryRouter initialEntries={['/login?identifier=someone%40example.com']}>
        <LoginPage />
      </MemoryRouter>,
    )
    // label 里带一个 aria-hidden 的必填星号，所以按正则取。
    expect(screen.getByLabelText(/账号/)).toHaveValue('someone@example.com')
  })
})

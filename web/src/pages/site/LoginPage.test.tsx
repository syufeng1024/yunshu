import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import { usePublicPlans } from '@/api/public'
import { fixtureResponses } from '@/fixtures/public'
import { useAuthStore } from '@/stores/auth'
import LoginPage from './LoginPage'

vi.mock('@/api/public', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/public')>()
  return { ...actual, usePublicPlans: vi.fn() }
})

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return { ...actual, api: { ...actual.api, post: vi.fn() } }
})

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
    await user.click(screen.getByRole('button', { name: '注册' }))

    expect(screen.getByText('验证码是 6 位数字，请检查是否输完整。')).toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()
  })
})

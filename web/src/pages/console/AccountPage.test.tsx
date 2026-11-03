import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import AccountPage from './AccountPage'

vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return { ...actual, api: { ...actual.api, post: vi.fn() } }
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
    const { container } = renderPage()

    const inputs = container.querySelectorAll('input')
    await user.type(inputs[0], 'old-password')
    await user.type(inputs[1], 'short')
    await user.type(inputs[2], 'short')
    await user.click(screen.getByRole('button', { name: '更新密码' }))
    expect(screen.getByText('新密码至少 8 位。')).toBeInTheDocument()

    await user.clear(inputs[1])
    await user.clear(inputs[2])
    await user.type(inputs[1], 'Str0ng-Passw0rd!')
    await user.type(inputs[2], 'Str0ng-Passw0rd?')
    await user.click(screen.getByRole('button', { name: '更新密码' }))
    expect(screen.getByText('两次输入的新密码不一致。')).toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()
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

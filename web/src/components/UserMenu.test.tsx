import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import type { CurrentUser } from '@/stores/auth'
import { useAuthStore } from '@/stores/auth'
import { UserMenu } from './UserMenu'

const account: CurrentUser = {
  id: '1',
  email: 'demo@yunshu.test',
  display_name: '孙丰',
  role: 'PERSONAL',
  status: 'NORMAL',
  csrf_token: 'csrf',
}

function signIn(overrides: Partial<CurrentUser> = {}) {
  act(() => {
    useAuthStore.setState({ status: 'authenticated', user: { ...account, ...overrides } })
  })
}

beforeEach(() => {
  signIn()
})

afterEach(() => {
  act(() => {
    useAuthStore.setState({ status: 'anonymous', user: null, error: null, hint: null })
  })
})

function renderMenu(scope: 'console' | 'admin' = 'console') {
  return render(
    <MemoryRouter>
      <UserMenu scope={scope} />
    </MemoryRouter>,
  )
}

describe('UserMenu', () => {
  it('collapses the account into an avatar until opened', async () => {
    const user = userEvent.setup()
    renderMenu()

    const trigger = screen.getByRole('button', { name: '账号菜单' })
    expect(trigger).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()

    await user.click(trigger)
    expect(trigger).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByRole('menu')).toBeInTheDocument()
    // 名字与角色在头部，自助入口在下面。
    expect(screen.getByText('孙丰')).toBeInTheDocument()
    expect(screen.getByText(/个人用户/)).toBeInTheDocument()
    for (const label of ['个人资料', '我的订阅', '钱包与充值', '退出登录']) {
      expect(screen.getByRole('menuitem', { name: label })).toBeInTheDocument()
    }
  })

  it('offers the cross-console switch only to admins', async () => {
    const user = userEvent.setup()
    const first = renderMenu()
    await user.click(screen.getByRole('button', { name: '账号菜单' }))
    expect(screen.queryByRole('menuitem', { name: '运营后台' })).not.toBeInTheDocument()
    first.unmount()

    act(() => {
      useAuthStore.setState({ status: 'authenticated', user: { ...account, role: 'SUPER_ADMIN' } })
    })
    renderMenu()
    await user.click(screen.getByRole('button', { name: '账号菜单' }))
    expect(screen.getByRole('menuitem', { name: '运营后台' })).toHaveAttribute('href', '/admin')
  })

  it('closes on Escape', async () => {
    const user = userEvent.setup()
    renderMenu()
    await user.click(screen.getByRole('button', { name: '账号菜单' }))
    expect(screen.getByRole('menu')).toBeInTheDocument()

    await user.keyboard('{Escape}')
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
  })
})

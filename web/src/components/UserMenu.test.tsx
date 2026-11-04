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

function renderMenu() {
  return render(
    <MemoryRouter>
      <UserMenu />
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

  /**
   * 防回退：账号菜单里不再有「运营后台」/「返回控制台」两个互跳项。
   *
   * 它们存在的唯一理由是当时有两个独立的后台壳；现在 `/console` 与 `/admin` 共用
   * 同一个侧栏，管理员入口就在侧栏「管理员」分组里。谁把互跳项加回来，说明侧栏
   * 又被拆成了两套。
   */
  it('不再提供跨后台互跳项，连管理员也没有', async () => {
    const user = userEvent.setup()
    const first = renderMenu()
    await user.click(screen.getByRole('button', { name: '账号菜单' }))
    expect(screen.queryByRole('menuitem', { name: '运营后台' })).not.toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: '返回控制台' })).not.toBeInTheDocument()
    first.unmount()

    act(() => {
      useAuthStore.setState({ status: 'authenticated', user: { ...account, role: 'SUPER_ADMIN' } })
    })
    renderMenu()
    await user.click(screen.getByRole('button', { name: '账号菜单' }))
    expect(screen.queryByRole('menuitem', { name: '运营后台' })).not.toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: '返回控制台' })).not.toBeInTheDocument()
    // 管理员的菜单项与个人用户完全一致：位置感交给侧栏的选中态，不再由菜单自称。
    expect(screen.getAllByRole('menuitem').map((item) => item.textContent)).toEqual([
      '个人资料',
      '我的订阅',
      '钱包与充值',
      '退出登录',
    ])
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

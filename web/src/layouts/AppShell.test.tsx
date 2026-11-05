import { act, render, screen, within } from '@testing-library/react'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { afterEach, describe, expect, it } from 'vitest'
import { SHELL_NAV_GROUP_TITLES } from '@/lib/constants'
import type { UserRole } from '@/lib/constants'
import type { CurrentUser } from '@/stores/auth'
import { useAuthStore } from '@/stores/auth'
import { AppShell } from './AppShell'

/**
 * 统一外壳 AppShell：顶栏站点导航、侧栏「常规 / 个人 / 管理员」三组、两列独立滚动。
 *
 * 背景：这里原来是两个壳（ConsoleLayout + AdminLayout），各有一套侧栏与分组口径，
 * 超管要在两个后台之间来回跳才能同时看用户和订单。合并成一个壳之后，个人功能与
 * 管理员功能在同一个侧栏里各归其组，靠角色过滤决定「我看不看得见这一组」。
 *
 * 地址一律由 `VITE_API_BASE_URL` 推导，未注入时回落到站点自身域名——因此断言
 * 期望值只能用 `window.location.origin` 计算，不能写死任何域名。
 */

const user: CurrentUser = {
  id: '1',
  email: 'demo@yunshu.test',
  display_name: '演示账号',
  role: 'SUPER_ADMIN',
  status: 'NORMAL',
  csrf_token: 'csrf',
}

/** 两个真实入口都注册成路由，`initialEntries` 才能落到外壳上而不是 404。 */
function renderShell(path: string) {
  const router = createMemoryRouter(
    [
      { path: '/console', element: <AppShell />, children: [{ index: true, element: null }] },
      { path: '/admin', element: <AppShell />, children: [{ index: true, element: null }] },
    ],
    { initialEntries: [path] },
  )
  return render(<RouterProvider router={router} />)
}

function signInAs(role: UserRole) {
  act(() => {
    useAuthStore.setState({ status: 'authenticated', user: { ...user, role } })
  })
}

/** 侧栏里所有 NavLink 的目标地址，顺序即渲染顺序。 */
function sidebarHrefs(): string[] {
  const nav = screen.getByRole('navigation', { name: '侧栏导航' })
  return within(nav)
    .getAllByRole('link')
    .map((link) => link.getAttribute('href') ?? '')
}

function sidebarLink(name: string): HTMLElement | null {
  const nav = screen.getByRole('navigation', { name: '侧栏导航' })
  return within(nav).queryByRole('link', { name })
}

afterEach(() => {
  act(() => {
    useAuthStore.setState({ status: 'anonymous', user: null, error: null, hint: null })
  })
})

describe('AppShell', () => {
  // 顶栏从「调用地址」换成了站点导航：地址在密钥详情与调试台的代码生成里都有，
  // 常驻占掉首屏最贵的一行不划算；而首页/文档/模型价格这些入口原来只能从侧栏底部走。
  it('顶栏提供站点导航，且不再显示调用地址', () => {
    signInAs('SUPER_ADMIN')
    renderShell('/console')

    const topNav = screen.getByRole('navigation', { name: '站点导航' })
    expect(topNav).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '首页' })).toHaveAttribute('href', '/')
    expect(screen.getByRole('link', { name: '文档' })).toBeInTheDocument()
    expect(screen.queryByText('调用地址')).not.toBeInTheDocument()
  })

  it('侧栏与内容区各自滚动，互不影响', () => {
    signInAs('SUPER_ADMIN')
    renderShell('/console')

    const aside = screen.getByRole('complementary')
    // 整页不再滚动：外层撑满视口并裁掉溢出，两列各自持有滚动容器。
    expect(aside.className).toContain('md:overflow-y-auto')
    const main = screen.getByRole('main')
    expect(main.className).toContain('overflow-y-auto')

    // 外层必须同时有 relative。只有 overflow-hidden 时，内容区（overflow-y-auto）
    // 里长页面的溢出仍会被算进根滚动容器的可滚动区域——实测密钥详情页
    // html.scrollHeight=1376、视口 529，窗口右侧于是与内容区并排多出一根滚动条
    // （2026-11-05 需求方报「页面右侧出现两个滚动条」）。jsdom 没有布局，测不到
    // scrollHeight，只能把决定性的类钉住，防止有人当冗余样式清掉。
    const shell = main.closest('.h-screen')
    expect(shell).not.toBeNull()
    expect(shell?.className).toContain('relative')
    expect(shell?.className).toContain('overflow-hidden')
  })

  it('侧栏按「常规 / 个人 / 管理员」三组渲染，顺序固定', () => {
    signInAs('SUPER_ADMIN')
    renderShell('/console')

    const titles = [SHELL_NAV_GROUP_TITLES.general, SHELL_NAV_GROUP_TITLES.personal, SHELL_NAV_GROUP_TITLES.admin]
    for (const title of titles) {
      expect(screen.getByText(title)).toBeInTheDocument()
    }

    // 分组顺序：常规组的三项在前，个人组的三项其后，管理员组接在最后。
    expect(sidebarHrefs()).toEqual([
      '/console',
      '/console/keys',
      '/console/models',
      '/console/playground',
      '/console/usage',
      '/console/logs',
      '/console/tickets',
      '/console/recharge',
      '/console/subscription',
      '/console/account',
      '/admin',
      '/admin/users',
      '/admin/channels',
      '/admin/models',
      '/admin/announcements',
      '/admin/reviews',
      '/admin/orders',
      '/admin/redeem-codes',
      '/admin/tickets',
      '/admin/system',
    ])
  })

  // 硬约束：「倍率与定价」继续不进菜单（2026-11-02 裁定），连 SUPER_ADMIN 也没有。
  it('侧栏不收录「倍率与定价」（2026-11-02 裁定）', () => {
    signInAs('SUPER_ADMIN')
    renderShell('/admin')

    expect(sidebarLink('倍率与定价')).not.toBeInTheDocument()
    expect(sidebarHrefs()).not.toContain('/admin/pricing')
  })

  // 核心不变量：两个入口渲染同一套侧栏。以前是两个壳两份分组，这条会直接红。
  it('/console 与 /admin 两个入口渲染完全一致的侧栏', () => {
    signInAs('SUPER_ADMIN')

    const consoleEntry = renderShell('/console')
    const fromConsole = sidebarHrefs()
    consoleEntry.unmount()

    const adminEntry = renderShell('/admin')
    const fromAdmin = sidebarHrefs()
    adminEntry.unmount()

    expect(fromAdmin).toEqual(fromConsole)
    expect(fromConsole.length).toBeGreaterThan(0)
  })

  // 需求锁死的不变量 a)：个人用户看不到「管理员」这个分组标题，也就看不到
  // 用户管理 / 渠道管理这类不该他操心的入口。
  it('PERSONAL 不渲染「管理员」分组与其中的入口', () => {
    signInAs('PERSONAL')
    renderShell('/console')

    expect(screen.queryByText(SHELL_NAV_GROUP_TITLES.admin)).not.toBeInTheDocument()
    expect(sidebarLink('用户管理')).not.toBeInTheDocument()
    expect(sidebarLink('渠道管理')).not.toBeInTheDocument()

    // 常规组与个人组照常出现：过滤只摘掉管理员那一组，不能把别人的菜单一起吃掉。
    expect(screen.getByText(SHELL_NAV_GROUP_TITLES.general)).toBeInTheDocument()
    expect(screen.getByText(SHELL_NAV_GROUP_TITLES.personal)).toBeInTheDocument()
    expect(sidebarLink('密钥管理')).toBeInTheDocument()
    expect(sidebarLink('充值中心')).toBeInTheDocument()
    expect(sidebarHrefs().some((href) => href.startsWith('/admin'))).toBe(false)
  })

  // 需求锁死的不变量 b)：财务只有财务与审核两组能力，够不着运营侧的页面。
  it('FINANCE 看不到用户管理与渠道管理，但看得到订单与兑换码', () => {
    signInAs('FINANCE')
    renderShell('/console')

    expect(sidebarLink('用户管理')).not.toBeInTheDocument()
    expect(sidebarLink('渠道管理')).not.toBeInTheDocument()

    expect(sidebarLink('订单与收款')).toHaveAttribute('href', '/admin/orders')
    expect(sidebarLink('兑换码')).toHaveAttribute('href', '/admin/redeem-codes')
    // 运营概览与审核中心是 FINANCE 也有的两项，用来确认「管理员」组没有整组消失。
    expect(screen.getByText(SHELL_NAV_GROUP_TITLES.admin)).toBeInTheDocument()
    expect(sidebarLink('运营概览')).toBeInTheDocument()
    expect(sidebarLink('审核中心')).toBeInTheDocument()
  })
})

import { act, render, screen } from '@testing-library/react'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { afterEach, describe, expect, it } from 'vitest'
import type { CurrentUser } from '@/stores/auth'
import { useAuthStore } from '@/stores/auth'
import { AdminLayout } from './AdminLayout'
import { ConsoleLayout } from './ConsoleLayout'

/**
 * 控制台外壳：顶栏站点导航、侧栏分组、两列独立滚动。
 *
 * 背景：顶栏原本钉着「调用地址」，占掉首屏最贵的一行；而首页/文档/模型价格
 * 这些入口只能从侧栏底部走。现在顶栏是站点导航，调用地址在密钥详情与调试台
 * 的代码生成里都有。
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

function renderShell(element: React.ReactElement, path: string) {
  const router = createMemoryRouter([{ path: '/', element, children: [{ index: true, element: null }] }], {
    initialEntries: [path],
  })
  return render(<RouterProvider router={router} />)
}

function signIn() {
  act(() => {
    useAuthStore.setState({ status: 'authenticated', user })
  })
}

afterEach(() => {
  act(() => {
    useAuthStore.setState({ status: 'anonymous', user: null, error: null, hint: null })
  })
})

describe('控制台外壳', () => {
  // 顶栏从「调用地址」换成了站点导航：地址在密钥详情与调试台的代码生成里都有，
  // 常驻占掉首屏最贵的一行不划算；而首页/文档/模型价格这些入口原来只能从侧栏底部走。
  it('顶栏提供站点导航，且不再显示调用地址', () => {
    signIn()
    renderShell(<ConsoleLayout />, '/')

    const topNav = screen.getByRole('navigation', { name: '站点导航' })
    expect(topNav).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '首页' })).toHaveAttribute('href', '/')
    expect(screen.getByRole('link', { name: '文档' })).toBeInTheDocument()
    expect(screen.queryByText('调用地址')).not.toBeInTheDocument()
  })

  it('侧栏与内容区各自滚动，互不影响', () => {
    signIn()
    renderShell(<ConsoleLayout />, '/')

    const aside = screen.getByRole('complementary')
    // 整页不再滚动：外层撑满视口并裁掉溢出，两列各自持有滚动容器。
    expect(aside.className).toContain('md:overflow-y-auto')
    const main = screen.getByRole('main')
    expect(main.className).toContain('overflow-y-auto')
  })

  // 侧边栏分组是「控制台与官网同一套语言」的一部分：一长条没有层次的菜单名
  // 是后台感最强的来源。分组标题同时也是分组结构唯一的可见证据。
  it('控制台侧边栏按分组渲染菜单', () => {
    signIn()
    renderShell(<ConsoleLayout />, '/')

    for (const title of ['工作台', '用量与记录', '支持']) {
      expect(screen.getByText(title)).toBeInTheDocument()
    }
    expect(screen.getByRole('link', { name: '概览' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '工单中心' })).toBeInTheDocument()
  })

  it('运营后台按角色过滤菜单项', () => {
    act(() => {
      useAuthStore.setState({ status: 'authenticated', user: { ...user, role: 'FINANCE' } })
    })
    renderShell(<AdminLayout />, '/')

    // FINANCE sees the shared 运营概览 but not the OPS-only channels page.
    expect(screen.getByRole('link', { name: '运营概览' })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '渠道管理' })).not.toBeInTheDocument()
    // 「财务」也是角色徽章的文字，用分组独有的标题断言分组本身。
    expect(screen.getByText('审核与支持')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '订单与收款' })).toBeInTheDocument()
  })
})

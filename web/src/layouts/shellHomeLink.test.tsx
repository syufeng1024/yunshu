import { act, render, screen, within } from '@testing-library/react'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { afterEach, describe, expect, it } from 'vitest'
import type { CurrentUser } from '@/stores/auth'
import { useAuthStore } from '@/stores/auth'
import { AppShell } from './AppShell'

/**
 * 登录后的外壳必须留一条回官网的路。
 *
 * 回归背景：布局的 Logo 指向壳内首页（/console），而侧栏里只有「退出登录」。
 * 也就是说登录之后控制台内没有任何通往官网的出口——用户想看价格、文档或公告，
 * 只能手动改地址栏。这不是"少个链接"，是把自己关在壳里。
 *
 * 两条链路都要断言：宽屏用**顶栏的站点导航**（首页等链接），窄屏用侧栏横条里的
 * 图标入口（aria-label 为「返回官网首页」）。侧栏底部那个「返回官网」已经去掉，
 * 桌面端的出口改为顶栏，顺带把文档与模型价格也放了进去。
 *
 * 合并成单一外壳后，`/console` 与 `/admin` 两个入口渲染的是同一个组件，但入口
 * 不同仍然是两条真实的进入路径（原先各自是一个 layout），所以两档都保留断言。
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

afterEach(() => {
  act(() => {
    useAuthStore.setState({ status: 'anonymous', user: null, error: null, hint: null })
  })
})

/** 把 store 变更包进 act，否则 React 会报「update not wrapped in act(...)」。 */
function signIn() {
  act(() => {
    useAuthStore.setState({ status: 'authenticated', user })
  })
}

describe('登录后的壳层保留回官网的入口', () => {
  for (const path of ['/console', '/admin'] as const) {
    it(`${path} 入口：顶栏「首页」与窄屏图标都指向 /`, () => {
      signIn()

      const view = renderShell(path)

      const topNav = screen.getByRole('navigation', { name: '站点导航' })
      expect(within(topNav).getByRole('link', { name: '首页' })).toHaveAttribute('href', '/')

      const narrow = screen.getByRole('link', { name: '返回官网首页' })
      expect(narrow).toHaveAttribute('href', '/')

      view.unmount()
    })
  }
})

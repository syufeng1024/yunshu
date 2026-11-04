import type { ReactElement } from 'react'
import type { RouteObject } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { AppShell } from '@/layouts/AppShell'
import { ADMIN_PRICING_ROLES, ADMIN_SYSTEM_ROLES, ROUTES } from '@/lib/constants'
import { router } from '@/router'
import type { RequireAuthProps } from '@/router/guards'
import { RequireAuth } from '@/router/guards'

/**
 * 路由**接线**测试（不是组件测试）。
 *
 * 背景：`router/guards.test.tsx` 测的是 `RequireAuth` 组件本身，它**无法**发现
 * 「`router.tsx` 里把 `roles={ADMIN_PRICING_ROLES}` 删了」——删掉那一行，守卫组件
 * 依然工作、组件测试依然全绿，但 FINANCE 在地址栏敲 `/admin/pricing` 就能看到完整
 * 页面：菜单藏了、路由没堵，是**假下架**。2026-11-02 变异测试实测确认过这个盲区
 * （删掉接线后 `guards.test.tsx` 仍 6/6 通过），所以本文件专门锁住接线这一层。
 *
 * 断言分三块，缺一不可：
 * 1. `/admin/pricing` 的 element **确实是** `RequireAuth`，且 `roles` 就是生产常量
 *    `ADMIN_PRICING_ROLES`（不是另抄一份字面量，否则白名单仍有第二个真理源）；
 * 2. `/admin` 父路由的 `RequireAuth` **没有**传 `roles`，FINANCE 进运营后台其他页
 *    的行为不受本次收窄影响；
 * 3. `pricing` 子路由**仍然存在并挂着页面**——防「用删掉路由的方式下架」，那会把
 *    OPS / SUPER_ADMIN 一起锁在门外。
 *
 * `/admin/system`（系统管理）走的是同一套接线，用例在下面独立一节。
 *
 * 只断言 `RequireAuth` 的 props，不断言 403 页的正文文案：那段文案是产品口径、
 * 会单独调整，不该让路由接线测试跟着红。
 */

/**
 * 取 `/admin` 这一棵子树。
 *
 * 注意不能全局搜 `path === 'pricing'`：官网 `SiteLayout` 下也有一条 `pricing`
 * （公开的 `/pricing` 价格页）。先锚定 `/admin`，再在它的 children 里找，才是
 * 「运营后台的倍率与定价页」。
 */
function adminRoute(): RouteObject {
  const admin = router.routes.find((route) => route.path === '/admin')
  if (admin === undefined) {
    throw new Error('路由树里找不到 path="/admin"，router.tsx 的结构变了，本测试需要同步更新')
  }
  return admin
}

/** 在 `/admin` 的 children 里按相对 path 找子路由，找不到就显式失败（避免断言空转）。 */
function adminChild(path: string): RouteObject {
  const child = (adminRoute().children ?? []).find((route) => route.path === path)
  if (child === undefined) {
    throw new Error(`/admin 下找不到 path="${path}" 的子路由，本测试需要同步更新`)
  }
  return child
}

/** `RouteObject.element` 的类型是 `ReactNode`，取 props 前先收窄成元素。 */
function elementOf(route: RouteObject, path: string): ReactElement<RequireAuthProps> {
  const element = route.element as ReactElement<RequireAuthProps> | null | undefined
  if (element === null || element === undefined) {
    throw new Error(`path="${path}" 没有 element`)
  }
  return element
}

describe('/admin/pricing 的角色收窄接线（防假下架）', () => {
  it('pricing 子路由用 RequireAuth 包住，且 roles 取自生产常量 ADMIN_PRICING_ROLES', () => {
    const element = elementOf(adminChild('pricing'), 'pricing')

    expect(element.type).toBe(RequireAuth)
    expect(element.props.role).toBe('admin')
    // 关键断言：删掉 `roles={ADMIN_PRICING_ROLES}` 会让这一行变红。
    expect(element.props.roles).toEqual(ADMIN_PRICING_ROLES)
  })

  it('ADMIN_PRICING_ROLES 不含 FINANCE，因此上面那条接线真的挡住了 FINANCE', () => {
    const element = elementOf(adminChild('pricing'), 'pricing')

    expect(element.props.roles).not.toContain('FINANCE')
    expect(element.props.roles).toEqual(expect.arrayContaining(['OPS', 'SUPER_ADMIN']))
  })

  it('pricing 子路由仍然挂着页面：下架是「加守卫」而不是「删路由」', () => {
    const element = elementOf(adminChild('pricing'), 'pricing')

    // OPS / SUPER_ADMIN 还要用这一页，children 必须还在。
    expect(element.props.children).toBeTruthy()
    expect(ROUTES.adminPricing).toBe('/admin/pricing')
  })

  it('/admin 父路由不传 roles：FINANCE 进运营后台其他页的行为不变', () => {
    const element = elementOf(adminRoute(), '/admin')

    expect(element.type).toBe(RequireAuth)
    expect(element.props.role).toBe('admin')
    expect(element.props.roles).toBeUndefined()
  })

  it('只有 pricing 与 system 两个子路由被套上角色白名单', () => {
    const guarded = (adminRoute().children ?? []).filter((child) => {
      const element = child.element as ReactElement<Partial<RequireAuthProps>> | null | undefined
      return element !== null && element !== undefined && element.props?.roles !== undefined
    })

    // 收窄名单是显式列举的：多出一个受守卫的子路由 = 有人新加了页面级角色限制，
    // 少一个 = 有人把守卫删了（假下架）。两种都要在这里被发现。
    expect(guarded.map((child) => child.path)).toEqual(['pricing', 'system'])
  })
})

/**
 * 「系统管理」只调 `GET/PUT /api/admin/config`，权限点 `config.write` 只给了
 * OPS 与 SUPER_ADMIN（`internal/control/mw/authz.go`）。菜单按 ADMIN_SYSTEM_ROLES
 * 过滤，这里锁住路由层的第二道闸——否则 FINANCE 在地址栏敲 `/admin/system` 就能
 * 看到一个必然全线 403 的后台页面。
 */
describe('/admin/system 的角色收窄接线', () => {
  it('system 子路由用 RequireAuth 包住，且 roles 取自生产常量 ADMIN_SYSTEM_ROLES', () => {
    const element = elementOf(adminChild('system'), 'system')

    expect(element.type).toBe(RequireAuth)
    expect(element.props.role).toBe('admin')
    // 删掉 `roles={ADMIN_SYSTEM_ROLES}` 会让这一行变红。
    expect(element.props.roles).toEqual(ADMIN_SYSTEM_ROLES)
  })

  it('ADMIN_SYSTEM_ROLES 不含 FINANCE，因此真的挡住了财务', () => {
    const element = elementOf(adminChild('system'), 'system')

    expect(element.props.roles).not.toContain('FINANCE')
    expect(element.props.roles).toEqual(expect.arrayContaining(['OPS', 'SUPER_ADMIN']))
  })

  it('system 子路由挂着页面，路由常量与页面标题齐全', () => {
    const element = elementOf(adminChild('system'), 'system')

    expect(element.props.children).toBeTruthy()
    expect(ROUTES.adminSystem).toBe('/admin/system')
  })
})

/**
 * 单一外壳的接线：`/console` 与 `/admin` 必须挂**同一个** `AppShell`。
 *
 * 组件测试证明不了这一层——`AppShell.test.tsx` 渲染的是 AppShell 自身，把
 * AdminLayout 重新加回来、两个分支各挂一套时它依然全绿，而侧栏就此悄悄裂成两份。
 * 所以这里断言 element 里包着的是同一个组件。
 *
 * `role` 语义同时锁住：`/console` 是 `role="user"`（任何登录用户可进，个人用户也
 * 要能看自己的密钥与账单），`/admin` 是 `role="admin"`；父路由都**没有** `roles`，
 * 单页收窄只发生在 `/admin/pricing` 与 `/admin/system` 两处。
 */
describe('两个入口共用同一个外壳（防侧栏再被拆成两套）', () => {
  it('/console 与 /admin 都是 RequireAuth 包着 AppShell，role 语义不变', () => {
    const consoleRoute = router.routes.find((route) => route.path === '/console')
    if (consoleRoute === undefined) {
      throw new Error('路由树里找不到 path="/console"，router.tsx 的结构变了，本测试需要同步更新')
    }

    const consoleElement = elementOf(consoleRoute, '/console')
    const adminElement = elementOf(adminRoute(), '/admin')

    expect(consoleElement.type).toBe(RequireAuth)
    expect(consoleElement.props.role).toBe('user')
    expect(consoleElement.props.roles).toBeUndefined()

    expect(adminElement.type).toBe(RequireAuth)
    expect(adminElement.props.role).toBe('admin')
    expect(adminElement.props.roles).toBeUndefined()

    // 关键断言：两边挂的必须是同一个外壳组件，不许各自 fork 一份。
    const consoleShell = consoleElement.props.children as ReactElement
    const adminShell = adminElement.props.children as ReactElement
    expect(consoleShell.type).toBe(AppShell)
    expect(adminShell.type).toBe(AppShell)
    expect(consoleShell.type).toBe(adminShell.type)
  })
})

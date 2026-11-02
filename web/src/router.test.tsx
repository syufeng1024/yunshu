import type { ReactElement } from 'react'
import type { RouteObject } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { ADMIN_PRICING_ROLES, ROUTES } from '@/lib/constants'
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

  it('除 pricing 外没有别的运营子路由被套上角色白名单', () => {
    const guarded = (adminRoute().children ?? []).filter((child) => {
      const element = child.element as ReactElement<Partial<RequireAuthProps>> | null | undefined
      return element !== null && element !== undefined && element.props?.roles !== undefined
    })

    expect(guarded.map((child) => child.path)).toEqual(['pricing'])
  })
})

import { describe, expect, it } from 'vitest'
import {
  ADMIN_NAV,
  ADMIN_PRICING_ROLES,
  ADMIN_ROLES,
  ROUTE_TITLES,
  ROUTES,
  isAdminRole,
} from './constants'

/**
 * 「倍率与定价」页的准入名单（2026-11-02 口径）。
 *
 * 该页调 `/api/admin/prices`(`price.write`)、`/api/admin/groups`(`group.write`)、
 * `/api/admin/integrations`(`config.write`)，FINANCE 调用全是 403，页面基本不可用。
 * 需求方裁定从 FINANCE 菜单摘掉、且不补 `price.read` / `group.read`。
 *
 * 这两条断言是防回退的闸门：以后有人手抖把菜单项加回去、或把 FINANCE 塞进
 * 白名单，都会在这里失败。路由层的收窄见 router/guards.test.tsx。
 */
describe('运营后台菜单与「倍率与定价」准入', () => {
  it('ADMIN_NAV 不再收录「倍率与定价」', () => {
    const pricing = ADMIN_NAV.find((item) => item.to === ROUTES.adminPricing)

    expect(pricing).toBeUndefined()
  })

  it('ADMIN_PRICING_ROLES 不含 FINANCE，只放 OPS 与 SUPER_ADMIN', () => {
    expect(ADMIN_PRICING_ROLES).not.toContain('FINANCE')
    expect([...ADMIN_PRICING_ROLES].sort()).toEqual(['OPS', 'SUPER_ADMIN'])
    // 白名单必须是运营后台的子集，否则 OPS/SUPER_ADMIN 之外的残留会变成死配置。
    for (const role of ADMIN_PRICING_ROLES) {
      expect(ADMIN_ROLES).toContain(role)
      expect(isAdminRole(role)).toBe(true)
    }
  })

  it('路由与页面标题常量保留：OPS / SUPER_ADMIN 仍要用这个页面', () => {
    expect(ROUTES.adminPricing).toBe('/admin/pricing')
    expect(ROUTE_TITLES[ROUTES.adminPricing]).toBe('倍率与定价')
  })
})

describe('ADMIN_NAV 剩余菜单项仍然可用', () => {
  it('每个菜单项都指向唯一路由，且角色名单非空', () => {
    const targets = ADMIN_NAV.map((item) => item.to)

    expect(new Set(targets).size).toBe(targets.length)
    for (const item of ADMIN_NAV) {
      expect(item.roles.length).toBeGreaterThan(0)
      expect(ROUTE_TITLES[item.to as keyof typeof ROUTE_TITLES]).toBeDefined()
    }
  })
})

import { describe, expect, it } from 'vitest'
import {
  ADMIN_NAV,
  ADMIN_NAV_ITEMS,
  ADMIN_PRICING_ROLES,
  ADMIN_ROLES,
  ADMIN_SYSTEM_ROLES,
  GENERAL_NAV_ITEMS,
  PERSONAL_NAV_ITEMS,
  ROUTE_TITLES,
  ROUTES,
  SHELL_NAV_GROUPS,
  SHELL_NAV_GROUP_TITLES,
  isAdminRole,
  visibleShellNavGroups,
} from './constants'
import type { NavGroup, ShellNavItem } from './constants'

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

/**
 * 「系统管理」的准入名单（本轮新增）。权限点是 `config.write`，只有 OPS 与
 * SUPER_ADMIN 持有（internal/control/mw/authz.go），FINANCE 调用全是 403。
 *
 * 与「倍率与定价」同一套做法：菜单与路由共用 ADMIN_SYSTEM_ROLES 一个常量，
 * 这两条断言防止有人把 FINANCE 塞回白名单、或把菜单项挪出管理员组末尾。
 */
describe('「系统管理」准入', () => {
  it('ADMIN_SYSTEM_ROLES 只放 OPS 与 SUPER_ADMIN，不含 FINANCE', () => {
    expect(ADMIN_SYSTEM_ROLES).not.toContain('FINANCE')
    expect([...ADMIN_SYSTEM_ROLES].sort()).toEqual(['OPS', 'SUPER_ADMIN'])
    for (const role of ADMIN_SYSTEM_ROLES) {
      expect(ADMIN_ROLES).toContain(role)
    }
  })

  it('菜单项挂在管理员组末尾，标题与路由齐全', () => {
    const last = ADMIN_NAV_ITEMS[ADMIN_NAV_ITEMS.length - 1]

    expect(last?.to).toBe(ROUTES.adminSystem)
    expect(last?.label).toBe('系统管理')
    expect(last?.roles).toEqual(ADMIN_SYSTEM_ROLES)
    expect(ROUTE_TITLES[ROUTES.adminSystem]).toBe('系统管理')
    expect(ROUTES.adminSystem).toBe('/admin/system')
  })

  it('FINANCE 拿不到「系统管理」入口', () => {
    const items = visibleShellNavGroups('FINANCE').flatMap((group) => group.items)

    expect(items.some((item) => item.to === ROUTES.adminSystem)).toBe(false)
    // OPS / SUPER_ADMIN 则都能看到。
    for (const role of ['OPS', 'SUPER_ADMIN'] as const) {
      const visible = visibleShellNavGroups(role).flatMap((group) => group.items)
      expect(visible.some((item) => item.to === ROUTES.adminSystem)).toBe(true)
    }
  })
})

describe('ADMIN_NAV 剩余菜单项仍然可用', () => {  it('每个菜单项都指向唯一路由，且角色名单非空', () => {
    const targets = ADMIN_NAV.map((item) => item.to)

    expect(new Set(targets).size).toBe(targets.length)
    for (const item of ADMIN_NAV) {
      expect(item.roles.length).toBeGreaterThan(0)
      expect(ROUTE_TITLES[item.to as keyof typeof ROUTE_TITLES]).toBeDefined()
    }
  })
})

/**
 * 侧栏分组结构的数据层断言（不渲染组件）。
 *
 * 为什么要在这一层再写一遍：`AppShell.test.tsx` 只锁住了「渲染出来的 href 顺序」，
 * 而分组**语义**——哪三项属于个人、管理员组里每一项的角色名单——在数据结构上是
 * 没有断言的。渲染快照一红就知道坏了，但没人看得懂是"多了个菜单"还是"个人功能被
 * 混进了 /admin"；结构化断言失败时能直接指出是哪一组的哪一项。
 *
 * 这份表来自需求方给的三组固定口径（对齐 new-api 的 常规 / 个人 / 管理员），
 * 组内顺序即菜单显示顺序，顺序敏感。改这里的期望值 = 改产品口径，不是改测试。
 */
describe('SHELL_NAV_GROUPS 分组结构', () => {
  /** 按标题取组；找不到就显式失败，避免断言在 undefined 上空转。 */
  function groupOf(title: string): NavGroup<ShellNavItem> {
    const group = SHELL_NAV_GROUPS.find((candidate) => candidate.title === title)
    if (group === undefined) {
      throw new Error(`SHELL_NAV_GROUPS 里找不到标题为「${title}」的分组，本测试需要同步更新`)
    }
    return group
  }

  it('恰好三组，标题顺序固定为 常规 / 个人 / 管理员', () => {
    expect(SHELL_NAV_GROUPS).toHaveLength(3)
    // toEqual 对数组是顺序敏感的：谁把「个人」与「管理员」调个位置就会红。
    expect(SHELL_NAV_GROUPS.map((group) => group.title)).toEqual(['常规', '个人', '管理员'])
    expect(SHELL_NAV_GROUPS.map((group) => group.title)).toEqual([
      SHELL_NAV_GROUP_TITLES.general,
      SHELL_NAV_GROUP_TITLES.personal,
      SHELL_NAV_GROUP_TITLES.admin,
    ])
  })

  it('各组条数为 7 / 3 / 10，合计 20', () => {
    expect(SHELL_NAV_GROUPS.map((group) => group.items.length)).toEqual([7, 3, 10])
    expect(SHELL_NAV_GROUPS.reduce((sum, group) => sum + group.items.length, 0)).toBe(20)
  })

  it('管理员组的路由按顺序严格等于约定表', () => {
    expect(groupOf('管理员').items.map((item) => item.to)).toEqual([
      ROUTES.admin,
      ROUTES.adminUsers,
      ROUTES.adminChannels,
      ROUTES.adminModels,
      ROUTES.adminAnnouncements,
      ROUTES.adminReviews,
      ROUTES.adminOrders,
      ROUTES.adminRedeemCodes,
      ROUTES.adminTickets,
      ROUTES.adminSystem,
    ])
    // 与字面量对齐一次：上面用的是 ROUTES 常量，这里防止常量本身被改坏。
    expect(groupOf('管理员').items.map((item) => item.to)).toEqual([
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

  // 最容易写错的两处：「审核中心」是三项（OPS + FINANCE + SUPER_ADMIN），
  // 「工单处理」不含 FINANCE。所以逐项断言整份名单，而不是只断言非空或长度。
  it('管理员组每一项的角色名单逐项正确', () => {
    expect(groupOf('管理员').items.map((item) => item.roles)).toEqual([
      ADMIN_ROLES,
      ['OPS', 'SUPER_ADMIN'],
      ['OPS', 'SUPER_ADMIN'],
      ['OPS', 'SUPER_ADMIN'],
      ['OPS', 'SUPER_ADMIN'],
      ['OPS', 'FINANCE', 'SUPER_ADMIN'],
      ['FINANCE', 'SUPER_ADMIN'],
      ['FINANCE', 'SUPER_ADMIN'],
      ['OPS', 'SUPER_ADMIN'],
      ['OPS', 'SUPER_ADMIN'],
    ])
    // 上面用 ADMIN_ROLES 常量表达「运营概览对三种管理员角色都开放」；
    // 这里再钉一次字面量，防有人把 ADMIN_ROLES 本身改窄。
    expect(ADMIN_ROLES).toEqual(['OPS', 'FINANCE', 'SUPER_ADMIN'])
  })

  it('「审核中心」与「工单处理」的角色差异单独钉死', () => {
    const reviews = groupOf('管理员').items.find((item) => item.to === ROUTES.adminReviews)
    const tickets = groupOf('管理员').items.find((item) => item.to === ROUTES.adminTickets)

    expect(reviews?.roles).toContain('FINANCE')
    expect(tickets?.roles).not.toContain('FINANCE')
    expect(tickets?.roles).toEqual(['OPS', 'SUPER_ADMIN'])
  })

  it('常规组与个人组不含任何 /admin 路由', () => {
    // 反向证明：个人功能（充值/订阅/账户）没有被混进管理员路由，
    // 否则「所有登录用户都能用」的组会把人带进需要 role="admin" 的页面。
    const userFacing = [
      ...groupOf('常规').items,
      ...groupOf('个人').items,
    ]
    expect(userFacing.every((item) => !item.to.startsWith('/admin'))).toBe(true)
    expect(userFacing).toHaveLength(10)
  })

  it('「倍率与定价」不出现在任何一组里（2026-11-02 裁定）', () => {
    const all = SHELL_NAV_GROUPS.flatMap((group) => group.items)

    expect(all.some((item) => item.to === ROUTES.adminPricing)).toBe(false)
    expect(all.map((item) => item.to)).not.toContain('/admin/pricing')
  })

  it('分组数据与三个扁平列表是同一份来源', () => {
    expect(groupOf('常规').items).toBe(GENERAL_NAV_ITEMS)
    expect(groupOf('个人').items).toBe(PERSONAL_NAV_ITEMS)
    expect(groupOf('管理员').items).toBe(ADMIN_NAV_ITEMS)
    expect(ADMIN_NAV).toBe(ADMIN_NAV_ITEMS)
  })
})

/**
 * 角色过滤的数据层断言。渲染层的对应用例在 `layouts/AppShell.test.tsx`，
 * 这一层只验「过滤函数的输出形状」：组标题顺序不变、个人功能不受影响、
 * 管理员项逐个按名单裁剪。
 */
describe('visibleShellNavGroups 角色过滤', () => {
  it('PERSONAL 只拿到 常规 + 个人 两组，管理员组整组消失', () => {
    const groups = visibleShellNavGroups('PERSONAL')

    expect(groups.map((group) => group.title)).toEqual([
      SHELL_NAV_GROUP_TITLES.general,
      SHELL_NAV_GROUP_TITLES.personal,
    ])
    expect(groups.flatMap((group) => group.items).some((item) => item.to.startsWith('/admin'))).toBe(
      false,
    )
  })

  it('FINANCE 拿到三组，管理员组只剩财务与审核能用的项', () => {
    const groups = visibleShellNavGroups('FINANCE')
    const admin = groups.find((group) => group.title === SHELL_NAV_GROUP_TITLES.admin)

    expect(groups.map((group) => group.title)).toEqual([
      SHELL_NAV_GROUP_TITLES.general,
      SHELL_NAV_GROUP_TITLES.personal,
      SHELL_NAV_GROUP_TITLES.admin,
    ])
    expect(admin?.items.map((item) => item.to)).toEqual([
      ROUTES.admin,
      ROUTES.adminReviews,
      ROUTES.adminOrders,
      ROUTES.adminRedeemCodes,
    ])
  })

  it('SUPER_ADMIN 拿到全部 20 项，顺序与 SHELL_NAV_GROUPS 一致', () => {
    const groups = visibleShellNavGroups('SUPER_ADMIN')
    const flattened = groups.flatMap((group) => group.items.map((item) => item.to))

    expect(groups).toHaveLength(3)
    expect(flattened).toEqual(SHELL_NAV_GROUPS.flatMap((group) => group.items.map((item) => item.to)))
    expect(flattened).toHaveLength(20)
  })

  it('角色缺失（未 hydrate）时只显示两组，不泄露任何管理员入口', () => {
    for (const role of [null, undefined]) {
      const groups = visibleShellNavGroups(role)
      expect(groups.map((group) => group.title)).toEqual([
        SHELL_NAV_GROUP_TITLES.general,
        SHELL_NAV_GROUP_TITLES.personal,
      ])
    }
  })
})

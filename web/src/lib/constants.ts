/**
 * Brand, route and enum constants.
 *
 * Enum values are copied verbatim from the backend naming used in
 * docs/技术架构设计文档 V1.2.md (t_user.role / t_user.status /
 * t_api_key.status). Do not rename them in the UI layer.
 */

export const BRAND_NAME = '云枢 API'
export const BRAND_DESCRIPTION = '一站式 API 中转接入平台'
export const DEFAULT_TITLE = `${BRAND_NAME} · ${BRAND_DESCRIPTION}`
export const COPYRIGHT = `© 2026 ${BRAND_NAME}`
export const LOGO_ARIA_LABEL = `${BRAND_NAME} 首页`

export const ROUTES = {
  home: '/',
  pricing: '/pricing',
  docs: '/docs',
  login: '/login',
  integrations: '/integrations',
  // 法律条款占位页（正文待法务定稿）；页脚与登录页的协议名都指向这里。
  terms: '/terms',
  privacy: '/privacy',

  console: '/console',
  consoleKeys: '/console/keys',
  consoleKeyDetail: '/console/keys/:id',
  consoleModels: '/console/models',
  consoleUsage: '/console/usage',
  consoleRecharge: '/console/recharge',
  consoleLogs: '/console/logs',
  consolePlayground: '/console/playground',
  consoleTickets: '/console/tickets',

  admin: '/admin',
  adminChannels: '/admin/channels',
  adminUsers: '/admin/users',
  adminPricing: '/admin/pricing',
  adminAnnouncements: '/admin/announcements',
  adminReviews: '/admin/reviews',
  adminOrders: '/admin/orders',
  adminRedeemCodes: '/admin/redeem-codes',
  adminTickets: '/admin/tickets',
  adminModels: '/admin/models',
} as const

export type RoutePath = (typeof ROUTES)[keyof typeof ROUTES]

export function consoleKeyDetailPath(id: string): string {
  return `${ROUTES.consoleKeys}/${encodeURIComponent(id)}`
}

export const ROUTE_TITLES = {
  [ROUTES.home]: '首页',
  [ROUTES.pricing]: '定价',
  [ROUTES.docs]: '开发者文档',
  [ROUTES.login]: '登录 / 注册',
  [ROUTES.integrations]: '工具集成',
  [ROUTES.terms]: '用户协议',
  [ROUTES.privacy]: '隐私政策',

  [ROUTES.console]: '控制台 · 概览',
  [ROUTES.consoleKeys]: '密钥管理',
  [ROUTES.consoleModels]: '模型广场',
  [ROUTES.consoleUsage]: '用量与账单',
  [ROUTES.consoleRecharge]: '充值中心',
  [ROUTES.consoleLogs]: '请求日志',
  [ROUTES.consolePlayground]: '在线调试台',
  [ROUTES.consoleTickets]: '工单中心',

  [ROUTES.admin]: '运营概览',
  [ROUTES.adminChannels]: '渠道管理',
  [ROUTES.adminUsers]: '用户管理',
  [ROUTES.adminPricing]: '倍率与定价',
  [ROUTES.adminAnnouncements]: '公告与风控',
  [ROUTES.adminReviews]: '审核中心',
  [ROUTES.adminOrders]: '订单与收款',
  [ROUTES.adminRedeemCodes]: '兑换码',
  [ROUTES.adminTickets]: '工单处理',
  [ROUTES.adminModels]: '模型主数据',
} as const

export const ICON_NAMES = [
  'home',
  'key',
  'grid',
  'chart',
  'wallet',
  'terminal',
  'ticket',
  'users',
  'tag',
  'megaphone',
  'shield',
  'database',
  'book',
  'plug',
  'logout',
  'alert',
  'check',
  'copy',
  'chevron-left',
  'chevron-right',
  'chevron-down',
  'login',
] as const

export type IconName = (typeof ICON_NAMES)[number]

export interface NavItem {
  to: string
  label: string
  icon: IconName
  end?: boolean
}

export const SITE_NAV: readonly NavItem[] = [
  { to: ROUTES.home, label: '首页', icon: 'home', end: true },
  { to: ROUTES.pricing, label: '定价', icon: 'tag' },
  { to: ROUTES.docs, label: '文档', icon: 'book' },
  { to: ROUTES.integrations, label: '工具集成', icon: 'plug' },
]

export const CONSOLE_NAV: readonly NavItem[] = [
  { to: ROUTES.console, label: '概览', icon: 'home', end: true },
  { to: ROUTES.consoleKeys, label: '密钥管理', icon: 'key' },
  { to: ROUTES.consoleModels, label: '模型广场', icon: 'grid' },
  { to: ROUTES.consoleUsage, label: '用量与账单', icon: 'chart' },
  { to: ROUTES.consoleRecharge, label: '充值中心', icon: 'wallet' },
  { to: ROUTES.consoleLogs, label: '请求日志', icon: 'terminal' },
  { to: ROUTES.consolePlayground, label: '在线调试台', icon: 'terminal' },
  { to: ROUTES.consoleTickets, label: '工单中心', icon: 'ticket' },
]

export type UserRole = 'PERSONAL' | 'OPS' | 'FINANCE' | 'SUPER_ADMIN'
export type UserStatus = 'NORMAL' | 'DISABLED' | 'FROZEN_OVERDRAFT'
export type KeyStatus = 'ENABLED' | 'DISABLED'

export const USER_ROLE_LABEL: Record<UserRole, string> = {
  PERSONAL: '个人用户',
  OPS: '运营',
  FINANCE: '财务',
  SUPER_ADMIN: '超级管理员',
}

export const USER_STATUS_LABEL: Record<UserStatus, string> = {
  NORMAL: '正常',
  DISABLED: '已禁用',
  FROZEN_OVERDRAFT: '透支冻结',
}

export const KEY_STATUS_LABEL: Record<KeyStatus, string> = {
  ENABLED: '启用',
  DISABLED: '已停用',
}

export const ADMIN_ROLES: readonly UserRole[] = ['OPS', 'FINANCE', 'SUPER_ADMIN']

/**
 * 「倍率与定价」页（ROUTES.adminPricing）的角色白名单——2026-11-02 与需求方确认的口径。
 *
 * 该页只对「能读价格与分组倍率」的人有意义：它调 `/api/admin/prices`
 * (`price.write`)、`/api/admin/groups` (`group.write`)、`/api/admin/integrations`
 * (`config.write`)，而这几个权限点只给了 OPS 与 SUPER_ADMIN，FINANCE 调它们全是
 * 403（改权限之前就是坏的）。因为 FINANCE 没有 `price.write` / `group.write`，
 * 所以该页不对其开放，也不为它新开 `price.read` / `group.read`。
 *
 * 菜单（ADMIN_NAV 不再收录该页）与路由（router.tsx 里 RequireAuth 的 roles）都以
 * 本常量为准，避免两处各写一份白名单后再漂移。
 */
export const ADMIN_PRICING_ROLES: readonly UserRole[] = ['OPS', 'SUPER_ADMIN']

export function isAdminRole(role: UserRole | null | undefined): boolean {
  return role !== null && role !== undefined && ADMIN_ROLES.includes(role)
}

export interface AdminNavItem extends NavItem {
  roles: readonly UserRole[]
}

/**
 * 运营后台侧边菜单。`roles` 只控制菜单是否显示（体验层），真正的权限仍由
 * 服务端按权限点校验；路由层的单页角色收窄见 router.tsx。
 *
 * 注意：这里刻意**不收**「倍率与定价」（ROUTES.adminPricing）。该页依赖的三个
 * 接口对 FINANCE 全 403，给 FINANCE 显示一个空壳页只是误导，故整页只对
 * ADMIN_PRICING_ROLES 开放（2026-11-02 需求方裁定）。
 */
export const ADMIN_NAV: readonly AdminNavItem[] = [
  { to: ROUTES.admin, label: '运营概览', icon: 'home', end: true, roles: ADMIN_ROLES },
  { to: ROUTES.adminChannels, label: '渠道管理', icon: 'plug', roles: ['OPS', 'SUPER_ADMIN'] },
  { to: ROUTES.adminUsers, label: '用户管理', icon: 'users', roles: ['OPS', 'SUPER_ADMIN'] },
  {
    to: ROUTES.adminAnnouncements,
    label: '公告与风控',
    icon: 'megaphone',
    roles: ['OPS', 'SUPER_ADMIN'],
  },
  {
    to: ROUTES.adminOrders,
    label: '订单与收款',
    icon: 'wallet',
    roles: ['FINANCE', 'SUPER_ADMIN'],
  },
  {
    to: ROUTES.adminRedeemCodes,
    label: '兑换码',
    icon: 'ticket',
    roles: ['FINANCE', 'SUPER_ADMIN'],
  },
  {
    to: ROUTES.adminReviews,
    label: '审核中心',
    icon: 'shield',
    roles: ['OPS', 'FINANCE', 'SUPER_ADMIN'],
  },
  { to: ROUTES.adminTickets, label: '工单处理', icon: 'ticket', roles: ['OPS', 'SUPER_ADMIN'] },
  { to: ROUTES.adminModels, label: '模型主数据', icon: 'database', roles: ['OPS', 'SUPER_ADMIN'] },
]

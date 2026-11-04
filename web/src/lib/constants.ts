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
  models: '/models',
  docs: '/docs',
  login: '/login',
  // 自助找回密码。挂在站点布局下、不加 RequireAuth：「已登录但忘了旧密码」是
  // 真实场景，重置成功后当前会话也会一起失效。
  resetPassword: '/reset-password',
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
  consoleAccount: '/console/account',
  consoleSubscription: '/console/subscription',

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
  adminSystem: '/admin/system',
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
  [ROUTES.resetPassword]: '找回密码',
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
  [ROUTES.adminSystem]: '系统管理',
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
 * 菜单（ADMIN_NAV 不收录该页）与路由（router.tsx 里 RequireAuth 的 roles）都以
 * 本常量为准，避免两处各写一份白名单后再漂移。
 */
export const ADMIN_PRICING_ROLES: readonly UserRole[] = ['OPS', 'SUPER_ADMIN']

/**
 * 「系统管理」页（ROUTES.adminSystem）的角色白名单。
 *
 * 该页只调 `GET/PUT /api/admin/config`，权限点是 `config.write`，而
 * `internal/control/mw/authz.go` 只把它授予 OPS 与 SUPER_ADMIN——FINANCE
 * 调用是 403。所以菜单不收录给 FINANCE，路由层也用本常量再收窄一次
 * （与「倍率与定价」同一套做法：一处常量，菜单与路由共用）。
 */
export const ADMIN_SYSTEM_ROLES: readonly UserRole[] = ['OPS', 'SUPER_ADMIN']

export function isAdminRole(role: UserRole | null | undefined): boolean {
  return role !== null && role !== undefined && ADMIN_ROLES.includes(role)
}

export interface NavItem {
  to: string
  label: string
  icon: IconName
  end?: boolean
}

/**
 * 侧栏项。`roles` 只有「管理员」组会填——「常规」「个人」两组的语义就是
 * 「所有登录用户都能用」，给它俩逐条写 `ADMIN_ROLES` 那种全角色名单只是噪音，
 * 而且在有人新增角色时会变成需要同步的假配置。
 */
export interface ShellNavItem extends NavItem {
  roles?: readonly UserRole[]
}

/** 「管理员」组的菜单项：角色名单必填，防止手滑写出一项谁都能看见的管理入口。 */
export interface AdminNavItem extends ShellNavItem {
  roles: readonly UserRole[]
}

/**
 * 侧边栏分组。中文后台的菜单一多就会变成一长条没有层次的名字列表，
 * 分组标题是唯一不占额外宽度的分节手段（52pay 的控制台也是这么分的）。
 */
export interface NavGroup<T extends NavItem = NavItem> {
  title: string
  items: readonly T[]
}

export const SITE_NAV: readonly NavItem[] = [
  { to: ROUTES.home, label: '首页', icon: 'home', end: true },
  { to: ROUTES.pricing, label: '定价', icon: 'tag' },
  { to: ROUTES.models, label: '模型价格', icon: 'grid' },
  { to: ROUTES.docs, label: '文档', icon: 'book' },
  { to: ROUTES.integrations, label: '工具集成', icon: 'plug' },
]

/**
 * 常规：所有登录用户都能用的一次性工作（拿密钥、看模型、调接口、核账、提工单）。
 * 顺序按「接入 → 调试 → 看账 → 找客服」的使用链路排，不按模块重要性排。
 */
export const GENERAL_NAV_ITEMS: readonly NavItem[] = [
  { to: ROUTES.console, label: '概览', icon: 'home', end: true },
  { to: ROUTES.consoleKeys, label: '密钥管理', icon: 'key' },
  { to: ROUTES.consoleModels, label: '模型广场', icon: 'grid' },
  { to: ROUTES.consolePlayground, label: '在线调试台', icon: 'terminal' },
  { to: ROUTES.consoleUsage, label: '用量与账单', icon: 'chart' },
  { to: ROUTES.consoleLogs, label: '请求日志', icon: 'terminal' },
  { to: ROUTES.consoleTickets, label: '工单中心', icon: 'ticket' },
]

/**
 * 个人：属于「当前登录的这个人」而不是「这个账号下的资源」的入口——钱、订阅、
 * 身份。原来这三项散在「用量与记录」「账户」两组里，和工单、日志混在一起。
 */
export const PERSONAL_NAV_ITEMS: readonly NavItem[] = [
  { to: ROUTES.consoleRecharge, label: '充值中心', icon: 'wallet' },
  { to: ROUTES.consoleSubscription, label: '我的订阅', icon: 'tag' },
  { to: ROUTES.consoleAccount, label: '账户设置', icon: 'users' },
]

/**
 * 管理员：运营/财务/审核三类后台页面合成**一组扁平菜单**。
 *
 * 原来运维后台自己一套壳、自己一套分组（运营 / 财务 / 审核与支持），超管要在
 * 两个后台之间来回跳才能同时看用户与订单。现在个人功能与管理员功能在同一个
 * 侧栏里各归其组：一眼能分清「哪些是我自己的，哪些是我在管别人的」。
 *
 * `roles` 只控制菜单是否显示（体验层），真正的权限仍由服务端按权限点校验；
 * 路由层的单页角色收窄见 router.tsx。
 *
 * 注意：这里刻意**不收**「倍率与定价」（ROUTES.adminPricing）。该页依赖的三个
 * 接口对 FINANCE 全 403，给 FINANCE 显示一个空壳页只是误导，故整页只对
 * ADMIN_PRICING_ROLES 开放（2026-11-02 需求方裁定）。
 */
export const ADMIN_NAV_ITEMS: readonly AdminNavItem[] = [
  { to: ROUTES.admin, label: '运营概览', icon: 'home', end: true, roles: ADMIN_ROLES },
  { to: ROUTES.adminUsers, label: '用户管理', icon: 'users', roles: ['OPS', 'SUPER_ADMIN'] },
  { to: ROUTES.adminChannels, label: '渠道管理', icon: 'plug', roles: ['OPS', 'SUPER_ADMIN'] },
  { to: ROUTES.adminModels, label: '模型主数据', icon: 'database', roles: ['OPS', 'SUPER_ADMIN'] },
  {
    to: ROUTES.adminAnnouncements,
    label: '公告与风控',
    icon: 'megaphone',
    roles: ['OPS', 'SUPER_ADMIN'],
  },
  {
    to: ROUTES.adminReviews,
    label: '审核中心',
    icon: 'shield',
    roles: ['OPS', 'FINANCE', 'SUPER_ADMIN'],
  },
  { to: ROUTES.adminOrders, label: '订单与收款', icon: 'wallet', roles: ['FINANCE', 'SUPER_ADMIN'] },
  { to: ROUTES.adminRedeemCodes, label: '兑换码', icon: 'ticket', roles: ['FINANCE', 'SUPER_ADMIN'] },
  { to: ROUTES.adminTickets, label: '工单处理', icon: 'ticket', roles: ['OPS', 'SUPER_ADMIN'] },
  // 「系统管理」= 45 个配置键的在线维护入口。图标用 terminal（现有 ICON_NAMES
  // 里没有齿轮 / 滑杆，「系统级命令行配置」是最接近的语义），不新增图标资源。
  { to: ROUTES.adminSystem, label: '系统管理', icon: 'terminal', roles: ADMIN_SYSTEM_ROLES },
]

/**
 * 管理员菜单的扁平列表，即「管理员」分组的 items（同一个数组，不是副本）。
 *
 * `constants.test.ts` 里针对管理员菜单的既有防回退断言（不收「倍率与定价」、
 * 路由唯一、角色名单非空、`ROUTE_TITLES` 标题齐全）都建立在这个列表上，因此它
 * 保留独立导出名。至于**分组结构**——三组、组标题与顺序、各组条数、管理员组逐项
 * 的路由与角色名单、常规/个人组不得出现 `/admin` 路由——由 `constants.test.ts`
 * 的「SHELL_NAV_GROUPS 分组结构」与「visibleShellNavGroups 角色过滤」两组用例
 * 断言；渲染层的对应证据在 `layouts/AppShell.test.tsx`。改结构前先读那两组用例。
 */
export const ADMIN_NAV: readonly AdminNavItem[] = ADMIN_NAV_ITEMS

/** 侧栏三个分组标题。测试与布局都引用这里，避免中文字面量散落。 */
export const SHELL_NAV_GROUP_TITLES = {
  general: '常规',
  personal: '个人',
  admin: '管理员',
} as const

/**
 * 全站唯一的侧栏分组定义。顺序固定：常规 → 个人 → 管理员。
 *
 * `/console/*` 与 `/admin/*` 两个路由分支共用这一份数据（渲染见
 * `layouts/AppShell.tsx`），所以两边看到的侧栏必然一致——不再有「两个壳各写
 * 一份菜单、改一处忘一处」的口子。
 */
export const SHELL_NAV_GROUPS: readonly NavGroup<ShellNavItem>[] = [
  { title: SHELL_NAV_GROUP_TITLES.general, items: GENERAL_NAV_ITEMS },
  { title: SHELL_NAV_GROUP_TITLES.personal, items: PERSONAL_NAV_ITEMS },
  { title: SHELL_NAV_GROUP_TITLES.admin, items: ADMIN_NAV_ITEMS },
]

/**
 * 按角色过滤侧栏分组：带 `roles` 的项（即「管理员」组）只留角色命中者，整组
 * 过滤后为空则整组丢弃（`PERSONAL` 看不到「管理员」这个空标题）。
 *
 * 未登录 / 角色缺失时把所有 `roles` 项一律隐藏——宁缺勿滥，这种状态只可能出现在
 * store 还没 hydrate 的瞬间，多显示一个入口比少显示一个贵得多。
 */
export function visibleShellNavGroups(
  role: UserRole | null | undefined,
): readonly NavGroup<NavItem>[] {
  return SHELL_NAV_GROUPS.map((group) => ({
    title: group.title,
    items: group.items.filter((item) =>
      item.roles === undefined
        ? true
        : role !== null && role !== undefined && item.roles.includes(role),
    ),
  })).filter((group) => group.items.length > 0)
}

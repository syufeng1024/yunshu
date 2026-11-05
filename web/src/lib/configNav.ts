import type { IconName } from '@/lib/constants'

/**
 * 「系统管理」的菜单结构。
 *
 * 对齐参考站（new-api 的 /system-settings）的信息架构：左侧可折叠分组，每组下面
 * 若干设置项，**点一项只看这一项的配置**。原来是一张 45 键的长页 + 5 个命名空间的
 * 锚点导航——命名空间是后端的存储概念（PRICING / UPSTREAM…），运营看不出
 * 「我要改退款口径」该点哪里。
 *
 * 两条硬约束：
 *
 * 1. **每个键必须且只能属于一个设置项**。`CONFIG_NAV` 的键集合由
 *    `configNav.test.ts` 对着 `CONFIG_FIELD_META` 做全等断言，漏登记或多登记都会红。
 * 2. **菜单里不出现没有配置项的条目**。参考站有 26+ 项，但那背后各有自己的子系统
 *    （SMTP 发信、敏感词过滤、OAuth…）；我们这些能力当前由部署环境变量或代码固定，
 *    没有可在线改的键。2026-11-05 需求方裁定「系统设置不计入本产品范围」，因此只留
 *    有真实配置的 11 项，空项一律从菜单移除，避免点开是空页。要恢复某一项，先把它
 *    背后的键与消费端补上，再加回这里。
 */

export interface ConfigNavItem {
  /** 稳定 id：用作 key 与窄屏下拉的 value，改动会让深链失效。 */
  id: string
  title: string
  /** 该设置项管什么，显示在内容区标题下。 */
  description: string
  /** 归属本项的配置键；空数组表示该项不该出现在菜单里（见文件头第 2 条）。 */
  keys: readonly string[]
}

export interface ConfigNavGroup {
  id: string
  title: string
  icon: IconName
  items: readonly ConfigNavItem[]
}

export const CONFIG_NAV: readonly ConfigNavGroup[] = [
  {
    id: 'site',
    title: '站点与品牌',
    icon: 'grid',
    items: [
      {
        id: 'site-info',
        title: '系统信息',
        description:
          '官网首页的展示口径。站点名称、品牌标识与顶部导航随发布包固定，不在这里改。',
        keys: ['site.recommend_limit'],
      },
    ],
  },
  {
    id: 'billing',
    title: '计费与支付',
    icon: 'wallet',
    items: [
      {
        id: 'quota-settings',
        title: '额度设置',
        description:
          '预扣比例与上限、扣减与退款顺序、结算窗口、透支额度。改动直接影响扣费结果，发布前请先核对结算链路口径。',
        keys: [
          'billing.overdraft_limit_default',
          'quota.overdraft_warn_days',
          'billing.prehold_ratio',
          'billing.prehold_max_ratio',
          'billing.prestimate_safety_bp',
          'billing.estimate_language_coef',
          'billing.long_ctx_prehold_guard_bp',
          'billing.deduct_policy',
          'billing.refund_policy',
          'billing.settle_deadline_ms',
        ],
      },
      {
        id: 'model-pricing',
        title: '模型定价',
        description: '取价层级、成本来源、毛利地板与缓存计价。单价本身在「倍率与定价」里按模型维护。',
        keys: [
          'pricing.cost_source',
          'pricing.tier_chain',
          'pricing.min_margin_bp',
          'pricing.tax_rate_multiplier',
          'pricing.cache_billing_enabled',
          'pricing.agent_markup',
        ],
      },
      {
        id: 'group-pricing',
        title: '分组定价',
        description: '套餐默认档位与体验期、分组倍率的毛利地板、赠送积分占比上限，以及 Key 分组切换权限。',
        keys: [
          'pricing.group_switch_mode',
          'pricing.max_bonus_ratio_bp',
          'pricing.plan_min_margin_bp',
          'plan.default_tier',
          'plan.trial_days',
        ],
      },
      {
        id: 'checkin-reward',
        title: '签到奖励',
        description: '赠送积分批次的到期天数。到期后未用完的赠送额度自动作废，充值额度不受影响。',
        keys: ['billing.bonus_expire_days'],
      },
    ],
  },
  {
    id: 'routing',
    title: '模型与路由',
    icon: 'plug',
    items: [
      {
        id: 'route-reliability',
        title: '路由可靠性',
        description:
          '上游超时、客户端总预算、流式首字节与总时长、重试次数与门槛、结算预留窗口。模型的上下架与别名在「模型主数据」维护。',
        keys: [
          'gw.upstream_timeout_ms',
          'gw.client_timeout_budget_ms',
          'gw.stream_first_byte_ms',
          'gw.stream_total_ms',
          'gw.settle_reserve_ms',
          'gw.retry_max',
          'gw.retry_threshold_ms',
        ],
      },
    ],
  },
  {
    id: 'security',
    title: '安全与限制',
    icon: 'shield',
    items: [
      {
        id: 'rate-limit',
        title: '速率限制',
        description:
          '日额度的计量口径、重置维度与时区，以及四维限流（并发 / 分钟请求 / 分钟 token / 日额度）的判定顺序。',
        keys: ['quota.rate_limit_order', 'quota.daily_meter', 'quota.reset_scope', 'quota.reset_tz'],
      },
      {
        id: 'token-limit',
        title: '令牌限制',
        description: '密钥的日额度模式与免费档活跃密钥上限。',
        keys: ['quota.key_daily_mode', 'quota.free_max_active_keys'],
      },
    ],
  },
  {
    id: 'ops',
    title: '运维',
    icon: 'terminal',
    items: [
      {
        id: 'system-behavior',
        title: '系统行为',
        description: '幂等去重窗口与租约参数，以及进程优雅退出时的等待上限。',
        keys: [
          'idem.duplicate_window_seconds',
          'idem.lease_ttl_seconds',
          'idem.lease_renew_seconds',
          'gw.drain_deadline_ms',
        ],
      },
      {
        id: 'log-maintenance',
        title: '日志维护',
        description: '请求体的落库模式、日志体积口径、保留天数，以及日志库是否与主库同实例。',
        keys: ['log.body_mode', 'log.volume_basis', 'log.retention_days', 'log.same_instance'],
      },
      {
        id: 'performance',
        title: '性能',
        description: '单实例的全局并发闸门。超过这个数的新请求会在网关排队而不是打穿上游。',
        keys: ['gw.max_inflight'],
      },
    ],
  },
]

/** 扁平化后的设置项：多带一份所属分组，窄屏下拉与页头徽章都要用。 */
export interface ConfigNavEntry extends ConfigNavItem {
  groupId: string
  groupTitle: string
}

/** 扁平化后的设置项列表，窄屏下拉与「跳到上一项」都用它。 */
export const CONFIG_NAV_ITEMS: readonly ConfigNavEntry[] = CONFIG_NAV.flatMap((group) =>
  group.items.map((item) => ({ ...item, groupId: group.id, groupTitle: group.title })),
)

/** 设置项 id → 该项；找不到返回 undefined（深链传了个不存在的 id）。 */
export const CONFIG_NAV_ITEM_BY_ID = new Map(CONFIG_NAV_ITEMS.map((item) => [item.id, item]))

/** 配置键 → 设置项 id。渲染时用它反查「这个脏键属于哪一项」。 */
export const CONFIG_NAV_ITEM_OF_KEY = new Map<string, string>(
  CONFIG_NAV_ITEMS.flatMap((item) => item.keys.map((key) => [key, item.id] as const)),
)

/** 默认选中的设置项：第一项。菜单里已不存在空项，所以第一项必然有配置。 */
export function firstItemWithKeys(): ConfigNavEntry {
  return CONFIG_NAV_ITEMS[0]
}

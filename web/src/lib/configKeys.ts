/**
 * 45 个配置键的中文文案与分组口径（`/admin/system`）。
 *
 * 键名、命名空间、默认值、是否运行时可写，全部以 `docs/config-keys.md`
 * 与 `internal/config/key.go` 为准。这里**只**放两样东西：
 *
 *  1. 面向运营的中文标签 + 一句话说明（从文档「消费方」列提炼，不是直译）；
 *  2. 页内 5 个分组与后端的 namespace 的对应关系。
 *
 * 刻意**不放**每个键的值类型、默认值或可写性——那三样只有服务端知道，
 * 前端抄一份必然随键表漂移。类型来自 `GET /api/admin/config` 返回的
 * `value` 的 JSON 类型，可写性来自 `runtime_writable`，默认值来自
 * `source === 'DEFAULT'`。这里只缺「这个词中文怎么说」这一件事。
 */

export interface ConfigFieldMeta {
  /** 控件上方的主标签，尽量短（运营扫一眼就知道在调什么）。 */
  label: string
  /** 一句话说明：这个键改变什么行为、对谁可见。 */
  description: string
}

/**
 * 键 → 中文文案。45 条，与 `docs/config-keys.md` 的键表一一对应。
 *
 * 未登记的键（后端以后补键）不会让页面报错：`configFieldMeta()` 会回落到
 * 等宽键名 + 一句「尚未登记中文说明」，键本身照常可读可改。
 */
export const CONFIG_FIELD_META: Readonly<Record<string, ConfigFieldMeta>> = {
  // ---- PRICING：计费与配额（billing.* 10 + pricing.* 9 + plan.* 2 = 21）----
  'billing.deduct_policy': {
    label: '扣减顺序',
    description: '结算时按什么顺序扣积分：fifo_expiry 表示先扣最快过期的批次，避免赠送积分白白作废。',
  },
  'billing.refund_policy': {
    label: '退款策略',
    description: '退款与回滚怎么退：by_order 表示退回当初扣减的那笔订单，不重新计算价格。',
  },
  'billing.overdraft_limit_default': {
    label: '默认透支上限',
    description: '新开账户的透支上限（积分）。余额扣穿后还能欠这么多，超过即停止调用。',
  },
  'billing.estimate_language_coef': {
    label: '语言预估系数',
    description: '按语言（zh / en / code / mixed）区分的 token 预估系数，请求前用它估算用量。',
  },
  'billing.prehold_ratio': {
    label: '预扣比例',
    description: '预估费用 × 该系数作为预扣金额，请求结束后按实际用量结算差额。',
  },
  'billing.prehold_max_ratio': {
    label: '预扣上限比例',
    description: '预扣金额占账户可用额度的上限（0.5 即最多预扣一半），防止单次请求锁死余额。',
  },
  'billing.settle_deadline_ms': {
    label: '结算时限',
    description: '结算必须在时限内完成：非流式 5000ms、流式 10000ms，超时的请求进入对账。',
  },
  'billing.bonus_expire_days': {
    label: '赠送积分有效期',
    description: '赠送积分批次的存活天数（90 天），到期后未用完的部分自动失效。',
  },
  'billing.long_ctx_prehold_guard_bp': {
    label: '长上下文预扣保护线',
    description: '预估用量超过长上下文阈值的 90% 时直接按阶梯价预扣，避免低估导致事后补扣。',
  },
  'billing.prestimate_safety_bp': {
    label: '预估安全余量',
    description: '请求前把预估 token 数放大到 1.10 倍（只放大 token 数，不放大金额），减少余额被击穿。',
  },
  'pricing.cost_source': {
    label: '成本来源',
    description: '毛利计算采用的成本来源：manual 表示用人工维护的成本价，不自动抓取上游账单。',
  },
  'pricing.tier_chain': {
    label: '取价层级',
    description: '定价的匹配优先级链 model>channel：先看模型级价格，找不到再回落到渠道级价格。',
  },
  'pricing.agent_markup': {
    label: '代理加价',
    description: '代理渠道加价开关。V1 恒为 off，不做代理加价，改动前请先确认结算链路是否支持。',
  },
  'pricing.cache_billing_enabled': {
    label: '缓存计价开关',
    description: '是否把命中缓存的 token 按缓存单价单独计价；关闭时缓存命中仍按普通输入价计费。',
  },
  'pricing.min_margin_bp': {
    label: '单价毛利地板',
    description: '模型单价的最低毛利率（2500 即 25%）。低于它保存价格时会告警，方便拦住亏本定价。',
  },
  'pricing.plan_min_margin_bp': {
    label: '套餐毛利地板',
    description: '套餐类定价的最低毛利率（1000 即 10%），口径与单价地板一致。',
  },
  'pricing.max_bonus_ratio_bp': {
    label: '赠送积分占比上限',
    description: '套餐内赠送积分占套餐额度的比例上限（3000 即 30%），防止套餐被当成免费额度卖。',
  },
  'pricing.tax_rate_multiplier': {
    label: '税率系数',
    description: '税率占位。V1 恒为 1.00（不含税），接入税制后由计价链路读取，不要手工调整。',
  },
  'pricing.group_switch_mode': {
    label: '分组切换方式',
    description: '密钥分组由谁切换：ops_only 表示只有运营能改，用户不能自助换分组。',
  },
  'plan.default_tier': {
    label: '默认套餐档位',
    description: '新注册用户自动落入的套餐档位（free），决定其初始额度与可用模型范围。',
  },
  'plan.trial_days': {
    label: '体验期天数',
    description: '新用户免费体验期长度（7 天），到期后按所选套餐正常计费。',
  },

  // ---- RATELIMIT：限流与额度（quota.* 6）----
  'quota.daily_meter': {
    label: '日额度计量口径',
    description: '日额度按什么口径占用：prehold 表示请求进来预扣时就计入当日用量，不等结算。',
  },
  'quota.key_daily_mode': {
    label: '密钥日额度模式',
    description: '单个密钥是否另有日额度。V1 只实现 none（不按键单独限日额度），填其他值会校验失败。',
  },
  'quota.reset_scope': {
    label: '日额度重置维度',
    description: '日额度按账户还是按密钥重置（account 为按账户），决定额度池是共享还是各自独立。',
  },
  'quota.reset_tz': {
    label: '日额度重置时区',
    description: '日额度在哪个时区跨天归零（Asia/Shanghai 即北京时间零点），影响用户体验到的「今天」。',
  },
  'quota.rate_limit_order': {
    label: '限流判定顺序',
    description: '四维限流的判定次序 并发>rpm>tpm>daily：先命中的一维先拒绝，决定报错文案与排查方向。',
  },
  'quota.free_max_active_keys': {
    label: '免费账户密钥上限',
    description: '免费账户最多能同时持有几个活跃密钥（3 个），超限时创建密钥会被拒绝。',
  },

  // ---- UPSTREAM：网关与超时（gw.* 9）----
  'gw.max_inflight': {
    label: '单实例并发上限',
    description: '单个网关实例同时处理的最大请求数（8000），触及上限后新请求排队或直接被拒。',
  },
  'gw.drain_deadline_ms': {
    label: '优雅退出等待上限',
    description: '进程退出前等待在途请求完成的最长时间（120 秒），超过即强制断开，发版滚动时用得上。',
  },
  'gw.upstream_timeout_ms': {
    label: '上游超时（非流式）',
    description: '非流式请求单次上游调用的超时（50 秒）。超时后按重试配置决定是否换渠道重试。',
  },
  'gw.client_timeout_budget_ms': {
    label: '客户端总超时预算',
    description: '一个客户端请求的总时间预算（120 秒），包含排队、重试与等待结算，超时即向客户端报错。',
  },
  'gw.retry_max': {
    label: '最大重试次数',
    description: '失败后最多再试几次（1 表示共 2 次尝试）。调高会放大上游压力，也可能放大重复计费风险。',
  },
  'gw.stream_first_byte_ms': {
    label: '流式首字节超时',
    description: '流式请求等上游吐出第一个字节的上限（30 秒），超过即判失败并重试下一个渠道。',
  },
  'gw.stream_total_ms': {
    label: '流式总时长上限',
    description: '流式响应最长持续多久（600 秒即 10 分钟），超长输出会被截断并结算已产生用量。',
  },
  'gw.settle_reserve_ms': {
    label: '结算预留窗口',
    description: '请求结束后留给结算对账的时间窗口（15 秒），窗口内余额处于「待结算」状态。',
  },
  'gw.retry_threshold_ms': {
    label: '重试门槛',
    description: '上游响应慢于该阈值就提前重试：非流式 65 秒、流式 45 秒，按请求是否流式取值。',
  },

  // ---- FEATURE：功能开关（log.* 4 + idem.* 3 + site.* 1 = 8）----
  'log.body_mode': {
    label: '正文落库模式',
    description: '请求与响应正文是否写入数据库：meta_only 表示只存元信息，正文不落库（合规与容量都更安全）。',
  },
  'log.volume_basis': {
    label: '日志体积口径',
    description: '日志体积按什么统计（meta 表示只算元信息），影响容量预估与保留策略的规划。',
  },
  'log.retention_days': {
    label: '日志保留期',
    description: '请求日志保留多少天（180 天），到期自动清理；调长会线性增加存储成本。',
  },
  'log.same_instance': {
    label: '日志库同实例',
    description: '日志库是否与主库同实例部署。仅作部署事实的登记，影响容量隔离与备份策略。',
  },
  'idem.duplicate_window_seconds': {
    label: '去重窗口',
    description: '未带幂等键的请求按秒级窗口自动去重，取值 0~3600；配 0 关闭网关自动去重（控制面写操作仍回落 10 秒）。',
  },
  'idem.lease_ttl_seconds': {
    label: '幂等租约有效期',
    description: '幂等租约的有效期（15 秒），过期后同一幂等键可被重新执行。',
  },
  'idem.lease_renew_seconds': {
    label: '幂等租约续约间隔',
    description: '执行中的请求每隔 5 秒续租一次，避免长耗时请求的租约中途过期被重复执行。',
  },
  'site.recommend_limit': {
    label: '推荐模型条数',
    description: '官网首页「推荐模型」区块展示几个模型（8 个），只影响展示，不影响可调用范围。',
  },

  // ---- UNASSIGNED：系统行为（未归类键）----
  'quota.overdraft_warn_days': {
    label: '负余额宽限天数',
    description: '账户负余额持续多少天后自动停用（7 天）。该功能尚未排期，键未归入任何命名空间。',
  },
}

/** 未登记键的回退文案：不猜内容，只保证键名可读可对照日志。 */
export function configFieldMeta(key: string): ConfigFieldMeta {
  const known = CONFIG_FIELD_META[key]
  if (known !== undefined) return known
  return {
    label: key,
    description: '该键尚未登记中文说明（后端新增键时前端文案需要同步补一行），请对照 docs/config-keys.md 使用。',
  }
}

export interface ConfigGroupMeta {
  /** 页内锚点 id 的一部分，同时也是 React key。 */
  id: string
  /** 分组标题（与需求方给的固定顺序一致）。 */
  title: string
  /** 后端 `namespace` 字段的取值。 */
  namespace: string
  description: string
}

/**
 * 页内 5 个分组，顺序固定。渲染时按 `namespace` 把接口返回的键归入对应组；
 * 某组没有键就整组不渲染（`UNASSIGNED` 目前只有 1 个键，后端以后补齐前
 * 也不会留下空壳）。
 */
export const CONFIG_GROUPS: readonly ConfigGroupMeta[] = [
  {
    id: 'pricing',
    title: '计费与配额',
    namespace: 'PRICING',
    description: '预扣、结算、退款与定价口径。改动会直接影响扣费结果，发布前请先确认结算链路口径。',
  },
  {
    id: 'ratelimit',
    title: '限流与额度',
    namespace: 'RATELIMIT',
    description: '日额度的计量与重置口径，以及四维限流的判定顺序。',
  },
  {
    id: 'upstream',
    title: '网关与超时',
    namespace: 'UPSTREAM',
    description: '并发闸门、超时预算与重试门槛。改动影响所有线上请求的成败与耗时。',
  },
  {
    id: 'feature',
    title: '功能开关',
    namespace: 'FEATURE',
    description: '日志落库、幂等窗口与站点展示类开关。',
  },
  {
    id: 'unassigned',
    title: '系统行为',
    namespace: 'UNASSIGNED',
    description: '后端尚未归入任何命名空间的键，仅作事实登记。',
  },
]

/**
 * 未知 namespace 的兜底分组。后端以后若新增第六个命名空间，键不会因为
 * 「不在已知分组里」被静默丢弃——它们会出现在这一组，提醒前端补文案。
 */
export const CONFIG_FALLBACK_GROUP: ConfigGroupMeta = {
  id: 'other',
  title: '其他',
  namespace: '',
  description: '接口返回了未登记的命名空间，这里原样列出，避免配置项被静默忽略。',
}

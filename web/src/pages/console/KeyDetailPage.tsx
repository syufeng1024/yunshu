import { useEffect, useMemo, useState } from 'react'
import { useAuthStore } from '@/stores/auth'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { errorHint, errorMessage } from '@/api/client'
import {
  fetchKeyDetail,
  useConsoleModels,
  useKeyAudit,
  useKeyDetail,
  useKeyUsage,
  useRotateKey,
  useUpdateKey,
} from '@/api/console'
import type { KeyAuditItem, KeyDetail, KeyModelRef, ModelScopeMode } from '@/api/console'
import {
  Badge,
  Button,
  ErrorState,
  LineChart,
  PageHeader,
  Skeleton,
  Table,
  useToast,
} from '@/components'
import type { TableColumn } from '@/components'
import {
  ConfirmDialog,
  KeySecretOnce,
  QuotaBar,
} from '@/components/console'
import type { QuotaDimension, RevealedKey } from '@/components/console'
import {
  auditActionLabel,
  auditFieldLabel,
  auditFieldValueLabel,
  groupLabel,
} from '@/lib/consoleLabels'
import { ROUTES, consoleKeyDetailPath } from '@/lib/constants'
import {
  creditsFromMicro,
  formatCreditAmount,
  formatDateTime,
  formatInteger,
  formatRatioBp,
} from '@/lib/format'
import { useRealtimeMetric } from '@/hooks/useRealtimeMetric'
import { usePageTitle } from '@/lib/usePageTitle'

/**
 * 操作日志里存的是机器值（ENABLED / ALL / OFF / 带 T 和 Z 的时间）。
 * 直接抛给用户就是一堆英文技术词，所以按字段翻译成人话再渲染：
 * 字段决定译法，未登记的字段退回原值（总比吞掉信息好）。
 */
/**
 * 日志里不展示的字段。两类：
 * - ip_allowlist / ip_whitelist_mode：来源地址限制已下线，留着会让人以为还有这功能。
 * - key_prefix / key_last4：密钥的存储实现细节（sk-TJ38V / VJW1 两段），
 *   对用户没有任何动作价值——掩码已经在「密钥信息」里，这里再拆两行只是噪声。
 */
const HIDDEN_AUDIT_FIELDS = new Set([
  'ip_allowlist',
  'ip_whitelist_mode',
  'key_prefix',
  'key_last4',
])

function auditValue(field: string, value: unknown): string {
  if (value === null || value === undefined || value === '') {
    return field === 'expire_at' ? '永不过期' : '—'
  }
  if (Array.isArray(value)) return value.length === 0 ? '（未指定）' : value.join('、')
  if (typeof value === 'boolean') return value ? '是' : '否'
  if (typeof value === 'object') return '（已变更）'
  if (field === 'expire_at') return formatDateTime(String(value))
  return auditFieldValueLabel(field, String(value))
}

function AuditDiff({ value }: { value: Record<string, unknown> | null }) {
  if (!value) return <span className="text-ink-muted">—</span>
  if (Array.isArray(value) || typeof value !== 'object') {
    return <span className="text-xs text-ink-secondary">{auditValue('', value)}</span>
  }
  if (Object.keys(value).length === 0) {
    return <span className="text-ink-muted">—</span>
  }
  return (
    <dl className="space-y-0.5 text-xs">
      {Object.entries(value)
        // 来源地址限制已从界面下线，历史记录里的这两个字段不再展示：
        // 留在页面上只会让人以为还有这个功能。
        .filter(([field]) => !HIDDEN_AUDIT_FIELDS.has(field))
        // 模型范围选「全部」时 model_ids 必然是空数组，写成「模型编号：（未指定）」
        // 会让人以为漏配了；范围本身已由「可调用模型：全部已上架模型」表达。
        .filter(
          ([field, fieldValue]) =>
            !(field === 'model_ids' && Array.isArray(fieldValue) && fieldValue.length === 0),
        )
        .map(([field, fieldValue]) => (
        <div key={field} className="flex gap-1">
          <dt className="shrink-0 text-ink-muted">{auditFieldLabel(field)}：</dt>
          <dd className="min-w-0 break-all text-ink-secondary">{auditValue(field, fieldValue)}</dd>
        </div>
        ))}
    </dl>
  )
}

function RealtimeCell({
  used,
  limit,
  available,
}: {
  used: number | null
  limit: number
  available: boolean
}) {
  if (!available || used === null) {
    return (
      <span className="text-ink-muted" title="实时指标获取失败或暂不可用">
        --
      </span>
    )
  }
  return (
    <span className="tabular-nums text-ink">
      {formatInteger(used)} <span className="text-ink-muted">/ {formatInteger(limit)}</span>
    </span>
  )
}

export default function KeyDetailPage() {
  // 审计里的 operator_id 与当前登录账号比对：自己做的操作显示「本人」，不暴露编号格式。
  const currentUserId = useAuthStore((state) => state.user?.id ?? null)
  usePageTitle('密钥详情')
  const { id } = useParams<{ id: string }>()

  // 趋势区间：24 小时看当下是否在用，7 天看是否还有必要保留这把密钥。
  const [usageRange, setUsageRange] = useState<'24h' | '7d'>('24h')
  const usageHours = usageRange === '24h' ? 24 : 168
  const usage = useKeyUsage(id, usageHours)
  const usageData = useMemo(
    () =>
      (usage.data?.points ?? []).map((point) => ({
        label:
          usageRange === '24h'
            ? point.hour.slice(11, 16)
            : point.hour.slice(5, 10),
        requests: point.requests,
        credits: creditsFromMicro(point.consumed_micro),
      })),
    [usage.data, usageRange],
  )

  /**
   * 后端会为区间内每个小时补 0，所以「没调用」时也会拿到 24/168 个全 0 点。
   * 直接喂给折线图画出来就是一条压在 0 上的直线——看起来像图坏了，其实是没数据。
   * 这里判定「整段都是 0」后交给空状态，只有真有过调用才画线。
   */
  const usageTotals = useMemo(
    () =>
      usageData.reduce(
        (acc, point) => ({
          requests: acc.requests + point.requests,
          credits: acc.credits + point.credits,
        }),
        { requests: 0, credits: 0 },
      ),
    [usageData],
  )
  const usageHasData = usageTotals.requests > 0 || usageTotals.credits > 0
  const usageRangeLabel = usageRange === '24h' ? '近 24 小时' : '近 7 天'
  const usageError = usage.isError ? errorMessage(usage.error) : null
  const navigate = useNavigate()
  const toast = useToast()

  const detail = useKeyDetail(id)
  const [auditPageNumber, setAuditPageNumber] = useState(1)
  const audit = useKeyAudit(id, auditPageNumber)

  // F7-2: poll the detail endpoint every 8s for the rate-limit block. A failed
  // poll yields null so the cells show "--" instead of a stale or fake zero.
  const live = useRealtimeMetric<KeyDetail['realtime']>(
    () => fetchKeyDetail(id as string).then((response) => response.realtime),
    { enabled: Boolean(id) && detail.isSuccess },
  )
  const realtime = live.error ? null : (live.value ?? detail.data?.realtime ?? null)

  const [editing, setEditing] = useState(false)
  const [draftMode, setDraftMode] = useState<ModelScopeMode>('ALL')
  const [draftIds, setDraftIds] = useState<number[]>([])
  const [draftError, setDraftError] = useState<string | null>(null)
  const [confirmSave, setConfirmSave] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [rotateOpen, setRotateOpen] = useState(false)
  const [rotateError, setRotateError] = useState<string | null>(null)
  const [revealed, setRevealed] = useState<RevealedKey[] | null>(null)
  const [revealedWarning, setRevealedWarning] = useState<string | undefined>(undefined)
  const [rotatedId, setRotatedId] = useState<number | null>(null)

  const updateKey = useUpdateKey()
  const rotateKey = useRotateKey()
  const modelsQuery = useConsoleModels(editing)

  useEffect(() => {
    setEditing(false)
    setConfirmSave(false)
    setRevealed(null)
    setRotatedId(null)
    setAuditPageNumber(1)
  }, [id])

  const data = detail.data

  const startEditing = () => {
    if (!data) return
    setDraftMode(data.model_scope_mode)
    setDraftIds(data.models.map((model) => model.id))
    setDraftError(null)
    setEditing(true)
  }

  const requestSave = () => {
    if (draftMode === 'SELECTED' && draftIds.length === 0) {
      setDraftError('请至少选择一个模型，或改为「全部模型」。')
      return
    }
    setDraftError(null)
    setSaveError(null)
    setConfirmSave(true)
  }

  const confirmModelSave = async () => {
    if (!data) return
    setSaveError(null)
    try {
      await updateKey.mutateAsync({
        id: data.id,
        input: {
          model_scope_mode: draftMode,
          ...(draftMode === 'SELECTED' ? { model_ids: draftIds } : {}),
        },
      })
      toast.success('模型权限已更新')
      setConfirmSave(false)
      setEditing(false)
    } catch (err) {
      setSaveError(errorMessage(err))
    }
  }

  const confirmRotate = async () => {
    if (!data) return
    setRotateError(null)
    try {
      const response = await rotateKey.mutateAsync(data.id)
      setRevealed([{ id: response.id, key_name: response.key_name, key: response.key }])
      setRevealedWarning(response.warning)
      setRotatedId(response.id)
      setRotateOpen(false)
      toast.success('密钥已轮换', '旧密钥已立即失效。')
    } catch (err) {
      setRotateError(errorMessage(err))
    }
  }

  const quotaDimension: QuotaDimension[] = useMemo(() => {
    const quota = data?.account_quota
    if (!quota) return []
    return [
      {
        key: 'daily',
        label: '账户日额度',
        used: quota.used_today_micro,
        limit: quota.daily_quota_micro,
        format: (value) => `${formatCreditAmount(value)} 积分`,
        note:
          quota.remaining_micro === null
            ? // 上一版这里写「套餐名；无日额度上限」，而上面的数值已经是「无上限」，
              // 同一句话说了两遍。改成讲清楚「没有上限意味着什么」。
              '本账户没有设置每日额度上限，调用只受账户余额与限流约束。'
            : `今日还剩 ${formatCreditAmount(quota.remaining_micro)} 积分可用。`,
      },
    ]
  }, [data?.account_quota])

  const auditColumns: TableColumn<KeyAuditItem>[] = [
    {
      key: 'created_at',
      header: '时间',
      render: (row) => <span className="text-xs text-ink-secondary">{formatDateTime(row.created_at)}</span>,
    },
    {
      key: 'action',
      header: '操作',
      render: (row) => <Badge variant="neutral">{auditActionLabel(row.action)}</Badge>,
    },
    {
      key: 'operator',
      header: '操作人',
      render: (row) => (
        <span className="text-xs text-ink-secondary">
          {row.operator_id === currentUserId ? '本人' : `账号 ${row.operator_id}`}
        </span>
      ),
    },
    { key: 'before', header: '变更前', render: (row) => <AuditDiff value={row.before} /> },
    { key: 'after', header: '变更后', render: (row) => <AuditDiff value={row.after} /> },
  ]

  if (detail.isLoading) {
    return (
      <div className="space-y-4">
        <PageHeader title="密钥详情" />
        <Skeleton className="h-40 w-full" />
        <Skeleton className="h-56 w-full" />
      </div>
    )
  }

  if (detail.isError || !data) {
    return (
      <div className="space-y-4">
        <PageHeader title="密钥详情" />
        <ErrorState
          title="无法加载密钥"
          message={errorMessage(detail.error)}
          hint={errorHint(detail.error)}
          onRetry={() => void detail.refetch()}
        />
        <Link to={ROUTES.consoleKeys} className="inline-block text-sm text-primary hover:underline">
          返回密钥列表
        </Link>
      </div>
    )
  }

  const realtimeAvailable = Boolean(realtime?.available)
  const groupName = groupLabel(data.group_code)
  const modelList: KeyModelRef[] = data.models
  const editingModels = modelsQuery.data ?? []

  return (
    <div className="space-y-5">
      <PageHeader
        title="密钥详情"
        description="这是一把调用密钥，名称由创建者填写，用于区分同一账户下的多把密钥。"
        actions={
          <>
            <Button
              variant="secondary"
              onClick={() => setRotateOpen(true)}
              title="生成一把新密钥并让当前这把立即失效，用于泄露后止损"
            >
              轮换密钥
            </Button>
          </>
        }
      />

      {/* 状态徽章已并入「密钥信息」卡，这里只留返回入口，避免同一状态出现两次。 */}
      <div>
        <Link to={ROUTES.consoleKeys} className="text-xs text-primary hover:underline">
          ← 返回密钥列表
        </Link>
      </div>

      <section className="grid gap-4 lg:grid-cols-2">
        <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
          <h2 className="text-sm font-semibold text-ink">密钥信息</h2>
          <dl className="mt-3 space-y-2 text-sm">
            {/* 名称放第一行并单独占一栏：它是用户自己起的标识，不是系统编号。
                原先只在页头当大标题，用户看到「ui-permanent」会以为是内部代号。 */}
            <div className="flex items-start justify-between gap-3">
              <dt className="text-ink-secondary">名称</dt>
              <dd className="text-ink">{data.key_name || '（未命名）'}</dd>
            </div>
            <div className="flex items-start justify-between gap-3">
              <dt className="text-ink-secondary">密钥</dt>
              <dd>
                <code className="rounded bg-surface-muted px-2 py-1 font-mono text-xs text-ink">
                  {data.key_masked}
                </code>
              </dd>
            </div>
            <div className="flex items-center justify-between gap-3">
              <dt className="text-ink-secondary">状态</dt>
              <dd>
                <Badge variant={data.status === 'ENABLED' ? 'success' : 'neutral'} dot>
                  {data.status === 'ENABLED' ? '启用' : '已停用'}
                </Badge>
                {data.expired ? (
                  <Badge variant="danger" className="ml-1.5">
                    已过期
                  </Badge>
                ) : null}
              </dd>
            </div>
            <div className="flex items-center justify-between gap-3">
              <dt className="text-ink-secondary">创建时间</dt>
              <dd className="text-ink">{formatDateTime(data.created_at)}</dd>
            </div>
            <div className="flex items-center justify-between gap-3">
              <dt className="text-ink-secondary">最近使用</dt>
              <dd className="text-ink">
                {data.last_used_at ? formatDateTime(data.last_used_at) : '还没调用过'}
              </dd>
            </div>
          </dl>
        </div>

        <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
          <div className="flex items-center justify-between gap-3">
            <h2 className="text-sm font-semibold text-ink">分组与到期</h2>
            <Badge variant="neutral">只读</Badge>
          </div>
          {/* 到期时间原在左卡，而右卡标题却叫「分组与有效期」却没有有效期——
              同一事实拆两处、标题还对不上。现在到期归右卡，标题改名「分组与到期」。 */}
          <dl className="mt-3 space-y-2 text-sm">
            <div className="flex items-center justify-between gap-3">
              <dt className="text-ink-secondary">当前分组</dt>
              <dd className="text-ink">{groupName ?? '—'}</dd>
            </div>
            <div className="flex items-center justify-between gap-3">
              <dt className="text-ink-secondary">分组倍率</dt>
              <dd className="text-ink">
                {data.group_ratio_bp === null ? '—' : `×${formatRatioBp(data.group_ratio_bp)}`}
              </dd>
            </div>
            <div className="flex items-center justify-between gap-3">
              <dt className="text-ink-secondary">到期时间</dt>
              <dd className="text-ink">
                {data.expire_at ? formatDateTime(data.expire_at) : '永不过期'}
                {data.remaining_days !== null ? (
                  <span className="ml-2 text-xs text-ink-muted">剩余 {data.remaining_days} 天</span>
                ) : null}
              </dd>
            </div>
          </dl>
          <p className="mt-3 text-xs text-ink-muted">
            {data.group_note ?? '分组由运营维护，用户侧不提供切换入口；如需调整请提交工单。'}
          </p>
        </div>
      </section>


      <section className="rounded-xl border border-border bg-surface p-5 shadow-card">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="space-y-1">
            <h2 className="text-sm font-semibold text-ink">调用趋势</h2>
            <p className="text-xs text-ink-secondary">
              {usageHasData
                ? `${usageRangeLabel}共 ${usageTotals.requests} 次调用，消耗 ${usageTotals.credits.toFixed(2)} 积分。`
                : `按小时统计这把密钥的调用次数。${usageRangeLabel}还没有调用记录。`}
            </p>
          </div>
          <div className="flex gap-1">
            {(['24h', '7d'] as const).map((option) => (
              <button
                key={option}
                type="button"
                aria-pressed={usageRange === option}
                onClick={() => setUsageRange(option)}
                className={
                  usageRange === option
                    ? 'rounded-md border border-primary bg-primary-soft px-2.5 py-1 text-xs font-medium text-primary'
                    : 'rounded-md border border-border bg-surface px-2.5 py-1 text-xs text-ink-secondary hover:bg-surface-muted'
                }
              >
                {option === '24h' ? '近 24 小时' : '近 7 天'}
              </button>
            ))}
          </div>
        </div>
        <LineChart
          className="mt-4"
          // 没有调用时传空数组，让组件走空状态；后端补的 24/168 个 0 点画出来
          // 是一条压在 0 上的直线，看着像图坏了。
          data={usageHasData ? usageData : []}
          xKey="label"
          // 只画请求数。消耗积分和请求数量纲差几个数量级，同轴画会被压成贴地线，
          // 读图时容易误判「消耗为 0」；积分改用上方那句汇总文字表达。
          series={[{ key: 'requests', name: '调用次数' }]}
          // 空态不需要 220px 的画布，留一块空白反而显得页面没加载完。
          height={usageHasData ? 220 : 132}
          loading={usage.isLoading}
          error={usageError}
          onRetry={() => void usage.refetch()}
          ariaLabel="密钥调用趋势图"
          emptyTitle="这段时间没有调用"
          emptyDescription="这把密钥还没有产生调用；发起一次请求后，这里会按小时显示曲线。"
        />
      </section>
      <section className="rounded-xl border border-border bg-surface p-5 shadow-card">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="space-y-1">
            <h2 className="text-sm font-semibold text-ink">当前这一分钟</h2>
            <p className="text-xs text-ink-secondary">
              数值为「已用 / 上限」，用来判断这把密钥是不是快被限流了。
            </p>
          </div>
          {live.loading ? <span className="text-xs text-ink-muted">刷新中…</span> : null}
        </div>
        {live.error ? (
          <p role="status" className="mt-2 text-xs text-warning">
            实时指标获取失败：{live.error}
          </p>
        ) : null}
        {!realtimeAvailable && !live.loading ? (
          <p role="status" className="mt-2 text-xs text-ink-muted">
            实时指标暂不可用，请稍后刷新。
          </p>
        ) : null}
        <dl className="mt-4 grid gap-3 sm:grid-cols-3">
          <div className="rounded-lg border border-border px-4 py-3">
            <dt className="text-xs text-ink-secondary">每分钟请求上限</dt>
            <dd className="mt-1 text-lg font-medium">
              <RealtimeCell
                used={realtime?.rpm_used ?? null}
                limit={realtime?.rpm_limit ?? 0}
                available={realtimeAvailable}
              />
            </dd>
          </div>
          <div className="rounded-lg border border-border px-4 py-3">
            <dt className="text-xs text-ink-secondary">每分钟用量上限</dt>
            <dd className="mt-1 text-lg font-medium">
              <RealtimeCell
                used={realtime?.tpm_used ?? null}
                limit={realtime?.tpm_limit ?? 0}
                available={realtimeAvailable}
              />
            </dd>
          </div>
          <div className="rounded-lg border border-border px-4 py-3">
            <dt className="text-xs text-ink-secondary">同时处理请求数</dt>
            <dd className="mt-1 text-lg font-medium">
              <RealtimeCell
                used={realtime?.concurrency_used ?? null}
                limit={realtime?.concurrency_limit ?? 0}
                available={realtimeAvailable}
              />
            </dd>
          </div>
        </dl>
      </section>

      <QuotaBar
        title="账户日额度"
        description="额度按账户维度计算，本密钥与同账户其他密钥共享。"
        dimensions={quotaDimension}
      />

      <section className="rounded-xl border border-border bg-surface p-5 shadow-card">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="space-y-1">
            <h2 className="text-sm font-semibold text-ink">模型权限</h2>
            {/* 这里原本又写了一遍「全部已上架模型」，和下面的徽章一字不差地重复。
                说明位改成讲「这栏是干什么的」，具体范围交给下面的徽章。 */}
            <p className="text-xs text-ink-secondary">决定这把密钥能调用哪些模型。</p>
          </div>
          {!editing ? (
            <Button variant="secondary" size="sm" onClick={startEditing}>
              编辑模型权限
            </Button>
          ) : null}
        </div>

        {!editing ? (
          <div className="mt-3 flex flex-wrap gap-1.5">
            {data.model_scope_mode === 'ALL' ? (
              <Badge variant="info">全部已上架模型</Badge>
            ) : modelList.length === 0 ? (
              <span className="text-xs text-ink-muted">未授权任何模型</span>
            ) : (
              modelList.map((model) => (
                <Badge key={model.id} variant="neutral">
                  {model.display_name}
                </Badge>
              ))
            )}
          </div>
        ) : (
          <div className="mt-3 space-y-3">
            <div className="flex flex-wrap gap-2">
              {(['ALL', 'SELECTED'] as ModelScopeMode[]).map((mode) => (
                <label
                  key={mode}
                  className="inline-flex items-center gap-2 rounded-lg border border-border px-3 py-2 text-xs"
                >
                  <input
                    type="radio"
                    name="model-scope-mode"
                    value={mode}
                    checked={draftMode === mode}
                    onChange={() => {
                      setDraftMode(mode)
                      if (mode === 'ALL') setDraftIds([])
                    }}
                  />
                  {mode === 'ALL' ? '全部已上架模型' : '指定模型'}
                </label>
              ))}
            </div>

            {draftMode === 'SELECTED' ? (
              <div className="max-h-64 space-y-1 overflow-y-auto rounded-lg border border-border p-2">
                {modelsQuery.isLoading ? (
                  <Skeleton className="h-8 w-full" />
                ) : modelsQuery.isError ? (
                  <p role="alert" className="px-2 py-1 text-xs text-danger">
                    {errorMessage(modelsQuery.error)}
                  </p>
                ) : editingModels.length === 0 ? (
                  <p className="px-2 py-1 text-xs text-ink-muted">暂无可用模型。</p>
                ) : (
                  editingModels.map((model) => (
                    <label
                      key={model.id}
                      className="flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 hover:bg-surface-muted"
                    >
                      <input
                        type="checkbox"
                        className="h-4 w-4 accent-[var(--primary)]"
                        checked={draftIds.includes(model.id)}
                        onChange={() =>
                          setDraftIds((current) =>
                            current.includes(model.id)
                              ? current.filter((item) => item !== model.id)
                              : [...current, model.id],
                          )
                        }
                      />
                      <span className="text-sm text-ink">{model.display_name}</span>
                      <span className="text-xs text-ink-muted">{model.model_name}</span>
                    </label>
                  ))
                )}
              </div>
            ) : null}

            {draftError ? (
              <p role="alert" className="text-xs text-danger">
                {draftError}
              </p>
            ) : null}

            <div className="flex justify-end gap-2">
              <Button
                variant="secondary"
                size="sm"
                onClick={() => {
                  setEditing(false)
                  setDraftError(null)
                }}
              >
                取消
              </Button>
              <Button size="sm" onClick={requestSave}>
                保存
              </Button>
            </div>
          </div>
        )}
      </section>


      <section className="space-y-2">
        <div className="flex items-center justify-between gap-2">
          <h2 className="text-sm font-semibold text-ink">操作日志</h2>
          <p className="text-xs text-ink-muted">含每次变更的前后值</p>
        </div>
        <Table
          ariaLabel="密钥操作日志"
          columns={auditColumns}
          rows={audit.data?.items ?? []}
          rowKey={(row) => `${row.action}-${row.created_at}-${row.operator_id}`}
          loading={audit.isLoading}
          error={audit.isError ? errorMessage(audit.error) : null}
          onRetry={() => void audit.refetch()}
          emptyTitle="暂无操作记录"
          emptyDescription="创建、修改权限、轮换等操作会记录在此。"
          pagination={
            audit.data
              ? {
                  page: audit.data.page,
                  pageSize: audit.data.page_size,
                  total: audit.data.total,
                  onPageChange: setAuditPageNumber,
                }
              : undefined
          }
        />
      </section>

      <ConfirmDialog
        open={confirmSave}
        title="确认修改模型权限"
        description={
          draftMode === 'ALL'
            ? '保存后该密钥将可调用全部已上架模型。'
            : `保存后该密钥仅可调用所选的 ${draftIds.length} 个模型。`
        }
        confirmLabel="确认保存"
        variant="primary"
        loading={updateKey.isPending}
        error={saveError}
        onConfirm={() => void confirmModelSave()}
        onCancel={() => {
          if (updateKey.isPending) return
          setConfirmSave(false)
          setSaveError(null)
        }}
      />

      <ConfirmDialog
        open={rotateOpen}
        title="轮换密钥"
        description="轮换后会立即生成新密钥，旧密钥立即失效且无法恢复。新密钥明文只展示一次。"
        confirmLabel="确认轮换"
        loading={rotateKey.isPending}
        error={rotateError}
        onConfirm={() => void confirmRotate()}
        onCancel={() => {
          if (rotateKey.isPending) return
          setRotateOpen(false)
          setRotateError(null)
        }}
      />

      <KeySecretOnce
        open={revealed !== null}
        keys={revealed ?? []}
        warning={revealedWarning}
        title="密钥已轮换"
        onClose={() => {
          setRevealed(null)
          setRevealedWarning(undefined)
          if (rotatedId !== null) {
            const next = rotatedId
            setRotatedId(null)
            navigate(consoleKeyDetailPath(String(next)), { replace: true })
          }
        }}
      />
    </div>
  )
}

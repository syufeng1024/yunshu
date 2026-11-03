import { Link } from 'react-router-dom'
import { useConsoleOverview } from '@/api/console'
import { Badge, Button, ErrorState, PageHeader, Skeleton, StatCard } from '@/components'
import { ROUTES } from '@/lib/constants'
import { formatCreditAmount, formatInteger } from '@/lib/format'
import { usePageTitle } from '@/lib/usePageTitle'

/** 套餐权益来源。后端 policy_source 是枚举，未知值原样显示而不是吞成「其他」。 */
const SOURCE_LABEL: Record<string, string> = {
  PLAN: '付费套餐',
  TRIAL: '体验版',
  DEFAULT: '免费默认',
  QUOTA_APPLY: '审批提额',
}

const DATE_FORMAT = new Intl.DateTimeFormat('zh-CN', {
  dateStyle: 'medium',
  timeZone: 'Asia/Shanghai',
})

/**
 * 我的订阅：当前套餐与限额。
 *
 * 数据来自 /api/console/overview 的 plan + quota 两段——概览页把它们混在用量图里，
 * 用户想知道「我买的是哪档、还有多少额度」要翻半天。这里只讲权益。
 */
export default function SubscriptionPage() {
  usePageTitle('控制台 · 我的订阅')
  const query = useConsoleOverview()

  if (query.isLoading) {
    return (
      <div className="space-y-6">
        <PageHeader title="我的订阅" description="当前套餐与调用限额。" />
        <Skeleton className="h-32 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    )
  }

  if (query.isError) {
    return (
      <div className="space-y-6">
        <PageHeader title="我的订阅" description="当前套餐与调用限额。" />
        <ErrorState
          message="订阅信息加载失败"
          hint="稍后重试；如果持续失败请联系客服。"
          onRetry={() => {
            void query.refetch()
          }}
        />
      </div>
    )
  }

  const plan = query.data?.plan
  const quota = query.data?.quota
  const source = plan ? (SOURCE_LABEL[plan.policy_source] ?? plan.policy_source) : '—'

  return (
    <div className="space-y-6">
      <PageHeader
        title="我的订阅"
        description="当前套餐与调用限额。"
        actions={
          <Link to={ROUTES.pricing}>
            <Button variant="secondary">查看全部套餐</Button>
          </Link>
        }
      />

      <section className="rounded-xl border border-border bg-surface p-5">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h2 className="text-sm font-semibold text-ink">当前套餐</h2>
            <p className="mt-1 text-2xl font-semibold tracking-tight text-ink">
              {plan?.name ?? '—'}
            </p>
            <p className="mt-1 text-xs text-ink-muted">
              套餐代码 <code className="font-mono">{plan?.code ?? '—'}</code>
            </p>
          </div>
          <div className="flex flex-col items-end gap-2">
            <Badge variant="info">{source}</Badge>
            <span className="text-xs text-ink-muted">
              {plan?.trial_expire_at
                ? `有效期至 ${DATE_FORMAT.format(new Date(plan.trial_expire_at))}`
                : '长期有效'}
            </span>
          </div>
        </div>
      </section>

      <section>
        <h2 className="text-sm font-semibold text-ink">调用限额</h2>
        <div className="mt-3 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <StatCard label="每分钟请求数" value={formatInteger(quota?.rpm ?? 0)} />
          <StatCard label="每分钟 Token" value={formatInteger(quota?.tpm ?? 0)} />
          <StatCard label="并发数" value={formatInteger(quota?.concurrency ?? 0)} />
          <StatCard
            label="今日额度"
            value={
              quota?.daily_quota_micro === null || quota?.daily_quota_micro === undefined
                ? '不限'
                : formatCreditAmount(quota.daily_quota_micro)
            }
          />
        </div>
      </section>

      <section className="rounded-xl border border-border bg-surface p-5">
        <h2 className="text-sm font-semibold text-ink">今日用量</h2>
        <dl className="mt-4 grid gap-4 text-sm sm:grid-cols-3">
          <div>
            <dt className="text-xs text-ink-muted">已用</dt>
            <dd className="mt-1 font-medium tabular-nums text-ink">
              {formatCreditAmount(quota?.used_today_micro ?? 0)}
            </dd>
          </div>
          <div>
            <dt className="text-xs text-ink-muted">剩余</dt>
            <dd className="mt-1 font-medium tabular-nums text-ink">
              {quota?.remaining_micro === null || quota?.remaining_micro === undefined
                ? '不限'
                : formatCreditAmount(quota.remaining_micro)}
            </dd>
          </div>
          <div>
            <dt className="text-xs text-ink-muted">超出后</dt>
            <dd className="mt-1 text-ink-secondary">
              当日额度用尽后按量计费，余额不足时返回 402。
            </dd>
          </div>
        </dl>
      </section>

      <p className="text-xs text-ink-muted">
        需要提高限额？可在「工单中心」提交提额申请，审核通过后当日生效。
      </p>
    </div>
  )
}

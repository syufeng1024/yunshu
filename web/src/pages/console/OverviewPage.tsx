import { useMemo, useState } from 'react'
import { errorHint, errorMessage } from '@/api/client'
import { useConsoleOverview } from '@/api/console'
import { Button, ErrorState, LineChart, PageHeader, StatCard } from '@/components'
import {
  AnnouncementSlot,
  EtaHint,
  GettingStarted,
  QuotaBar,
  RaiseQuotaDialog,
} from '@/components/console'
import type { QuotaDimension } from '@/components/console'
import {
  creditsFromMicro,
  formatBasisPoints,
  formatCreditAmount,
  formatInteger,
} from '@/lib/format'
import { usePageTitle } from '@/lib/usePageTitle'

type Range = '4h' | '24h'

const HOUR_FORMAT = new Intl.DateTimeFormat('zh-CN', {
  hour: '2-digit',
  minute: '2-digit',
  timeZone: 'Asia/Shanghai',
  hour12: false,
})

function hourLabel(iso: string): string {
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? '—' : HOUR_FORMAT.format(date)
}

export default function ConsoleOverviewPage() {
  usePageTitle('控制台 · 概览')
  const [range, setRange] = useState<Range>('24h')
  const [raiseOpen, setRaiseOpen] = useState(false)
  const query = useConsoleOverview()

  const chartData = useMemo(() => {
    const hourly = query.data?.hourly ?? []
    const points = hourly.map((point) => ({
      label: hourLabel(point.hour),
      requests: point.requests,
      credits: creditsFromMicro(point.consumed_micro),
    }))
    return range === '4h' ? points.slice(-4) : points
  }, [query.data?.hourly, range])

  const dimensions: QuotaDimension[] = useMemo(() => {
    const quota = query.data?.quota
    if (!quota) return []
    const remaining = quota.remaining_micro
    return [
      {
        key: 'daily',
        label: '日额度',
        used: quota.used_today_micro,
        limit: quota.daily_quota_micro,
        format: (value) => `${formatCreditAmount(value)} 积分`,
        note:
          remaining === null
            ? '账户维度；当前套餐无日额度上限'
            : `账户维度；剩余 ${formatCreditAmount(remaining)} 积分`,
      },
      {
        key: 'rpm',
        label: 'RPM',
        used: null,
        limit: quota.rpm,
        format: formatInteger,
        note: '每分钟请求上限；实时使用量见密钥详情',
      },
      {
        key: 'tpm',
        label: 'TPM',
        used: null,
        limit: quota.tpm,
        format: formatInteger,
        note: '每分钟 token 上限；实时使用量见密钥详情',
      },
      {
        key: 'concurrency',
        label: '并发',
        used: null,
        limit: quota.concurrency,
        format: formatInteger,
        note: '并发请求上限；实时使用量见密钥详情',
      },
    ]
  }, [query.data?.quota])

  const error = query.isError ? errorMessage(query.error) : null
  const hint = query.isError ? errorHint(query.error) : null

  return (
    <div className="space-y-6">
      <PageHeader
        title="概览"
        description="账户余额、今日用量与额度进度的单一入口。"
        actions={
          <Button
            variant="secondary"
            onClick={() => setRaiseOpen(true)}
            disabled={query.isLoading || !query.data || query.data.quota.daily_quota_micro === null}
            title={
              query.data?.quota.daily_quota_micro === null
                ? '当前套餐没有日额度上限，无需申请提额'
                : undefined
            }
          >
            申请提额
          </Button>
        }
      />

      <GettingStarted />

      {error ? (
        <ErrorState
          message={error}
          hint={hint}
          onRetry={() => void query.refetch()}
          className="bg-surface"
        />
      ) : (
        <>
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            <StatCard
              label="可用余额"
              loading={query.isLoading}
              value={query.data ? `¥${query.data.balance.cny}` : undefined}
              hint={query.data ? `${query.data.balance.credits} 积分` : undefined}
            />
            <StatCard
              label="今日消耗"
              loading={query.isLoading}
              value={query.data ? `${query.data.today.consumed_credits} 积分` : undefined}
              hint="按今日 00:00（UTC+8）起统计"
            />
            <StatCard
              label="今日请求数"
              loading={query.isLoading}
              value={query.data ? formatInteger(query.data.today.requests) : undefined}
            />
            <StatCard
              label="今日成功率"
              loading={query.isLoading}
              value={
                query.data
                  ? query.data.today.success_rate_bp === null
                    ? '—'
                    : formatBasisPoints(query.data.today.success_rate_bp)
                  : undefined
              }
              hint={
                query.data?.today.success_rate_bp === null
                  ? '今日暂无请求，无法计算成功率'
                  : '429 / 402 业务拒绝不计入分子与分母'
              }
            />
          </div>

          <AnnouncementSlot />

          <section className="rounded-xl border border-border bg-surface p-5 shadow-card">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <div className="space-y-1">
                <h2 className="text-sm font-semibold text-ink">
                  {range === '4h' ? '近 4 小时趋势' : '近 24 小时趋势'}
                </h2>
                <p className="text-xs text-ink-secondary">请求数与消耗积分，数据来自真实调用记录。</p>
              </div>
              <div className="flex items-center gap-1" role="group" aria-label="趋势时间范围">
                {(['4h', '24h'] as Range[]).map((option) => (
                  <Button
                    key={option}
                    size="sm"
                    variant={range === option ? 'primary' : 'secondary'}
                    aria-pressed={range === option}
                    onClick={() => setRange(option)}
                  >
                    {option === '4h' ? '近 4 小时' : '近 24 小时'}
                  </Button>
                ))}
              </div>
            </div>
            <LineChart
              className="mt-4"
              data={chartData}
              xKey="label"
              series={[
                { key: 'requests', name: '请求数' },
                { key: 'credits', name: '消耗积分' },
              ]}
              height={280}
              loading={query.isLoading}
              error={error}
              onRetry={() => void query.refetch()}
              ariaLabel="请求数与消耗积分趋势图"
            />
          </section>

          <div className="grid gap-4 lg:grid-cols-3">
            <QuotaBar className="lg:col-span-2" dimensions={dimensions} description="账户维度" />
            <div className="space-y-3">
              <section className="rounded-xl border border-border bg-surface p-5 shadow-card">
                <h2 className="text-sm font-semibold text-ink">日额度触顶预测</h2>
                <p className="mt-1 text-xs text-ink-secondary">
                  按近 1 小时的实际消耗速度外推，速度归零时不可预测。
                </p>
                <EtaHint eta={query.data?.eta} className="mt-3" />
              </section>
            </div>
          </div>
        </>
      )}

      <RaiseQuotaDialog
        open={raiseOpen}
        currentQuotaMicro={query.data?.quota.daily_quota_micro ?? null}
        onClose={() => setRaiseOpen(false)}
      />
    </div>
  )
}

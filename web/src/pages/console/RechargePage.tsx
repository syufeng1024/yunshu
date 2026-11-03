import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { errorHint, errorMessage } from '@/api/client'
import {
  useInvoices,
  useOrders,
  usePlans,
  useRedeemCode,
  useRefunds,
  type Invoice,
  type InvoiceStatusFilter,
  type Order,
  type OrderState,
  type OrderStatusFilter,
  type Plan,
  type RedeemResult,
  type Refund,
  type RefundStatusFilter,
} from '@/api/console'
import { Badge, Button, ErrorState, Input, PageHeader, Select, Table } from '@/components'
import type { BadgeVariant, TableColumn, TableSort } from '@/components'
import { InvoiceDialog, PayModal, RefundDialog } from '@/components/console'
import type { PayTarget } from '@/components/console'
import { invoiceStateLabel, orderStateLabel, refundStateLabel } from '@/lib/consoleLabels'
import {
  fenFromYuanInput,
  formatCreditAmount,
  formatDateTime,
  formatInteger,
  formatPlanPrice,
  formatQuotaCredits,
  formatYuanFromFen,
} from '@/lib/format'
import { useDebouncedValue } from '@/lib/hooks/useDebouncedValue'
import { usePageTitle } from '@/lib/usePageTitle'

type StatusFilter = 'ALL' | OrderStatusFilter
type RefundFilter = 'ALL' | RefundStatusFilter
type InvoiceFilter = 'ALL' | InvoiceStatusFilter

const PAGE_SIZE = 20

const ORDER_STATUS_OPTIONS: { value: StatusFilter; label: string }[] = [
  { value: 'ALL', label: '全部状态' },
  { value: 'pending', label: '待支付' },
  { value: 'paid', label: '已支付' },
  { value: 'closed', label: '已关闭' },
  { value: 'refunding', label: '退款中' },
  { value: 'refunded', label: '已退款 / 部分退款' },
]

const REFUND_STATUS_OPTIONS: { value: RefundFilter; label: string }[] = [
  { value: 'ALL', label: '全部状态' },
  { value: 'applied', label: '已申请' },
  { value: 'approved', label: '已通过' },
  { value: 'paying', label: '退款中' },
  { value: 'paid', label: '已退款' },
  { value: 'rejected', label: '已驳回' },
  { value: 'failed', label: '退款失败' },
]

const INVOICE_PAGE_SIZE_OPTIONS = [10, 20, 50] as const

const INVOICE_STATUS_OPTIONS: { value: InvoiceFilter; label: string }[] = [
  { value: 'ALL', label: '全部状态' },
  { value: 'applied', label: '已申请' },
  { value: 'approved', label: '已通过' },
  { value: 'issued', label: '已开具' },
  { value: 'rejected', label: '已驳回' },
  { value: 'voided', label: '已作废' },
]

const ORDER_BADGE: Record<OrderState, BadgeVariant> = {
  PENDING: 'warning',
  PAID: 'success',
  CLOSED: 'neutral',
  REFUNDING: 'warning',
  REFUNDED: 'info',
  PARTIAL_REFUNDED: 'info',
}

function PlanCard({
  plan,
  selected,
  onSelect,
}: {
  plan: Plan
  selected: boolean
  onSelect: () => void
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      disabled={!plan.purchasable}
      aria-pressed={selected}
      aria-label={`选择套餐 ${plan.name}`}
      className={[
        'flex h-full flex-col gap-3 rounded-xl border p-4 text-left transition-colors duration-fast focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary',
        selected ? 'border-primary bg-primary-soft' : 'border-border bg-surface',
        plan.purchasable ? 'hover:border-primary-ring' : 'cursor-not-allowed opacity-70',
      ].join(' ')}
    >
      <div className="flex items-start justify-between gap-2">
        <div>
          <p className="text-sm font-semibold text-ink">{plan.name}</p>
          <p className="text-xs text-ink-muted">{plan.code}</p>
        </div>
        {plan.purchasable ? (
          selected ? (
            <Badge variant="primary">已选择</Badge>
          ) : null
        ) : (
          <Badge variant="neutral">不可购买</Badge>
        )}
      </div>

      <p className="text-2xl font-semibold tracking-tight text-ink">
        {formatPlanPrice(plan.price_fen)}
      </p>

      <dl className="space-y-1 text-xs text-ink-secondary">
        <div className="flex justify-between gap-2">
          <dt>充值积分</dt>
          <dd className="tabular-nums text-ink">{formatInteger(plan.grant_recharge)} 积分</dd>
        </div>
        <div className="flex justify-between gap-2">
          <dt>赠送积分</dt>
          <dd className="tabular-nums text-ink">
            {formatInteger(plan.grant_bonus)} 积分
            {plan.grant_bonus > 0 ? `（${plan.bonus_valid_days} 天有效）` : ''}
          </dd>
        </div>
        <div className="flex justify-between gap-2">
          <dt>RPM / TPM</dt>
          <dd className="tabular-nums text-ink">
            {formatInteger(plan.rpm)} / {formatInteger(plan.tpm)}
          </dd>
        </div>
        <div className="flex justify-between gap-2">
          <dt>并发</dt>
          <dd className="tabular-nums text-ink">{formatInteger(plan.concurrency)}</dd>
        </div>
        <div className="flex justify-between gap-2">
          <dt>日额度</dt>
          <dd className="text-ink">{formatQuotaCredits(plan.daily_quota_micro)}</dd>
        </div>
      </dl>
    </button>
  )
}

export default function RechargePage() {
  usePageTitle('充值中心')

  const plans = usePlans()
  const [selectedPlanId, setSelectedPlanId] = useState<number | null>(null)
  const [payTarget, setPayTarget] = useState<PayTarget | null>(null)
  // Deep links from /pricing carry the target tier in ?plan=<t_plan.code>.
  const [searchParams] = useSearchParams()
  const requestedPlanParam = searchParams.get('plan') ?? ''
  const [planNotice, setPlanNotice] = useState<string | null>(null)

  useEffect(() => {
    if (selectedPlanId !== null) return
    const items = plans.data?.items
    if (!items) return

    const requestedCode = requestedPlanParam.trim().toLowerCase()
    if (!requestedCode) {
      const first = items.find((plan) => plan.purchasable)
      if (first) setSelectedPlanId(first.id)
      return
    }

    // Match against the codes the API returned; no tier name is hardcoded here.
    const requested = items.find((plan) => plan.code.toLowerCase() === requestedCode)
    if (requested?.purchasable) {
      setSelectedPlanId(requested.id)
      return
    }

    const fallback = items.find((plan) => plan.purchasable)
    if (fallback) setSelectedPlanId(fallback.id)
    setPlanNotice(
      requested
        ? `目标档位 ${requested.code} 不可直接购买，已为你选择第一个可购买档位。`
        : `未找到目标档位「${requestedPlanParam.trim()}」，已为你选择第一个可购买档位。`,
    )
  }, [plans.data, requestedPlanParam, selectedPlanId])

  const selectedPlan = useMemo(
    () => plans.data?.items.find((plan) => plan.id === selectedPlanId && plan.purchasable) ?? null,
    [plans.data, selectedPlanId],
  )

  const [customAmount, setCustomAmount] = useState('')
  const [customError, setCustomError] = useState<string | null>(null)

  const [redeemCode, setRedeemCode] = useState('')
  const [redeemResult, setRedeemResult] = useState<RedeemResult | null>(null)
  const [redeemFailure, setRedeemFailure] = useState<{ message: string; hint: string | null } | null>(
    null,
  )
  const redeem = useRedeemCode()

  const [status, setStatus] = useState<StatusFilter>('ALL')
  const [search, setSearch] = useState('')
  const [page, setPage] = useState(1)
  const debouncedSearch = useDebouncedValue(search, 300)

  useEffect(() => {
    setPage(1)
  }, [debouncedSearch, status])

  // Status and order-number search go to the API together; the backend ANDs
  // them, which is the intersection F10-4 asks for.
  const orders = useOrders({
    status: status === 'ALL' ? undefined : status,
    q: debouncedSearch,
    page,
    pageSize: PAGE_SIZE,
  })

  const [refundStatus, setRefundStatus] = useState<RefundFilter>('ALL')
  const [refundPage, setRefundPage] = useState(1)
  const refunds = useRefunds({
    status: refundStatus === 'ALL' ? undefined : refundStatus,
    page: refundPage,
    pageSize: 10,
  })

  const [invoiceStatus, setInvoiceStatus] = useState<InvoiceFilter>('ALL')
  const [invoicePage, setInvoicePage] = useState(1)
  const [invoicePageSize, setInvoicePageSize] = useState(10)
  // 默认按申请时间倒序，与后端默认一致；开票时间另开一列，否则「按开票时间排」
  // 在界面上看不出依据。
  const [invoiceSort, setInvoiceSort] = useState<TableSort>({ key: 'created', direction: 'desc' })
  const invoices = useInvoices({
    status: invoiceStatus === 'ALL' ? undefined : invoiceStatus,
    page: invoicePage,
    pageSize: invoicePageSize,
    sort: invoiceSort.key === 'issued' ? 'issued_at' : 'created_at',
    order: invoiceSort.direction,
  })

  const [refundOrder, setRefundOrder] = useState<Order | null>(null)
  const [invoiceOrder, setInvoiceOrder] = useState<Order | null>(null)

  const orderError = orders.isError ? errorMessage(orders.error) : null
  const refundError = refunds.isError ? errorMessage(refunds.error) : null
  const invoiceError = invoices.isError ? errorMessage(invoices.error) : null

  const submitCustom = () => {
    setCustomError(null)
    const fen = fenFromYuanInput(customAmount)
    if (fen === null) {
      setCustomError('请输入有效的充值金额（最多两位小数）')
      return
    }
    setPayTarget({ orderType: 'CUSTOM', payAmountFen: fen })
  }

  const submitRedeem = async () => {
    const code = redeemCode.trim()
    setRedeemFailure(null)
    setRedeemResult(null)
    if (!code) {
      setRedeemFailure({ message: '请输入兑换码', hint: null })
      return
    }
    try {
      setRedeemResult(await redeem.mutateAsync(code))
      setRedeemCode('')
    } catch (error) {
      setRedeemFailure({ message: errorMessage(error), hint: errorHint(error) })
    }
  }

  const orderColumns: TableColumn<Order>[] = [
    {
      key: 'order_no',
      header: '订单号',
      render: (order) => (
        <code className="break-all text-xs text-ink-secondary">{order.order_no}</code>
      ),
    },
    {
      key: 'type',
      header: '类型',
      render: (order) => (
        <span className="text-xs text-ink-secondary">
          {order.order_type === 'PLAN' ? order.plan_code ?? '套餐' : '自定义充值'}
        </span>
      ),
    },
    {
      key: 'amount',
      header: '金额',
      align: 'right',
      render: (order) => (
        <span className="tabular-nums text-ink">{formatYuanFromFen(order.pay_amount_fen)} 元</span>
      ),
    },
    {
      key: 'grants',
      header: '充值 / 赠送',
      align: 'right',
      render: (order) => (
        <span className="text-xs tabular-nums text-ink-secondary">
          {formatInteger(order.recharge)} / {formatInteger(order.bonus)}
        </span>
      ),
    },
    {
      key: 'state',
      header: '状态',
      render: (order) => (
        <Badge variant={ORDER_BADGE[order.state]} dot>
          {orderStateLabel(order.state)}
        </Badge>
      ),
    },
    {
      key: 'created',
      header: '创建时间',
      render: (order) => (
        <span className="text-xs text-ink-secondary">{formatDateTime(order.created_at)}</span>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (order) =>
        order.state === 'PAID' ? (
          <div className="flex flex-wrap items-center justify-end gap-1">
            <Button variant="ghost" size="sm" onClick={() => setRefundOrder(order)}>
              退款
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setInvoiceOrder(order)}>
              开票
            </Button>
          </div>
        ) : (
          <span className="text-xs text-ink-muted">—</span>
        ),
    },
  ]

  const refundColumns: TableColumn<Refund>[] = [
    {
      key: 'no',
      header: '退款单号',
      render: (row) => <code className="break-all text-xs text-ink-secondary">{row.refund_no}</code>,
    },
    {
      key: 'order',
      header: '关联订单',
      render: (row) => <span className="text-xs text-ink-secondary">#{row.recharge_order_id}</span>,
    },
    {
      key: 'credits',
      header: '申请积分',
      align: 'right',
      render: (row) => (
        <span className="tabular-nums text-ink">{formatInteger(row.apply_credits)}</span>
      ),
    },
    {
      key: 'refund',
      header: '退款金额',
      align: 'right',
      render: (row) => (
        <span className="tabular-nums text-ink">{formatYuanFromFen(row.refund_fen)} 元</span>
      ),
    },
    {
      key: 'fee',
      header: '手续费',
      align: 'right',
      render: (row) => (
        <span className="tabular-nums text-ink-secondary">{formatYuanFromFen(row.fee_fen)} 元</span>
      ),
    },
    {
      key: 'state',
      header: '状态',
      render: (row) => (
        <div className="space-y-1">
          <Badge variant={row.state === 'PAID' ? 'success' : row.state === 'FAILED' ? 'danger' : 'warning'}>
            {refundStateLabel(row.state)}
          </Badge>
          {row.reject_reason ? (
            <p className="text-xs text-ink-muted">{row.reject_reason}</p>
          ) : null}
          {row.fail_reason ? <p className="text-xs text-ink-muted">{row.fail_reason}</p> : null}
        </div>
      ),
    },
    {
      key: 'created',
      header: '申请时间',
      render: (row) => (
        <span className="text-xs text-ink-secondary">{formatDateTime(row.created_at)}</span>
      ),
    },
  ]

  const invoiceColumns: TableColumn<Invoice>[] = [
    {
      key: 'no',
      header: '申请单号',
      render: (row) => <code className="break-all text-xs text-ink-secondary">{row.apply_no}</code>,
    },
    {
      key: 'order',
      header: '关联订单',
      render: (row) => <span className="text-xs text-ink-secondary">#{row.recharge_order_id}</span>,
    },
    {
      key: 'title',
      header: '抬头',
      render: (row) => (
        <div className="space-y-0.5">
          <p className="text-xs text-ink">{row.title}</p>
          <p className="text-xs text-ink-muted">
            {row.title_type === 'COMPANY' ? '企业' : '个人'}
            {row.tax_no ? ` · ${row.tax_no}` : ''}
          </p>
        </div>
      ),
    },
    {
      key: 'amount',
      header: '金额',
      align: 'right',
      render: (row) => (
        <span className="tabular-nums text-ink">{formatYuanFromFen(row.amount_fen)} 元</span>
      ),
    },
    {
      key: 'state',
      header: '状态',
      render: (row) => (
        <div className="space-y-1">
          <Badge variant={row.state === 'ISSUED' ? 'success' : row.state === 'REJECTED' ? 'danger' : 'warning'}>
            {invoiceStateLabel(row.state)}
          </Badge>
          {row.reject_reason ? <p className="text-xs text-ink-muted">{row.reject_reason}</p> : null}
        </div>
      ),
    },
    {
      key: 'created',
      header: '申请时间',
      sortable: true,
      render: (row) => (
        <span className="text-xs text-ink-secondary">{formatDateTime(row.created_at)}</span>
      ),
    },
    {
      key: 'issued',
      header: '开票时间',
      sortable: true,
      render: (row) =>
        row.issued_at ? (
          <span className="text-xs text-ink-secondary">{formatDateTime(row.issued_at)}</span>
        ) : (
          <span className="text-xs text-ink-muted">—</span>
        ),
    },
    {
      key: 'file',
      header: '发票',
      render: (row) =>
        row.invoice_url ? (
          <a
            href={row.invoice_url}
            target="_blank"
            rel="noreferrer"
            className="text-xs text-primary hover:underline"
          >
            查看发票
          </a>
        ) : (
          <span className="text-xs text-ink-muted">—</span>
        ),
    },
  ]

  return (
    <div className="space-y-6">
      <PageHeader
        title="充值中心"
        description="套餐、自定义充值、兑换码与订单退款开票的统一入口。"
      />

      <section className="space-y-3">
        <div className="space-y-1">
          <h2 className="text-sm font-semibold text-ink">套餐</h2>
          <p className="text-xs text-ink-secondary">
            档位、价格与额度全部来自服务端；默认档与体验档不参与购买。
          </p>
        </div>

        {plans.isError ? (
          <ErrorState
            message={errorMessage(plans.error)}
            hint={errorHint(plans.error)}
            onRetry={() => void plans.refetch()}
            className="bg-surface"
          />
        ) : plans.isLoading ? (
          <p className="text-sm text-ink-secondary">正在加载套餐…</p>
        ) : (
          <>
            {planNotice ? (
              <p
                role="status"
                data-testid="plan-notice"
                className="rounded-lg border border-warning bg-warning-soft px-4 py-2 text-xs text-warning"
              >
                {planNotice}
              </p>
            ) : null}

            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
              {(plans.data?.items ?? []).map((plan) => (
                <PlanCard
                  key={plan.id}
                  plan={plan}
                  selected={plan.id === selectedPlanId}
                  onSelect={() => setSelectedPlanId(plan.id)}
                />
              ))}
            </div>

            <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-surface p-4 shadow-card">
              <div className="text-sm">
                <p className="text-ink">
                  已选：
                  <span className="font-medium">
                    {selectedPlan ? selectedPlan.name : '尚未选择可购买套餐'}
                  </span>
                </p>
                {selectedPlan ? (
                  <p className="text-xs text-ink-secondary">
                    {formatPlanPrice(selectedPlan.price_fen)} · 支付渠道在下一步选择
                  </p>
                ) : null}
              </div>
              <Button
                onClick={() =>
                  selectedPlan && setPayTarget({ orderType: 'PLAN', plan: selectedPlan })
                }
                disabled={!selectedPlan}
              >
                立即购买
              </Button>
            </div>
          </>
        )}
      </section>

      <div className="grid gap-4 lg:grid-cols-2">
        <section className="space-y-3 rounded-xl border border-border bg-surface p-5 shadow-card">
          <div className="space-y-1">
            <h2 className="text-sm font-semibold text-ink">自定义充值</h2>
            <p className="text-xs text-ink-secondary">
              金额与到账积分由服务端换算并校验上下限。
            </p>
          </div>
          <Input
            label="充值金额（元）"
            inputMode="decimal"
            value={customAmount}
            onChange={(event) => {
              setCustomAmount(event.target.value)
              setCustomError(null)
            }}
            error={customError ?? undefined}
            hint="支持最多两位小数；超出服务端允许范围时会返回具体原因。"
          />
          <Button variant="secondary" onClick={submitCustom}>
            创建充值订单
          </Button>
        </section>

        <section className="space-y-3 rounded-xl border border-border bg-surface p-5 shadow-card">
          <div className="space-y-1">
            <h2 className="text-sm font-semibold text-ink">兑换码</h2>
            <p className="text-xs text-ink-secondary">
              单次有效；重复使用、已过期或不存在时展示服务端返回的原因。
            </p>
          </div>
          <Input
            label="兑换码"
            value={redeemCode}
            onChange={(event) => setRedeemCode(event.target.value)}
            autoComplete="off"
          />
          <Button
            variant="secondary"
            onClick={() => void submitRedeem()}
            loading={redeem.isPending}
            disabled={!redeemCode.trim()}
          >
            兑换
          </Button>

          {redeemResult ? (
            <div
              role="status"
              className="space-y-1 rounded-lg border border-success bg-success-soft px-4 py-3 text-xs text-success"
            >
              <p className="text-sm font-medium">兑换成功：{redeemResult.batch_name}</p>
              {/* 到账积分必须从 amount_micro 换算：接口里的 amount 是"元"
                  （microToFen/100），拿它当积分渲染会把 50 元券显示成
                  "到账 50 积分"，实际入账 5000 积分（100 倍差）。 */}
              <p>到账 {formatCreditAmount(redeemResult.amount_micro)} 积分</p>
              <p className="text-ink-secondary">
                {redeemResult.credit_target === 'BONUS'
                  ? '赠送积分，不可退款'
                  : '充值积分'}{' '}
                · 有效期至 {formatDateTime(redeemResult.expire_at)}
              </p>
              <p className="text-ink-secondary">
                当前余额 {formatCreditAmount(redeemResult.balance_after_micro)} 积分
              </p>
            </div>
          ) : null}

          {redeemFailure ? (
            <div role="alert" className="rounded-lg border border-danger bg-danger-soft px-4 py-3 text-xs text-danger">
              <p>{redeemFailure.message}</p>
              {redeemFailure.hint ? <p className="mt-1 text-ink-muted">{redeemFailure.hint}</p> : null}
            </div>
          ) : null}
        </section>
      </div>

      <section className="space-y-3">
        <div className="space-y-1">
          <h2 className="text-sm font-semibold text-ink">订单记录</h2>
          <p className="text-xs text-ink-secondary">
            状态筛选与订单号搜索共同生效；只有已支付订单可以申请退款或开票。
          </p>
        </div>

        <div className="grid gap-3 sm:grid-cols-[minmax(0,1fr)_12rem]">
          <Input
            label="按订单号搜索"
            type="search"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            placeholder="输入订单号片段"
            maxLength={32}
            aria-label="按订单号搜索"
          />
          <Select
            label="订单状态"
            value={status}
            onChange={(event) => setStatus(event.target.value as StatusFilter)}
            options={ORDER_STATUS_OPTIONS}
          />
        </div>

        <Table
          ariaLabel="订单列表"
          columns={orderColumns}
          rows={orders.data?.items ?? []}
          rowKey={(order) => String(order.id)}
          loading={orders.isLoading}
          error={orderError}
          onRetry={() => void orders.refetch()}
          emptyTitle="暂无订单"
          emptyDescription={
            debouncedSearch || status !== 'ALL'
              ? '当前状态与订单号组合没有匹配结果。'
              : '创建第一笔充值订单后会在这里显示。'
          }
          pagination={
            orders.data
              ? {
                  page: orders.data.page,
                  pageSize: orders.data.page_size,
                  total: orders.data.total,
                  onPageChange: setPage,
                }
              : undefined
          }
        />
      </section>

      <div className="grid gap-4 xl:grid-cols-2">
        <section className="space-y-3">
          <div className="flex flex-wrap items-end justify-between gap-2">
            <div className="space-y-1">
              <h2 className="text-sm font-semibold text-ink">退款记录</h2>
              <p className="text-xs text-ink-secondary">只退未消耗的充值积分。</p>
            </div>
            <div className="w-36">
              <Select
                label="退款状态"
                value={refundStatus}
                onChange={(event) => {
                  setRefundStatus(event.target.value as RefundFilter)
                  setRefundPage(1)
                }}
                options={REFUND_STATUS_OPTIONS}
              />
            </div>
          </div>
          <Table
            ariaLabel="退款记录"
            columns={refundColumns}
            rows={refunds.data?.items ?? []}
            rowKey={(row) => String(row.id)}
            loading={refunds.isLoading}
            error={refundError}
            onRetry={() => void refunds.refetch()}
            emptyTitle="暂无退款记录"
            emptyDescription="在订单列表对已支付订单发起退款。"
            pagination={
              refunds.data
                ? {
                    page: refunds.data.page,
                    pageSize: refunds.data.page_size,
                    total: refunds.data.total,
                    onPageChange: setRefundPage,
                  }
                : undefined
            }
          />
        </section>

        <section className="space-y-3">
          <div className="flex flex-wrap items-end justify-between gap-2">
            <div className="space-y-1">
              <h2 className="text-sm font-semibold text-ink">开票记录</h2>
              <p className="text-xs text-ink-secondary">未实名时提交会给出实名引导。</p>
            </div>
            <div className="w-36">
              <Select
                label="开票状态"
                value={invoiceStatus}
                onChange={(event) => {
                  setInvoiceStatus(event.target.value as InvoiceFilter)
                  setInvoicePage(1)
                }}
                options={INVOICE_STATUS_OPTIONS}
              />
            </div>
          </div>
          <Table
            ariaLabel="开票记录"
            columns={invoiceColumns}
            rows={invoices.data?.items ?? []}
            rowKey={(row) => String(row.id)}
            loading={invoices.isLoading}
            error={invoiceError}
            onRetry={() => void invoices.refetch()}
            emptyTitle="暂无开票申请"
            emptyDescription="在订单列表对已支付订单申请开票。"
            sort={invoiceSort}
            onSortChange={(next) => {
              setInvoiceSort(next)
              setInvoicePage(1)
            }}
            pagination={
              invoices.data
                ? {
                    page: invoices.data.page,
                    pageSize: invoices.data.page_size,
                    total: invoices.data.total,
                    pageSizeOptions: INVOICE_PAGE_SIZE_OPTIONS,
                    onPageChange: setInvoicePage,
                    onPageSizeChange: (size) => {
                      setInvoicePageSize(size)
                      setInvoicePage(1)
                    },
                  }
                : undefined
            }
          />
        </section>
      </div>

      <PayModal
        open={payTarget !== null}
        target={payTarget}
        onClose={() => setPayTarget(null)}
      />
      <RefundDialog
        open={refundOrder !== null}
        order={refundOrder}
        onClose={() => setRefundOrder(null)}
      />
      <InvoiceDialog
        open={invoiceOrder !== null}
        order={invoiceOrder}
        onClose={() => setInvoiceOrder(null)}
      />
    </div>
  )
}

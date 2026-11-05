import { useState } from 'react'
import { errorHint, errorMessage } from '@/api/client'
import { useApproveReview, useRejectReview, useReviews } from '@/api/admin'
import type { ReviewBizType, ReviewItem, ReviewState } from '@/api/admin'
import {
  Badge,
  Button,
  Input,
  Modal,
  PageHeader,
  Select,
  Table,
  Tabs,
  useToast,
} from '@/components'
import type { TableColumn } from '@/components'
import { AuditTrail } from '@/components/admin'
import { formatCreditAmount, formatDateTime, formatYuanFromFen } from '@/lib/format'
import { usePageTitle } from '@/lib/usePageTitle'
import { useAuthStore } from '@/stores/auth'

const PAGE_SIZE = 20
const REJECT_REASON_MIN = 5
const REASON_MAX = 255
const INVOICE_URL_MAX = 512

type StateFilter = ReviewState | 'ALL'
type BizTypeFilter = 'ALL' | ReviewBizType
type DecisionKind = 'approve' | 'reject'

const STATE_LABEL: Record<ReviewState, string> = {
  PENDING: '待审核',
  APPROVED: '已通过',
  REJECTED: '已驳回',
}

const BIZ_TYPE_LABEL: Record<ReviewBizType, string> = {
  QUOTA_APPLY: '提额申请',
  INVOICE_APPLY: '开票申请',
  REFUND_APPLY: '退款申请',
  REDEEM_BATCH: '兑换批次',
  BALANCE_ADJUST: '余额调整',
}

const BALANCE_DIRECTION_LABEL: Record<string, string> = {
  INCREASE: '增加',
  DECREASE: '减少',
}

const BIZ_TYPE_OPTIONS: Array<{ value: BizTypeFilter; label: string }> = [
  { value: 'ALL', label: '全部单据类型' },
  { value: 'QUOTA_APPLY', label: BIZ_TYPE_LABEL.QUOTA_APPLY },
  { value: 'INVOICE_APPLY', label: BIZ_TYPE_LABEL.INVOICE_APPLY },
  { value: 'REFUND_APPLY', label: BIZ_TYPE_LABEL.REFUND_APPLY },
  { value: 'REDEEM_BATCH', label: BIZ_TYPE_LABEL.REDEEM_BATCH },
  { value: 'BALANCE_ADJUST', label: BIZ_TYPE_LABEL.BALANCE_ADJUST },
]

const STATE_TABS: Array<{ value: StateFilter; label: string }> = [
  { value: 'PENDING', label: '待审核' },
  { value: 'APPROVED', label: '已通过' },
  { value: 'REJECTED', label: '已驳回' },
  { value: 'ALL', label: '全部' },
]

/**
 * The API types CurrentUser.id as a string while applicant_id is an int64
 * JSON number. A strict === would silently never match; normalise both sides.
 */
export function isSelfAudit(
  applicantId: number,
  userId: string | null | undefined,
): boolean {
  if (userId === null || userId === undefined || userId === '') return false
  return String(applicantId) === String(userId)
}

function amountText(item: ReviewItem): string {
  if (item.amount_micro !== null && item.amount_micro !== undefined) {
    return `${formatCreditAmount(item.amount_micro)} 积分`
  }
  if (item.amount_fen !== null && item.amount_fen !== undefined) {
    return `${formatYuanFromFen(item.amount_fen)} 元`
  }
  return '—'
}

export default function AdminReviewsPage() {
  usePageTitle('审核中心')
  const toast = useToast()
  const user = useAuthStore((state) => state.user)

  const [stateFilter, setStateFilter] = useState<StateFilter>('PENDING')
  const [bizTypeFilter, setBizTypeFilter] = useState<BizTypeFilter>('ALL')
  const [page, setPage] = useState(1)

  const list = useReviews({
    state: stateFilter,
    bizType: bizTypeFilter === 'ALL' ? undefined : bizTypeFilter,
    page,
    pageSize: PAGE_SIZE,
    // Deliberately no sort/order: review/list.go passes no allowed sort
    // columns to ParsePage, so sort= is a 400 and the order is fixed to
    // created_at ASC, id ASC.
  })

  const approve = useApproveReview()
  const reject = useRejectReview()

  const [decisionTarget, setDecisionTarget] = useState<ReviewItem | null>(null)
  const [decisionKind, setDecisionKind] = useState<DecisionKind>('approve')
  const [reason, setReason] = useState('')
  const [invoiceUrl, setInvoiceUrl] = useState('')
  const [reasonError, setReasonError] = useState<string | null>(null)
  const [invoiceError, setInvoiceError] = useState<string | null>(null)
  const [decisionError, setDecisionError] = useState<string | null>(null)
  const [decisionHint, setDecisionHint] = useState<string | null>(null)

  const [auditOpen, setAuditOpen] = useState(false)
  const [auditTarget, setAuditTarget] = useState<{ type?: string; id?: string }>({})
  const [auditRefreshKey, setAuditRefreshKey] = useState(0)

  const openAudit = (targetType?: string, targetId?: number | string) => {
    setAuditTarget({
      type: targetType,
      id: targetId === undefined ? undefined : String(targetId),
    })
    setAuditRefreshKey((value) => value + 1)
    setAuditOpen(true)
  }

  const openDecision = (item: ReviewItem, kind: DecisionKind) => {
    if (item.state !== 'PENDING') return
    if (isSelfAudit(item.applicant_id, user?.id)) return
    setDecisionTarget(item)
    setDecisionKind(kind)
    setReason('')
    setInvoiceUrl('')
    setReasonError(null)
    setInvoiceError(null)
    setDecisionError(null)
    setDecisionHint(null)
  }

  const closeDecision = () => {
    if (approve.isPending || reject.isPending) return
    setDecisionTarget(null)
  }

  const handleDecision = async () => {
    if (!decisionTarget) return
    const target = decisionTarget
    const reasonText = reason.trim()
    let invalid = false
    setReasonError(null)
    setInvoiceError(null)
    setDecisionError(null)
    setDecisionHint(null)

    if (decisionKind === 'reject') {
      const length = [...reasonText].length
      if (length < REJECT_REASON_MIN) {
        setReasonError(`驳回理由至少 ${REJECT_REASON_MIN} 个字。`)
        invalid = true
      } else if (length > REASON_MAX) {
        setReasonError(`驳回理由最多 ${REASON_MAX} 个字符。`)
        invalid = true
      }
    }

    const invoiceRequired =
      decisionKind === 'approve' && target.biz_type === 'INVOICE_APPLY'
    if (invoiceRequired) {
      if (!invoiceUrl.trim()) {
        setInvoiceError('开票审批必须填写发票文件链接。')
        invalid = true
      } else if ([...invoiceUrl.trim()].length > INVOICE_URL_MAX) {
        setInvoiceError(`发票文件链接最多 ${INVOICE_URL_MAX} 个字符。`)
        invalid = true
      }
    }

    if (invalid) return

    try {
      if (decisionKind === 'approve') {
        await approve.mutateAsync({
          id: target.id,
          input: {
            reason: reasonText || undefined,
            invoice_url: invoiceUrl.trim() || undefined,
          },
        })
        toast.success('审核已通过')
      } else {
        await reject.mutateAsync({
          id: target.id,
          input: { reason: reasonText },
        })
        toast.success('审核已驳回')
      }
      closeDecision()
      if (target.biz_type === 'BALANCE_ADJUST') {
        // The balance decision audit row targets the business document, not
        // the queue row, so opening t_review_task here would show nothing.
        openAudit('t_balance_adjust', target.biz_id)
      } else {
        openAudit('t_review_task', target.id)
      }
    } catch (error) {
      // 403 permission_denied (self-audit) and 409 state conflicts arrive here
      // with the backend message + hint; show both verbatim.
      setDecisionError(errorMessage(error))
      setDecisionHint(errorHint(error))
    }
  }

  const currentUserId = user?.id
  const listError = list.isError ? errorMessage(list.error) : null
  const listHint = list.isError ? errorHint(list.error) : null
  const saving = approve.isPending || reject.isPending

  const columns: TableColumn<ReviewItem>[] = [
    {
      key: 'biz',
      header: '单据',
      render: (item) => (
        <div className="space-y-0.5">
          <p className="font-medium text-ink">{BIZ_TYPE_LABEL[item.biz_type] ?? item.biz_type}</p>
          <p className="text-xs text-ink-secondary">
            {item.biz_no || `#${item.biz_id}`} · 队列#{item.id}
          </p>
        </div>
      ),
    },
    {
      key: 'applicant',
      header: '申请人',
      render: (item) => (
        <div className="space-y-0.5">
          <p className="text-sm text-ink">{item.applicant_name}</p>
          <p className="text-xs text-ink-muted">#{item.applicant_id}</p>
        </div>
      ),
    },
    {
      key: 'summary',
      header: '摘要',
      render: (item) => (
        <div className="max-w-md space-y-1">
          {item.direction ? (
            <p className="text-xs text-ink">
              方向：{BALANCE_DIRECTION_LABEL[item.direction] ?? item.direction}
            </p>
          ) : null}
          <p className="text-xs text-ink-secondary">{item.summary || '—'}</p>
          {item.anomaly ? (
            <div className="space-y-0.5">
              <Badge variant="danger">数据异常</Badge>
              {item.anomaly_reason ? (
                <p className="text-xs text-danger">{item.anomaly_reason}</p>
              ) : null}
            </div>
          ) : null}
        </div>
      ),
    },
    {
      key: 'amount',
      header: '金额',
      align: 'right',
      render: (item) => <span className="tabular-nums text-xs">{amountText(item)}</span>,
    },
    {
      key: 'business_state',
      header: '业务状态',
      render: (item) => (
        <span className="text-xs text-ink-secondary">{item.business_state || '—'}</span>
      ),
    },
    {
      key: 'state',
      header: '审核状态',
      render: (item) => (
        <div className="space-y-1">
          <Badge
            variant={
              item.state === 'PENDING'
                ? 'warning'
                : item.state === 'APPROVED'
                  ? 'success'
                  : 'danger'
            }
          >
            {STATE_LABEL[item.state]}
          </Badge>
          {item.decision_note ? (
            <p className="text-xs text-ink-secondary">理由：{item.decision_note}</p>
          ) : null}
          {item.decided_at ? (
            <p className="text-xs text-ink-muted">{formatDateTime(item.decided_at)}</p>
          ) : null}
        </div>
      ),
    },
    {
      key: 'created_at',
      header: '提交时间',
      render: (item) => (
        <span className="whitespace-nowrap text-xs text-ink-secondary">
          {formatDateTime(item.created_at)}
        </span>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (item) => {
        if (item.state !== 'PENDING') {
          return <span className="text-xs text-ink-muted">已处理</span>
        }
        const selfAudit = isSelfAudit(item.applicant_id, currentUserId)
        return (
          <div className="flex flex-col items-end gap-1">
            <div className="flex flex-wrap items-center justify-end gap-1">
              <Button
                variant="secondary"
                size="sm"
                disabled={selfAudit}
                title={
                  selfAudit ? '审批人不得为申请人：请转交其他审核人处理' : undefined
                }
                onClick={() => openDecision(item, 'approve')}
              >
                通过
              </Button>
              <Button
                variant="ghost"
                size="sm"
                className="text-danger"
                disabled={selfAudit}
                title={
                  selfAudit ? '审批人不得为申请人：请转交其他审核人处理' : undefined
                }
                onClick={() => openDecision(item, 'reject')}
              >
                驳回
              </Button>
            </div>
            {selfAudit ? (
              <span className="text-right text-xs text-danger">
                审批人不得为申请人：请转交其他审核人处理
              </span>
            ) : null}
          </div>
        )
      },
    },
  ]

  const tableSection = (
    <div className="space-y-3">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <Select
          label="单据类型"
          value={bizTypeFilter}
          onChange={(event) => {
            setBizTypeFilter(event.target.value as BizTypeFilter)
            setPage(1)
          }}
          options={BIZ_TYPE_OPTIONS}
        />
        <Button variant="secondary" onClick={() => openAudit()}>
          审计记录
        </Button>
      </div>

      {listHint ? (
        <p role="status" className="rounded-lg border border-danger bg-danger-soft px-4 py-2 text-xs text-danger">
          {listHint}
        </p>
      ) : null}

      <Table
        ariaLabel="审核队列"
        columns={columns}
        rows={list.data?.items ?? []}
        rowKey={(item) => String(item.id)}
        loading={list.isLoading}
        error={listError}
        onRetry={() => void list.refetch()}
        rowClassName={(item) => (item.anomaly ? 'bg-danger-soft' : undefined)}
        emptyTitle="暂无审核单据"
        emptyDescription={
          bizTypeFilter !== 'ALL'
            ? '当前单据类型没有匹配的审核任务。'
            : stateFilter === 'PENDING'
              ? '当前没有待审核单据。'
              : '当前状态下没有审核记录。'
        }
        pagination={
          list.data
            ? {
                page: list.data.page,
                pageSize: list.data.page_size,
                total: list.data.total,
                onPageChange: setPage,
              }
            : undefined
        }
      />
    </div>
  )

  const invoiceRequired =
    decisionKind === 'approve' && decisionTarget?.biz_type === 'INVOICE_APPLY'

  return (
    <div className="space-y-5">
      <PageHeader
        title="审核中心"
        description="默认只看待处理的单据，按提交时间从早到晚排序；审批人不能审批自己提交的单据。"
        actions={
          <Button variant="secondary" onClick={() => openAudit()}>
            审计记录
          </Button>
        }
      />

      <Tabs
        ariaLabel="审核状态"
        value={stateFilter}
        onChange={(id) => {
          setStateFilter(id as StateFilter)
          setPage(1)
        }}
        items={STATE_TABS.map((tab) => ({
          id: tab.value,
          label: tab.label,
          content: tableSection,
        }))}
      />

      <Modal
        open={decisionTarget !== null}
        onClose={closeDecision}
        title={decisionKind === 'approve' ? '通过审核' : '驳回审核'}
        description="决定会写入审核队列与审计记录，后端仍会复核状态与职责分离。"
        size="md"
        footer={
          <>
            <Button variant="secondary" onClick={closeDecision} disabled={saving}>
              取消
            </Button>
            <Button
              variant={decisionKind === 'reject' ? 'danger' : 'primary'}
              loading={saving}
              onClick={() => void handleDecision()}
            >
              {decisionKind === 'approve' ? '确认通过' : '确认驳回'}
            </Button>
          </>
        }
      >
        {decisionTarget ? (
          <div className="space-y-4">
            <div className="space-y-1 rounded-lg border border-border bg-surface-muted p-3 text-xs text-ink-secondary">
              <p>
                单据：{BIZ_TYPE_LABEL[decisionTarget.biz_type]} ·{' '}
                {decisionTarget.biz_no || `#${decisionTarget.biz_id}`}
              </p>
              <p>申请人：{decisionTarget.applicant_name}（#{decisionTarget.applicant_id}）</p>
              {decisionTarget.direction ? (
                <p>
                  方向：
                  {BALANCE_DIRECTION_LABEL[decisionTarget.direction] ??
                    decisionTarget.direction}
                </p>
              ) : null}
              <p>摘要：{decisionTarget.summary || '—'}</p>
              <p>金额：{amountText(decisionTarget)}</p>
            </div>

            <div className="space-y-1.5">
              <label className="block text-sm font-medium text-ink" htmlFor="review-reason">
                {decisionKind === 'reject'
                  ? `驳回理由（必填，至少 ${REJECT_REASON_MIN} 字）`
                  : '审批理由（可选，写入审核备注）'}
                {decisionKind === 'reject' ? (
                  <span className="ml-0.5 text-danger" aria-hidden="true">
                    *
                  </span>
                ) : null}
              </label>
              <textarea
                id="review-reason"
                className="min-h-24 w-full rounded-lg border border-border-strong bg-surface px-3 py-2 text-sm text-ink placeholder:text-ink-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
                value={reason}
                onChange={(event) => {
                  setReason(event.target.value)
                  setReasonError(null)
                }}
              />
              {reasonError ? (
                <p className="text-xs text-danger">{reasonError}</p>
              ) : (
                <p className="text-xs text-ink-muted">
                  {decisionKind === 'reject'
                    ? `少于 ${REJECT_REASON_MIN} 字不会提交。`
                    : '可留空；后端最多保留 255 个字符。'}
                </p>
              )}
            </div>

            {invoiceRequired ? (
              <Input
                label="发票文件链接（开票申请必填）"
                required
                value={invoiceUrl}
                error={invoiceError ?? undefined}
                hint="开票必须上传发票文件，没有文件无法提交。"
                onChange={(event) => {
                  setInvoiceUrl(event.target.value)
                  setInvoiceError(null)
                }}
              />
            ) : null}

            {decisionError ? (
              <div role="alert" className="space-y-1 rounded-lg border border-danger bg-danger-soft px-3 py-2">
                <p className="text-xs text-danger">{decisionError}</p>
                {decisionHint ? <p className="text-xs text-ink-muted">{decisionHint}</p> : null}
              </div>
            ) : null}
          </div>
        ) : null}
      </Modal>

      <AuditTrail
        open={auditOpen}
        onClose={() => setAuditOpen(false)}
        module="REVIEW"
        targetType={auditTarget.type}
        targetId={auditTarget.id}
        refreshKey={auditRefreshKey}
      />
    </div>
  )
}

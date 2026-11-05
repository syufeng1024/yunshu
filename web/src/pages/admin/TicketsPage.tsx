import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { errorHint, errorMessage } from '@/api/client'
import {
  useAdminTicketDetail,
  useAdminTickets,
  useReplyAdminTicket,
  useSetAdminTicketState,
} from '@/api/admin'
import type { AdminTicketItem, TicketState, TicketType } from '@/api/admin'
import {
  Badge,
  Button,
  Drawer,
  ErrorState,
  Input,
  PageHeader,
  Select,
  Table,
  useToast,
} from '@/components'
import type { TableColumn } from '@/components'
import { AuditTrail } from '@/components/admin'
import { ConfirmDialog } from '@/components/console'
import { ticketStateLabel, ticketTypeLabel } from '@/lib/consoleLabels'
import { formatDateTime, formatInteger } from '@/lib/format'
import { useDebouncedValue } from '@/lib/hooks/useDebouncedValue'
import { usePageTitle } from '@/lib/usePageTitle'

const PAGE_SIZE = 20
const REPLY_MAX = 5_000

type StateFilter = 'ALL' | TicketState
type TypeFilter = 'ALL' | TicketType
type OverdueFilter = 'ALL' | 'true' | 'false'

interface SessionEntry {
  key: string
  replyId: number
  kind: 'REPLY' | 'INTERNAL_NOTE'
  content: string
  createdAt: string
}

const STATE_OPTIONS = [
  { value: 'ALL', label: '全部状态' },
  { value: 'OPEN', label: ticketStateLabel('OPEN') },
  { value: 'RESPONDED', label: ticketStateLabel('RESPONDED') },
  { value: 'CLOSED', label: ticketStateLabel('CLOSED') },
]

const TYPE_OPTIONS = [
  { value: 'ALL', label: '全部类型' },
  { value: 'BILLING', label: ticketTypeLabel('BILLING') },
  { value: 'TECH', label: ticketTypeLabel('TECH') },
  { value: 'CONTENT_APPEAL', label: ticketTypeLabel('CONTENT_APPEAL') },
  { value: 'OTHER', label: ticketTypeLabel('OTHER') },
]

function stateVariant(state: TicketState): 'warning' | 'primary' | 'neutral' {
  if (state === 'OPEN') return 'warning'
  if (state === 'RESPONDED') return 'primary'
  return 'neutral'
}

function InfoRow({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-3 border-b border-border py-1.5 text-xs last:border-0">
      <span className="text-ink-muted">{label}</span>
      <span className="text-right text-ink">{value}</span>
    </div>
  )
}

export default function AdminTicketsPage() {
  usePageTitle('工单处理')
  const toast = useToast()

  const [stateFilter, setStateFilter] = useState<StateFilter>('ALL')
  const [typeFilter, setTypeFilter] = useState<TypeFilter>('ALL')
  const [overdueFilter, setOverdueFilter] = useState<OverdueFilter>('ALL')
  const [search, setSearch] = useState('')
  const [page, setPage] = useState(1)
  const debouncedSearch = useDebouncedValue(search, 300)

  useEffect(() => {
    setPage(1)
  }, [debouncedSearch, stateFilter, typeFilter, overdueFilter])

  const list = useAdminTickets({
    page,
    pageSize: PAGE_SIZE,
    state: stateFilter === 'ALL' ? undefined : stateFilter,
    type: typeFilter === 'ALL' ? undefined : typeFilter,
    overdue: overdueFilter === 'ALL' ? undefined : overdueFilter === 'true',
    q: debouncedSearch || undefined,
    // No sort/order: the backend's overdue view is ordered by
    // first_response_due_at ASC (longest overdue first), and that default is
    // the contract we want to keep.
  })

  const replyMutation = useReplyAdminTicket()
  const stateMutation = useSetAdminTicketState()

  const [selected, setSelected] = useState<AdminTicketItem | null>(null)
  const detail = useAdminTicketDetail(selected?.id)
  const [replyContent, setReplyContent] = useState('')
  const [replyError, setReplyError] = useState<string | null>(null)
  const [internalConfirmOpen, setInternalConfirmOpen] = useState(false)

  const [closeOpen, setCloseOpen] = useState(false)
  const [closeReason, setCloseReason] = useState('')
  const [closeError, setCloseError] = useState<string | null>(null)

  const [sessionEntries, setSessionEntries] = useState<Record<number, SessionEntry[]>>({})

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

  const openTicket = (ticket: AdminTicketItem) => {
    setSelected(ticket)
    setReplyContent('')
    setReplyError(null)
    setInternalConfirmOpen(false)
    setCloseOpen(false)
    setCloseReason('')
    setCloseError(null)
  }

  const closeTicketDrawer = () => {
    if (replyMutation.isPending || stateMutation.isPending) return
    setSelected(null)
    setReplyError(null)
    setInternalConfirmOpen(false)
    setCloseOpen(false)
    setCloseError(null)
  }

  const validateReply = (): string | null => {
    if (!selected) return '请选择工单。'
    if (selected.state === 'CLOSED') return '工单已关闭，不能回复或添加备注。'
    const content = replyContent.trim()
    if (!content) return '请填写回复内容。'
    if ([...content].length > REPLY_MAX) return `回复内容最多 ${REPLY_MAX} 个字符。`
    return null
  }

  const submitReply = async (isInternal: boolean) => {
    if (!selected) return
    const error = validateReply()
    if (error) {
      setReplyError(error)
      setInternalConfirmOpen(false)
      return
    }
    setReplyError(null)
    const ticketId = selected.id
    const content = replyContent.trim()
    try {
      const response = await replyMutation.mutateAsync({
        id: ticketId,
        input: { content, is_internal: isInternal },
      })
      const entry: SessionEntry = {
        key: `${isInternal ? 'note' : 'reply'}-${response.reply.id}`,
        replyId: response.reply.id,
        kind: isInternal ? 'INTERNAL_NOTE' : 'REPLY',
        content,
        createdAt: response.reply.created_at,
      }
      setSessionEntries((current) => ({
        ...current,
        [ticketId]: [...(current[ticketId] ?? []), entry],
      }))
      setSelected((current) =>
        current && current.id === ticketId
          ? {
              ...current,
              state: response.ticket.state,
              first_responded_at: response.ticket.first_responded_at,
              last_replied_at: response.ticket.last_replied_at,
              sla: response.ticket.sla,
            }
          : current,
      )
      setReplyContent('')
      setInternalConfirmOpen(false)
      if (isInternal) {
        toast.success('内部备注已记录（用户不可见）', '状态与处理时限均未改变。')
      } else {
        toast.success('已回复用户', '工单状态已变为已响应。')
      }
      openAudit('t_ticket', ticketId)
    } catch (error) {
      setInternalConfirmOpen(false)
      const hint = errorHint(error)
      setReplyError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const beginReply = (isInternal: boolean) => {
    const error = validateReply()
    if (error) {
      setReplyError(error)
      return
    }
    setReplyError(null)
    if (isInternal) {
      setInternalConfirmOpen(true)
      return
    }
    void submitReply(false)
  }

  const handleCloseTicket = async () => {
    if (!selected) return
    setCloseError(null)
    try {
      await stateMutation.mutateAsync({
        id: selected.id,
        input: {
          state: 'CLOSED',
          reason: closeReason.trim() || undefined,
        },
      })
      toast.success('工单已关闭')
      openAudit('t_ticket', selected.id)
      setCloseOpen(false)
      setSelected(null)
    } catch (error) {
      const hint = errorHint(error)
      setCloseError(hint ? `${errorMessage(error)}（${hint}）` : errorMessage(error))
    }
  }

  const listError = list.isError ? errorMessage(list.error) : null
  const listHint = list.isError ? errorHint(list.error) : null
  const ticketDetail = detail.data
  const serverReplies = ticketDetail?.replies ?? []
  const localEntries = selected ? sessionEntries[selected.id] ?? [] : []
  // The detail refetch includes a just-posted reply; suppress the optimistic
  // local copy once the server list contains the same reply id.
  const pendingEntries = localEntries.filter(
    (entry) => !serverReplies.some((reply) => reply.id === entry.replyId),
  )

  const columns: TableColumn<AdminTicketItem>[] = [
    {
      key: 'ticket_no',
      header: '工单号',
      render: (ticket) => (
        <div className="space-y-0.5">
          <span className="font-mono text-xs">{ticket.ticket_no}</span>
          <p className="text-xs text-ink-muted">{ticket.user_email || '—'}</p>
        </div>
      ),
    },
    {
      key: 'type',
      header: '类型',
      render: (ticket) => (
        <span className="whitespace-nowrap text-xs">
          {ticketTypeLabel(ticket.ticket_type)}
        </span>
      ),
    },
    {
      key: 'title',
      header: '标题',
      render: (ticket) => <span className="text-xs text-ink">{ticket.title}</span>,
    },
    {
      key: 'state',
      header: '状态',
      render: (ticket) => (
        <Badge variant={stateVariant(ticket.state)}>{ticketStateLabel(ticket.state)}</Badge>
      ),
    },
    {
      key: 'sla',
      header: '处理时限（系统计算）',
      render: (ticket) => (
        <div className={ticket.sla.overdue ? 'space-y-1 text-danger' : 'space-y-1'}>
          <p className="whitespace-nowrap text-xs">{ticket.sla.display}</p>
          {ticket.sla.overdue ? <Badge variant="danger">已超时</Badge> : null}
        </div>
      ),
    },
    {
      key: 'created_at',
      header: '创建时间',
      render: (ticket) => (
        <span className="whitespace-nowrap text-xs text-ink-secondary">
          {formatDateTime(ticket.created_at)}
        </span>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (ticket) => (
        <Button variant="ghost" size="sm" onClick={() => openTicket(ticket)}>
          处理
        </Button>
      ),
    },
  ]

  const ticketState = ticketDetail?.state ?? selected?.state ?? 'OPEN'
  const ticketSla = ticketDetail?.sla ?? selected?.sla
  const ticketUserEmail = ticketDetail?.user_email ?? selected?.user_email ?? ''
  const closed = ticketState === 'CLOSED'
  const savingReply = replyMutation.isPending
  const savingState = stateMutation.isPending

  return (
    <div className="space-y-5">
      <PageHeader
        title="工单处理"
        description="处理时限由系统计算，只提示不处置；超时工单不会自动关闭，需运营人工回复或关闭。"
        actions={
          <Button variant="secondary" onClick={() => openAudit()}>
            审计记录
          </Button>
        }
      />

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Input
          label="搜索工单"
          type="search"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="工单号或标题片段"
          hint="按工单号或标题模糊搜索。"
        />
        <Select
          label="状态"
          value={stateFilter}
          onChange={(event) => setStateFilter(event.target.value as StateFilter)}
          options={STATE_OPTIONS}
        />
        <Select
          label="类型"
          value={typeFilter}
          onChange={(event) => setTypeFilter(event.target.value as TypeFilter)}
          options={TYPE_OPTIONS}
        />
        <Select
          label="处理时限"
          value={overdueFilter}
          onChange={(event) => setOverdueFilter(event.target.value as OverdueFilter)}
          options={[
            { value: 'ALL', label: '全部处理时限' },
            { value: 'true', label: '仅已超时' },
            { value: 'false', label: '仅未超时' },
          ]}
        />
      </div>

      {listHint ? (
        <p role="status" className="rounded-lg border border-danger bg-danger-soft px-4 py-2 text-xs text-danger">
          {listHint}
        </p>
      ) : null}

      <Table
        ariaLabel="工单队列"
        columns={columns}
        rows={list.data?.items ?? []}
        rowKey={(ticket) => String(ticket.id)}
        loading={list.isLoading}
        error={listError}
        onRetry={() => void list.refetch()}
        rowClassName={(ticket) => (ticket.sla.overdue ? 'bg-danger-soft' : undefined)}
        emptyTitle="暂无工单"
        emptyDescription={
          search || stateFilter !== 'ALL' || typeFilter !== 'ALL' || overdueFilter !== 'ALL'
            ? '当前搜索与筛选组合没有匹配的工单。'
            : '后端当前没有工单数据。'
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

      <Drawer
        open={selected !== null}
        onClose={closeTicketDrawer}
        title="处理工单"
        description="运营侧详情读取任意用户工单；用户回复与内部备注分开标记。"
        size="lg"
      >
        {selected ? (
          detail.isLoading && !ticketDetail ? (
            <p className="text-sm text-ink-secondary">正在读取工单详情…</p>
          ) : detail.isError ? (
            <ErrorState
              message={errorMessage(detail.error)}
              hint={errorHint(detail.error)}
              onRetry={() => void detail.refetch()}
            />
          ) : (
            <div className="space-y-5">
              <section className="space-y-1">
                <h3 className="text-sm font-semibold text-ink">工单信息</h3>
                <InfoRow label="工单号" value={<code>{selected.ticket_no}</code>} />
                <InfoRow label="类型" value={ticketTypeLabel(selected.ticket_type)} />
                <InfoRow label="标题" value={selected.title} />
                <InfoRow label="用户邮箱" value={ticketUserEmail || '—'} />
                <InfoRow
                  label="状态"
                  value={
                    <Badge variant={stateVariant(ticketState)}>
                      {ticketStateLabel(ticketState)}
                    </Badge>
                  }
                />
                <InfoRow label="请求编号" value={selected.request_id || '—'} />
                <InfoRow
                  label="规则命中编号"
                  value={
                    selected.policy_hit_id === null || selected.policy_hit_id === undefined
                      ? '—'
                      : `#${selected.policy_hit_id}`
                  }
                />
                {ticketDetail?.policy_rule_id ? (
                  <InfoRow label="策略规则" value={ticketDetail.policy_rule_id} />
                ) : null}
                {ticketDetail?.policy_category ? (
                  <InfoRow label="策略分类" value={ticketDetail.policy_category} />
                ) : null}
                <InfoRow label="创建时间" value={formatDateTime(selected.created_at)} />
                <InfoRow label="重开次数" value={formatInteger(ticketDetail?.reopen_count ?? 0)} />
                <InfoRow
                  label="处理时限"
                  value={
                    ticketSla ? (
                      <span className={ticketSla.overdue ? 'text-danger' : undefined}>
                        {ticketSla.display}
                        {ticketSla.overdue ? ' · 已超时' : ''}
                      </span>
                    ) : (
                      '—'
                    )
                  }
                />
              </section>

              <section className="space-y-1">
                <h3 className="text-sm font-semibold text-ink">工单描述</h3>
                <p className="whitespace-pre-wrap rounded-lg border border-border bg-surface-muted p-3 text-xs text-ink-secondary">
                  {ticketDetail?.description || '—'}
                </p>
              </section>

              <section className="space-y-2">
                <h3 className="text-sm font-semibold text-ink">对话记录</h3>
                {serverReplies.length === 0 && pendingEntries.length === 0 ? (
                  <p className="text-xs text-ink-muted">暂无回复或内部备注。</p>
                ) : (
                  <ul className="space-y-2">
                    {serverReplies.map((reply) => (
                      <li key={`server-${reply.id}`} className="space-y-1 rounded-lg border border-border p-3">
                        <div className="flex flex-wrap items-center gap-2">
                          <Badge
                            variant={
                              reply.is_internal
                                ? 'warning'
                                : reply.author_type === 'USER'
                                  ? 'neutral'
                                  : 'info'
                            }
                          >
                            {reply.is_internal
                              ? '内部备注'
                              : reply.author_type === 'USER'
                                ? '用户回复'
                                : '回复用户'}
                          </Badge>
                          {reply.is_internal ? (
                            <span className="text-xs text-warning">仅运营可见</span>
                          ) : null}
                          <span className="text-xs text-ink-muted">
                            {formatDateTime(reply.created_at)}
                          </span>
                        </div>
                        <p className="whitespace-pre-wrap text-xs text-ink">{reply.content}</p>
                      </li>
                    ))}
                    {pendingEntries.map((entry) => (
                      <li key={entry.key} className="space-y-1 rounded-lg border border-border p-3">
                        <div className="flex flex-wrap items-center gap-2">
                          <Badge variant={entry.kind === 'INTERNAL_NOTE' ? 'warning' : 'info'}>
                            {entry.kind === 'INTERNAL_NOTE' ? '内部备注' : '回复用户'}
                          </Badge>
                          <Badge variant="success">本次会话新建</Badge>
                          <span className="text-xs text-ink-muted">
                            {formatDateTime(entry.createdAt)}
                          </span>
                        </div>
                        <p className="whitespace-pre-wrap text-xs text-ink">{entry.content}</p>
                      </li>
                    ))}
                  </ul>
                )}
              </section>

              <section className="space-y-3">
                <h3 className="text-sm font-semibold text-ink">回复</h3>
                {closed ? (
                  <p className="rounded-lg border border-border bg-surface-muted px-3 py-2 text-xs text-ink-secondary">
                    工单已关闭，不能回复或添加备注。
                  </p>
                ) : (
                  <>
                    <div className="space-y-1.5">
                      <label className="block text-sm font-medium text-ink" htmlFor="ticket-reply">
                        回复内容
                      </label>
                      <textarea
                        id="ticket-reply"
                        className="min-h-24 w-full rounded-lg border border-border-strong bg-surface px-3 py-2 text-sm text-ink placeholder:text-ink-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
                        value={replyContent}
                        onChange={(event) => {
                          setReplyContent(event.target.value)
                          setReplyError(null)
                        }}
                        disabled={savingReply}
                      />
                      {replyError ? (
                        <p role="alert" className="text-xs text-danger">
                          {replyError}
                        </p>
                      ) : (
                        <p className="text-xs text-ink-muted">最多 {REPLY_MAX} 个字符。</p>
                      )}
                    </div>
                    <div className="flex flex-wrap items-center gap-2">
                      <Button
                        onClick={() => beginReply(false)}
                        loading={savingReply && !internalConfirmOpen}
                        disabled={savingReply || savingState}
                      >
                        回复用户
                      </Button>
                      <Button
                        variant="secondary"
                        onClick={() => beginReply(true)}
                        loading={savingReply && internalConfirmOpen}
                        disabled={savingReply || savingState}
                      >
                        内部备注
                      </Button>
                    </div>
                    <p className="rounded-lg border border-info bg-info-soft px-3 py-2 text-xs text-info">
                      内部备注仅运营可见，不会发给用户、不改变工单状态，也不停止处理时限计时；提交前会再次确认。
                    </p>
                  </>
                )}
              </section>

              <section className="space-y-2">
                <h3 className="text-sm font-semibold text-ink">状态</h3>
                <p className="text-xs text-ink-muted">
                  工单只有「待处理 / 已回复 / 已关闭」三种状态。关闭是运营手工动作；超过处理时限只做提示，需人工处理。
                </p>
                <Button
                  variant="danger"
                  size="sm"
                  disabled={closed || savingReply || savingState}
                  title={closed ? '工单已关闭' : undefined}
                  onClick={() => {
                    setCloseReason('')
                    setCloseError(null)
                    setCloseOpen(true)
                  }}
                >
                  关闭工单
                </Button>
              </section>
            </div>
          )
        ) : null}
      </Drawer>

      <ConfirmDialog
        open={internalConfirmOpen}
        title="提交内部备注"
        description="内部备注仅运营可见，不会发给用户、不改变工单状态、也不停止处理时限计时。确认提交？"
        confirmLabel="确认提交内部备注"
        loading={savingReply}
        error={replyError}
        onConfirm={() => void submitReply(true)}
        onCancel={() => {
          if (savingReply) return
          setInternalConfirmOpen(false)
        }}
      />

      <ConfirmDialog
        open={closeOpen}
        title="关闭工单"
        description={
          <div className="space-y-3">
            <p>关闭后不可再回复；这是人工关闭，处理时限只提示不处置。</p>
            <Input
              label="关闭原因（可选）"
              value={closeReason}
              onChange={(event) => setCloseReason(event.target.value)}
              hint="最多 255 个字符，会写入审计。"
            />
          </div>
        }
        confirmLabel="确认关闭"
        loading={savingState}
        error={closeError}
        onConfirm={() => void handleCloseTicket()}
        onCancel={() => {
          if (savingState) return
          setCloseOpen(false)
          setCloseError(null)
        }}
      />

      <AuditTrail
        open={auditOpen}
        onClose={() => setAuditOpen(false)}
        module="TICKET"
        targetType={auditTarget.type}
        targetId={auditTarget.id}
        refreshKey={auditRefreshKey}
      />
    </div>
  )
}

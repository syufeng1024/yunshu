import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { ApiError, errorHint, errorMessage } from '@/api/client'
import {
  useConsoleModels,
  useLogBody,
  useLogDetail,
  useLogs,
  type LogDetailRequest,
  type LogDetailResponse,
  type LogItem,
  type LogStateFilter,
} from '@/api/console'
import {
  Badge,
  Button,
  CopyBlock,
  Drawer,
  ErrorState,
  Input,
  PageHeader,
  Select,
  Skeleton,
  Table,
} from '@/components'
import type { TableColumn } from '@/components'
import { LogTimeline } from '@/components/console'
import { daysAgoISO, todayISO } from '@/lib/consoleDates'
import {
  failOwnerLabel,
  failStageLabel,
  logResultLabel,
} from '@/lib/consoleLabels'
import {
  formatCreditAmount,
  formatDateTimeMs,
  formatDurationMs,
  formatInteger,
} from '@/lib/format'
import { usePageTitle } from '@/lib/usePageTitle'

const PAGE_SIZE = 20
const PAGE_SIZE_OPTIONS = [20, 50, 100] as const

const STATE_OPTIONS: readonly { value: LogStateFilter; label: string }[] = [
  { value: 'all', label: '全部状态' },
  { value: 'success', label: '成功' },
  { value: 'partial', label: '部分成功' },
  { value: 'failed', label: '失败' },
  { value: 'rate_limited', label: '限流' },
]

function resultVariant(result: LogItem['result']): 'success' | 'warning' | 'danger' {
  if (result === 'SUCCESS') return 'success'
  if (result === 'PARTIAL') return 'warning'
  return 'danger'
}

function dash(value: string | null | undefined): string {
  return value && value.length > 0 ? value : '—'
}

function creditText(micro: number): string {
  return `${formatCreditAmount(micro)} 积分`
}

/** Columns stay free of local sorting: the backend fixes created_at DESC. */
function buildColumns(
  onOpen: (requestId: string) => void,
): TableColumn<LogItem>[] {
  return [
    {
      key: 'created_at',
      header: '时间',
      render: (row) => (
        <span className="whitespace-nowrap tabular-nums text-xs">
          {formatDateTimeMs(row.created_at)}
        </span>
      ),
    },
    {
      key: 'request_id',
      header: '请求 ID',
      render: (row) => (
        <span className="break-all font-mono text-xs">{row.request_id}</span>
      ),
    },
    {
      key: 'model',
      header: '模型',
      render: (row) => (
        <div className="space-y-0.5">
          <p className="text-xs text-ink">{row.model_name}</p>
          {row.request_model && row.request_model !== row.model_name ? (
            <p className="text-xs text-ink-muted">请求别名：{row.request_model}</p>
          ) : null}
          <p className="text-xs text-ink-muted">{row.inbound_protocol}</p>
        </div>
      ),
    },
    {
      key: 'status',
      header: '状态',
      render: (row) => {
        const stage = failStageLabel(row.fail_stage)
        return (
          <div className="space-y-1">
            <div className="flex items-center gap-2">
              <Badge variant={resultVariant(row.result)}>
                {logResultLabel(row.result)}
              </Badge>
              <span className="text-xs tabular-nums text-ink-secondary">
                HTTP {row.http_status}
              </span>
            </div>
            {stage ? <p className="text-xs text-danger">阶段：{stage}</p> : null}
            {row.http_status === 429 ? (
              <p className="text-xs text-warning">限流</p>
            ) : null}
          </div>
        )
      },
    },
    {
      key: 'latency',
      header: '耗时',
      render: (row) => (
        <div className="space-y-0.5 text-xs tabular-nums">
          <p className="text-ink">{formatDurationMs(row.total_ms)}</p>
          <p className="text-ink-muted">
            首字节 {row.ttft_ms === null ? '—' : formatDurationMs(row.ttft_ms)}
          </p>
        </div>
      ),
    },
    {
      key: 'tokens',
      header: 'Token（入 / 出）',
      render: (row) => (
        <span className="whitespace-nowrap text-xs tabular-nums">
          {formatInteger(row.tokens_in)} / {formatInteger(row.tokens_out)}
        </span>
      ),
    },
    {
      key: 'consumed',
      header: '消耗',
      align: 'right',
      render: (row) => (
        <span className="whitespace-nowrap text-xs tabular-nums">
          {creditText(row.consumed_micro)}
        </span>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (row) => (
        <Button
          size="sm"
          variant="ghost"
          aria-label={`查看请求 ${row.request_id}`}
          onClick={() => onOpen(row.request_id)}
        >
          详情
        </Button>
      ),
    },
  ]
}

export default function LogsPage() {
  usePageTitle('请求日志')

  const [from, setFrom] = useState(() => daysAgoISO(6))
  const [to, setTo] = useState(() => todayISO())
  const [stateFilter, setStateFilter] = useState<LogStateFilter>('all')
  const [model, setModel] = useState('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(PAGE_SIZE)
  const [selectedRequestId, setSelectedRequestId] = useState<string | null>(null)

  useEffect(() => {
    setPage(1)
  }, [from, to, stateFilter, model])

  const params = useMemo(
    () => ({
      from,
      to,
      state: stateFilter,
      model: model || undefined,
      page,
      pageSize,
    }),
    [from, to, stateFilter, model, page, pageSize],
  )

  const logs = useLogs(params)
  const models = useConsoleModels()
  const columns = useMemo(() => buildColumns(setSelectedRequestId), [])

  const modelOptions = useMemo(
    () => [
      { value: '', label: '全部模型' },
      ...(models.data ?? []).map((item) => ({
        value: item.model_name,
        label: item.display_name || item.model_name,
      })),
    ],
    [models.data],
  )

  const listError = logs.isError ? errorMessage(logs.error) : null
  const listHint = logs.isError ? errorHint(logs.error) : null
  const forbidden =
    logs.isError && logs.error instanceof ApiError && logs.error.status === 403
  const filtered = stateFilter !== 'all' || model !== ''

  return (
    <div className="space-y-5">
      <PageHeader
        title="请求日志"
        description="按请求追溯状态、耗时、Token 与重试链路；时间倒序由服务端保证。"
      />

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Input
          label="开始日期"
          type="date"
          value={from}
          max={to}
          onChange={(event) => setFrom(event.target.value)}
          hint="在线窗口为最近 365 天"
        />
        <Input
          label="结束日期"
          type="date"
          value={to}
          min={from}
          onChange={(event) => setTo(event.target.value)}
        />
        <Select
          label="状态"
          value={stateFilter}
          onChange={(event) => setStateFilter(event.target.value as LogStateFilter)}
          options={STATE_OPTIONS}
        />
        <Select
          label="模型筛选"
          value={model}
          onChange={(event) => setModel(event.target.value)}
          options={modelOptions}
          hint="按对外模型名精确匹配"
        />
      </div>

      {listError ? (
        <ErrorState
          title={forbidden ? '无权限' : undefined}
          message={listError}
          hint={listHint}
          onRetry={() => void logs.refetch()}
        />
      ) : (
        <Table
          ariaLabel="请求日志列表"
          columns={columns}
          rows={logs.data?.items ?? []}
          rowKey={(row) => row.request_id}
          loading={logs.isLoading}
          emptyTitle="暂无请求日志"
          emptyDescription={
            filtered
              ? '当前时间、状态与模型筛选组合没有匹配的请求。'
              : '该时间范围内没有请求记录。'
          }
          rowClassName={(row) =>
            row.result === 'FAILED'
              ? 'border-l-2 border-l-danger bg-danger-soft/30'
              : 'border-l-2 border-l-transparent'
          }
          pagination={
            logs.data
              ? {
                  page: logs.data.page,
                  pageSize: logs.data.page_size,
                  total: logs.data.total,
                  pageSizeOptions: PAGE_SIZE_OPTIONS,
                  onPageChange: setPage,
                  onPageSizeChange: (size) => {
                    setPageSize(size)
                    setPage(1)
                  },
                }
              : undefined
          }
        />
      )}

      <LogDetailDrawer
        requestId={selectedRequestId}
        onClose={() => setSelectedRequestId(null)}
      />
    </div>
  )
}

// ---------------------------------------------------------------------------
// Detail drawer
// ---------------------------------------------------------------------------

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-ink-muted">{label}</dt>
      <dd className="break-words text-ink">{children}</dd>
    </div>
  )
}

function Metadata({ request }: { request: LogDetailRequest }) {
  const stage = failStageLabel(request.fail_stage)
  const owner = failOwnerLabel(request.fail_owner)
  return (
    <section aria-label="请求元数据" className="space-y-2">
      <h3 className="text-sm font-semibold text-ink">请求元数据</h3>
      <dl className="grid gap-2 rounded-lg border border-border px-3 py-3 text-xs sm:grid-cols-2">
        <Field label="请求时间">{formatDateTimeMs(request.created_at)}</Field>
        <Field label="请求 ID">
          <span className="break-all font-mono">{request.request_id}</span>
        </Field>
        <Field label="对外模型">{request.model_name}</Field>
        <Field label="请求模型">{dash(request.request_model)}</Field>
        <Field label="入站协议">{dash(request.inbound_protocol)}</Field>
        <Field label="出站协议">{dash(request.outbound_protocol)}</Field>
        <Field label="渠道">{dash(request.channel_name)}</Field>
        <Field label="分组">{dash(request.group_code)}</Field>
        <Field label="API Key">{dash(request.api_key_name)}</Field>
        <Field label="客户端 IP">{dash(request.client_ip)}</Field>
        <Field label="结果">{logResultLabel(request.result)}</Field>
        <Field label="HTTP / 业务码">
          {request.http_status} / {dash(request.biz_code)}
        </Field>
        <Field label="失败阶段">{stage ?? '—'}</Field>
        <Field label="失败归属">{owner ?? '—'}</Field>
        <Field label="Token 入 / 出">
          {formatInteger(request.tokens_in)} / {formatInteger(request.tokens_out)}
        </Field>
        <Field label="首字节 / 总耗时 / 上游耗时">
          {request.ttft_ms === null ? '—' : formatDurationMs(request.ttft_ms)} /{' '}
          {formatDurationMs(request.total_ms)} / {formatDurationMs(request.upstream_ms)}
        </Field>
        <Field label="消耗积分">{creditText(request.consumed_micro)}</Field>
        <Field label="预扣积分">{creditText(request.prehold_micro)}</Field>
        <Field label="充值消耗 / 赠送消耗">
          {creditText(request.consumed_recharge_micro)} /{' '}
          {creditText(request.consumed_bonus_micro)}
        </Field>
        <Field label="上游尝试次数">{formatInteger(request.upstream_attempts)}</Field>
        <Field label="流式 / 客户端中断">
          {request.is_stream ? '流式' : '非流式'} /{' '}
          {request.client_aborted ? '是' : '否'}
        </Field>
        <Field label="正文留存">{request.full_log_saved ? '已开启' : '未开启'}</Field>
      </dl>
    </section>
  )
}

function BodyPanel({
  requestId,
  detail,
}: {
  requestId: string
  detail: LogDetailResponse
}) {
  const [requested, setRequested] = useState(false)
  const body = useLogBody(requestId, requested)

  useEffect(() => {
    setRequested(false)
  }, [requestId])

  const message = body.isError ? errorMessage(body.error) : null
  const hint = body.isError ? errorHint(body.error) : null

  return (
    <section aria-label="请求正文" className="space-y-2">
      <h3 className="text-sm font-semibold text-ink">请求正文</h3>
      {!requested ? (
        <>
          <p className="text-xs text-ink-secondary">
            {detail.request.full_log_saved
              ? '该请求开启了正文留存，可尝试读取；正文仅保留 180 天。'
              : '该请求未开启正文留存。'}
          </p>
          <Button size="sm" variant="secondary" onClick={() => setRequested(true)}>
            加载正文
          </Button>
        </>
      ) : body.isLoading ? (
        <p className="rounded-lg border border-border px-3 py-2 text-xs text-ink-muted">
          正在读取正文…
        </p>
      ) : message ? (
        <ErrorState
          compact
          title="正文不可用"
          message={message}
          hint={hint}
          onRetry={() => void body.refetch()}
        />
      ) : body.data ? (
        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-2 text-xs text-ink-secondary">
            <Badge variant={body.data.redacted ? 'warning' : 'neutral'}>
              {body.data.redacted ? '敏感字段已脱敏' : '未发生脱敏'}
            </Badge>
            <span>模型：{body.data.model_name}</span>
            <span>{formatDateTimeMs(body.data.created_at)}</span>
          </div>
          <CopyBlock code={body.data.body} language="json" label="正文内容" />
        </div>
      ) : null}
    </section>
  )
}

function LogDetailDrawer({
  requestId,
  onClose,
}: {
  requestId: string | null
  onClose: () => void
}) {
  const detail = useLogDetail(requestId)
  const message = detail.isError ? errorMessage(detail.error) : null
  const hint = detail.isError ? errorHint(detail.error) : null

  return (
    <Drawer
      open={requestId !== null}
      onClose={onClose}
      title="请求详情"
      description={requestId ?? undefined}
      size="lg"
    >
      {detail.isLoading ? (
        <div className="space-y-3">
          <Skeleton className="h-6 w-2/3" />
          <Skeleton className="h-40 w-full" />
          <Skeleton className="h-40 w-full" />
        </div>
      ) : message ? (
        <ErrorState message={message} hint={hint} onRetry={() => void detail.refetch()} />
      ) : detail.data ? (
        <div className="space-y-5">
          <Metadata request={detail.data.request} />
          <LogTimeline
            attempts={detail.data.timeline}
            request={detail.data.request}
          />
          {requestId ? (
            <BodyPanel requestId={requestId} detail={detail.data} />
          ) : null}
        </div>
      ) : null}
    </Drawer>
  )
}

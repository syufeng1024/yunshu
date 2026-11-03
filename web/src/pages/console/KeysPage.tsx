import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { errorMessage } from '@/api/client'
import {
  FREE_PLAN_MAX_ACTIVE_KEYS,
  useActiveKeyCount,
  useConsoleOverview,
  useDeleteKey,
  useKeys,
  useUpdateKey,
} from '@/api/console'
import type { CreatedKey, KeyListItem } from '@/api/console'
import { Badge, Button, Input, PageHeader, Select, Table, useToast } from '@/components'
import type { TableColumn, TableSort } from '@/components'
import { ConfirmDialog, CreateKeyDialog, KeySecretOnce } from '@/components/console'
import { groupLabel } from '@/lib/consoleLabels'
import { ROUTES, consoleKeyDetailPath } from '@/lib/constants'
import { formatCreditAmount, formatDateTime } from '@/lib/format'
import { useDebouncedValue } from '@/lib/hooks/useDebouncedValue'
import { usePageTitle } from '@/lib/usePageTitle'

type StatusFilter = 'ALL' | 'ENABLED' | 'DISABLED'

const PAGE_SIZE = 20
const PAGE_SIZE_OPTIONS = [20, 50, 100] as const

// Column key -> the column name the list endpoint accepts. Keeping the mapping
// here (rather than sending the UI key) means a column rename can never turn
// into a silently ignored sort parameter.
const SORT_PARAM: Record<string, 'key_name' | 'last_used_at' | 'created_at'> = {
  name: 'key_name',
  created: 'created_at',
  lastUsed: 'last_used_at',
}

export default function KeysPage() {
  usePageTitle('密钥管理')
  const toast = useToast()

  const [search, setSearch] = useState('')
  const [status, setStatus] = useState<StatusFilter>('ALL')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(PAGE_SIZE)
  // Default ordering matches the backend default (created_at DESC); the
  // created/last-used columns make the current order visible instead of
  // implicit.
  const [sort, setSort] = useState<TableSort>({ key: 'created', direction: 'desc' })
  // Web search must be debounced (PRD §3.2); 300ms keeps typing responsive
  // without firing one request per keystroke.
  const debouncedSearch = useDebouncedValue(search, 300)

  useEffect(() => {
    setPage(1)
  }, [debouncedSearch, status])

  // Name search and status filter go to the API together: the backend ANDs
  // them, which is the intersection F6-1 asks for.
  const query = useKeys({
    q: debouncedSearch,
    status: status === 'ALL' ? undefined : status,
    page,
    pageSize,
    sort: SORT_PARAM[sort.key] ?? 'created_at',
    order: sort.direction,
  })

  const overview = useConsoleOverview({ poll: false })
  const freeTier = overview.data?.plan.code === 'FREE_DEFAULT'
  const activeCount = useActiveKeyCount(freeTier)
  const atLimit = freeTier && (activeCount.data ?? 0) >= FREE_PLAN_MAX_ACTIVE_KEYS

  const [createOpen, setCreateOpen] = useState(false)
  const [revealed, setRevealed] = useState<{ items: CreatedKey[]; warning: string } | null>(null)
  const [disableTarget, setDisableTarget] = useState<KeyListItem | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<KeyListItem | null>(null)
  const [confirmError, setConfirmError] = useState<string | null>(null)

  const updateKey = useUpdateKey()
  const deleteKey = useDeleteKey()

  const error = query.isError ? errorMessage(query.error) : null

  const openCreate = () => {
    setCreateOpen(true)
  }

  const handleEnable = async (key: KeyListItem) => {
    try {
      await updateKey.mutateAsync({ id: key.id, input: { status: 'ENABLED' } })
      toast.success('密钥已启用')
    } catch (err) {
      toast.error('启用失败', errorMessage(err))
    }
  }

  const handleConfirmDisable = async () => {
    if (!disableTarget) return
    setConfirmError(null)
    try {
      await updateKey.mutateAsync({ id: disableTarget.id, input: { status: 'DISABLED' } })
      toast.success('密钥已停用')
      setDisableTarget(null)
    } catch (err) {
      setConfirmError(errorMessage(err))
    }
  }

  const handleConfirmDelete = async () => {
    if (!deleteTarget) return
    setConfirmError(null)
    try {
      await deleteKey.mutateAsync(deleteTarget.id)
      toast.success('密钥已删除')
      setDeleteTarget(null)
    } catch (err) {
      setConfirmError(errorMessage(err))
    }
  }

  const columns: TableColumn<KeyListItem>[] = [
    {
      key: 'name',
      header: '名称',
      sortable: true,
      render: (row) => (
        <Link
          to={consoleKeyDetailPath(String(row.id))}
          className="font-medium text-primary hover:underline"
        >
          {row.key_name}
        </Link>
      ),
    },
    {
      key: 'prefix',
      header: 'Key 前缀',
      render: (row) => <code className="font-mono text-xs text-ink-secondary">{row.key_masked}</code>,
    },
    {
      key: 'status',
      header: '状态',
      render: (row) => (
        <span className="inline-flex flex-wrap gap-1">
          <Badge variant={row.status === 'ENABLED' ? 'success' : 'neutral'} dot>
            {row.status === 'ENABLED' ? '启用' : '已停用'}
          </Badge>
          {row.expired ? <Badge variant="danger">已过期</Badge> : null}
        </span>
      ),
    },
    {
      key: 'group',
      header: '分组',
      render: (row) => {
        const label = groupLabel(row.group_code)
        return label ? (
          <span className="text-ink-secondary">{label}</span>
        ) : (
          <span className="text-ink-muted">—</span>
        )
      },
    },
    {
      key: 'expire',
      header: '有效期',
      render: (row) =>
        row.expire_at ? (
          <span className="text-xs text-ink-secondary">{formatDateTime(row.expire_at)}</span>
        ) : (
          <span className="text-xs text-ink-muted">永不过期</span>
        ),
    },
    {
      key: 'created',
      header: '创建时间',
      sortable: true,
      render: (row) => (
        <span className="whitespace-nowrap text-xs text-ink-secondary">
          {formatDateTime(row.created_at)}
        </span>
      ),
    },
    {
      key: 'lastUsed',
      header: '最近使用',
      sortable: true,
      render: (row) =>
        row.last_used_at ? (
          <span className="whitespace-nowrap text-xs text-ink-secondary">
            {formatDateTime(row.last_used_at)}
          </span>
        ) : (
          <span className="text-xs text-ink-muted">从未使用</span>
        ),
    },
    {
      key: 'usage',
      header: '今日用量',
      align: 'right',
      render: (row) => (
        <span className="tabular-nums">{formatCreditAmount(row.today_consumed_micro)} 积分</span>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (row) => (
        <div className="flex flex-wrap items-center justify-end gap-1">
          <Link
            to={consoleKeyDetailPath(String(row.id))}
            className="rounded-md px-2 py-1 text-xs text-primary hover:bg-surface-muted"
          >
            详情
          </Link>
          {row.status === 'ENABLED' ? (
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                setConfirmError(null)
                setDisableTarget(row)
              }}
            >
              停用
            </Button>
          ) : (
            <Button variant="ghost" size="sm" onClick={() => void handleEnable(row)}>
              启用
            </Button>
          )}
          <Button
            variant="ghost"
            size="sm"
            className="text-danger"
            onClick={() => {
              setConfirmError(null)
              setDeleteTarget(row)
            }}
          >
            删除
          </Button>
        </div>
      ),
    },
  ]

  return (
    <div className="space-y-5">
      <PageHeader
        title="密钥管理"
        description="每个密钥共享账户额度；分组只读，变更需联系运营。"
        actions={
          <Button onClick={openCreate} disabled={atLimit}>
            新建密钥
          </Button>
        }
      />

      {atLimit ? (
        <div
          role="status"
          className="rounded-lg border border-warning bg-warning-soft px-4 py-3 text-xs text-warning"
        >
          已达免费档活跃 Key 上限，升级套餐可扩容。
          <Link to={ROUTES.consoleRecharge} className="ml-1 underline">
            前往升级
          </Link>
        </div>
      ) : null}

      <div className="grid gap-3 sm:grid-cols-[minmax(0,1fr)_12rem]">
        <Input
          label="按名称搜索"
          type="search"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="输入密钥名称片段"
          aria-label="按名称搜索密钥"
        />
        <Select
          label="状态"
          value={status}
          onChange={(event) => setStatus(event.target.value as StatusFilter)}
          options={[
            { value: 'ALL', label: '全部状态' },
            { value: 'ENABLED', label: '启用' },
            { value: 'DISABLED', label: '已停用' },
          ]}
        />
      </div>

      <Table
        ariaLabel="密钥列表"
        columns={columns}
        rows={query.data?.items ?? []}
        rowKey={(row) => String(row.id)}
        loading={query.isLoading}
        error={error}
        onRetry={() => void query.refetch()}
        emptyTitle="暂无密钥"
        emptyDescription={
          debouncedSearch || status !== 'ALL'
            ? '当前搜索与筛选组合没有匹配结果。'
            : '创建第一个密钥以开始调用 API。'
        }
        emptyAction={
          debouncedSearch || status !== 'ALL' ? undefined : (
            <Button size="sm" onClick={openCreate} disabled={atLimit}>
              新建密钥
            </Button>
          )
        }
        sort={sort}
        onSortChange={(next) => {
          setSort(next)
          setPage(1)
        }}
        pagination={
          query.data
            ? {
                page: query.data.page,
                pageSize: query.data.page_size,
                total: query.data.total,
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

      <CreateKeyDialog
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={(items, warning) => setRevealed({ items, warning })}
      />

      <KeySecretOnce
        open={revealed !== null}
        keys={
          revealed?.items.map((item) => ({
            id: item.id,
            key_name: item.key_name,
            key: item.key,
          })) ?? []
        }
        warning={revealed?.warning}
        onClose={() => setRevealed(null)}
      />

      <ConfirmDialog
        open={disableTarget !== null}
        title="停用密钥"
        description={
          disableTarget
            ? `停用后「${disableTarget.key_name}」立即无法调用，可随时重新启用。确认停用？`
            : undefined
        }
        confirmLabel="确认停用"
        loading={updateKey.isPending}
        error={confirmError}
        onConfirm={() => void handleConfirmDisable()}
        onCancel={() => {
          if (updateKey.isPending) return
          setDisableTarget(null)
          setConfirmError(null)
        }}
      />

      <ConfirmDialog
        open={deleteTarget !== null}
        title="删除密钥"
        description={
          deleteTarget
            ? `删除后「${deleteTarget.key_name}」立即失效且无法恢复，历史调用记录仍保留。确认删除？`
            : undefined
        }
        confirmLabel="确认删除"
        loading={deleteKey.isPending}
        error={confirmError}
        onConfirm={() => void handleConfirmDelete()}
        onCancel={() => {
          if (deleteKey.isPending) return
          setDeleteTarget(null)
          setConfirmError(null)
        }}
      />
    </div>
  )
}

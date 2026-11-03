import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { cn } from '@/lib/cn'
import { Button } from './Button'
import { EmptyState } from './EmptyState'
import { ErrorState } from './ErrorState'
import { Skeleton } from './Skeleton'

export type SortDirection = 'asc' | 'desc'

export interface TableSort {
  key: string
  direction: SortDirection
}

export interface TableColumn<T> {
  key: string
  header: string
  render: (row: T) => ReactNode
  sortable?: boolean
  /** Enables local sorting when the table is used without server-side sort. */
  sortValue?: (row: T) => string | number
  align?: 'left' | 'center' | 'right'
  className?: string
}

export interface TablePagination {
  /** 1-based page number. */
  page: number
  pageSize: number
  total: number
  onPageChange: (page: number) => void
  /**
   * Supply this to show a page-size switcher next to the page counter. Omitted
   * everywhere it does not exist, so no existing table changes shape.
   */
  onPageSizeChange?: (pageSize: number) => void
  pageSizeOptions?: readonly number[]
}

export interface TableProps<T> {
  columns: readonly TableColumn<T>[]
  rows: readonly T[]
  rowKey: (row: T) => string
  ariaLabel?: string
  loading?: boolean
  loadingRows?: number
  error?: string | null
  onRetry?: () => void
  emptyTitle?: string
  emptyDescription?: string
  emptyAction?: ReactNode
  sort?: TableSort | null
  onSortChange?: (sort: TableSort) => void
  pagination?: TablePagination
  className?: string
  /** Optional per-row classes (e.g. a danger accent for failed request rows). */
  rowClassName?: (row: T) => string | undefined
}

const ALIGN: Record<NonNullable<TableColumn<unknown>['align']>, string> = {
  left: 'text-left',
  center: 'text-center',
  right: 'text-right',
}

function compareValues(a: string | number, b: string | number): number {
  if (typeof a === 'number' && typeof b === 'number') return a - b
  return String(a).localeCompare(String(b), 'zh-CN')
}

export function Table<T>({
  columns,
  rows,
  rowKey,
  ariaLabel = '数据表格',
  loading = false,
  loadingRows = 5,
  error = null,
  onRetry,
  emptyTitle = '暂无数据',
  emptyDescription,
  emptyAction,
  sort,
  onSortChange,
  pagination,
  className,
  rowClassName,
}: TableProps<T>) {
  const [internalSort, setInternalSort] = useState<TableSort | null>(null)
  const activeSort = sort === undefined ? internalSort : sort
  const columnCount = columns.length

  const visibleRows = useMemo(() => {
    if (!activeSort || onSortChange) return rows
    const column = columns.find((candidate) => candidate.key === activeSort.key)
    if (!column?.sortValue) return rows
    const factor = activeSort.direction === 'asc' ? 1 : -1
    return [...rows].sort(
      (a, b) => factor * compareValues(column.sortValue!(a), column.sortValue!(b)),
    )
  }, [activeSort, columns, onSortChange, rows])

  const handleSort = (column: TableColumn<T>) => {
    if (!column.sortable) return
    const nextDirection: SortDirection =
      activeSort?.key === column.key && activeSort.direction === 'asc' ? 'desc' : 'asc'
    const next: TableSort = { key: column.key, direction: nextDirection }
    if (sort === undefined) setInternalSort(next)
    onSortChange?.(next)
  }

  const pageCount = pagination ? Math.max(1, Math.ceil(pagination.total / pagination.pageSize)) : 1
  const showPagination = Boolean(pagination) && !error && (pagination?.total ?? 0) > 0

  return (
    <div
      className={cn(
        'overflow-hidden rounded-xl border border-border bg-surface shadow-card',
        className,
      )}
    >
      <div className="overflow-x-auto">
        <table className="min-w-full border-collapse text-sm">
          <caption className="sr-only">{ariaLabel}</caption>
          <thead className="bg-surface-muted">
            <tr>
              {columns.map((column) => {
                const isActive = activeSort?.key === column.key
                const ariaSort = column.sortable
                  ? isActive
                    ? activeSort.direction === 'asc'
                      ? 'ascending'
                      : 'descending'
                    : 'none'
                  : undefined
                return (
                  <th
                    key={column.key}
                    scope="col"
                    aria-sort={ariaSort}
                    className={cn(
                      'whitespace-nowrap px-4 py-3 text-xs font-semibold text-ink-secondary',
                      ALIGN[column.align ?? 'left'],
                      column.className,
                    )}
                  >
                    {column.sortable ? (
                      <button
                        type="button"
                        onClick={() => handleSort(column)}
                        className="inline-flex items-center gap-1 rounded-sm text-xs font-semibold text-ink-secondary transition-colors duration-fast hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
                        aria-label={`按 ${column.header} 排序`}
                      >
                        {column.header}
                        <span aria-hidden="true" className="text-ink-muted">
                          {isActive ? (activeSort.direction === 'asc' ? '↑' : '↓') : '↕'}
                        </span>
                      </button>
                    ) : (
                      column.header
                    )}
                  </th>
                )
              })}
            </tr>
          </thead>
          <tbody>
            {error ? (
              <tr>
                <td colSpan={columnCount} className="p-4">
                  <ErrorState compact message={error} onRetry={onRetry} />
                </td>
              </tr>
            ) : loading ? (
              Array.from({ length: loadingRows }, (_, rowIndex) => (
                <tr key={`skeleton-${rowIndex}`} className="border-t border-border">
                  {columns.map((column) => (
                    <td key={column.key} className="px-4 py-3">
                      <Skeleton className="h-4 w-full" />
                    </td>
                  ))}
                </tr>
              ))
            ) : visibleRows.length === 0 ? (
              <tr>
                <td colSpan={columnCount} className="p-4">
                  <EmptyState
                    compact
                    title={emptyTitle}
                    description={emptyDescription}
                    action={emptyAction}
                  />
                </td>
              </tr>
            ) : (
              visibleRows.map((row) => (
                <tr
                  key={rowKey(row)}
                  className={cn(
                    'border-t border-border transition-colors duration-fast hover:bg-surface-muted',
                    rowClassName?.(row),
                  )}
                >
                  {columns.map((column) => (
                    <td
                      key={column.key}
                      className={cn(
                        'px-4 py-3 text-ink',
                        ALIGN[column.align ?? 'left'],
                        column.className,
                      )}
                    >
                      {column.render(row)}
                    </td>
                  ))}
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      {showPagination && pagination ? (
        <div className="flex flex-wrap items-center justify-between gap-3 border-t border-border px-4 py-3">
          <div className="flex flex-wrap items-center gap-3 text-xs text-ink-secondary">
            <p>
              第 {pagination.page} / {pageCount} 页 · 共 {pagination.total} 条
            </p>
            {pagination.onPageSizeChange ? (
              <label className="inline-flex items-center gap-1">
                每页
                <select
                  aria-label="每页条数"
                  value={pagination.pageSize}
                  onChange={(event) => pagination.onPageSizeChange?.(Number(event.target.value))}
                  className="h-7 rounded-md border border-border bg-surface px-1.5 text-xs text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
                >
                  {(pagination.pageSizeOptions ?? [20, 50, 100]).map((size) => (
                    <option key={size} value={size}>
                      {size}
                    </option>
                  ))}
                </select>
                条
              </label>
            ) : null}
          </div>
          <div className="flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              disabled={loading || pagination.page <= 1}
              onClick={() => pagination.onPageChange(pagination.page - 1)}
            >
              上一页
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={loading || pagination.page >= pageCount}
              onClick={() => pagination.onPageChange(pagination.page + 1)}
            >
              下一页
            </Button>
          </div>
        </div>
      ) : null}
    </div>
  )
}

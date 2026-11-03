import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { usePublicModels } from '@/api/public'
import type { PublicModel } from '@/api/public'
import { Badge, EmptyState, ErrorState, Input, Skeleton } from '@/components'
import { modelTypeLabel } from '@/lib/consoleLabels'
import { ROUTES } from '@/lib/constants'
import { formatPricePerK, formatTokens } from '@/lib/format'
import { usePageTitle } from '@/lib/usePageTitle'

type TypeFilter = 'ALL' | 'TEXT' | 'IMAGE'
type SortKey = 'recommend' | 'input_asc' | 'input_desc' | 'context_desc'

const TYPE_OPTIONS: { value: TypeFilter; label: string }[] = [
  { value: 'ALL', label: '全部类型' },
  { value: 'TEXT', label: '文本' },
  { value: 'IMAGE', label: '图像' },
]

const SORT_OPTIONS: { value: SortKey; label: string }[] = [
  { value: 'recommend', label: '推荐顺序' },
  { value: 'input_asc', label: '输入价从低到高' },
  { value: 'input_desc', label: '输入价从高到低' },
  { value: 'context_desc', label: '上下文从大到小' },
]

/**
 * 模型价格表。
 *
 * 参考的同类站点把「模型名 + 输入/输出单价」做成卡片网格，因为用户在这个页面上
 * 做的事是**挑模型**：先扫价格，再看能不能满足上下文。表格在这里反而更差——
 * 一行里塞不下描述与标签，扫读也慢。
 *
 * 单位沿用站内口径「积分/千 token」，不照搬参考站的「¥/百万 token」：同一份价格
 * 用两种单位展示，用户会以为哪里算错了。
 */
export default function ModelPricePage() {
  usePageTitle('模型价格')
  const [search, setSearch] = useState('')
  const [type, setType] = useState<TypeFilter>('ALL')
  const [sort, setSort] = useState<SortKey>('recommend')

  const query = usePublicModels()

  const models = useMemo(() => {
    const all = query.data?.data ?? []
    const keyword = search.trim().toLowerCase()
    const filtered = all.filter((model) => {
      if (type !== 'ALL' && model.model_type !== type) return false
      if (!keyword) return true
      return (
        model.model_name.toLowerCase().includes(keyword) ||
        model.display_name.toLowerCase().includes(keyword)
      )
    })
    const sorted = [...filtered]
    switch (sort) {
      case 'input_asc':
        sorted.sort((a, b) => a.input_price_micro_per_k - b.input_price_micro_per_k)
        break
      case 'input_desc':
        sorted.sort((a, b) => b.input_price_micro_per_k - a.input_price_micro_per_k)
        break
      case 'context_desc':
        sorted.sort((a, b) => b.context_window - a.context_window)
        break
      default:
        // 推荐顺序由后端给出（recommend_weight），这里保持原样。
        break
    }
    return sorted
  }, [query.data, search, type, sort])

  const total = query.data?.data.length ?? 0

  return (
    <div className="mx-auto w-full max-w-6xl px-4 py-12 sm:px-6">
      <header>
        <Badge variant="primary" dot>
          按量计费
        </Badge>
        <h1 className="mt-4 text-3xl font-semibold tracking-tight text-ink">模型价格</h1>
        <p className="mt-3 max-w-2xl text-sm leading-relaxed text-ink-secondary">
          价格按每千 token 计，输入与输出分别计价。缓存读取、长上下文加价等细则以文档页为准。
        </p>
      </header>

      <div className="mt-8 flex flex-col gap-3 sm:flex-row sm:items-end">
        <div className="sm:max-w-xs sm:flex-1">
          <Input
            type="search"
            aria-label="搜索模型"
            placeholder="搜索模型名或显示名"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
        </div>
        <div className="flex gap-2">
          {TYPE_OPTIONS.map((option) => (
            <button
              key={option.value}
              type="button"
              onClick={() => setType(option.value)}
              aria-pressed={type === option.value}
              className={
                type === option.value
                  ? 'rounded-lg border border-primary bg-primary-soft px-3 py-2 text-xs font-medium text-primary'
                  : 'rounded-lg border border-border bg-surface px-3 py-2 text-xs text-ink-secondary hover:bg-surface-muted'
              }
            >
              {option.label}
            </button>
          ))}
        </div>
        <select
          aria-label="排序"
          value={sort}
          onChange={(event) => setSort(event.target.value as SortKey)}
          className="h-10 rounded-lg border border-border bg-surface px-3 text-sm text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
        >
          {SORT_OPTIONS.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      </div>

      <p className="mt-4 text-xs text-ink-muted">
        共 {total} 个模型
        {models.length !== total ? `，当前筛选出 ${models.length} 个` : ''}
      </p>

      {query.isLoading ? (
        <div className="mt-6 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {[0, 1, 2, 3, 4, 5].map((key) => (
            <Skeleton key={key} className="h-44 w-full" />
          ))}
        </div>
      ) : query.isError ? (
        <div className="mt-6">
          <ErrorState
            message="模型价格加载失败"
            hint="稍后重试；如果持续失败请联系客服。"
            onRetry={() => {
              void query.refetch()
            }}
          />
        </div>
      ) : models.length === 0 ? (
        <div className="mt-6">
          <EmptyState
            title="没有匹配的模型"
            description={
              total === 0 ? '当前没有已上架的模型。' : '换个关键词，或把类型切回全部。'
            }
            action={
              total > 0 ? (
                <button
                  type="button"
                  className="text-sm text-primary hover:underline"
                  onClick={() => {
                    setSearch('')
                    setType('ALL')
                  }}
                >
                  清空筛选
                </button>
              ) : undefined
            }
          />
        </div>
      ) : (
        <div className="mt-6 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {models.map((model) => (
            <ModelCard key={model.id} model={model} />
          ))}
        </div>
      )}

      <p className="mt-10 text-xs text-ink-muted">
        没找到需要的模型？
        <Link to={ROUTES.integrations} className="ml-1 text-primary hover:underline">
          查看接入方式
        </Link>
        ，或在控制台提交工单。
      </p>
    </div>
  )
}

function ModelCard({ model }: { model: PublicModel }) {
  const priced = model.input_price_micro_per_k > 0 || model.output_price_micro_per_k > 0
  return (
    <article className="flex flex-col rounded-xl border border-border bg-surface p-5">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="truncate text-sm font-semibold text-ink">{model.display_name}</h2>
          <code className="mt-1 block truncate font-mono text-xs text-ink-secondary">
            {model.model_name}
          </code>
        </div>
        <Badge variant="primary">{modelTypeLabel(model.model_type)}</Badge>
      </div>

      <dl className="mt-4 grid grid-cols-2 gap-3 border-t border-border pt-4 text-xs">
        <div>
          <dt className="text-ink-muted">输入</dt>
          <dd className="mt-1 font-medium tabular-nums text-ink">
            {priced ? formatPricePerK(model.input_price_micro_per_k) : '待定价'}
          </dd>
        </div>
        <div>
          <dt className="text-ink-muted">输出</dt>
          <dd className="mt-1 font-medium tabular-nums text-ink">
            {priced ? formatPricePerK(model.output_price_micro_per_k) : '待定价'}
          </dd>
        </div>
      </dl>

      <p className="mt-3 text-xs text-ink-muted">
        上下文 {formatTokens(model.context_window)} · 协议{' '}
        {model.protocols.length > 0 ? model.protocols.join(' / ') : '—'}
      </p>
    </article>
  )
}

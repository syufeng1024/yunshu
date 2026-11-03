import { useEffect, useMemo, useState } from 'react'
import { errorHint, errorMessage } from '@/api/client'
import {
  useConsoleModels,
  useModelCatalog,
  type CatalogModel,
  type ModelType,
} from '@/api/console'
import { Badge, Input, PageHeader, Select, Table } from '@/components'
import type { TableColumn, TableSort } from '@/components'
import { modelTypeLabel, protocolLabel } from '@/lib/consoleLabels'
import {
  formatCreditAmount,
  formatPricePerK,
  formatTokens,
} from '@/lib/format'
import { useDebouncedValue } from '@/lib/hooks/useDebouncedValue'
import { usePageTitle } from '@/lib/usePageTitle'

type TypeFilter = 'ALL' | ModelType
type ProtocolFilter = string

const PAGE_SIZE = 20
const PAGE_SIZE_OPTIONS = [20, 50, 100] as const

const MODEL_TYPE_OPTIONS = [
  { value: 'ALL', label: '全部类型' },
  { value: 'TEXT', label: modelTypeLabel('TEXT') },
  { value: 'IMAGE', label: modelTypeLabel('IMAGE') },
  { value: 'EMBEDDING', label: modelTypeLabel('EMBEDDING') },
]

function CapabilityBadge({ label, value }: { label: string; value: boolean | null }) {
  if (value === true) return <Badge variant="success">{label}</Badge>
  if (value === false) return <Badge variant="neutral">{label}：不支持</Badge>
  return (
    <span className="text-xs text-ink-muted" title="后端未配置该能力，与“不支持”含义不同">
      {label}：—
    </span>
  )
}

function PriceCell({ model }: { model: CatalogModel }) {
  const pricing = model.pricing
  if (!pricing) {
    return (
      <span className="text-xs text-ink-muted" title="模型已上架但缺少生效价格规则">
        价格未配置
      </span>
    )
  }

  if (model.model_type === 'IMAGE') {
    return (
      <div className="space-y-0.5 text-xs">
        <p className="tabular-nums text-ink">
          {formatCreditAmount(pricing.per_call_price_micro)} 积分/次
        </p>
        <p className="tabular-nums text-ink-muted">
          参考 {pricing.per_call_cny_per_call} 元/次
        </p>
      </div>
    )
  }

  return (
    <div className="space-y-0.5 text-xs">
      <p className="tabular-nums text-ink">输入 {formatPricePerK(pricing.in_price_micro_per_k)}</p>
      <p className="tabular-nums text-ink">
        输出 {formatPricePerK(pricing.out_price_micro_per_k)}
      </p>
      <p className="tabular-nums text-ink-muted">
        参考 {pricing.in_price_cny_per_k} / {pricing.out_price_cny_per_k} 元/千 token
      </p>
    </div>
  )
}

/** The catalog is server-sorted; this page never reorders rows locally (F8-4). */
export default function ConsoleModelsPage() {
  usePageTitle('模型广场')
  const [search, setSearch] = useState('')
  const [type, setType] = useState<TypeFilter>('ALL')
  const [protocol, setProtocol] = useState<ProtocolFilter>('ALL')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(PAGE_SIZE)
  // Default is the API's recommended order; only the two columns the backend
  // whitelists are sortable, so a click never becomes a silently ignored param.
  const [sort, setSort] = useState<TableSort | null>(null)
  const debouncedSearch = useDebouncedValue(search, 300)

  useEffect(() => {
    setPage(1)
  }, [debouncedSearch, type, protocol])

  const catalog = useModelCatalog({
    page,
    pageSize,
    modelType: type === 'ALL' ? undefined : type,
    protocol: protocol === 'ALL' ? undefined : protocol,
    q: debouncedSearch,
    sort: sort?.key === 'model' ? 'model_name' : sort?.key === 'context' ? 'context_length' : undefined,
    order: sort?.direction,
  })

  // The API does not expose a protocol dictionary; the listed models are the
  // only place the codes appear, so the filter options are derived from them.
  const allModels = useConsoleModels()
  const protocolOptions = useMemo(() => {
    const codes = new Set<string>()
    for (const model of allModels.data ?? []) codes.add(model.protocol)
    return [
      { value: 'ALL', label: '全部厂商 / 协议' },
      ...[...codes].sort().map((code) => ({ value: code, label: protocolLabel(code) })),
    ]
  }, [allModels.data])

  const error = catalog.isError ? errorMessage(catalog.error) : null
  const hint = catalog.isError ? errorHint(catalog.error) : null

  const columns: TableColumn<CatalogModel>[] = [
    {
      key: 'model',
      header: '模型',
      sortable: true,
      render: (model) => (
        <div className="space-y-0.5">
          <p className="font-medium text-ink">{model.display_name}</p>
          <code className="text-xs text-ink-secondary">{model.model_name}</code>
          {model.aliases.length > 0 ? (
            <p className="text-xs text-ink-muted">别名：{model.aliases.join('、')}</p>
          ) : null}
        </div>
      ),
    },
    {
      key: 'type',
      header: '类型',
      render: (model) => <Badge variant="primary">{modelTypeLabel(model.model_type)}</Badge>,
    },
    {
      key: 'context',
      header: '上下文',
      align: 'right',
      sortable: true,
      render: (model) => (
        <span className="tabular-nums text-xs text-ink-secondary">
          {formatTokens(model.context_length)}
        </span>
      ),
    },
    {
      key: 'protocol',
      header: '协议',
      render: (model) => (
        <span className="text-xs text-ink-secondary">{protocolLabel(model.protocol)}</span>
      ),
    },
    {
      key: 'price',
      header: '标准单价',
      render: (model) => <PriceCell model={model} />,
    },
    {
      key: 'capabilities',
      header: '能力',
      render: (model) => (
        <div className="flex flex-wrap gap-1">
          <CapabilityBadge label="流式" value={model.capabilities.stream} />
          <CapabilityBadge label="工具调用" value={model.capabilities.tool_call} />
        </div>
      ),
    },
  ]

  return (
    <div className="space-y-5">
      <PageHeader
        title="模型广场"
        description="仅展示已上架模型；排序固定为推荐权重与上架时间，价格为标准单价。"
      />

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-[minmax(0,1fr)_10rem_12rem]">
        <Input
          label="搜索模型"
          type="search"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="模型名称或别名片段"
          aria-label="搜索模型"
        />
        <Select
          label="模型类型"
          value={type}
          onChange={(event) => setType(event.target.value as TypeFilter)}
          options={MODEL_TYPE_OPTIONS}
        />
        <Select
          label="厂商 / 协议"
          value={protocol}
          onChange={(event) => setProtocol(event.target.value)}
          options={protocolOptions}
          hint="协议代码来自后端模型目录"
        />
      </div>

      {error && hint ? (
        <p role="status" className="rounded-lg border border-danger bg-danger-soft px-4 py-2 text-xs text-danger">
          {hint}
        </p>
      ) : null}

      <Table
        ariaLabel="模型广场列表"
        columns={columns}
        sort={sort}
        onSortChange={(next) => {
          setSort(next)
          setPage(1)
        }}
        rows={catalog.data?.items ?? []}
        rowKey={(model) => String(model.id)}
        loading={catalog.isLoading}
        error={error}
        onRetry={() => void catalog.refetch()}
        emptyTitle="暂无已上架模型"
        emptyDescription={
          debouncedSearch || type !== 'ALL' || protocol !== 'ALL'
            ? '当前筛选条件没有匹配的模型。'
            : '后端当前没有对外展示的模型。'
        }
        pagination={
          catalog.data
            ? {
                page: catalog.data.page,
                pageSize: catalog.data.page_size,
                total: catalog.data.total,
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
    </div>
  )
}

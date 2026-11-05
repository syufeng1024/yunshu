import { useEffect, useMemo, useState } from 'react'
import { ApiError, errorHint, errorMessage } from '@/api/client'
import { useChannelUpstreamModels, useModels } from '@/api/admin'
import { Badge, Button, Modal, Select } from '@/components'

export interface ModelMapPickerProps {
  open: boolean
  /** 新建渠道时没有 id，接口无从调用，调用方据此禁用入口。 */
  channelId: number | null
  onClose: () => void
  /** 把「平台模型名 → 上游模型名」写进表单里的映射文本，由调用方决定怎么落。 */
  onApply: (platformModel: string, upstreamModel: string) => void
}

/**
 * 上游模型名不该由运营手打。
 *
 * 厂商会改目录（DeepSeek 现在只给 deepseek-flash / deepseek-v4-pro），手打的名字
 * 不会报错，只会在真实调用时变成 502。这里用渠道自己的密钥读一次厂商的
 * /v1/models，把「上游到底有哪些名字」变成可选项，并标出平台这一侧的状态：
 *
 *   已映射 → 已经有平台模型指向它（改映射会覆盖）
 *   已登记 → 平台有同名模型，但它没有指向这个上游名
 *   未登记 → 上游有、平台没有，先去「模型管理」登记并定价
 */
export function ModelMapPicker({
  open,
  channelId,
  onClose,
  onApply,
}: ModelMapPickerProps) {
  const upstream = useChannelUpstreamModels(channelId, open)
  // 100 是后端 MaxPageSize（mw/paging.go）；传 200 会拿到 400，下拉就空了。
  const models = useModels({ pageSize: 100 })
  const [platformModel, setPlatformModel] = useState('')
  const [upstreamModel, setUpstreamModel] = useState('')

  // 每次重新打开都从「没选」开始：带着上次的选择回来，很容易把映射写到错的模型上。
  useEffect(() => {
    if (!open) {
      setPlatformModel('')
      setUpstreamModel('')
    }
  }, [open])

  const modelOptions = useMemo(
    () =>
      (models.data?.items ?? []).map((model) => ({
        value: model.model_name,
        label: `${model.display_name}（${model.model_name}）`,
      })),
    [models.data],
  )

  const items = upstream.data?.items ?? []
  const upstreamOptions = useMemo(
    () =>
      items.map((item) => ({
        value: item.id,
        label: item.name ? `${item.id} · ${item.name}` : item.id,
      })),
    [items],
  )

  const error = upstream.error
  const canApply = platformModel !== '' && upstreamModel !== ''

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="上游可用模型"
      description="用这个渠道自己的密钥，向厂商的 /v1/models 查一次；只读，不会改动任何配置。"
      size="lg"
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            关闭
          </Button>
          <Button
            disabled={!canApply}
            onClick={() => {
              onApply(platformModel, upstreamModel)
              onClose()
            }}
          >
            写入映射
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        {upstream.isPending ? (
          <p className="text-sm text-ink-secondary">正在向上游查询模型列表…</p>
        ) : error !== null && error !== undefined ? (
          <div className="rounded-lg border border-danger bg-danger-soft px-3 py-2">
            <p className="text-sm font-medium text-danger">
              {error instanceof ApiError ? errorMessage(error) : '查询失败'}
            </p>
            <p className="mt-1 text-xs text-ink-secondary">
              {error instanceof ApiError
                ? errorHint(error)
                : '检查这个渠道的接入地址与上游密钥后重试。'}
            </p>
          </div>
        ) : (
          <>
            <div className="grid gap-3 sm:grid-cols-2">
              <Select
                label="平台模型（客户调用时写的名字）"
                value={platformModel}
                onChange={(event) => setPlatformModel(event.target.value)}
                options={
                  modelOptions.length > 0
                    ? [{ value: '', label: '请选择平台模型' }, ...modelOptions]
                    : [{ value: '', label: '模型管理里还没有模型' }]
                }
              />
              <Select
                label="上游模型（厂商那边的名字）"
                value={upstreamModel}
                onChange={(event) => setUpstreamModel(event.target.value)}
                options={[
                  { value: '', label: `请选择（上游共 ${items.length} 个）` },
                  ...upstreamOptions,
                ]}
              />
            </div>

            {modelOptions.length === 0 ? (
              <p className="text-xs text-ink-muted">
                平台还没有登记任何模型。先到「模型管理」新建模型并设置价格，再回来做映射。
              </p>
            ) : null}

            <div className="max-h-64 space-y-1 overflow-y-auto rounded-lg border border-border p-2">
              {items.map((item) => (
                <div
                  key={item.id}
                  className="flex items-center justify-between gap-2 rounded-md px-2 py-1.5 text-sm"
                >
                  <button
                    type="button"
                    className="min-w-0 flex-1 truncate text-left text-ink hover:text-primary"
                    onClick={() => setUpstreamModel(item.id)}
                    title="点击选中这个上游模型"
                  >
                    <span className="font-mono text-xs text-ink">{item.id}</span>
                    {item.name ? (
                      <span className="ml-2 text-xs text-ink-secondary">{item.name}</span>
                    ) : null}
                  </button>
                  <div className="flex shrink-0 items-center gap-1.5">
                    {item.context_window ? (
                      <span className="text-xs text-ink-muted">
                        上下文 {Math.round(item.context_window / 1000)}k
                      </span>
                    ) : null}
                    {item.platform_model ? (
                      <Badge variant="success">已映射 {item.platform_model}</Badge>
                    ) : item.registered ? (
                      <Badge variant="info">平台已登记</Badge>
                    ) : (
                      <Badge variant="warning">平台未登记</Badge>
                    )}
                  </div>
                </div>
              ))}
            </div>

            <p className="text-xs text-ink-muted">
              「平台未登记」表示上游有这个模型，但平台还没有对应的模型与价格：请先到「模型管理」新建，
              否则即使写了映射也无法对外提供。
            </p>
          </>
        )}
      </div>
    </Modal>
  )
}

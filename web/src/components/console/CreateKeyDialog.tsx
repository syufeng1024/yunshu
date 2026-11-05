import { useEffect, useMemo, useState } from 'react'
import { ApiError, errorHint, errorMessage } from '@/api/client'
import {
  KEY_BATCH_MAX,
  KEY_BATCH_MIN,
  useConsoleModels,
  useCreateKeys,
} from '@/api/console'
import type { CreatedKey, ModelScopeMode } from '@/api/console'
import { formatTokens } from '@/lib/format'
import { Button } from '../Button'
import { Input } from '../Input'
import { Modal } from '../Modal'
import { Select } from '../Select'
import { Skeleton } from '../Skeleton'

export interface CreateKeyDialogProps {
  open: boolean
  onClose: () => void
  /** Receives the plaintext batch; the caller opens KeySecretOnce. */
  onCreated: (items: CreatedKey[], warning: string) => void
}

function localDatetimeValue(date: Date): string {
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

/**
 * 新建密钥：一屏填完，与参考站一致（名称 + 范围 + 可选有效期）。
 *
 * 两次收敛（2026-11-05 需求方裁定）：
 * - 有效期不再强制：留空即「永不过期」，对应 K13 里 EXP=0；
 * - 来源地址白名单整段撤掉：它是给极少数固定出口的团队用的，对普通用户只是
 *   多一步要填的东西，参考站也没有。
 *
 * 仍然没有分组与额度字段：新密钥一律落在标准分组，分组变更由运营处理。
 */
export function CreateKeyDialog({ open, onClose, onCreated }: CreateKeyDialogProps) {
  const mutation = useCreateKeys()
  const modelsQuery = useConsoleModels(open)
  const [name, setName] = useState('')
  const [count, setCount] = useState(String(KEY_BATCH_MIN))
  const [scopeMode, setScopeMode] = useState<ModelScopeMode>('ALL')
  const [selectedModels, setSelectedModels] = useState<number[]>([])
  const [expireAt, setExpireAt] = useState('')
  const [modelFilter, setModelFilter] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [hint, setHint] = useState<string | null>(null)

  // Recomputed on every render so a dialog left open across midnight (or a
  // laptop sleep) never offers a min in the past.
  const minExpire = localDatetimeValue(new Date(Date.now() + 60_000))

  useEffect(() => {
    if (!open) return
    setName('')
    setCount(String(KEY_BATCH_MIN))
    setScopeMode('ALL')
    setSelectedModels([])
    setExpireAt('')
    setModelFilter('')
    setError(null)
    setHint(null)
    mutation.reset()
    // mutation.reset is stable; open is the only trigger we want.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  const models = modelsQuery.data ?? []
  const filteredModels = useMemo(() => {
    const keyword = modelFilter.trim().toLowerCase()
    if (!keyword) return models
    return models.filter(
      (model) =>
        model.model_name.toLowerCase().includes(keyword) ||
        model.display_name.toLowerCase().includes(keyword) ||
        model.aliases.some((alias) => alias.toLowerCase().includes(keyword)),
    )
  }, [modelFilter, models])

  if (!open) return null

  const validate = (): string | null => {
    const trimmed = name.trim()
    if (!trimmed) return '请输入密钥名称。'
    if (Array.from(trimmed).length > 64) return '密钥名称最多 64 个字符。'
    const parsed = Number(count)
    if (!Number.isInteger(parsed) || parsed < KEY_BATCH_MIN || parsed > KEY_BATCH_MAX) {
      return `创建数量需为 ${KEY_BATCH_MIN}–${KEY_BATCH_MAX} 的整数。`
    }
    if (scopeMode === 'SELECTED' && selectedModels.length === 0) {
      return '请至少选择一个模型，或改为「全部已上架模型」。'
    }
    // 有效期为空是合法的：不设到期时间即永不过期。
    if (expireAt && new Date(expireAt).getTime() <= Date.now()) {
      return '到期时间必须晚于当前时间。'
    }
    return null
  }

  const toggleModel = (id: number) => {
    setSelectedModels((current) =>
      current.includes(id) ? current.filter((item) => item !== id) : [...current, id],
    )
  }

  const changeScope = (mode: ModelScopeMode) => {
    setScopeMode(mode)
    if (mode === 'ALL') setSelectedModels([])
  }

  const submit = async () => {
    const message = validate()
    if (message) {
      setError(message)
      return
    }
    setError(null)
    setHint(null)

    try {
      const response = await mutation.mutateAsync({
        name: name.trim(),
        count: Number(count),
        model_scope_mode: scopeMode,
        ...(scopeMode === 'SELECTED' ? { model_ids: selectedModels } : {}),
        // 留空即「永不过期」：不传这个字段，后端会写成空值。
        ...(expireAt ? { expire_at: new Date(expireAt).toISOString() } : {}),
      })
      onCreated(response.items, response.warning)
      onClose()
    } catch (err) {
      setError(errorMessage(err))
      setHint(errorHint(err))
      // The free-tier ceiling is a state conflict, not a typo: keep the dialog
      // open so the copy (which names the ceiling) stays visible.
      if (err instanceof ApiError && err.code === 'key_limit_reached') {
        setError(errorMessage(err))
      }
    }
  }

  return (
    <Modal
      open={open}
      onClose={mutation.isPending ? () => undefined : onClose}
      title="新建密钥"
      description="新密钥默认使用标准分组；分组变更需联系运营。"
      size="lg"
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={mutation.isPending}>
            取消
          </Button>
          <Button onClick={() => void submit()} loading={mutation.isPending}>
            生成密钥
          </Button>
        </>
      }
    >
      <form
        id="create-key-form"
        onSubmit={(event) => {
          event.preventDefault()
          void submit()
        }}
        className="space-y-4"
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Input
            label="密钥名称"
            required
            value={name}
            maxLength={64}
            onChange={(event) => setName(event.target.value)}
            placeholder="例如：生产环境、测试环境"
            hint="最多 64 个字符，用于区分不同环境或用途。"
          />
          <Input
            label="创建数量"
            required
            type="number"
            min={KEY_BATCH_MIN}
            max={KEY_BATCH_MAX}
            step={1}
            value={count}
            onChange={(event) => setCount(event.target.value)}
            hint={`一次可创建 ${KEY_BATCH_MIN}–${KEY_BATCH_MAX} 个，权限与有效期相同。`}
          />
        </div>

        <Select
          label="可调用的模型"
          value={scopeMode}
          onChange={(event) => changeScope(event.target.value as ModelScopeMode)}
          options={[
            { value: 'ALL', label: '全部已上架模型' },
            { value: 'SELECTED', label: '仅指定模型' },
          ]}
        />

        {scopeMode === 'SELECTED' ? (
          <div className="space-y-2">
            <Input
              aria-label="筛选模型"
              placeholder="按模型名或别名筛选"
              value={modelFilter}
              onChange={(event) => setModelFilter(event.target.value)}
            />

            {modelsQuery.isLoading ? (
              <div className="space-y-2" aria-busy="true">
                <Skeleton className="h-8 w-full" />
                <Skeleton className="h-8 w-full" />
                <Skeleton className="h-8 w-3/4" />
              </div>
            ) : modelsQuery.isError ? (
              <div role="alert" className="rounded-lg border border-danger bg-danger-soft px-3 py-2 text-xs text-danger">
                {errorMessage(modelsQuery.error)}
                <Button
                  variant="ghost"
                  size="sm"
                  className="ml-2"
                  onClick={() => void modelsQuery.refetch()}
                >
                  重试
                </Button>
              </div>
            ) : filteredModels.length === 0 ? (
              <p className="px-1 text-xs text-ink-muted">没有匹配的模型。</p>
            ) : (
              <ul className="max-h-56 space-y-1 overflow-y-auto rounded-lg border border-border p-2">
                {filteredModels.map((model) => (
                  <li key={model.id}>
                    <label className="flex cursor-pointer items-start gap-2 rounded-md px-2 py-1.5 hover:bg-surface-muted">
                      <input
                        type="checkbox"
                        className="mt-1 h-4 w-4 accent-[var(--primary)]"
                        checked={selectedModels.includes(model.id)}
                        onChange={() => toggleModel(model.id)}
                      />
                      <span className="min-w-0">
                        <span className="block truncate text-sm text-ink">
                          {model.display_name}
                        </span>
                        <span className="block truncate text-xs text-ink-muted">
                          {model.model_name} · {formatTokens(model.context_length)}
                        </span>
                      </span>
                    </label>
                  </li>
                ))}
              </ul>
            )}
            <p className="text-xs text-ink-muted">已选择 {selectedModels.length} 个模型。</p>
          </div>
        ) : null}

        <Input
          label="到期时间（可选）"
          type="datetime-local"
          min={minExpire}
          value={expireAt}
          onChange={(event) => setExpireAt(event.target.value)}
          hint="留空表示永不过期；填写后到期自动失效，且无法恢复。"
        />

        {error ? (
          <div role="alert" className="rounded-lg border border-danger bg-danger-soft px-3 py-2 text-xs">
            <p className="text-danger">{error}</p>
            {hint ? <p className="mt-0.5 text-ink-secondary">{hint}</p> : null}
          </div>
        ) : null}
      </form>
    </Modal>
  )
}

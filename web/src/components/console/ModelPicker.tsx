import { useEffect, useMemo, useRef, useState } from 'react'
import { cn } from '@/lib/cn'

interface ModelOption {
  value: string
  label: string
}

interface ModelPickerProps {
  label: string
  value: string
  onChange: (value: string) => void
  options: readonly ModelOption[]
  placeholder?: string
  disabled?: boolean
}

/**
 * 可搜索的模型选择器。
 *
 * 调试台原来用一个原生下拉：模型一多（几十上百个）就只能靠滚。输入框 + 过滤列表
 * 让「知道模型名」的人一次就能定位，同时保留完整列表给「不知道叫什么」的人。
 *
 * 输入框的值就是当前选中的调用名，所以外部（测试、代码生成）读 value 的语义不变。
 */
export function ModelPicker({
  label,
  value,
  onChange,
  options,
  placeholder = '搜索模型',
  disabled = false,
}: ModelPickerProps) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const containerRef = useRef<HTMLDivElement | null>(null)

  // 关闭外部点击：没有这个，点别处列表还挂着，遮住下面的对话区。
  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: MouseEvent) => {
      if (!containerRef.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onPointerDown)
    return () => document.removeEventListener('mousedown', onPointerDown)
  }, [open])

  const visible = useMemo(() => {
    const keyword = query.trim().toLowerCase()
    if (!keyword) return options
    return options.filter(
      (option) =>
        option.value.toLowerCase().includes(keyword) ||
        option.label.toLowerCase().includes(keyword),
    )
  }, [options, query])

  const pick = (next: string) => {
    onChange(next)
    setQuery('')
    setOpen(false)
  }

  return (
    <div ref={containerRef} className="relative w-full space-y-1.5">
      <label className="text-xs font-medium text-ink-secondary" htmlFor="playground-model">
        {label}
      </label>
      <input
        id="playground-model"
        type="text"
        role="combobox"
        aria-expanded={open}
        aria-controls="playground-model-list"
        aria-autocomplete="list"
        autoComplete="off"
        disabled={disabled}
        value={open ? query : value}
        placeholder={placeholder}
        onFocus={() => setOpen(true)}
        onChange={(event) => {
          setQuery(event.target.value)
          setOpen(true)
        }}
        onKeyDown={(event) => {
          if (event.key === 'Escape') {
            setOpen(false)
            setQuery('')
          }
          if (event.key === 'Enter' && open) {
            const first = visible[0]
            if (first) {
              event.preventDefault()
              pick(first.value)
            }
          }
        }}
        className="h-10 w-full rounded-lg border border-border bg-surface px-3 text-sm text-ink placeholder:text-ink-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary disabled:cursor-not-allowed disabled:bg-surface-muted"
      />
      {open ? (
        <ul
          id="playground-model-list"
          role="listbox"
          aria-label="可选模型"
          className="absolute z-20 mt-1 max-h-64 w-full overflow-y-auto rounded-lg border border-border bg-surface p-1 shadow-card"
        >
          {visible.length === 0 ? (
            <li className="px-3 py-2 text-xs text-ink-muted">没有匹配的模型</li>
          ) : (
            visible.map((option) => (
              <li key={option.value}>
                <button
                  type="button"
                  role="option"
                  aria-selected={option.value === value}
                  onClick={() => pick(option.value)}
                  className={cn(
                    'flex w-full items-baseline justify-between gap-2 rounded-md px-3 py-2 text-left text-sm hover:bg-surface-muted',
                    option.value === value && 'bg-surface-muted font-medium text-ink',
                  )}
                >
                  <span className="truncate font-mono text-xs text-ink">{option.value}</span>
                  <span className="shrink-0 truncate text-xs text-ink-muted">{option.label}</span>
                </button>
              </li>
            ))
          )}
        </ul>
      ) : null}
    </div>
  )
}

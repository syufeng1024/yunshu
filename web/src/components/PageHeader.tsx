import type { ReactNode } from 'react'
import { cn } from '@/lib/cn'

export interface PageHeaderProps {
  title: string
  description?: string
  actions?: ReactNode
  className?: string
}

export function PageHeader({ title, description, actions, className }: PageHeaderProps) {
  return (
    <div
      className={cn(
        'flex flex-col gap-3 border-b border-border pb-5 sm:flex-row sm:items-end sm:justify-between',
        className,
      )}
    >
      <div className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight text-ink">{title}</h1>
        {description ? <p className="text-sm text-ink-secondary">{description}</p> : null}
      </div>
      {/*
        操作区禁止收缩：描述文字长的时候，两个按钮会被挤到换行、宽度不一还会左右
        错位（/admin/users 上「新建运营账号 / 审计记录」就是这么竖起来的）。
        空间不够时应该让描述换行，而不是把按钮挤成一列。
      */}
      {actions ? <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div> : null}
    </div>
  )
}

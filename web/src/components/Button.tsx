import { forwardRef } from 'react'
import type { ButtonHTMLAttributes, ReactNode } from 'react'
import { cn } from '@/lib/cn'

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'
export type ButtonSize = 'sm' | 'md' | 'lg'

const BASE =
  // 禁用态不能只靠降透明度：实心主按钮在 60% 不透明度下仍是鲜明的紫色，看着像能点
  // （系统管理页的「保存更改」在没有改动时就是这个样子）。抽掉饱和度把它压成灰面，
  // 语义才和「点不动」对上；secondary / ghost 本来就没有彩色底，不受影响。
  'inline-flex items-center justify-center gap-2 rounded-lg font-medium transition-colors duration-fast focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-60 disabled:saturate-0'

const VARIANTS: Record<ButtonVariant, string> = {
  primary: 'bg-primary text-ink-inverse hover:bg-primary-hover active:bg-primary-strong',
  secondary:
    'border border-border-strong bg-surface text-ink hover:bg-surface-muted active:bg-canvas',
  ghost: 'text-ink-secondary hover:bg-surface-muted hover:text-ink',
  danger: 'bg-danger text-ink-inverse hover:bg-danger-hover',
}

const SIZES: Record<ButtonSize, string> = {
  sm: 'h-8 px-3 text-xs',
  md: 'h-10 px-4 text-sm',
  lg: 'h-11 px-5 text-sm',
}

export function buttonVariants(options: { variant?: ButtonVariant; size?: ButtonSize } = {}): string {
  const { variant = 'primary', size = 'md' } = options
  return cn(BASE, VARIANTS[variant], SIZES[size])
}

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  size?: ButtonSize
  loading?: boolean
  leadingIcon?: ReactNode
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  {
    variant = 'primary',
    size = 'md',
    loading = false,
    leadingIcon,
    className,
    children,
    disabled,
    type = 'button',
    ...rest
  },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      className={cn(buttonVariants({ variant, size }), className)}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      {...rest}
    >
      {loading ? (
        <svg
          className="h-4 w-4 animate-spin"
          viewBox="0 0 24 24"
          fill="none"
          aria-hidden="true"
        >
          <circle cx="12" cy="12" r="9" stroke="currentColor" strokeOpacity="0.25" strokeWidth="3" />
          <path d="M21 12a9 9 0 0 0-9-9" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
        </svg>
      ) : (
        leadingIcon
      )}
      {children}
    </button>
  )
})

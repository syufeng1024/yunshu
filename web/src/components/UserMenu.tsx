import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Icon } from './Icon'
import { ROUTES, USER_ROLE_LABEL } from '@/lib/constants'
import type { IconName } from '@/lib/constants'
import { cn } from '@/lib/cn'
import { useAuthStore } from '@/stores/auth'

/**
 * 右上角的账号菜单。
 *
 * 原来账号与「退出登录」钉在侧栏最底部：那里的可点区域小、离内容远，而且侧栏
 * 一旦变长就得滚到底才能退出。收到顶栏右上角是这类产品的通用位置——它属于
 * 「当前登录的人」，不属于任何一组菜单。
 *
 * 这里曾经还有「运营后台」/「返回控制台」两项，用来在两个独立的后台壳之间互跳。
 * 现在 `/console` 与 `/admin` 共用同一个侧栏（layouts/AppShell.tsx），管理员入口
 * 就在侧栏的「管理员」分组里，互跳项已无意义，连同 `scope` 属性一并删掉——
 * 「当前在哪一端」由侧栏的选中态回答，菜单不必再自称位置。
 */
export function UserMenu() {
  const user = useAuthStore((state) => state.user)
  const logout = useAuthStore((state) => state.logout)
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)
  const containerRef = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: MouseEvent) => {
      if (!containerRef.current?.contains(event.target as Node)) setOpen(false)
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onPointerDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onPointerDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open])

  if (!user) return null

  const roleLabel = USER_ROLE_LABEL[user.role]
  const displayName = user.display_name || user.username || user.email
  const initial = (displayName ?? '').trim().charAt(0).toUpperCase() || '?'

  // 昵称叫「超级管理员」的内置账号，名字与角色名一模一样，两行并排就是重复。
  // 这种情况下副标题换成登录名，信息不丢也不再复读。
  const subtitle =
    displayName === roleLabel && user.username ? `@${user.username}` : roleLabel

  // 账号菜单只放「属于这个人」的三项（资料 / 订阅 / 钱包）。这些入口在侧栏的
  // 「个人」分组里也有：菜单是快捷入口，侧栏是完整导航，两处不冲突——但名字必须
  // 一致，否则同一目的地出现两套叫法，用户会以为是两个页面。
  const items: { to: string; icon: IconName; label: string }[] = [
    { to: ROUTES.consoleAccount, icon: 'users', label: '账户设置' },
    { to: ROUTES.consoleSubscription, icon: 'tag', label: '我的订阅' },
    { to: ROUTES.consoleRecharge, icon: 'wallet', label: '充值中心' },
  ]

  const handleLogout = async () => {
    setOpen(false)
    await logout()
    navigate(ROUTES.login, { replace: true })
  }

  return (
    <div ref={containerRef} className="relative">
      <button
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="账号菜单"
        onClick={() => setOpen((value) => !value)}
        className="flex items-center gap-2 rounded-full border border-border bg-surface py-1 pl-1 pr-2.5 transition-colors duration-fast hover:bg-surface-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
      >
        <span
          aria-hidden="true"
          className="flex h-7 w-7 items-center justify-center rounded-full bg-primary text-xs font-semibold text-ink-inverse"
        >
          {initial}
        </span>
        <Icon
          name="chevron-down"
          className={cn('h-3.5 w-3.5 text-ink-muted transition-transform', open && 'rotate-180')}
        />
      </button>

      {open ? (
        <div
          role="menu"
          aria-label="账号菜单"
          className="absolute right-0 z-40 mt-2 w-60 overflow-hidden rounded-xl border border-border bg-surface shadow-card"
        >
          <div className="flex items-center gap-3 border-b border-border px-4 py-3">
            <span
              aria-hidden="true"
              className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-primary text-sm font-semibold text-ink-inverse"
            >
              {initial}
            </span>
            <div className="min-w-0">
              <p className="truncate text-sm font-medium text-ink">{displayName}</p>
              <p className="truncate text-xs text-ink-muted">{subtitle}</p>
            </div>
          </div>

          <div className="p-1">
            {items.map((item) => (
              <Link
                key={item.to}
                to={item.to}
                role="menuitem"
                onClick={() => setOpen(false)}
                className="flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm text-ink-secondary transition-colors duration-fast hover:bg-surface-muted hover:text-ink"
              >
                <Icon name={item.icon} className="h-4 w-4" />
                {item.label}
              </Link>
            ))}
          </div>

          <div className="border-t border-border p-1">
            <button
              type="button"
              role="menuitem"
              onClick={() => void handleLogout()}
              className="flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-left text-sm text-danger transition-colors duration-fast hover:bg-danger-soft"
            >
              <Icon name="logout" className="h-4 w-4" />
              退出登录
            </button>
          </div>
        </div>
      ) : null}
    </div>
  )
}

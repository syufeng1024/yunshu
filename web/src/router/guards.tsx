import { useEffect } from 'react'
import type { ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { EmptyState } from '@/components/EmptyState'
import { ErrorState } from '@/components/ErrorState'
import { Skeleton } from '@/components/Skeleton'
import { ROUTES, isAdminRole } from '@/lib/constants'
import type { UserRole } from '@/lib/constants'
import { usePageTitle } from '@/lib/usePageTitle'
import { useAuthStore } from '@/stores/auth'

export function RouteLoading({ fullScreen = false }: { fullScreen?: boolean }) {
  return (
    <div
      className={
        fullScreen
          ? 'flex min-h-screen items-center justify-center bg-canvas p-6'
          : 'flex min-h-48 items-center justify-center p-6'
      }
      aria-busy="true"
      aria-label="页面加载中"
    >
      <div className="w-full max-w-sm space-y-3">
        <Skeleton className="h-6 w-40" />
        <Skeleton className="h-4 w-full" />
        <Skeleton className="h-4 w-2/3" />
      </div>
    </div>
  )
}

export function ForbiddenPage() {
  usePageTitle('无权限')
  return (
    <div className="flex min-h-screen items-center justify-center bg-canvas p-6">
      <div className="w-full max-w-md space-y-4">
        <EmptyState
          title="403 · 无权限"
          description="当前账号无权访问该区域。前端菜单隐藏只做体验，服务端仍会对每个接口校验权限。"
          icon={<span className="text-sm font-semibold text-ink-secondary">403</span>}
        />
        <p className="text-center text-xs text-ink-muted">
          该区域按角色开放，当前账号无权访问本页。
        </p>
      </div>
    </div>
  )
}

export interface RequireAuthProps {
  role: 'user' | 'admin'
  /**
   * 在 `role` 已放行之后的**二次收窄**，用于「运营后台里只有部分角色能进」的单页。
   *
   * 语义：仅当 `role` 校验通过后才生效；不传时行为与加这个属性之前完全一致
   * （既有路由一条都没传，因此不受影响）。传空数组表示谁都不放行——守卫按
   * fail-closed 处理，免得以后有人用 `roles={[]}` 表达「不限」时误开权限。
   */
  roles?: readonly UserRole[]
  children: ReactNode
}

/**
 * UX-only route guard. It redirects anonymous visitors to /login and renders
 * a 403 shell for the wrong role; every page must still validate permissions
 * against the backend when its data loads (PRD §1.3, hard constraint 8).
 */
export function RequireAuth({ role, roles, children }: RequireAuthProps) {
  const location = useLocation()
  const status = useAuthStore((state) => state.status)
  const user = useAuthStore((state) => state.user)
  const error = useAuthStore((state) => state.error)
  const hint = useAuthStore((state) => state.hint)
  const bootstrap = useAuthStore((state) => state.bootstrap)

  useEffect(() => {
    if (status === 'unknown') {
      void bootstrap()
    }
  }, [bootstrap, status])

  if (status === 'unknown' || status === 'loading') {
    return <RouteLoading fullScreen />
  }

  if (status === 'error') {
    return (
      <div className="flex min-h-screen items-center justify-center bg-canvas p-6">
        <div className="w-full max-w-md">
          <ErrorState
            message={error ?? '登录状态校验失败，请重试。'}
            hint={hint}
            onRetry={() => {
              void bootstrap()
            }}
          />
        </div>
      </div>
    )
  }

  if (status === 'anonymous') {
    const next = encodeURIComponent(`${location.pathname}${location.search}`)
    return <Navigate to={`${ROUTES.login}?next=${next}`} replace />
  }

  if (role === 'admin' && !isAdminRole(user?.role)) {
    return <ForbiddenPage />
  }

  // 单页角色收窄：只有在 `role` 已通过、且调用方显式传了 `roles` 时才判定。
  if (roles !== undefined && (user === null || !roles.includes(user.role))) {
    return <ForbiddenPage />
  }

  return <>{children}</>
}

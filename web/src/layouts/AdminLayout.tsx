import { Link, NavLink, Outlet, useNavigate } from 'react-router-dom'
import { Badge } from '@/components/Badge'
import { Icon } from '@/components/Icon'
import { Logo } from '@/components/Logo'
import { ScrollRestorer } from '@/components/ScrollRestorer'
import { ADMIN_NAV_GROUPS, ROUTES, USER_ROLE_LABEL } from '@/lib/constants'
import { cn } from '@/lib/cn'
import { useAuthStore } from '@/stores/auth'

function navClass({ isActive }: { isActive: boolean }): string {
  return cn(
    'flex items-center gap-3 rounded-lg px-3 py-2 text-sm transition-colors duration-fast focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary',
    isActive
      ? 'bg-sidebar-active font-semibold text-sidebar-text'
      : 'font-medium text-sidebar-muted hover:bg-sidebar-hover hover:text-sidebar-text',
  )
}

export function AdminLayout() {
  const user = useAuthStore((state) => state.user)
  const logout = useAuthStore((state) => state.logout)
  const navigate = useNavigate()
  // 按角色过滤后为空的组直接不渲染：FINANCE 看不到「运营」组时，留一个只有
  // 标题的空段落比不显示更让人困惑。
  const visibleGroups = ADMIN_NAV_GROUPS.map((group) => ({
    title: group.title,
    items: group.items.filter((item) => user !== null && item.roles.includes(user.role)),
  })).filter((group) => group.items.length > 0)

  const handleLogout = async () => {
    await logout()
    navigate(ROUTES.login, { replace: true })
  }

  return (
    <div className="min-h-screen bg-canvas md:flex">
      <ScrollRestorer />
      <aside className="sticky top-0 z-30 flex w-full shrink-0 flex-row items-center gap-2 bg-sidebar-bg px-3 py-2 md:h-screen md:border-r md:border-sidebar-border md:w-sidebar md:flex-col md:items-stretch md:gap-5 md:px-4 md:py-5">
        <div className="flex items-center justify-between gap-2 md:justify-start">
          <Logo size="sm" collapsible to={ROUTES.admin} />
          {user ? (
            <span className="hidden md:inline-flex">
              <Badge variant="info">{USER_ROLE_LABEL[user.role]}</Badge>
            </span>
          ) : null}
        </div>

        <nav
          aria-label="运营后台导航"
          className="flex flex-1 items-center gap-1 overflow-x-auto md:mt-1 md:flex-col md:items-stretch md:gap-2 md:overflow-visible"
        >
          {visibleGroups.map((group) => (
            <div key={group.title} className="flex items-center gap-1 md:block">
              <p className="hidden px-3 pb-1 text-xs font-medium text-ink-muted md:block">
                {group.title}
              </p>
              <div className="flex items-center gap-1 md:flex-col md:items-stretch md:gap-1">
                {group.items.map((item) => (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    end={item.end}
                    className={navClass}
                    title={item.label}
                    aria-label={item.label}
                  >
                    <Icon name={item.icon} className="h-5 w-5" />
                    <span className="hidden whitespace-nowrap md:inline">{item.label}</span>
                  </NavLink>
                ))}
              </div>
            </div>
          ))}
        </nav>

        <Link
          to={ROUTES.home}
          className="inline-flex items-center gap-2 rounded-md px-2 py-2 text-xs text-sidebar-muted transition-colors duration-fast hover:bg-sidebar-hover hover:text-sidebar-text focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary md:hidden"
          aria-label="返回官网首页"
        >
          <Icon name="home" className="h-5 w-5" />
        </Link>

        <button
          type="button"
          onClick={handleLogout}
          className="inline-flex items-center gap-2 rounded-md px-2 py-2 text-xs text-sidebar-muted transition-colors duration-fast hover:bg-sidebar-hover hover:text-sidebar-text focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary md:hidden"
          aria-label="退出登录"
        >
          <Icon name="logout" className="h-5 w-5" />
        </button>

        <div className="mt-auto hidden border-t border-sidebar-border pt-4 md:block">
          <p className="truncate text-xs text-sidebar-muted">{user?.email ?? ''}</p>
          {/* 运营后台同样没有回官网的出口：Logo 指向后台自身。补一个显式入口，
              方便运营在核价/看公告时直接对照官网页面。 */}
          <Link
            to={ROUTES.home}
            className="mt-2 inline-flex items-center gap-2 rounded-md px-1 py-1 text-xs text-sidebar-muted transition-colors duration-fast hover:text-sidebar-text focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
          >
            <Icon name="home" className="h-4 w-4" />
            返回官网
          </Link>
          <button
            type="button"
            onClick={handleLogout}
            className="mt-2 inline-flex items-center gap-2 rounded-md px-1 py-1 text-xs text-sidebar-muted transition-colors duration-fast hover:text-sidebar-text focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
          >
            <Icon name="logout" className="h-4 w-4" />
            退出登录
          </button>
        </div>
      </aside>

      <div className="min-w-0 flex-1">
        <main className="mx-auto w-full max-w-6xl px-4 py-6 sm:px-6 lg:px-8">
          <Outlet />
        </main>
      </div>
    </div>
  )
}

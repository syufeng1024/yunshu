import { Link, NavLink, Outlet, useNavigate } from 'react-router-dom'
import { Icon } from '@/components/Icon'
import { Logo } from '@/components/Logo'
import { UserMenu } from '@/components/UserMenu'
import { ScrollRestorer } from '@/components/ScrollRestorer'
import { CONSOLE_NAV_GROUPS, ROUTES, SITE_NAV } from '@/lib/constants'
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

/**
 * 控制台外壳。
 *
 * 布局要点：外层 `h-screen overflow-hidden`，侧栏与内容区各自滚动（侧栏
 * `md:overflow-y-auto`，内容区 `flex-1 overflow-y-auto`）。原来是整页滚动，
 * 菜单跟着长表格一起走——翻到一半想换页面得先把滚动条拉回顶部。
 *
 * 顶栏放**站点导航**（首页/定价/模型价格/文档/工具集成）。这里原本钉着一条
 * 「调用地址」，占了首屏最贵的一行，而那个地址在密钥详情与调试台的代码生成里
 * 都有，不需要常驻。
 */
export function ConsoleLayout() {
  const logout = useAuthStore((state) => state.logout)
  const navigate = useNavigate()

  const handleLogout = async () => {
    await logout()
    navigate(ROUTES.login, { replace: true })
  }

  return (
    <div className="flex h-screen flex-col overflow-hidden bg-canvas md:flex-row">
      <ScrollRestorer />
      <aside className="flex shrink-0 flex-row items-center gap-2 bg-sidebar-bg px-3 py-2 md:h-full md:w-sidebar md:flex-col md:items-stretch md:gap-6 md:overflow-y-auto md:border-r md:border-sidebar-border md:px-4 md:py-5">
        <div className="flex items-center justify-between md:justify-start">
          <Logo size="sm" collapsible to={ROUTES.console} />
        </div>

        <nav
          aria-label="控制台导航"
          className="flex flex-1 items-center gap-1 overflow-x-auto md:mt-2 md:flex-col md:items-stretch md:gap-2 md:overflow-visible"
        >
          {CONSOLE_NAV_GROUPS.map((group) => (
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

      </aside>

      <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
        <header className="hidden shrink-0 items-center gap-6 border-b border-border bg-surface px-6 md:flex">
          <nav aria-label="站点导航" className="flex h-14 items-center gap-5 text-sm">
            {SITE_NAV.map((item) => (
              <Link
                key={item.to}
                to={item.to}
                className="text-ink-secondary transition-colors duration-fast hover:text-ink"
              >
                {item.label}
              </Link>
            ))}
          </nav>
          <div className="ml-auto">
            <UserMenu scope="console" />
          </div>
        </header>

        <main className="flex-1 overflow-y-auto">
          <div className="mx-auto w-full max-w-6xl px-4 py-6 sm:px-6 lg:px-8">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  )
}

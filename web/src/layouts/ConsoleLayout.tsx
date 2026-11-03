import { useEffect, useRef, useState } from 'react'
import { Link, NavLink, Outlet, useNavigate } from 'react-router-dom'
import { Icon } from '@/components/Icon'
import { Logo } from '@/components/Logo'
import { ScrollRestorer } from '@/components/ScrollRestorer'
import { resolveGatewayBaseUrl } from '@/lib/apiBase'
import { writeClipboard } from '@/lib/clipboard'
import { CONSOLE_NAV_GROUPS, ROUTES } from '@/lib/constants'
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
 * 常驻的「调用地址」条。
 *
 * 控制台里任何页面都看不到自己的网关地址，用户要写代码只能去猜或翻文档，
 * 所以这里把它钉在内容区最上方并支持一键复制。
 *
 * 地址来源：`VITE_API_BASE_URL`（构建期注入，生产必须注入，见 OI-12 / G9）；
 * 未注入时回落到站点自身域名（本地/容器同源部署即如此）。**不硬编码任何域名**，
 * 否则换环境就会把流量指到别人的服务器。
 */
function ApiBaseBar() {
  const gatewayUrl = resolveGatewayBaseUrl()
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)
  const resetTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    return () => {
      if (resetTimer.current) clearTimeout(resetTimer.current)
    }
  }, [])

  if (!gatewayUrl) return null

  const handleCopy = async () => {
    try {
      await writeClipboard(gatewayUrl)
      setFailed(false)
      setCopied(true)
      if (resetTimer.current) clearTimeout(resetTimer.current)
      resetTimer.current = setTimeout(() => setCopied(false), 2_000)
    } catch {
      setCopied(false)
      setFailed(true)
    }
  }

  return (
    <div className="border-b border-border bg-surface">
      <div className="mx-auto flex w-full max-w-6xl flex-wrap items-center gap-x-2 gap-y-1 px-4 py-1.5 sm:px-6 sm:py-2 lg:px-8">
        <span className="text-xs font-medium text-ink-muted">调用地址</span>
        <code className="min-w-0 flex-1 truncate font-mono text-xs text-ink">{gatewayUrl}</code>
        <button
          type="button"
          onClick={() => void handleCopy()}
          className="inline-flex shrink-0 items-center gap-1 rounded-sm px-2 py-1 text-xs text-ink-secondary transition-colors duration-fast hover:bg-surface-muted hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
          aria-label={copied ? '已复制调用地址' : '复制调用地址'}
        >
          <Icon name={copied ? 'check' : 'copy'} className="h-3.5 w-3.5" />
          {copied ? '已复制' : '复制'}
        </button>
        {failed ? (
          <span role="alert" className="text-xs text-danger">
            复制失败，请手动选择地址复制。
          </span>
        ) : null}
      </div>
    </div>
  )
}

export function ConsoleLayout() {
  const user = useAuthStore((state) => state.user)
  const logout = useAuthStore((state) => state.logout)
  const navigate = useNavigate()

  const handleLogout = async () => {
    await logout()
    navigate(ROUTES.login, { replace: true })
  }

  return (
    <div className="min-h-screen bg-canvas md:flex">
      <ScrollRestorer />
      <aside className="sticky top-0 z-30 flex w-full shrink-0 flex-row items-center gap-2 bg-sidebar-bg px-3 py-2 md:h-screen md:border-r md:border-sidebar-border md:w-sidebar md:flex-col md:items-stretch md:gap-6 md:px-4 md:py-5">
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

        <div className="mt-auto hidden border-t border-sidebar-border pt-4 md:block">
          <p className="truncate text-xs text-sidebar-muted">{user?.email ?? ''}</p>
          {/* 登录后 Logo 指向控制台自身，控制台里没有任何出口通向官网：
              用户想回首页看价格/文档只能改地址栏。这里补一个显式入口。 */}
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
        {/* 内容区最上方常驻调用地址：运营后台不加（运营不用自己的 Key 调模型）。 */}
        <ApiBaseBar />
        <main className="mx-auto w-full max-w-6xl px-4 py-6 sm:px-6 lg:px-8">
          {/* The announcement strip lives on the overview (T19): the ops
              announcement API is not exposed to console users yet, so it
              renders an explicit empty state there instead of sample copy. */}
          <Outlet />
        </main>
      </div>
    </div>
  )
}

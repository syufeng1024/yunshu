import { Link, NavLink, Outlet, useNavigate } from 'react-router-dom'
import { Badge } from '@/components/Badge'
import { Icon } from '@/components/Icon'
import { Logo } from '@/components/Logo'
import { ScrollRestorer } from '@/components/ScrollRestorer'
import { UserMenu } from '@/components/UserMenu'
import { ROUTES, SITE_NAV, USER_ROLE_LABEL, visibleShellNavGroups } from '@/lib/constants'
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
 * 登录后的唯一外壳，`/console/*` 与 `/admin/*` 两个路由分支共用它。
 *
 * 信息架构：一个侧栏、三个分组（常规 / 个人 / 管理员，见 constants.ts 的
 * SHELL_NAV_GROUPS）。以前是「控制台壳 + 运营后台壳」两套侧栏、两套分组口径，
 * 超管要跨两个后台来回跳；现在个人功能与管理员功能各归其组，用角色过滤解决
 * 「我不该看见这一组」。两个分支共用同一个组件，因此两边渲染结果不可能漂移。
 *
 * 布局要点：外层 `relative h-screen overflow-hidden`，侧栏与内容区各自滚动（侧栏
 * `md:overflow-y-auto`，内容区 `flex-1 overflow-y-auto`）。原来是整页滚动，
 * 菜单跟着长表格一起走——翻到一半想换页面得先把滚动条拉回顶部。运营页的表格
 * 比用户侧更长，这条尤其重要。
 *
 * `relative` 不是装饰：只写 `overflow-hidden` 时，内容区（overflow-y-auto）里超长
 * 页面的溢出仍会被算进根滚动容器的可滚动区域——实测密钥详情页 html.scrollHeight
 * 是 1376 而视口只有 529，于是窗口右侧多出一根滚动条，与内容区那根并排（2026-11-05
 * 需求方报「页面右侧出现两个滚动条」）。给根节点建立包含块后，溢出被真正裁在壳内，
 * 窗口不再可滚；也试过 html/body 加 overflow:hidden，都能绕过但不是正解，其它路由
 * （公开页）还要靠根滚动。fixed 定位的抽屉/弹窗不受影响：relative 不建立包含块
 * 给 fixed（只有 transform/filter/contain 之类会）。
 *
 * 顶栏放**站点导航**（首页/定价/模型价格/文档/工具集成）。这里原本钉着一条
 * 「调用地址」，占了首屏最贵的一行，而那个地址在密钥详情与调试台的代码生成里
 * 都有，不需要常驻。
 */
export function AppShell() {
  const user = useAuthStore((state) => state.user)
  const logout = useAuthStore((state) => state.logout)
  const navigate = useNavigate()
  const groups = visibleShellNavGroups(user?.role)

  const handleLogout = async () => {
    await logout()
    navigate(ROUTES.login, { replace: true })
  }

  return (
    <div className="relative flex h-screen flex-col overflow-hidden bg-canvas md:flex-row">
      <ScrollRestorer />
      {/* 侧栏在矮窗口（20 项菜单放不下）里会自己滚，滚动条收细一点：它和内容区
          的滚动条不是同一件事，两条粗滚动条并排会让人以为页面滚重了。 */}
      <aside className="flex shrink-0 flex-row items-center gap-2 bg-sidebar-bg px-3 py-2 [scrollbar-width:thin] md:h-full md:w-sidebar md:flex-col md:items-stretch md:gap-6 md:overflow-y-auto md:border-r md:border-sidebar-border md:px-4 md:py-5">
        <div className="flex items-center justify-between gap-2 md:justify-start">
          <Logo size="sm" collapsible to={ROUTES.console} />
          {user ? (
            <span className="hidden md:inline-flex">
              <Badge variant="info">{USER_ROLE_LABEL[user.role]}</Badge>
            </span>
          ) : null}
        </div>

        <nav
          aria-label="侧栏导航"
          className="flex flex-1 items-center gap-1 overflow-x-auto md:mt-2 md:flex-col md:items-stretch md:gap-2 md:overflow-visible"
        >
          {groups.map((group) => (
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
            <UserMenu />
          </div>
        </header>

        <main className="flex-1 overflow-y-auto">
          {/* 初始口令提醒：只提示、不拦截。账号仍使用开通时下发的口令时出现，
              改密成功后 password_warning 变 false，横幅自然消失。原来只挂在运营
              后台壳上——可改密码的入口就在「个人 › 账户设置」，人在控制台更需要
              这条提醒，所以合并后两边都显示。 */}
          {user?.password_warning ? (
            <div
              role="alert"
              className="border-b border-warning bg-warning-soft px-4 py-2 text-xs text-warning sm:px-6"
            >
              当前账号仍在使用初始密码，请尽快修改。
              <Link to={ROUTES.consoleAccount} className="ml-1 font-semibold underline">
                去修改密码
              </Link>
            </div>
          ) : null}
          {/* 两壳的容器宽度原本就都是 max-w-6xl（1152px），合并后沿用同一个值，
              不额外放宽：运营侧最宽的表格（渠道 / 用户 / 订单）在 6xl 里已经排得
              下，再宽只会拉长每行的扫视距离；真有超宽表格，横向滚动由 Table 自己
              承担，不该让整页跟着变宽。 */}
          <div className="mx-auto w-full max-w-6xl px-4 py-6 sm:px-6 lg:px-8">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  )
}

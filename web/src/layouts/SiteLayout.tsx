import { useEffect } from 'react'
import type { ReactNode } from 'react'
import { Link, NavLink, Outlet } from 'react-router-dom'
import { buttonVariants } from '@/components/Button'
import { Copyright } from '@/components/layout-parts'
import { Logo } from '@/components/Logo'
import { ScrollRestorer } from '@/components/ScrollRestorer'
import { SERVICE_TERMS_SECTION_ID } from '@/content/legal'
import { cn } from '@/lib/cn'
import { BRAND_DESCRIPTION, ROUTES, SITE_NAV } from '@/lib/constants'
import { PENDING_BEFORE_LAUNCH } from '@/lib/placeholders'
import { useAuthStore } from '@/stores/auth'

function navLinkClass({ isActive }: { isActive: boolean }): string {
  return cn(
    'rounded-md px-3 py-2 text-sm font-medium transition-colors duration-fast',
    isActive
      ? 'bg-primary-soft text-primary'
      : 'text-ink-secondary hover:bg-surface-muted hover:text-ink',
  )
}

function FooterGroup({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div>
      <h2 className="text-sm font-semibold text-ink">{title}</h2>
      <ul className="mt-3 space-y-2 text-sm text-ink-secondary">{children}</ul>
    </div>
  )
}

const PRODUCT_LINKS = [
  { label: '定价', to: ROUTES.pricing },
  { label: '工具集成', to: ROUTES.integrations },
  { label: '控制台', to: ROUTES.console },
  { label: '登录', to: ROUTES.login },
] as const

const DOC_LINKS = [
  { label: '开发者文档', to: ROUTES.docs },
  { label: '快速开始', to: `${ROUTES.docs}?chapter=quickstart` },
  { label: '错误码与限流', to: `${ROUTES.docs}?chapter=errors` },
  { label: '工具集成配置', to: `${ROUTES.docs}?chapter=tools` },
] as const

const AGREEMENT_LINKS = [
  { label: '用户协议', to: ROUTES.terms },
  { label: '隐私政策', to: ROUTES.privacy },
  // 服务条款是用户协议中的一节，锚点 id 定义在 src/content/legal.ts。
  { label: '服务条款', to: `${ROUTES.terms}#${SERVICE_TERMS_SECTION_ID}` },
] as const

export function SiteLayout() {
  const status = useAuthStore((state) => state.status)
  const user = useAuthStore((state) => state.user)
  const bootstrap = useAuthStore((state) => state.bootstrap)

  // Public pages have to learn who the visitor is too. Only RequireAuth called
  // bootstrap before, so arriving at "/" by URL — or reloading it — left the
  // store at "unknown" and the header showed 登录 / 免费开始 to a signed-in
  // user. That reads as "I got logged out" even though the session cookie was
  // fine (the first report of this bug was exactly that).
  useEffect(() => {
    if (status === 'unknown') void bootstrap()
  }, [status, bootstrap])

  const authenticated = status === 'authenticated'

  return (
    <div className="flex min-h-screen flex-col bg-canvas">
      <ScrollRestorer />
      <a href="#main-content" className="skip-link">
        跳到主内容
      </a>

      <header className="sticky top-0 z-30 border-b border-border bg-surface">
        <div className="mx-auto flex h-16 w-full max-w-6xl items-center justify-between gap-4 px-4 sm:px-6">
          <Logo />

          <nav aria-label="主导航" className="hidden items-center gap-1 md:flex">
            {SITE_NAV.map((item) => (
              <NavLink key={item.to} to={item.to} end={item.end} className={navLinkClass}>
                {item.label}
              </NavLink>
            ))}
          </nav>

          <div className="flex items-center gap-2">
            {authenticated ? (
              <>
                <span className="hidden max-w-[12rem] truncate text-xs text-ink-secondary sm:inline">
                  {user?.email ?? ''}
                </span>
                <Link
                  to={ROUTES.console}
                  className={buttonVariants({ variant: 'primary', size: 'sm' })}
                >
                  进入控制台
                </Link>
              </>
            ) : (
              <>
                <Link to={ROUTES.login} className={buttonVariants({ variant: 'ghost', size: 'sm' })}>
                  登录
                </Link>
                <Link
                  to={`${ROUTES.login}?tab=register`}
                  className={buttonVariants({ variant: 'primary', size: 'sm' })}
                >
                  免费开始
                </Link>
              </>
            )}
          </div>
        </div>

        <nav
          aria-label="移动端导航"
          className="flex gap-1 overflow-x-auto border-t border-border px-4 py-2 md:hidden"
        >
          {SITE_NAV.map((item) => (
            <NavLink key={item.to} to={item.to} end={item.end} className={navLinkClass}>
              {item.label}
            </NavLink>
          ))}
        </nav>
      </header>

      <main id="main-content" className="flex-1">
        <Outlet />
      </main>

      <footer className="site-footer border-t border-border bg-surface">
        <div className="mx-auto w-full max-w-6xl px-4 py-10 sm:px-6">
          <div className="grid gap-8 lg:grid-cols-[1.1fr_2fr]">
            <div className="space-y-2">
              <Logo />
              <p className="max-w-xs text-xs leading-relaxed text-ink-muted">{BRAND_DESCRIPTION}</p>
            </div>

            <div className="grid grid-cols-2 gap-6 sm:grid-cols-4">
              <FooterGroup title="产品">
                {/* 页脚的「登录」和 header 同理：已登录时留着它只会让人以为
                    会话没了。控制台入口本来就在同一组里，不需要替换。 */}
                {PRODUCT_LINKS.filter((item) => !(authenticated && item.label === '登录')).map((item) => (
                  <li key={item.label}>
                    <Link to={item.to} className="hover:text-ink">
                      {item.label}
                    </Link>
                  </li>
                ))}
              </FooterGroup>

              <FooterGroup title="文档">
                {DOC_LINKS.map((item) => (
                  <li key={item.label}>
                    <Link to={item.to} className="hover:text-ink">
                      {item.label}
                    </Link>
                  </li>
                ))}
              </FooterGroup>

              <FooterGroup title="协议">
                {AGREEMENT_LINKS.map((item) => (
                  <li key={item.label}>
                    <Link to={item.to} className="hover:text-ink">
                      {item.label}
                    </Link>
                  </li>
                ))}
              </FooterGroup>

              <FooterGroup title="备案信息">
                {/* 资质材料需求方尚未提供：只说明「待补什么」，不写任何具体
                    号码——编造 ICP / 许可证号等于伪造证照。 */}
                <li className="text-xs leading-relaxed text-ink-secondary">
                  证照与备案信息：{PENDING_BEFORE_LAUNCH}
                </li>
                <li className="text-xs leading-relaxed text-ink-muted">
                  （ICP 备案号 / 增值电信业务许可证 / 软件著作权）
                </li>
              </FooterGroup>
            </div>
          </div>

          <div className="mt-8 border-t border-border pt-4">
            <Copyright />
          </div>
        </div>
      </footer>
    </div>
  )
}

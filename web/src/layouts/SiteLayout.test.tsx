import { render, screen, within } from '@testing-library/react'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import { ROUTES, SOURCE_CODE_URL } from '@/lib/constants'
import { PENDING_BEFORE_LAUNCH } from '@/lib/placeholders'
import { useAuthStore } from '@/stores/auth'
import { SiteLayout } from './SiteLayout'

// SiteLayout renders <ScrollRestorer />, which requires a data router, so the
// harness uses createMemoryRouter rather than MemoryRouter.
function renderLayout() {
  const router = createMemoryRouter(
    [
      {
        element: <SiteLayout />,
        children: [{ index: true, element: <div>首页内容</div> }],
      },
    ],
    { initialEntries: ['/'] },
  )
  return render(<RouterProvider router={router} />)
}

/** 取页脚某个分组（h2 标题）下的 div 容器。 */
function footerGroup(name: string): HTMLElement {
  const heading = screen.getByRole('heading', { level: 2, name })
  const group = heading.closest('div')
  if (!group) throw new Error(`页脚分组「${name}」未找到`)
  return group as HTMLElement
}

describe('SiteLayout', () => {
  // Anonymous by default: "unknown" would make the layout bootstrap against a
  // backend that does not exist in this test file.
  beforeEach(() => {
    useAuthStore.setState({ status: 'anonymous', user: null, error: null, hint: null })
  })

  it('A23: footer carries product, docs, agreement and filing groups', () => {
    renderLayout()

    const headings = screen.getAllByRole('heading', { level: 2 }).map((node) => node.textContent)
    expect(headings).toEqual(expect.arrayContaining(['产品', '文档', '协议', '备案信息']))
  })

  it('A24: footer agreement group links terms / privacy / service-terms', () => {
    renderLayout()

    // 协议三项必须是真链接：用户协议 → /terms、隐私政策 → /privacy、
    // 服务条款 → /terms#service-terms（该节在 TermsPage 里带 id）。
    const links = Array.from(footerGroup('协议').querySelectorAll('a'))
    expect(links.map((link) => link.getAttribute('href'))).toEqual([
      '/terms',
      '/privacy',
      '/terms#service-terms',
    ])
    expect(links.map((link) => link.textContent)).toEqual(['用户协议', '隐私政策', '服务条款'])
  })

  it('A24: footer filing group states what is missing without inventing numbers', () => {
    renderLayout()

    const group = footerGroup('备案信息')
    // 资质由需求方另行提供：只能标注待补，不能填任何具体证号。
    expect(group.querySelectorAll('a')).toHaveLength(0)
    expect(group.textContent).toContain(PENDING_BEFORE_LAUNCH)
    expect(group.textContent).toContain('ICP 备案号')
    expect(group.textContent).toContain('增值电信业务许可证')
    expect(group.textContent).toContain('软件著作权')
    expect(group.textContent).not.toMatch(/ICP\s*备\s*\d/i)
  })

  it('keeps the brand link labelled and routes the header CTA to registration', () => {
    renderLayout()

    expect(screen.getAllByRole('link', { name: '云枢 API 首页' })).toHaveLength(2)
    expect(screen.getByRole('link', { name: '免费开始' })).toHaveAttribute(
      'href',
      '/login?tab=register',
    )
  })

  // Regression: the header used to be static, so a signed-in visitor who went
  // back to the site saw 登录 / 免费开始 and concluded the session was gone.
  it('shows the console entry instead of login CTAs once signed in', () => {
    useAuthStore.setState({
      status: 'authenticated',
      user: {
        id: '1',
        email: 'user@example.com',
        display_name: 'user@example.com',
        role: 'PERSONAL',
        status: 'NORMAL',
        csrf_token: '',
      },
      error: null,
      hint: null,
    })

    renderLayout()

    const header = screen.getByRole('banner')
    expect(within(header).getByRole('link', { name: '进入控制台' })).toHaveAttribute(
      'href',
      ROUTES.console,
    )
    expect(within(header).getByText('user@example.com')).toBeInTheDocument()
    // Neither the header nor the footer may keep offering 登录 to a signed-in
    // visitor: that is exactly what made the session look dropped.
    expect(within(header).queryByRole('link', { name: '免费开始' })).toBeNull()
    expect(within(header).queryByRole('link', { name: '登录' })).toBeNull()
    expect(screen.queryByRole('link', { name: '登录' })).toBeNull()
  })

  // AGPL-3.0 §13: whoever uses the site over the network must be able to reach
  // the source. Removing this link silently breaks the licence, so it is pinned.
  it('exposes the source code link in the footer', () => {
    renderLayout()

    expect(screen.getByRole('link', { name: '源代码（AGPL-3.0）' })).toHaveAttribute(
      'href',
      SOURCE_CODE_URL,
    )
  })
})

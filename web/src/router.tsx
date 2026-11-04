import { lazy } from 'react'
import { createBrowserRouter } from 'react-router-dom'
import { AppShell } from '@/layouts/AppShell'
import { SiteLayout } from '@/layouts/SiteLayout'
import { ADMIN_PRICING_ROLES, ADMIN_SYSTEM_ROLES } from '@/lib/constants'
import { RequireAuth } from '@/router/guards'

// Route-level code splitting keeps the first paint free of chart/admin code.
const LandingPage = lazy(() => import('@/pages/site/LandingPage'))
const PricingPage = lazy(() => import('@/pages/site/PricingPage'))
const ModelPricePage = lazy(() => import('@/pages/site/ModelPricePage'))
const DocsPage = lazy(() => import('@/pages/site/DocsPage'))
const LoginPage = lazy(() => import('@/pages/site/LoginPage'))
const ResetPasswordPage = lazy(() => import('@/pages/site/ResetPasswordPage'))
const IntegrationsPage = lazy(() => import('@/pages/site/IntegrationsPage'))
const TermsPage = lazy(() => import('@/pages/site/TermsPage'))
const PrivacyPage = lazy(() => import('@/pages/site/PrivacyPage'))
const NotFoundPage = lazy(() => import('@/pages/site/NotFoundPage'))

const ConsoleOverviewPage = lazy(() => import('@/pages/console/OverviewPage'))
const KeysPage = lazy(() => import('@/pages/console/KeysPage'))
const KeyDetailPage = lazy(() => import('@/pages/console/KeyDetailPage'))
const ConsoleModelsPage = lazy(() => import('@/pages/console/ModelsPage'))
const UsagePage = lazy(() => import('@/pages/console/UsagePage'))
const RechargePage = lazy(() => import('@/pages/console/RechargePage'))
const LogsPage = lazy(() => import('@/pages/console/LogsPage'))
const PlaygroundPage = lazy(() => import('@/pages/console/PlaygroundPage'))
const ConsoleTicketsPage = lazy(() => import('@/pages/console/TicketsPage'))
const ConsoleAccountPage = lazy(() => import('@/pages/console/AccountPage'))
const ConsoleSubscriptionPage = lazy(() => import('@/pages/console/SubscriptionPage'))

const AdminOverviewPage = lazy(() => import('@/pages/admin/OverviewPage'))
const ChannelsPage = lazy(() => import('@/pages/admin/ChannelsPage'))
const UsersPage = lazy(() => import('@/pages/admin/UsersPage'))
const AdminPricingPage = lazy(() => import('@/pages/admin/PricingPage'))
const AnnouncementsPage = lazy(() => import('@/pages/admin/AnnouncementsPage'))
const ReviewsPage = lazy(() => import('@/pages/admin/ReviewsPage'))
const AdminTicketsPage = lazy(() => import('@/pages/admin/TicketsPage'))
const AdminModelsPage = lazy(() => import('@/pages/admin/ModelsPage'))
const AdminOrdersPage = lazy(() => import('@/pages/admin/OrdersPage'))
const AdminRedeemCodesPage = lazy(() => import('@/pages/admin/RedeemCodesPage'))
const AdminSystemPage = lazy(() => import('@/pages/admin/SystemPage'))

export const router = createBrowserRouter([
  {
    element: <SiteLayout />,
    children: [
      { index: true, element: <LandingPage /> },
      { path: 'pricing', element: <PricingPage /> },
      { path: 'models', element: <ModelPricePage /> },
      { path: 'docs', element: <DocsPage /> },
      { path: 'login', element: <LoginPage /> },
      // 找回密码：放在 login 之后，且**不**加 RequireAuth（见 ROUTES.resetPassword）。
      { path: 'reset-password', element: <ResetPasswordPage /> },
      { path: 'integrations', element: <IntegrationsPage /> },
      // 法律条款占位页：挂在 SiteLayout 之下，自动获得页头页脚。
      { path: 'terms', element: <TermsPage /> },
      { path: 'privacy', element: <PrivacyPage /> },
      { path: '*', element: <NotFoundPage /> },
    ],
  },
  {
    path: '/console',
    // 两个入口共用同一个外壳 AppShell：侧栏（常规 / 个人 / 管理员）在 /console 与
    // /admin 下渲染结果完全一致，超管不必再跨两个后台跳。`role` 的语义不变：
    // "user" = 任何登录用户可进，"admin" = 需管理员角色。
    element: (
      <RequireAuth role="user">
        <AppShell />
      </RequireAuth>
    ),
    children: [
      { index: true, element: <ConsoleOverviewPage /> },
      { path: 'keys', element: <KeysPage /> },
      { path: 'keys/:id', element: <KeyDetailPage /> },
      { path: 'models', element: <ConsoleModelsPage /> },
      { path: 'usage', element: <UsagePage /> },
      { path: 'recharge', element: <RechargePage /> },
      { path: 'logs', element: <LogsPage /> },
      { path: 'playground', element: <PlaygroundPage /> },
      { path: 'tickets', element: <ConsoleTicketsPage /> },
      { path: 'account', element: <ConsoleAccountPage /> },
      { path: 'subscription', element: <ConsoleSubscriptionPage /> },
    ],
  },
  {
    path: '/admin',
    // 与外层 `/console` 用同一个 AppShell，只是守卫换成 role="admin"。
    element: (
      <RequireAuth role="admin">
        <AppShell />
      </RequireAuth>
    ),
    children: [
      { index: true, element: <AdminOverviewPage /> },
      { path: 'channels', element: <ChannelsPage /> },
      { path: 'users', element: <UsersPage /> },
      {
        // 「倍率与定价」只对 OPS / SUPER_ADMIN 开放：该页依赖的 prices / groups /
        // integrations 对 FINANCE 全是 403（2026-11-02 口径）。菜单已不再收录该页，
        // 这里再堵一次直接输地址进来的人。ROUTES.adminPricing 与 ROUTE_TITLES 保留，
        // 因为 OPS / SUPER_ADMIN 仍要用这个页面。
        path: 'pricing',
        element: (
          <RequireAuth role="admin" roles={ADMIN_PRICING_ROLES}>
            <AdminPricingPage />
          </RequireAuth>
        ),
      },
      { path: 'announcements', element: <AnnouncementsPage /> },
      { path: 'reviews', element: <ReviewsPage /> },
      { path: 'tickets', element: <AdminTicketsPage /> },
      { path: 'models', element: <AdminModelsPage /> },
      { path: 'orders', element: <AdminOrdersPage /> },
      { path: 'redeem-codes', element: <AdminRedeemCodesPage /> },
      {
        // 「系统管理」只调 /api/admin/config（权限点 config.write，只有 OPS 与
        // SUPER_ADMIN 有）。菜单已按 ADMIN_SYSTEM_ROLES 过滤，这里再堵一次直接
        // 输地址进来的 FINANCE。
        path: 'system',
        element: (
          <RequireAuth role="admin" roles={ADMIN_SYSTEM_ROLES}>
            <AdminSystemPage />
          </RequireAuth>
        ),
      },
    ],
  },
])

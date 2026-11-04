import { lazy } from 'react'
import { createBrowserRouter } from 'react-router-dom'
import { AdminLayout } from '@/layouts/AdminLayout'
import { ConsoleLayout } from '@/layouts/ConsoleLayout'
import { SiteLayout } from '@/layouts/SiteLayout'
import { ADMIN_PRICING_ROLES } from '@/lib/constants'
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
    element: (
      <RequireAuth role="user">
        <ConsoleLayout />
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
    element: (
      <RequireAuth role="admin">
        <AdminLayout />
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
    ],
  },
])

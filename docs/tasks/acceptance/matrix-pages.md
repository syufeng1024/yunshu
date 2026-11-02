# 22 页验收对照表（matrix-pages）

| 项 | 值 |
| :--- | :--- |
| 数据来源 | `docs/_API 中转站 · 需求文档 V1.2.md` §2.1「页面总表（V1.0 范围内 22 个页面）」 |
| 生成日期 | 2026-10-29 |
| 行数 | 22 |
| 前端路由实现 | `web/src/router.tsx:35-84` |
| 运行站点 | `http://127.0.0.1:8088`（`yunshu-stack-web`，未新起服务） |
| 登录账号 | `demo@yunshu.test / Review#2026`（登录字段 `identifier`，实测角色 `PERSONAL`；密码于 2026-10-30 由 `Demo#2026` 轮换，见 `HANDOFF-2026-10-30.md:86`，本行 2026-11-02 据实测更正） |
| 页面可达性命令 | `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8088<路由>` |
| 接口权限命令 | 匿名：`curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8088<接口>`；登录：`curl -s -b /tmp/cj.txt -o /dev/null -w '%{http_code}' ...` |

说明：站点为 SPA，nginx 对所有前端路由均返回 `index.html`（`200`），登录态与角色由 `RequireAuth`（`web/src/router/guards.tsx:59`）在浏览器侧判定；因此「可访问性」列同时记录**页面 HTTP 状态**与**该页数据接口的真实拦截结果**。接口权限以服务端 `internal/control/server.go` 的 `mux.Handle` 为准。22 条路由逐条用 curl 实测，无「未验证」条目。

| 路由 | 后端接口 | 前端文件 | 权限 | 可访问性(200/403) | 结论 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| / | `GET /api/public/models`、`GET /api/public/plans`、`GET /api/public/announcements`、`GET /api/public/endpoints` | `web/src/pages/site/LandingPage.tsx` | 公开（无鉴权） | 页面 200；四个公开接口匿名均 200 | ✅ |
| /pricing | `GET /api/public/plans` | `web/src/pages/site/PricingPage.tsx` | 公开（无鉴权） | 页面 200；接口匿名 200 | ✅ |
| /docs | `GET /api/public/models` | `web/src/pages/site/DocsPage.tsx` | 公开（无鉴权） | 页面 200；接口匿名 200 | ✅ |
| /login | `POST /api/v1/auth/login`、`POST /api/v1/auth/register`、`GET /api/public/plans` | `web/src/pages/site/LoginPage.tsx` | 公开（无鉴权） | 页面 200；登录接口实测 200 并下发 `sid`/`csrf` Cookie | ✅ |
| /console | `GET /api/console/overview` | `web/src/pages/console/OverviewPage.tsx` | 登录用户（`PermUserRead`） | 页面 200；接口 demo 200 / 匿名 401 | ✅ |
| /console/keys | `GET /api/console/keys` | `web/src/pages/console/KeysPage.tsx` | 登录用户（`PermKeyRead`） | 页面 200；接口 demo 200 / 匿名 401 | ✅ |
| /console/keys/:id | `GET /api/console/keys/{id}` | `web/src/pages/console/KeyDetailPage.tsx` | 登录用户（`PermKeyRead`，仅本人 Key） | 页面 200；`/api/console/keys/1` demo 200 / 匿名 401 | ✅ |
| /console/models | `GET /api/console/models` | `web/src/pages/console/ModelsPage.tsx` | 登录用户（`PermModelRead`） | 页面 200；接口 demo 200 / 匿名 401 | ✅ |
| /console/usage | `GET /api/console/usage`、`GET /api/console/bills` | `web/src/pages/console/UsagePage.tsx` | 登录用户（`PermUsageRead`） | 页面 200；`/api/console/usage` demo 200 / 匿名 401 | ✅ |
| /console/recharge | `GET /api/console/plans`、`GET /api/console/orders` | `web/src/pages/console/RechargePage.tsx` | 登录用户（`PermFundRead`） | 页面 200；两接口 demo 200 / 匿名 401 | ✅ |
| /console/logs | `GET /api/console/logs` | `web/src/pages/console/LogsPage.tsx` | 登录用户（`PermLogRead`） | 页面 200；接口 demo 200 / 匿名 401 | ✅ |
| /console/playground | `POST /api/console/playground/chat`、`GET /api/console/models` | `web/src/pages/console/PlaygroundPage.tsx` | 登录用户（`PermKeyRead`） | 页面 200；探测 `GET /api/console/models` demo 200 / 匿名 401（未发起计费的 chat 调用） | ✅ |
| /admin | `GET /api/admin/overview` | `web/src/pages/admin/OverviewPage.tsx` | 运营/ADMIN（`PermAdminRead`） | 页面 200；接口 demo 403 `permission_denied` / 匿名 401 | ✅ |
| /admin/channels | `GET /api/admin/channels` | `web/src/pages/admin/ChannelsPage.tsx` | 运营/ADMIN（`PermAdminRead`） | 页面 200；接口 demo 403 / 匿名 401 | ✅ |
| /admin/users | `GET /api/admin/users` | `web/src/pages/admin/UsersPage.tsx` | 运营/ADMIN（`PermAdminRead`） | 页面 200；接口 demo 403 / 匿名 401 | ✅ |
| /admin/pricing | `GET /api/admin/prices`、`GET /api/admin/groups` | `web/src/pages/admin/PricingPage.tsx` | 运营/ADMIN（`PermPriceWrite`、`PermGroupWrite`） | 页面 200；两接口 demo 403 / 匿名 401 | ✅ |
| /admin/announcements | `GET /api/admin/announcements`、`GET /api/admin/risk/rules` | `web/src/pages/admin/AnnouncementsPage.tsx` | 运营/ADMIN（`PermNoticeWrite`、`PermRiskWrite`） | 页面 200；两接口 demo 403 / 匿名 401 | ✅ |
| /integrations | `GET /api/public/integration-links`、`POST /api/v1/integrations/import-credentials` | `web/src/pages/site/IntegrationsPage.tsx` | 公开（无鉴权）；一键导入需登录 | 页面 200；links 匿名 200；导入接口未实现（404，已在 matrix-82 F4-2 登记） | ✅ |
| /console/tickets | `GET /api/console/tickets` | `web/src/pages/console/TicketsPage.tsx` | 登录用户（`PermTicketRead`） | 页面 200；接口 demo 200 / 匿名 401 | ✅ |
| /admin/reviews | `GET /api/admin/reviews` | `web/src/pages/admin/ReviewsPage.tsx` | 运营/财务/ADMIN（`PermFundApprove`） | 页面 200；接口 demo 403 / 匿名 401 | ✅ |
| /admin/tickets | `GET /api/admin/tickets` | `web/src/pages/admin/TicketsPage.tsx` | 运营/ADMIN（`PermTicketReply`） | 页面 200；接口 demo 403 / 匿名 401 | ✅ |
| /admin/models | `GET /api/admin/models` | `web/src/pages/admin/ModelsPage.tsx` | 运营/ADMIN（`PermAdminRead`） | 页面 200；接口 demo 403 / 匿名 401 | ✅ |

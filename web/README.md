# 云枢 API · Web

Vite + React 18 + TypeScript(strict) + Tailwind。目录内所有颜色来自
`src/styles/tokens.css`，Logo 与 favicon 从 `public/assets/logo/**` 自托管。

## 1. 起本地

```bash
cp .env.example .env.local   # 可选：只需要改 VITE_DEV_API_TARGET 时
npm install
npm run dev                  # http://localhost:5173
```

开发服务器把 `/api`、`/healthz`、`/readyz` 代理到 `VITE_DEV_API_TARGET`
（默认 `http://localhost:8081`，即 `cmd/controld`）。生产构建如与控制面同源，
`VITE_API_BASE_URL` 留空即可。

## 2. 构建与校验

```bash
npm run verify        # typecheck + test + check:copy + build
npm run preview
```

`check:copy` 扫描 `src/` 的品牌名、裸色值、Web Storage 与页面二次换算；构建插件
`assertNoPricingLiterals` 在产物落盘后扫描 `dist/assets/*.js`，命中价格/额度字面量
（如数字 + 积分/元/RPM/TPM/并发/日额度）即让构建失败。

`npm run gen:api` 从仓库根 `api/openapi.yaml` 生成 `src/api/types.ts`；后端改契约后
重新执行，不要手写响应类型。OpenAPI 文件当前是 T17 最小骨架，业务接口由 T11 扩充。

## 3. 加一个页面

1. 在 `src/pages/<site|console|admin>/` 新建页面文件，默认导出组件，并调用
   `usePageTitle('页面名')`（品牌段由 hook 统一拼成 `云枢 API`）。
2. 在 `src/router.tsx` 添加 `lazy(() => import(...))` 与路由项，路径逐字对齐
   PRD §2.1；控制台与后台路由必须包在 `RequireAuth` 内。
3. 数据用 `api` + React Query 拉取：失败渲染 `ErrorState`/`Table` 的错误态，
   金额与积分调用 `src/lib/format.ts`，颜色只用 Tailwind 语义名或 token。

## 目录

| 路径 | 职责 |
| :--- | :--- |
| `src/styles/tokens.css` | 唯一颜色/圆角/阴影/字体来源 |
| `src/api/client.ts` | fetch 封装：统一解析 `{"error":{code,message,hint}}`，401 跳登录 |
| `src/api/types.ts` | `openapi-typescript` 生成，禁止手改 |
| `src/components/` | Button / Input / Select / Table / Modal / Drawer / Toast / Tabs / Badge / Tooltip / CopyBlock / EmptyState / Skeleton / StatCard / LineChart |
| `src/layouts/` | SiteLayout（Header/Footer）、AppShell（登录后唯一的壳：侧栏「常规 / 个人 / 管理员」三组） |
| `src/lib/format.ts` | 积分 ↔ 元、微积分 ↔ 积分的唯一换算点 |
| `src/lib/i18n.ts` | 专属渠道对外文案 + 禁用词常量（`TODO(Legal)`） |
| `src/router/guards.tsx` | 体验层权限守卫；服务端仍需逐接口校验 |

自托管资源只复制运行所需文件：`svg/`、`png/`、`favicon.ico`。`assets/logo/source/`
与 `preview.png` 是设计取证材料，不进入生产构建。

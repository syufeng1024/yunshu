# T49 · 全量补齐 admin 接口的 openapi 契约（45 条 operation）

| 项 | 值 |
| :--- | :--- |
| 卡片编号 | T49 |
| 阶段 | 6 · 交付（契约补全，T46 P1 的正式收口） |
| 优先级 | **P1**（不阻塞运行时，但生成物缺 45 个接口，前端只能手写类型，契约与实现长期脱节） |
| 依赖 | T46、T48 必须先完成（T48 新增的跨字段校验要写进 `AdminModelPatchRequest` 描述） |
| 交付根 | `api/openapi.yaml`、`web/src/api/types.ts`（生成）、`web/src/api/admin.ts`（消除手写类型）、`scripts/` 下一支防回归校验脚本 |
| 状态 | 待实现 |
| 分段 | **大卡**，按面分 5 组推进，每组做完即自检，不要憋到最后 |

## 1. 事实（实测，勿重复调查）

| 事实 | 证据 |
| :--- | :--- |
| 代码实际注册 **64** 条 admin 路由 | `internal/control/server.go:353-441`，正则 `mux.Handle\("([A-Z]+) (/api/admin[^"]*)"` 提取 |
| openapi 只登记 **19** 条 admin operation | `api/openapi.yaml` 的 `paths:` 段解析 |
| **缺 45 条**，反向 **0** 条（文档里没有代码不存在的接口） | 双向差集比对，反向为空 |
| 模型面缺视图 schema，这才是 T46 §2.4 空转的原因 | 只有 `AdminModelCreateRequest`(:4582) 与 `AdminModelPatchRequest`(:4632)，**没有** `AdminModel` / `AdminModelList` / `AdminModelDetail` |
| 前端因此只能手写类型 | `web/src/api/admin.ts` 的 `AdminModel` / `ModelCreateInput` / `ModelPatchInput` 是手写，不是生成物 |
| 已有完整 CRUD 范例可直接照抄结构 | `/api/admin/routes`（`api/openapi.yaml:2163-2280`）+ `AdminRoute`(:4522) / `AdminRouteList`(:4544) / `AdminRouteDetail`(:4554) / `AdminRouteCreateRequest`(:4559) / `AdminRoutePatchRequest`(:4572) |
| 现有约定 | `operationId` 为 `listAdminRoutes` / `createAdminRoute` 这类 camelCase；`tags: [admin]`；**读**只挂 `sessionCookie`，**写**再加 `csrfHeader`；分页参数 `page` / `page_size` / `sort` / `order`；错误码只登记 `400/401/403/404/409`（不登记 500） |

## 2. 缺失清单（45 条，按面分组，也就是推进顺序）

### 组 1 · 模型面（8 条）
```
GET    /api/admin/models
GET    /api/admin/models/{id}
PATCH  /api/admin/models/{id}
POST   /api/admin/models/{id}/listing-state
POST   /api/admin/models/{id}/recommend-weight
GET    /api/admin/models/{id}/aliases
POST   /api/admin/models/{id}/aliases
DELETE /api/admin/models/{id}/aliases/{alias}
```
需新增 schema：`AdminModel`、`AdminModelList`、`AdminModelDetail`、`AdminModelListingStateRequest`、`AdminModelRecommendWeightRequest`、`AdminAlias`、`AdminAliasList`、`AdminAliasCreateRequest`。

**`AdminModel` 必须含 `max_output_tokens`**（T46 加的列，`integer` / `int64` / `minimum: 0`，描述「0 = 未配置，按 `min(context_length, 32768)` 兜底」）—— 这是 T46 §2.4 当初无处可补的那一条，本卡落实。

### 组 2 · 渠道面（7 条）
```
GET    /api/admin/channels/{id}
GET    /api/admin/channels/{id}/upstream-models
POST   /api/admin/channels
PATCH  /api/admin/channels/{id}
DELETE /api/admin/channels/{id}
POST   /api/admin/channels/{id}/disable
POST   /api/admin/channels/{id}/register-upstream-models
```
`AdminChannel` / `AdminChannelList` 已存在（:4478/:4512），补 `AdminChannelDetail`、`AdminChannelCreateRequest`、`AdminChannelPatchRequest`、`AdminUpstreamModelList`、`AdminRegisterUpstreamModelsRequest`。

### 组 3 · 用户与会话（3 条）
```
GET  /api/admin/whoami
POST /api/admin/users/{id}/disable
POST /api/admin/users/{id}/enable
```
`AdminUser` 系列已存在（:4329/:4352/:4391），补 `AdminWhoami` 与 disable/enable 的请求体（若 handler 需要 reason）。

### 组 4 · 定价与分组（5 条）
```
GET   /api/admin/prices
POST  /api/admin/prices
PATCH /api/admin/prices/{id}
GET   /api/admin/groups
PATCH /api/admin/groups/{id}
```

### 组 5 · 运营 + 风控 + 系统（22 条）
```
GET  /api/admin/orders
POST /api/admin/orders/{id}/close
POST /api/admin/orders/{id}/confirm-payment
GET  /api/admin/redeem-codes
POST /api/admin/redeem-codes/batch
GET  /api/admin/reviews
POST /api/admin/reviews/{id}/approve
POST /api/admin/reviews/{id}/reject

GET    /api/admin/risk/rules
POST   /api/admin/risk/rules
PATCH  /api/admin/risk/rules/{id}
DELETE /api/admin/risk/rules/{id}
GET    /api/admin/risk/hits
POST   /api/admin/risk/users/{id}/score

GET   /api/admin/config
PUT   /api/admin/config
GET   /api/admin/integrations
PATCH /api/admin/integrations/{toolKey}
GET   /api/admin/announcements
POST  /api/admin/announcements
POST  /api/admin/announcements/{id}/publish
POST  /api/admin/upstream-bills/import
```

## 3. 做法要求（关键，别只写"看起来合理"的契约）

**每一条 operation 的契约必须从 handler 源码反推，不许凭接口名猜。** 具体要读：

- **路径与方法**：`internal/control/server.go` 对应行，确认 method 与权限（`mw.PermXxx`）。
- **query 参数**：handler 里 `r.URL.Query().Get(...)` 的每一个 key，以及默认值、枚举、上限。空手写一个 `page_size` 却漏掉真实存在的 `sort` 字段，就是错的。
- **请求体字段**：handler 绑定的 request struct（如 `modelCreateRequest`、`modelPatchRequest`），字段名、类型、指针语义（`*int64` 表示可省略）、必填性。**PATCH 类一律 `minProperties: 1`**，与既有 `AdminRoutePatchRequest` 一致。
- **响应形态**：返回的 view struct（如 `modelView`）及其 JSON tag；列表接口确认是否 `items/page/page_size/total` 四件套；单对象接口按既有约定包一层（`AdminRouteDetail` 是 `{route: ...}`，模型面应为 `{model: ...}`）。
- **错误码**：读 handler 里的 `mw.WriteError` 分支。**只登记代码里真实可能返回的**，不要套模板硬凑 409。404 只在有 `resource_not_found` 的接口上写。
- **错误字段名**：`mw.InvalidField("xxx")` 里的 xxx 要体现在 `400` 的 description 里（如「字段非法（invalid_request_body）」→ 具体到 `max_output_tokens` 等）。

**T48 的成果必须落到文档**：`AdminModelPatchRequest` 的 `context_length` 描述要写清「不得小于当前存量 `max_output_tokens`，否则 400 且错误字段指向 `context_length`」。

## 4. 防回归校验脚本（必做，这是本卡最有价值的产出）

新增 `scripts/check-openapi-routes.sh`：提取 `internal/control/server.go` 里所有 `mux.Handle("METHOD /api/admin...")`，与 `api/openapi.yaml` 的 `paths:` 段比对，**输出缺失与多余的 method+path，任一方向非空即退出码非 0**。

实现方式自选（bash + python3、或一个 Go 小命令都行），但要求：
- 只读，不改任何文件；
- 能区分「缺失」与「多余」两个方向；
- 全量对齐时退出 0 并打印 `admin 路由 N 条，openapi 全部覆盖`。

这个脚本要能当作 CI 关卡，防止以后再次脱节。参考实现思路（我已验证可用）：解析 `paths:` 段时按两空格缩进的 `/` 开头键取路径、四空格缩进的 `get|post|put|patch|delete` 取方法。

## 5. 硬约束

1. **只补文档，不改后端行为。** 不允许为了「让文档说得通」去改 handler、改校验、改返回结构。文档要追随实现，不是实现追随文档。若发现实现真的有 bug，**记下来报给主理人**，另开卡，不在本卡顺手改。
2. **不许编造字段。** 每个字段都要能在 handler struct 或 SQL 列里找到出处。宁可少写一个不确定的字段，也不要写一个不存在的。
3. **不手改 `web/src/api/types.ts`**，必须 `cd web && npm run gen:api`。
4. **不改已有的 19 条正确内容**，可补 description 与 schema 引用。
5. `web/src/api/admin.ts` 的手写类型改为从生成物派生（`components['schemas']['AdminModel']` 这类），但要**先确认改动面**：若某页面的用法与生成类型不兼容，先报差异再决定，不要为了消类型而改页面逻辑。
6. **不 commit、不 push、不建分支。**
7. `bash scripts/check-eol.sh` 退出 0；`go build ./...` 不受影响；`cd web && npm run typecheck` 通过。

## 6. 自检命令

```bash
cd /d/codes/yunshu-api

# 契约覆盖（本卡核心指标）
bash scripts/check-openapi-routes.sh; echo "cover_exit=$?"

# YAML 语法
python -c "import yaml,sys; yaml.safe_load(open('api/openapi.yaml',encoding='utf-8')); print('YAML OK')"

# 类型生成与前端
cd web && npm run gen:api && npm run gen:api && npm run typecheck && cd ..
# 注意：连续跑两次 gen:api 确认幂等（第二次不应产生新 diff）
# 前端测试必须串行：并行时两个 worker 抢写 .vitest-tmp 会触发 EPERM
# unhandled error，把退出码顶成 1，并且只收集到一个测试文件 —— 极易被
# 误判成「用例失败」或「另一个文件没跑」。串行后 2 files / 15 tests / exit 0。
cd web && npx vitest run --no-file-parallelism ModelsPage && cd ..

# 后端未被误改
go build ./... && git diff --stat -- 'internal/**/*.go' 'cmd/**/*.go'   # 期望为空

bash scripts/check-eol.sh; echo "eol_exit=$?"
```

## 7. 验收口径（交付总监独立复跑）

| # | 查什么 | 通过标准 |
| :--- | :--- | :--- |
| 1 | 覆盖完整 | `check-openapi-routes.sh` 退出 0，两个方向均 0 缺失；独立用另一套实现再比对一次结果一致 |
| 2 | 语法合法 | YAML 可解析；`gen:api` 无报错（生成器对结构错误很敏感） |
| 3 | 契约与实现一致（重点抽查） | 随机抽 **5 条**，逐条对照 handler：query 参数键、请求体字段、必填性、响应字段与 JSON tag、错误码。**必须能指到 handler 的具体行** |
| 4 | 模型面专项 | `AdminModel` 含 `max_output_tokens` 且描述含「0 = 未配置」；`AdminModelPatchRequest` 的 `context_length` 描述含 T48 的跨字段约束；`GET /api/admin/models` 的响应是 `items/page/page_size/total` 四件套 |
| 5 | 无编造 | 抽查 3 个 schema 的 5 个字段，全部能在代码里找到出处（struct 字段名或 JSON tag） |
| 6 | 生成物幂等 | 连续两次 `gen:api` 第二次无 diff；`types.ts` 无手改痕迹 |
| 7 | 未改后端 | `git diff --stat` 下 `internal/**` 与 `cmd/**` 为空；`git status` 只有 openapi.yaml + types.ts + admin.ts + 新脚本 |
| 8 | 前端仍可用 | `npm run typecheck` 通过；`npm test -- ModelsPage` 绿（T48 改动未被契约变更破坏） |

## 8. 不在本卡范围（写了会被打回）

- 修改任何 handler 的行为、校验、返回结构（发现 bug 另开卡）。
- 非 admin 接口（`/api/console/*`、`/api/v1/*`）的契约补全。
- 为已登记接口补未实现的字段（例如给 `AdminChannel` 补代码里没有的列）。
- 移除 `maxOutputTokensCeiling`（T46 P2 已判可接受）。
- T47 试用积分防刷。

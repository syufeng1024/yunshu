# T15 · 运营后台 API（渠道/模型/定价/分组/公告/风控）

| 项 | 值 |
| :--- | :--- |
| 卡片编号 | T15 |
| 阶段 | 4 · 控制面 |
| 优先级 | P0 |
| 依赖 | T11、T05、**T10**（错误码） |
| 可并行 | 与 T12 / T13 / T14 并行 |
| 交付根 | `internal/control/ops/` |
| 状态 | ✅ 两段已交付（2026-10-27）· 交付物 1–18 + 状态码收敛 |

## 1. 目标

运营的六件事：配渠道、定价、分组、发公告、管用户、盯风控。本卡的核心不是 CRUD，而是**几条会直接造成资金或声誉损失的校验**：境外来源地拒绝保存、毛利地板、调价公示期、价格唯一真相源、渠道被引用不可删、上游 Key 密文存储。

## 2. 必读

| 文档 | 区间 | 要点 |
| :--- | :--- | :--- |
| PRD §6.1 | 752–763 | 运营概览 |
| PRD §6.2 | 763–787 | 渠道管理：F14-1–F14-6（含来源地拒绝、专属范围） |
| PRD §6.3 | 787–811 | 用户管理：搜索、禁用/启用、禁用原因不对外 |
| PRD §6.4 | 811–860 | 倍率与定价：F16-1–F16-7（公示期、地板、唯一真相源） |
| PRD §6.5 | 860–875 | 公告与风控 |
| PRD §3.4 / §3.5 | 364–417 | 来源地强制、模型三态、定价层级、倍率 `>0 且 ≤1`、地板公式 |
| PRD D.3 | 1716–1731 | F14 / F16 / F17 验收路径（逐条落测试） |
| TDS §1.4 | 1424–2023 | `t_channel` / `t_model` / `t_price_rule` / `t_price_group` / `t_config_*` |
| TDS §1.8 | 2698–2884 | 公告位掩码、风控规则与命中、审计 |
| TDS §3.7 | 434–517 | `/admin/config` 只接受 12 个白名单键 |

## 3. 交付物清单

| # | 路径 | 职责 |
| :--- | :--- | :--- |
| 1 | `internal/control/ops/channel.go` | 渠道 CRUD：列表/详情/新建/编辑/停用/删除；删除时若被路由引用 → **409 `channel_in_use`** |
| 2 | `internal/control/ops/channel.go` | **来源地校验**：选「境外」→ 拒绝保存（400 + 明确提示），境内可保存 |
| 3 | `internal/control/ops/channel.go` | 上游 Key：入库存密文（信封加密），出参掩码（只显末 4 位），**任何接口不得回吐明文** |
| 4 | `internal/control/ops/channel.go` | 权重变更即时生效 + 审计前后值；`group_scope_mode` 允许分组；`dedicated_scope_mode` + 专属用户名单维护 |
| 5 | `internal/control/ops/model.go` | 模型主数据管理：对外名、别名、类型、上下文、单价、缓存倍率、阶梯、协议、归属渠道、`listing_state`、`recommend_weight` |
| 6 | `internal/control/ops/model.go` | 三态切换：`LISTED` ↔ `CATALOG_ONLY` ↔ `DELISTED`；`LISTED→CATALOG_ONLY` 不需公示期，写审计 + **生成公告草稿** |
| 7 | `internal/control/ops/model.go` | 推荐权重：`CATALOG_ONLY` 模型设非 0 权重 → 拒绝；`LISTED` 改 `CATALOG_ONLY` 时权重自动失效 |
| 8 | `internal/control/ops/pricing.go` | 单价保存：负数/0 → 400 指出字段；**唯一真相源校验**（同一模型两处配价 → 拦截并指出两份来源） |
| 9 | `internal/control/ops/pricing.go` | **公示期**：涨价须提前 ≥ 7 天生效，否则拒绝；降价可即时生效 |
| 10 | `internal/control/ops/pricing.go` | **毛利地板**：`标准单价 × 倍率 ≥ 成本价 × 4/3`；缺成本价时降级为「警告 + 二次确认」，**不得静默上架**；`internal` 分组例外 |
| 11 | `internal/control/ops/group.go` | 分组管理：`standard 10000` / `contract 8500` / `internal 0`；倍率须 `>0 且 ≤1`（`internal` 的 0 为唯一例外）；范围（模型集 / 渠道集）维护 |
| 12 | `internal/control/ops/group.go` | 倍率变更 → 批量写穿该分组下全部 Key 的 K13 `gp` 字段（异步，允许短暂旧值） |
| 13 | `internal/control/ops/announcement.go` | 公告：发布/定时发布/草稿；「活动」类不得与调价公示冲突；模型上架自动生成待发布「上新」草稿 |
| 14 | `internal/control/ops/announcement.go` | 公告**不反向回滚**模型状态（PRD F17-3） |
| 15 | `internal/control/ops/risk.go` | 风控：规则 CRUD、命中记录查询、用户风险分调整（审计） |
| 16 | `internal/control/ops/users.go` | 用户管理：列表（邮箱/手机/用户号/状态筛选）、禁用/启用（`disable_source` = `OPS_MANUAL`）、余额与套餐查看 |
| 17 | `internal/control/ops/users.go` | 禁用原因**仅运营可见**，用户侧只看到「账号已被禁用，请联系客服」 |
| 18 | `internal/control/ops/config.go` | `GET/PUT /admin/config`：只接受 12 个白名单键；`quota.*` 四键只接受 V1 已实现值；其余一律 `config_value_invalid` |
| 19 | `internal/control/ops/*_test.go` | F14-1–F14-6、F16-1–F16-7、F17-1–F17-3 全覆盖 |
| 20 | `api/openapi.yaml`（追加） | 本卡接口 schema |

## 4. 硬约束（违反即驳回）

1. **来源地境外一律拒绝保存**：渠道表单与模型主数据选「境外」→ 400 拒绝。且即使被误配入库，路由层（T07）也不得选中——**双保险，缺一不可**。
2. **删除渠道前查引用**：被 `t_route` 引用 → 409 `channel_in_use`，`hint` 给出解除路径（先解引用或改为停用）。
3. **上游 Key 密文**：DB 存密文，接口出参掩码，审计日志不落明文。`grep` 检查序列化结构里不得含 `upstream_key` 明文字段。
4. **毛利地板公式精确**：`标准单价 × 分组倍率 ≥ 上游成本单价 × 4/3`，**唯一实现口径** = `成本 × 10000 ÷ 7500` 向上取整 + 整数交叉相乘比较。**禁止**写 `× 1.25` / `× 1.333` / `×(10000+bp)`。
5. **`internal` 分组（倍率 0）不参与地板校验**，改用 PRD §6.4 F16-5 四项约束兜底。
6. **调价公示期 7 天**：涨价且生效时间距今 < 7 天 → 拒绝；降价允许即时。判定以服务端时间为准，不信任前端。
7. **调价不回溯历史账单**：改价只影响生效时间之后的请求（PRD F16-1）。
8. **价格唯一真相源**：同一模型不允许两处配置不同单价；冲突时拦截并**指出两份来源**（表名 + 行 id）。
9. **分组倍率区间**：`>0 且 ≤1`（`internal` 的 `0` 是唯一例外，语义为「不计费」而非「免费打折」，`remark` 必须写明）。**不设 >1 的倍率**。
10. **`group_code` 只能由运营设置**：`contract` 分组必须经审批（走 T16 审核中心）；用户侧无入口。
11. **推荐权重约束**：非 0 仅对 `LISTED` 有效；`CATALOG_ONLY` 设权重 → 拒绝。
12. **公告不回滚业务状态**：公告只是通知载体，`F17-3` 明确要求状态与公告解耦。
13. **用户禁用原因不对外**：`account_disabled` 的错误体只给通用文案，**不透露类别**。
14. **配置白名单**：只 12 键可运行时写；写其它键 → 400 `config_value_invalid`；写 `quota.overdraft_warn_days` 同样拒绝。
15. **所有写操作审计含前后值**（T11 中间件）+ 涉及价格/倍率/上限的变更必须写 `t_config_publish` 或等价留痕。
16. **不得提供"一键清除缓存"的万能接口**：失效必须按 namespace / 按 key 精确失效。

## 5. 验收标准

| # | 判定 | 方式 |
| :--- | :--- | :--- |
| A1 | `go test ./internal/control/ops/... -race` 全绿 | 复跑 |
| A2 | F14-5：来源地「境外」→ 拒绝保存；「境内」→ 成功 | 单测 |
| A3 | F14-1：删除被引用渠道 → 409 `channel_in_use` + hint；访问已删渠道详情 → 友好提示无 500 | 单测 |
| A4 | F14-2：新建渠道含上游 Key → 列表掩码、DB 密文（贴 SQL 证据） | 单测 |
| A5 | F14-3：权重 100→10 → 生效且审计含前后值 | 单测 |
| A6 | F14-4：渠道只允许 `contract` → `standard` 用户请求不被路由 | 单测（配合 T07） |
| A7 | F14-6：专属渠道 → 非名单用户候选集不含；渠道停用后走降级且日志可见 | 单测（配合 T07） |
| A8 | F16-2：单价 0 或负数 → 拒绝并指出字段 | 单测 |
| A9 | F16-3：改输出价 → 官网价格表与控制台广场同步新价 | 单测 |
| A10 | F16-4：涨价且 3 天后生效 → 拒绝；降价即时 → 通过 | 单测 |
| A11 | F16-5：`std=1000,ratio=8500,cost=800` → 拒绝并展示差额；`cost=600` → 通过 | 单测（精确数值） |
| A12 | F16-6：同模型两处配价 → 拦截并列出两份来源 | 单测 |
| A13 | F16-7 / F17-1 / F17-3：`LISTED→CATALOG_ONLY` 生成草稿、无需公示期；`CATALOG_ONLY→LISTED` 生成上新草稿；公告未发布不回滚状态 | 单测 |
| A14 | F17-2：公告定时到点自动发布 | 单测（假时钟） |
| A15 | 配置：12 键写入成功、第 13 键与 `quota.overdraft_warn_days` 被拒 | 表驱动单测 |
| A16 | 倍率：`0.85` 存 `8500`；`1.2` → 拒绝；`internal` 为 0 例外 | 单测 |

## 6. 自检命令

```bash
go test ./internal/control/ops/... -race -cover
grep -rn "1.25\|1.333\|float64" internal/control/ops/pricing.go || echo "OK: 地板用整数"
M() { docker exec -i $(docker compose -f deploy/docker-compose.yml ps -q mysql) mysql -uroot -proot yunshu -N -e "$1"; }
M "SELECT id,name,region,upstream_key_enc IS NOT NULL FROM t_channel"
```

## 7. 禁止事项

- 不得为"运营方便"放开来源地校验。
- 不得用浮点算地板或金额。
- 不得让公告发布去改模型 `listing_state`。
- 不得删除被引用的渠道（必须 409 并给路径）。
- 不得回吐上游 Key 明文。

## 8. 完成回执

```
卡号: T15
接口清单: 路径 / 方法 / 权限点 / 对应 F 编号
F 覆盖: F14-1..6、F16-1..7、F17-1..3 → 用例名
地板校验: 公式代码片段 + A11 输出
公示期: A10 输出
来源地: A2 输出
上游 Key: A4 的 SQL 证据
配置白名单: A15 输出
遗留:
```

---

## 9. 第一段交付记录（2026-10-27）

**范围**：交付物 1–7（渠道 + 模型主数据）+ 技术债清偿（状态类错误码收敛、权限矩阵扩容）。定价/分组/公告/风控/用户/配置留第二段。

### 步骤 A · 状态类错误码收敛（技术债清偿）

上一轮立的规矩：**「用户照 hint 改请求体能否成功」——改得成功是 body 问题，改不成功是状态/配额问题，必须有自己的码**。T12 已建 `key_limit_reached`，但 T13 为控制改动面把 12 处状态类拒绝暂堆在 `invalid_request_body` 上。

本轮新增 4 码（均 409 / `conflict_error` / `BUSINESS` / 不可重试），总数 **70 → 74**：

| 码 | 覆盖场景 |
| :--- | :--- |
| `resource_state_conflict` | 退款单/订单/提额单状态迁移不合法（订单非 `PAID`、已有进行中退款、CAS 失败、套餐无日额度可提额等） |
| `trial_already_claimed` | 体验版防刷命中（设备指纹 / 手机号 / IP 段 / 账号已领），四因子共用一条 message，**不披露命中项** |
| `invoice_already_issued` | 同订单 `ISSUED` 后再申请 |
| `price_conflict` | 价格唯一真相源冲突（第二段使用，本段建档） |

12 处调用点逐处判定：**10 处改专用码，2 处保留 `invalid_request_body`**——`invoice.go:294`（改小 `amount_fen` 即可成功）与 `quota_apply.go:136`（改小目标额度即可成功）确属 body 类。

### 步骤 B · 权限矩阵

权限点扩到 **19 个**：`user.read/write`、`key.read/write`、`model.read/write`、`price.write`、`group.write`、`channel.write`、`notice.write`、`risk.write`、`config.write`、`fund.read/write/approve`、`ticket.reply`、`audit.read`、`admin.read`、`admin.all`。（**已扩到 25 个**，见下方「权限点按资源作用域拆分」。）

| 角色 | 关键权限 |
| :--- | :--- |
| `PERSONAL` | `account.read`、`key.*`、`model.read`、`fund.read/write`（仅自己的数据），**不含** `user.read`/`channel.read` |
| `OPS` | 运营全集，**不含** `fund.approve` |
| `FINANCE` | `fund.read`、`fund.approve`、`audit.read`、`model.read`、`admin.read`，**不含** `price.write`/`channel.write`/`user.read`/`channel.read` |
| `SUPER_ADMIN` | 全部 + `admin.all` |

**`admin.read` 是本次新增**（E-31）：后台读接口若复用 `model.read`，`PERSONAL` 就能读到 `cost_*` 成本价与渠道 `base_url`，违反 B-48。已实测全部 49 条 `/api/admin/*` 路由对 `PERSONAL` 全 403 `permission_denied`（`server_authz_test.go`，49 个子测试）。

**「10 个角色」是文档笔误**（E-30）：PRD §6.3、TDS 1209 行、openapi `UserRole` 均只有 4 个角色。按 4 个实现，未编造角色名。

#### 权限点按资源作用域拆分（2026-11-02 修复越权读）

`admin.read` 只解决了「客户 vs 运营」，没解决「运营角色之间」：`FINANCE` 也持有 `admin.read`（它要进 `/api/admin/overview`），于是 `GET /api/admin/users`（含每条账号的 `disable_reason`）与 `GET /api/admin/channels`（含 `base_url`、上游密钥掩码）对 `FINANCE` 全程 200，而前端 `ADMIN_NAV` 对 `FINANCE` 是隐藏这两项的——服务端口径比菜单宽。修法是把「同一个资源、不同作用域」拆成不同权限名，而不是改菜单：

| 权限点 | 作用域 | 持有角色 | 路由 |
| :--- | :--- | :--- | :--- |
| `account.read` | 客户自助，只读**自己**那一行（handler 按 session uid 过滤） | `PERSONAL` / `SUPER_ADMIN` | `GET /api/console/overview` |
| `user.read` | 运营，读**任意**账号（状态、套餐、禁用原因） | `OPS` / `SUPER_ADMIN` | `GET /api/admin/users`、`/api/admin/users/{id}` |
| `channel.read` | 运营，读**任意**上游渠道（`base_url`、密钥掩码） | `OPS` / `SUPER_ADMIN` | `GET /api/admin/channels`、`/api/admin/channels/{id}` |

`user.read` 从 `PERSONAL` 移除是这次的关键：`/api/admin/users` 不按 uid 过滤，一个名字跨两个控制台就等于让每个客户枚举全站账号。`FINANCE` 不持有 `user.read`/`channel.read`：它的面是订单、发票、兑换码与模型成本表（`admin.read` 保留在 `overview`/`models` 读上）。权限点总数 23 → **25**。

### 步骤 C · 渠道（交付物 1–4）

6 条路由（读 `channel.read`，写 `channel.write`；读权限自 2026-11-02 从 `admin.read` 收窄）：`GET /api/admin/channels`、`GET/PATCH/DELETE /api/admin/channels/{id}`、`POST /api/admin/channels`、`POST /api/admin/channels/{id}/disable`。

四条硬约束实测：

| 约束 | 证据 |
| :--- | :--- |
| 来源地拒绝 | `region=OVERSEAS` → 400「V1.0 仅支持境内渠道」，DB 计数 0 |
| 删除查引用 | 被 `t_route` 引用 → 409 `channel_in_use` + hint；解引用后 200；软删后详情 404 `channel_not_found`（不 500） |
| 上游 Key 密文 | `HEX(LEFT(upstream_key_enc,3))=76313A`（`v1:` 前缀的信封本体，**未二次 base64**，E-17）；`upstream_key_mask=sk-****1234`；`Box.Open` 回原文；响应结构体 `channelView` **无** `upstream_key` 字段，明文只存在于入站请求 DTO |
| 权重即时生效 | 改权重后 `PublishRoutes` 调用 1 次；审计 before/after 含 `weight` 100→10 |

专属名单表 `t_channel_dedicated_user`（0017）已存在，切回 `ALL` 时清空。

### 步骤 D · 模型主数据（交付物 5–7）

`GET /api/admin/models`、`GET/POST/PATCH /api/admin/models/{id}`、`POST .../listing-state`、`POST .../recommend-weight`。

- `LISTED → CATALOG_ONLY`：不需公示期，权重**自动清零**并留痕，生成「成本调整」公告草稿（`t_announcement` 仅 INSERT `DRAFT`）
- `CATALOG_ONLY → LISTED`：生成「上新」草稿
- `CATALOG_ONLY` 设非 0 权重 → 409 `resource_state_conflict`
- 模型 `region=OVERSEAS` → 400 拒绝
- 单价/缓存倍率/阶梯本段**只读**（属第二段定价域）

### 遗留

1. **模型详情 404 语义**：现有码表只有数据面 400 的 `model_not_found`，后台详情复用它返回 400。若要求 404，需再加码或复用一个 404 通用码，留第二段裁定。
2. 专属用户 ID 未校验存在性（表无外键），仅校验 `> 0` 与去重。
3. `price_conflict` 已建档但实际使用在第二段（定价唯一真相源）。
4. 未新增迁移（`0022`/`0023` 之外无占用）、未新增配置键。
5. 本机无 gcc，`-race` 未跑；`gofmt` 未重排既有 CRLF 文件。

---

## 10. 第二段交付记录（2026-10-27）

**范围**：交付物 8–18（定价 / 分组 / 公告 / 风控 / 用户 / 配置）+ 404 码收口。

### 步骤 0 · `resource_not_found`（404）

新增第 75 码，收口后台详情语义（第一段遗留：查不存在的模型返回 400）。后台模型的详情/改/删改用它；**渠道仍用 `channel_not_found`**（更具体）。实测：`model detail 999999 → 404 resource_not_found`、`channel detail 999999 → 404 channel_not_found`。

### 定价（A8/A10/A11/A12）

3 条路由（`price.write`）。三条硬校验：

**毛利地板（A11）—— 唯一允许的实现**：
```go
// 3*P*r/10000 >= 4*C/3  ⇔  3*P*r >= 40000*C
lhs := 3 * price * ratioBP      // 3*P*r
rhs := 40000 * cost             // 40000*C
ok  := lhs >= rhs
// 拒绝时展示 required = ceil(4C/3) = (40000*C + 29999) / 30000
```
`grep 1.25|1.333|float32|float64 pricing.go` → `OK_INTEGER_ONLY`。`internal` 分组（`ratio_bp=0`）跳过。

实测：`P=1000, r=8500, C=800` → 400 并报「850 < 1067，差额 217」；`C=600` → 通过。（`3×1000×8500=25.5e6 < 40000×800=32e6`，`25.5e6 ≥ 40000×600=24e6`，与公式一致。）

**公示期（A10）**：涨价且 `effective_from` 距今 < 7 天 → 400 并给出最早可生效时间；降价即时生效。判定只用服务端 `now()`，请求里的时间被钳到 now。

**唯一真相源（A12）**：同模型多处生效价不一致 → 409 `price_conflict`，message 列出全部来源 `t_price_rule#328、#329 与 #330`。正常换价（单一前驱）不算冲突：前驱在 `effective_from` 处收口，历史版本保留。

**缺成本价**：不静默通过——必须 `confirm_no_cost=true` 二次确认，审计记 `confirm_no_cost=true` 并落 WARN。

### 分组（A16）

`GET /api/admin/groups`、`PATCH /{id}`（`group.write`）。`0.85 → 8500`、`1.2 → 400`、`internal=0` 例外且 `remark` 必填。倍率变更异步写穿该分组全部 Key 的 K13 `gp`（`BulkUpdateGroupRatio`，失败仅 WARN 由 Refresher 兜底），避免与请求事务耦合。

### 公告（A13/A14）

4 条路由（`notice.write`）。新增内部态 `SCHEDULED`（E-32，无需迁移）：`POST` 带未来 `publish_at` → `SCHEDULED`，`PublishDueAnnouncements` 到点批量转 `PUBLISHED`；`DRAFT` 永不被时间触发。A14 用注入的 `clock.Fixed` 测，无 sleep。

**活动类冲突**：模型涨价在 7 天公示期内 → 拒绝发布「活动」类公告并给出冲突模型与生效时间。

**公告不回滚业务状态**（硬约束 12）：两条用例锁死——发布定时公告后、草稿发布+重复发布 409 后，`t_model.listing_state` 均未变；发布路径只 UPDATE `t_announcement`。

### 风控 / 用户 / 配置

- 风控（`risk.write`）：规则 CRUD（DELETE 为 `enabled=0`，保留命中归因）、命中查询、风险分调整含 before/after 审计
- 用户（读 `user.read`、写 `user.write`；读权限自 2026-11-02 从 `admin.read` 收窄，`OPS`/`SUPER_ADMIN` 持有）：列表筛选、详情（余额+套餐）、禁用/启用，`disable_source=OPS_MANUAL`
- **禁用原因不对外（硬约束 13）**：运营侧可见 `风控命中：异常退款-敏感原因-XYZ`，用户侧走真实 `auth.Service.Login` 返回「账号已被禁用，请联系客服」，断言不含原因文本
- 配置（`config.write`）：`GET` 返回 45 键 + `runtime_writable` 标记；`PUT` 只收 12 键。第 13 键、`quota.overdraft_warn_days`、`quota.*` 未实现取值、未登记键 → 全 400 `config_value_invalid`。**无「一键清缓存」接口**（反射用例锁死 `FlushConfigCache`/`ClearCache`/`PurgeConfig` 不存在），发布只带本次涉及的 namespace

### 遗留

1. ~~**`GET /api/admin/users` 用 `admin.read` 而非卡面的 `user.read`**（E-33）~~：**已修（2026-11-02）**。原做法是让运营读借道 `admin.read`，代价是同样持有 `admin.read` 的 `FINANCE` 也能读全量用户与渠道。现按资源作用域拆名：`PERSONAL` 改持 `account.read`（只开 `/api/console/overview`），`user.read` 收窄为运营目录读（`OPS`/`SUPER_ADMIN`），渠道读单列 `channel.read`（`OPS`/`SUPER_ADMIN`）。实测 `FINANCE` → `/api/admin/users`、`/api/admin/channels` 均 403 `permission_denied`，`/api/admin/overview` 仍 200。
2. **公告 `SCHEDULED` 态**（E-32）待回写 TDS。
3. **PRD F16-7「未配价不得上架」第一段未实现**（E-34）：允许无价 `LISTED`。已列专项修复，回填到模型状态机。
4. 公告 `model_ids` 只用于冲突检查，`t_announcement` 无模型列故不持久化。
5. 分组 K13 扇出为异步，响应 `key_sync.keys=0`（不等待）；测试注入 writer 取证。

### 补充修复 · F16-7 未配价不得上架（E-34）

`CreateModel` 与 `listing-state` 两个入口都加校验：切 `LISTED` 前必须存在**当前生效**的 `t_price_rule`（`enabled=1` 且 `effective_from <= now`，复用定价模块的 `activePriceRule`，未另立口径），否则 409 `resource_state_conflict` + hint 指向定价页。

**未来生效的价格不算当前有价**——另有专门用例锁死（只种一条 `effective_from=now+48h` 的行 → 仍拒绝）。

**连带行为变更**：`listing_state` 缺省值由 `LISTED` 改为 `CATALOG_ONLY`。原因：新行在 INSERT 前不可能有价，缺省 LISTED 会让所有普通创建都变 409。这是安全默认（新模型默认不上广场）。

5 条既有用例随之调整（均非删除）：上架前补配价、草稿数改基线增量、fixture 改 `CATALOG_ONLY`、公告用例断言由硬编码 `LISTED` 改为与实际状态比对。

**一处实现补充**：`apierr` 增加 `WithHint`，让单次拒绝可以覆盖 spec 的通用 hint（本例要从「刷新后重试」改成「去定价页」），仍走 `renderHint`/`sanitizeText`，码表不变。

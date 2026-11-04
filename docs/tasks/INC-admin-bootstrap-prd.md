# 增量 PRD：内置超级管理员 admin 启动引导

| 项目 | 内容 |
| --- | --- |
| Language | 中文 |
| Project Name | yunshu_api |
| 需求原文 | 项目后台应该默认存在一个超级管理员 admin，初始密码为 12345678 |
| 已拍板决策 | ① 登录标识为纯用户名 `admin`；② 所有环境（含 prod）启动时自动引导，只创建一次，已存在不重置密码，创建时打印"请立即修改初始密码"告警 |
| 范围 | 增量，不含竞品/市场调研 |

## 1. 背景与目标

**目标**：全新部署后无需人工介入即可获得一个可登录的超级管理员，打通"部署 → 登录运营后台"的第一步。

**为什么现在必须做（两个硬阻塞）**

1. **首管死锁**：创建 SUPER_ADMIN 的唯一入口 `POST /api/admin/users` 由 `admin.all` 保护（`internal/control/server.go`），必须持有 SUPER_ADMIN 会话才能调用。全新库没有任何超管 → 永远建不出第一个超管。当前只能靠人工改库绕过。
2. **没有可用登录标识**：`t_user` 只有 `email` / `phone`（`0001_account_credential.sql` 的 `uk_user_email` / `uk_user_phone`），登录只走 `NormalizeIdentifier` 的 email/phone 两分支。即使用 SQL 插一行超管，交付方也没有一个"约定俗成"的账号可登录（邮箱需要真实域名、手机需要真实号段），交付文档无法给出确定凭据。

**成功度量**：空库首次 `make run-controld` 后，用 `admin / 12345678` 可在 1 次尝试内登录并访问运营后台首页；重复启动不产生第二条 admin 记录、不改变其密码。

## 2. 用户故事

- **作为交付/实施工程师**，我希望按部署文档用固定账号 `admin` 登录运营后台，这样我不用先连数据库手工造账号就能完成验收。
- **作为平台超级管理员**，我希望首次登录后立刻把初始密码改掉，并且系统在启动日志里提醒我，这样默认口令不会长期留在生产环境。
- **作为运维（OPS）**，我希望重复发布、多副本滚动重启都不会生成第二个 admin 或把我的密码重置回初始值，这样发布不会把我锁在门外。

## 3. 需求池

### P0（必须）

| ID | 需求 | 验收标准（可度量） |
| --- | --- | --- |
| P0-1 | `t_user` 增加 `username` 列 + 唯一键 | 新增迁移（下一版本号）加 `username varchar(32) DEFAULT NULL`、`UNIQUE KEY uk_user_username (username)`；存量行保持 NULL；迁移可重复执行不报错、`down` 可回滚 |
| P0-2 | 登录标识扩展为三种 kind：email / phone / username | `NormalizeIdentifier` 新增 `KindUsername`；判定顺序固定为：含 `@` → email；否则去分隔符后**全为数字且长度 6..20** → phone；否则 → username。用户名规则：3..32 字节，首字符为字母，仅 `[a-z0-9._-]`，**归一化为小写**后入库/查询（避免 `Admin` 与 `admin` 双份账号）。不满足则返回与现有一致的 `invalid_credentials`，不新增错误码 |
| P0-3 | 登录查询按 kind 选列 | `findUser` 由 `Identifier.Kind` 映射列（email/phone/username），**不接受客户端指定字段**；账号不存在仍走 bcrypt 假哈希做时序均衡 |
| P0-4 | 启动引导建号（幂等） | control 进程启动、迁移完成后执行 `EnsureSuperAdmin`：<br>① 已存在 `username='admin'` → 不插入、不重置密码、不打印口令告警，只打 1 条 INFO；<br>② 不存在 → 插入 `role=SUPER_ADMIN`、`status=NORMAL`、`password_algo=bcrypt`、cost=12、`nickname='超级管理员'`、email/phone 为 NULL；<br>③ 并发/多副本安全：依赖 `uk_user_username`，捕获重复键错误即视为"已存在"，进程不退出；<br>④ 连续启动 3 次，`SELECT COUNT(*) FROM t_user WHERE username='admin'` 恒为 1，`password_updated_at` 不变 |
| P0-5 | 创建时输出"请立即修改初始密码"告警 | 首次插入成功后输出 1 条 WARN，文案含用户名与"请立即修改初始密码"；**日志中不出现密码**（不打印 `ADMIN_BOOTSTRAP_PASSWORD` 的值、不打印 hash）|
| P0-6 | 审计留痕 | 实际发生插入时写入 `t_admin_audit_log`（原稿笔误写 `t_audit_log`，已按 `internal/control/audit/audit.go:82` 更正）：`module=USER`、`action=CREATE`、`target_type=t_user`、`target_id=<新 uid>`、`operator_id=0`、`operator_role=SYSTEM`、`result=OK`；未插入时不写审计 |
| P0-7 | 环境变量与文档 | 新增 `ADMIN_BOOTSTRAP_USERNAME`（默认 `admin`）、`ADMIN_BOOTSTRAP_PASSWORD`（默认 `12345678`）、`ADMIN_BOOTSTRAP_DISABLED`（默认 `false`）；写入 `.env.example` 与部署文档（`docs/ops-runbook.md` / `docs/go-live-checklist.md` 增一条"上线后立即改 admin 密码"）；密码长度不满足 8..72 时**拒绝启动并报错退出**，不静默降级 |
| P0-8 | 登录页文案对齐 | `web/src/pages/site/LoginPage.tsx`：label 由"邮箱"改为"账号"，placeholder 改为"邮箱 / 手机号 / 用户名"；三处错误提示改为"请输入邮箱、手机号或用户名。"；页脚"暂只支持邮箱登录"改为"支持邮箱、手机号或用户名登录"。注册页文案不变 |

### P1（应当）

| ID | 需求 | 验收标准 |
| --- | --- | --- |
| P1-1 | 首次登录强制改密 | `t_user` 增 `must_change_password` 标记，登录后除 `auth/me`、`auth/password`、`auth/logout` 外全部接口返回 403 并携带明确 code；前端弹不可关闭的改密框；改密成功清除标记。理由见 §4 |
| P1-2 | 控制台常驻弱口令提示 | admin 未改密时，顶部栏常驻红条"当前使用初始密码，请立即修改"，点击直达 `/account` |
| P1-3 | `userView` 暴露 `username` / `phone` | 登录与 `auth/me` 响应新增 `username`（无则为空串），前端账号信息页展示；不破坏现有消费者（新增字段） |
| P1-4 | 运营后台"重置密码 + 强制下次改密" | SUPER_ADMIN 可对指定用户重置，写入 `must_change_password=1`，审计留痕 |

### P2（可选）

| ID | 需求 | 验收标准 |
| --- | --- | --- |
| P2-1 | 引导指标 | 暴露 `admin_bootstrap_total{result="created\|skipped\|failed"}`，供告警面板使用 |
| P2-2 | 用户名修改 | 超管可在运营后台改 username，受唯一键约束并审计 |

### 明确不做

- **不开放用户名自助注册**：注册仍只接受邮箱/手机；`username` 仅由引导或运营后台写入。
- **不改 PERSONAL 用户的登录语义**：纯数字标识仍按手机号解析（仅 6..20 位判定），历史账号行为零变化。
- **不做邮箱/手机与用户名的绑定、找回、迁移流程**。
- **只引导一个** `admin`，不做多超管批量种子，不做"内置账号"白名单机制。
- **不把 admin 硬编码进代码常量表**：账号与口令只从环境变量取，便于交付方覆盖。

## 4. 交互与文案要求

**输入判定规则（前后端一致，顺序即优先级）**

1. 含 `@` → 邮箱（沿用现有小写归一化与格式校验）。
2. 不含 `@`，去空格/`-`/`()`/`+`/`.` 后全为数字且长度 6..20 → 手机号（沿用现有语义）。
3. 其余 → 用户名：trim + 全角转半角 + 转小写；长度 3..32；必须含至少一个字母；仅 `[a-z0-9._-]`。**"必须含字母"这一条保证任何合法用户名都不会被误判成手机号**。
4. 客户端只提交 `identifier` 一个字段，服务端自行判定，不做前端下拉框选类型。

**文案**：见 P0-8。登录失败提示保持"账号或密码不正确"（不区分账号是否存在）。

**首次登录是否强制改密**：P0 **不做**，落到 P1-1。原因：强制改密需要新增列 + 全局路由拦截（会波及现有所有已登录流程与会话刷新路径）+ 前端不可关闭弹窗，是独立且风险更高的一次改动；而改密能力已存在（`POST /api/v1/auth/password`），P0 通过"启动 WARN 日志 + 部署文档 checklist"即可控制风险。P0 期间 admin 登录后手动改密即可达成同等效果。

## 5. 边界与安全约束

- **默认口令风险**：`12345678` 为公开弱口令。缓解：启动 WARN、部署文档强制改密步骤、登录失败锁定（现有 IP + 账号维度锁定对 admin 同样生效，默认口令反而更易被爆破，因此锁定不可豁免）。不因为"是内置账号"而放宽限流或跳过锁定。
- **日志红线**：创建日志只打印用户名与角色，**不打印密码、不打印 bcrypt hash、不打印 DSN**；环境变量值不得回显到任何日志或接口。
- **不覆盖原则**：`username='admin'` 已存在时一律跳过，即使其 `role` 不是 SUPER_ADMIN 或 `status` 不为 NORMAL（避免"重启一次就被悄悄提权"）。异常情况由运维排查，不由引导程序自动修复。
- **并发与幂等**：多副本同时启动必须以唯一键兜底而非先查后插；唯一键冲突不视为错误（不打 ERROR、不退出进程），最多 1 条审计。
- **审计**：创建、后续改密、重置均留痕；引导的审计 `operator_role=SYSTEM`，与人工操作可区分。
- **可关闭**：`ADMIN_BOOTSTRAP_DISABLED=true` 时完全跳过引导（供已有超管的存量环境或高安全要求环境使用），且跳过时不报错、只打 INFO。
- **不得影响迁移**：引导在迁移成功之后执行；引导失败（如 DB 不可写）应使进程启动失败并给出明确原因，而不是带着"没有超管"的状态继续提供服务。

## 6. 待确认问题

1. `admin` 是否受现有"最后一个超管不可降级/禁用"保护？建议**纳入保护**（禁止对 `username='admin'` 禁用或降级），需确认是否同意把该口令固定为不可变常量。
2. 生产环境是否**强制**用 `ADMIN_BOOTSTRAP_PASSWORD` 覆盖默认值（未设置则拒绝启动）？当前按已拍板决策为"不强制、默认 12345678 生效"，如后续要收紧只需改 P0-7 的一个分支。

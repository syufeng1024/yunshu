# 云枢（yunshu-api）

LLM API 中转与聚合网关：把多家**境内**上游大模型厂商的能力统一聚合到平台侧，对外只暴露一套标准协议。客户端只需替换 `base_url` 与 API Key，请求参数与上游官方完全一致。

> **V1.0 渠道范围**：仅接入境内上游厂商（阿里云百炼、DeepSeek 及后续境内厂商），不接入任何境外厂商。协议能力（OpenAI 兼容 / Anthropic 兼容 / 百炼协议）全量保留——「兼容」描述的是接入协议，与厂商来源地无关。

---

## 核心能力

平台在这条链路上统一承担：**鉴权 → 路由选路 → 限流控制 → 计费扣费 → 日志留痕 → 用量分析**。

| 能力 | 说明 |
| :--- | :--- |
| 多厂商聚合 | 一个 Key 调通多家模型，客户端无需感知上游差异 |
| 协议兼容 | OpenAI 兼容 / Anthropic 兼容 / 百炼协议 |
| 智能路由 | 同模型多路由按权重分发 + 失败自动降级 |
| 积分计费 | 1 元 = 100 积分，按 token 精确计费，失败请求不计费 |
| 四维限流 | RPM / TPM / 并发（per-Key）+ 账户日额度（per-User） |
| 全链路日志 | 每次调用留存请求 ID、耗时、扣费、重试链路 |

---

## 技术栈

| 层 | 技术 |
| :--- | :--- |
| 后端 | Go 1.27（`go.mod` 声明，发布构建用 ≥1.27.3） |
| 前端 | React + TypeScript + Vite，Vitest 单测 + Playwright e2e |
| 存储 | MySQL 8.0（账本/计费/日志）+ Redis（限流/缓存/分布式锁） |
| 部署 | Docker Compose（全栈容器化）+ 可选 k8s 清单 |

---

## 目录结构

```
yunshu-api/
├── cmd/                 # 可执行入口
│   ├── gatewayd/        #   数据面网关（对外 API + 限流 + 计费）
│   ├── controld/        #   控制面（密钥/模型/资金/运营 API）
│   ├── jobsd/           #   后台任务（对账/关单/日志清理）
│   ├── migrate/         #   迁移工具（up/down/checksum --verify）
│   ├── recondrill/      #   日对账演练
│   ├── keytool/         #   上游密钥信封加密工具
│   ├── seedctl/         #   演示数据种子
│   └── fakeupstream/    #   假上游（本地联调/测试）
├── internal/            # 核心业务包
│   ├── gw/              #   网关流水线 P01–P31（鉴权→路由→限流→预扣→结算）
│   ├── ratelimit/       #   四维限流 + 降级
│   ├── ledger/          #   账本（append-only，余额是物化视图）
│   ├── billing/         #   计费
│   ├── settle/          #   结算清算与对账
│   ├── dispatch/        #   上游分发与 SSE 转发
│   ├── config/          #   配置快照与策略注册表
│   └── ...              #   apierr / control / obs / recon / stream 等
├── relaykit/            # 独立子模块（replace 到本地）
├── web/                 # 前端（官网 + 用户控制台 + 运营后台 + 调试台）
├── deploy/              # Dockerfile、compose、k8s、seed
├── scripts/             # 构建/测试/门禁/备份脚本
├── docs/                # 需求、架构、运维文档（见下方索引）
└── api/                 # OpenAPI 定义
```

---

## 快速开始

### 环境要求

- Go ≥ 1.27（`go.mod` 声明；发布构建用最新 1.27.x）
- Docker Engine + Docker Compose
- GNU Make（Windows 需 Git Bash + GNU Make，没有 `make` 时用 Makefile 里的「等价命令」）

### 1. 本地开发（跑依赖 + 三进程）

```bash
make up           # 起 MySQL + Redis 依赖容器
make run-ctl      # 控制面 :8081（ENV=dev 时启动过程会自动补迁移，不必先 make migrate）
make run-gw       # 网关 :8080，metrics :9090
make run-jobs     # 后台任务
```

> 迁移时机：`ENV=dev/local` 时 `controld` 启动会先自动执行待应用迁移（持有 MySQL 咨询锁，
> 多副本安全）；其它环境默认不执行，由部署编排的 `migrate` 服务 / Job 先跑完。
> 想固定行为可显式设 `AUTO_MIGRATE=true|false`（见 `.env.example`）。手动迁移仍可用 `make migrate`。

进程就绪自检：

```bash
curl -s localhost:8081/healthz   # 200
curl -s localhost:8081/readyz    # 依赖就绪后 200（依赖抖动时非 200）
```

> 端口冲突时改 `.env`：`GATEWAY_HTTP_ADDR`、`CONTROL_HTTP_ADDR`、`GATEWAY_METRICS_ADDR` 等（见 `docs/dev-setup.md`）。

`controld` 启动时会幂等创建内置超级管理员：用 `admin` / `ADMIN_BOOTSTRAP_PASSWORD`（默认 `12345678`，见 `.env.example`）登录 `/admin`。登录后运营后台顶部会显示「仍在使用初始密码」的提醒条（只提醒、不拦截），改密后自动消失；已存在时不会被覆盖，也不会重置口令。要跳过引导设 `ADMIN_BOOTSTRAP_DISABLED=true`。

### 2. 全栈容器化部署（一条命令起完整站点）

```bash
make stack-env    # 生成 deploy/.env.stack（含随机密钥）
make stack-up     # MySQL + Redis + 三进程 + 前端
make stack-smoke  # 冒烟：迁移落库、三进程 ready、站点首页、内网门
```

首次部署后用 `admin` / `ADMIN_BOOTSTRAP_PASSWORD`（默认 `12345678`，在 `deploy/.env.stack` 里覆盖成强口令）登录 `/admin`，并**立即改密**；三个变量与适用场景见 `docs/ops-runbook.md` §9。

---

## 测试与质量门禁

```bash
make test         # go test -p 2 ./...
make vet          # go vet ./...
make ci           # 硬约束扫描（lint.sh + grep 断言）
```

- **覆盖率门禁**：`bash scripts/coverage-gate.sh`（ledger ≥75 / billing ≥83 / apierr ≥90 / overall ≥59）。
- **前端**：`cd web && npm run verify`（typecheck + vitest + check:copy + build）。
- **CI**：`.github/workflows/ci.yml` 六个 job（static / unit -race / migrations / coverage / e2e / acceptance）。

> 注意：覆盖率数字依赖 Go 版本口径。本机 Go 版本与 `go.mod` 声明不一致时，本地覆盖率可能误报绿；验收务必与 CI 用同一 Go 版本（`go.mod` 声明 `go 1.27`，CI 会解析到最新 1.27.x）。详见 `docs/test-matrix.md`。

---

## 文档索引

| 文档 | 说明 |
| :--- | :--- |
| `docs/_API 中转站 · 需求文档 V1.2.md` | 产品需求（PRD） |
| `docs/技术架构设计文档 V1.2.md` | 技术架构（TDS） |
| `docs/dev-setup.md` | 本地开发环境与行尾约定 |
| `docs/config-keys.md` | 配置键总表 |
| `docs/control-api.md` | 控制面 API |
| `docs/finance-rules.md` / `fund-flow.md` / `settlement.md` | 资金口径、资金流、结算 |
| `docs/degradation.md` | 降级矩阵与限流降级 |
| `docs/ops-runbook.md` | 运维手册 |
| `docs/go-live-checklist.md` | 上线清单 |
| `docs/tasks/` | 任务卡（T01–T36）与 BACKLOG 台账 |

---

## 部署状态

代码质量已符合上线条件；发布配置（`VITE_API_BASE_URL` 生产注入等）按需求方决策暂缓。详见 `docs/tasks/delivery/go-live.md`。

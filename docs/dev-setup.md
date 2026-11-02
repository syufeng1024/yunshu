# 本地开发环境（T01 基线）

目标：五个步骤跑通 `gatewayd` / `controld` / `jobsd` 骨架，并验证 `healthz` 与 `readyz` 的语义差异。

## 1. Clone 与工具链

```bash
git clone <repo-url> yunshu-api
cd yunshu-api
go version                 # 语言级别 >= 1.24；发布构建使用 >= 1.27.3
docker version             # 需要 Docker Engine 可用
docker compose version
```

`go.mod` 中的 `go 1.24.0` 是最低语言版本，不锁定 toolchain。`govulncheck` 在 go1.27.1 上报告 7 条标准库可达漏洞（GO-2026-6607 等，修复于 go1.27.3）；发布构建使用 go1.27.3 或对应版本的最新补丁，当前代码在 go1.27.3 下扫描结果为 0 条可达漏洞。

`make` 需要 GNU Make。Windows 仅在 Git Bash + GNU Make 下可用；没有 `make` 时执行下方“等价命令”列。

### 行尾（Windows / `core.autocrlf=true`）

`core.autocrlf=true` 的 checkout 会把文本转成 CRLF，本机 607 个非 `web/` 已跟踪文件中 467 个是 CRLF（含 360 个 `.go`，2026-10-28 实测），这是预期状态。**不要执行 `git add --renormalize .`**：会把 400+ 文件塞进同一次 diff，且与 autocrlf 的行为冲突。

只有「字节被 SHA-256 校验」或「被 embed 且不做行尾归一」的文件必须为 LF。否则同一份代码在 Windows 与 CI 上构建出的镜像内嵌不同字节，`schema_migrations` 记录的 checksum 对不上，`migrate up` 报：

```
迁移 0001 的 checksum 不匹配：库中 55b0b46…，文件 78273f6…
```

强制清单由 `scripts/check-eol.sh` 维护：

| 路径 | 数量 | 原因 |
| :--- | :--- | :--- |
| `internal/platform/migrate/sql/mysql/*.sql` | 46 | 文件字节的 SHA-256 写入 `schema_migrations` |
| `internal/platform/migrate/checksums.golden` | 1 | `golden_test.go` 与 embed 迁移逐条比对 |

`internal/{breaker,idemstore,lock,ratelimit}/scripts/*.lua`（6 个）也被 embed，但不参与任何 checksum，Lua 引擎容忍 `\r`；`.gitattributes` 已钉 LF，`check-eol.sh` 不强制检查这一项。

检查入口：

```bash
bash scripts/check-eol.sh    # 正常：无输出、退出码 0；命中：列出文件 + 修复命令、退出码 1
bash scripts/lint.sh         # 第 10 项；scripts/dev.sh ci 与 CI static job 都会执行到这里
```

`.gitattributes` 只对新 clone 生效。**规则加入之前就已存在的旧工作区不会自动重写**，一次性修复如下。`git checkout -- <path>` 会因 stat 缓存把这些文件当作未改动而跳过，必须先 `touch` 改变 mtime，再用 `git checkout-index -f` 从索引强制覆盖：

```bash
# 迁移 SQL 与 golden：--renormalize 先把 LF 版本写进索引，未提交的内容改动不会丢
git add --renormalize internal/platform/migrate/sql/mysql/ internal/platform/migrate/checksums.golden
touch internal/platform/migrate/sql/mysql/*.sql internal/platform/migrate/checksums.golden
git checkout-index -f -- internal/platform/migrate/sql/mysql/*.sql internal/platform/migrate/checksums.golden

# Lua 同理
git add --renormalize internal/breaker/scripts/*.lua internal/idemstore/scripts/*.lua internal/lock/scripts/*.lua internal/ratelimit/scripts/*.lua
touch internal/breaker/scripts/*.lua internal/idemstore/scripts/*.lua internal/lock/scripts/*.lua internal/ratelimit/scripts/*.lua
git checkout-index -f -- internal/breaker/scripts/*.lua internal/idemstore/scripts/*.lua internal/lock/scripts/*.lua internal/ratelimit/scripts/*.lua

bash scripts/check-eol.sh    # 确认无输出、退出码 0
```

TODO(T26)：release 流水线尚未校验「镜像内 embed checksum == `checksums.golden`」；该校验需要在 CI 内起容器执行，另开卡处理。

## 2. 启动依赖

```bash
cp .env.example .env
make up
docker compose -f deploy/docker-compose.yml ps
```

| 容器                           | 期望状态      | 端口     |
| :--------------------------- | :-------- | :----- |
| `yunshu-mysql` (`mysql:8.0`) | `healthy` | `3306` |
| `yunshu-redis` (`redis:7`)   | `healthy` | `6379` |

`make up` 失败时先看 `deploy/README.md` 的端口占用与健康检查两行。3306/6379 被其他项目占用时，停掉占用方；不要把 DSN 指向本机 MySQL 8.4。

## 3. 迁移

```bash
make migrate
# 输出：T01 has no migrations; schema migrations start in T02.
```

T01 只建空 schema（`yunshu`、`yunshu_log`），建表从 T02 开始。

## 4. 启动进程

三个终端分别执行：

```bash
make run-ctl      # 控制面 :8081
make run-gw       # 网关 :8080，metrics :9090
make run-jobs     # 后台任务，无对外端口
```

等价命令（Windows 无 make 时）：

```bash
set -a; . ./.env; set +a; go run ./cmd/controld
set -a; . ./.env; set +a; go run ./cmd/gatewayd
set -a; . ./.env; set +a; go run ./cmd/jobsd
```

期望日志为单行 JSON，含 `ts,level,svc,env,pid,trace_id,request_id,uid,kid,model,stage,latency_ms,status,msg` 全部键。

## 5. 探针验证

```bash
curl -s localhost:8081/healthz
# {"status":"ok"}

curl -s -o /dev/null -w '%{http_code}\n' localhost:8081/readyz
# MySQL 与 Redis 均可用：200

docker compose -f deploy/docker-compose.yml stop redis
curl -s -o /dev/null -w '%{http_code}\n' localhost:8081/healthz   # 200
curl -s localhost:8081/readyz; echo
# 503 {"checks":{"config":"ok","mysql":"ok","redis":"down"},"status":"not_ready"}
docker compose -f deploy/docker-compose.yml start redis
```

判定规则：

| 端点         | 探测依赖           | Redis 停止后的结果                   |
| :--------- | :------------- | :----------------------------- |
| `/healthz` | 否，只反映进程存活      | 仍 `200 {"status":"ok"}`        |
| `/readyz`  | Redis、MySQL、配置 | `503`，`checks.redis == "down"` |

`jobsd` 没有 HTTP 端口；通过日志确认任务表已注册，`Ctrl+C` 后应出现 `drain started` 与 `drain complete`。

## 环境变量

| 变量                      | 必填进程                  | 说明                                |
| :---------------------- | :-------------------- | :-------------------------------- |
| `ENV`                   | 三进程                   | `dev` / `staging` / `prod`，写入每条日志 |
| `SESSION_SECRET`        | `gatewayd`、`controld` | 会话签名密钥                            |
| `DB_DSN`                | `controld`、`jobsd`    | 主库读写 DSN                          |
| `LOG_SQL_DSN`           | 三进程                   | 日志库 DSN（同实例独立 schema）             |
| `GW_WRITE_DSN`          | `gatewayd`            | 网关唯一主库受限通道                        |
| `REDIS_ADDR`            | 三进程                   | `host:port`                       |
| `REDIS_PASSWORD`        | 可选                    | 空表示无密码                            |
| `GATEWAY_HTTP_ADDR`     | 可选                    | 网关监听地址，默认 `:8080`；被占用时改（本机用 `:8082`） |
| `CONTROL_HTTP_ADDR`     | 可选                    | 控制面监听地址，默认 `:8081`                 |
| `GATEWAY_METRICS_ADDR`  | 可选                    | 默认 `:9090`                        |
| `GATEWAY_METRICS_TOKEN` | 可选                    | 非空时 `/metrics` 要求 Bearer          |
| `GATEWAY_DB_MAX_CONNS`  | 可选                    | 默认 `8`，必须 `>= 1`                  |
| `GW_RATELIMIT_DEGRADE_MODE` | 可选（`gatewayd`）    | **默认 `off`**；可设 `concurrency_only`，**其它值启动失败**。为什么默认 off（以及什么条件下才该改回去）见 `docs/degradation.md` §1.1 与 errata E-38 |

缺失必填变量时进程不会逐条报错：一次列出全部缺失项后 `exit 1`。

## 常用命令

```bash
go build ./... && go vet ./... && go test ./...
make check          # vet + test + encoding/json、context.Background、许可断言（Go/SQL 无 new-api 代码字符串）
make down           # 停止依赖，保留数据卷
```

## 端口一览与本机冲突处理

| 用途 | 默认 | 本机实际 | 说明 |
| :--- | :--- | :--- | :--- |
| MySQL | `3306` | **`13306`** | 3306 被本机 MySQL 8.4 占用，用 `deploy/docker-compose.portmap.yml` 换宿主端口 |
| Redis | `6379` | **`16379`** | 6379 被其他项目容器占用 |
| 网关 `gatewayd` | `:8080` | **`:8082`** | 8080 被其他项目容器占用，`.env` 里 `GATEWAY_HTTP_ADDR=:8082` |
| 控制面 `controld` | `:8081` | `:8081` | 空闲 |
| 网关 metrics | `:9090` | `:9090` | 空闲 |
| 前端 dev | `5180` | `5180` | 5173 被其他项目容器占用，`VITE_DEV_PORT` 可改 |

**规则**：容器内端口永远是 3306 / 6379，只有宿主侧可变；应用监听地址一律走环境变量，不要在代码里写死。

```bash
scripts/dev.sh ports        # 打印当前解析到的端口与是否套用覆盖文件
scripts/dev.sh env          # 按解析结果重写 .env（DSN / REDIS_ADDR）
```

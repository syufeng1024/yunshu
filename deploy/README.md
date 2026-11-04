# 部署说明

两条路径，别混用：

| 目录 / 文件 | 用途 | 应用进程跑在哪 |
| :--- | :--- | :--- |
| `docker-compose.yml` | **本地开发**：只起 MySQL + Redis | 宿主机 `make run-*` / `scripts/dev.sh run-*` |
| `docker-compose.stack.yml` | **全栈部署**：存储 + 三进程 + 前端全部容器化 | 容器内 |

下面的「本地依赖服务」讲第一条；「全栈容器化部署」讲第二条。

---

# 本地依赖服务

`docker-compose.yml` 只启动 MySQL 8.0 与 Redis 7，应用进程由 `make run-*` 在本机运行。

## 端口与镜像

| 服务 | 镜像 | 宿主端口 | 数据卷 | 关键参数 |
| :--- | :--- | :--- | :--- | :--- |
| MySQL | `mysql:8.0` | `3306`（本机覆盖为 `13306`） | `mysql_data` | `--character-set-server=utf8mb4 --collation-server=utf8mb4_0900_as_cs` |
| Redis | `redis:7` | `6379`（本机覆盖为 `16379`） | `redis_data` | `--appendonly yes --appendfsync everysec` |

MySQL 固定 8.0，不使用本机 MySQL 8.4（架构决策 GR-01）。初始化脚本 `mysql/init/00-grants.sql` 建 `yunshu` 与 `yunshu_log` 两个 schema，排序规则均为 `utf8mb4_0900_as_cs`。

## 凭据边界

`MYSQL_ROOT_PASSWORD` 缺省为 `root`，且 `MYSQL_ROOT_HOST=%` 允许宿主进程直连。这两项只服务于本地开发；生产环境的口令必须由密钥管理注入，不得复用此 compose 文件中的默认值。

## 起停

```bash
make up      # docker compose -f deploy/docker-compose.yml up -d --wait
make down    # 保留数据卷
docker compose -f deploy/docker-compose.yml ps
```

`make up` 会等待两个 healthcheck 通过；任一服务不健康时命令以非零码退出。

### 无 make 的机器

`scripts/dev.sh` 是 Makefile 的等价入口（本机未安装 GNU make，两条路径共存，行为一致）：

```bash
scripts/dev.sh up      # 自动探测端口占用，必要时套用覆盖文件
scripts/dev.sh env     # 按探测到的端口生成 .env
scripts/dev.sh check   # vet + test + 三条 grep 断言
scripts/dev.sh run-ctl # 载入 .env 后启动控制面
```

### 宿主端口已被占用时

若 3306 / 6379 已被本机 MySQL 8.4 或其他项目容器占用，套用覆盖文件把**宿主侧**端口换掉（容器内仍是 3306 / 6379，schema 与排序规则不受影响）：

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.portmap.yml up -d --wait
# 或
scripts/dev.sh up    # 探测到占用会自动加 -f deploy/docker-compose.portmap.yml
```

覆盖文件必须用 `ports: !override`：compose 对序列默认是**追加**，普通写法会同时发布 3306 而继续冲突。

换了宿主端口后 `.env` 里的 DSN 与 `REDIS_ADDR` 必须同步（`scripts/dev.sh env` 会自动改写）：

```
DB_DSN='root:root@tcp(127.0.0.1:13306)/yunshu?...'
REDIS_ADDR=127.0.0.1:16379
```

**不得**改为指向本机 MySQL 8.4（GR-01 锁 8.0）。

## 验证

```bash
# 服务端字符集与排序规则
docker exec -i yunshu-mysql mysql -uroot -proot -N \
  -e "SELECT @@collation_server, @@character_set_server"
# 期望：utf8mb4_0900_as_cs  utf8mb4

# schema 排序规则
docker exec -i yunshu-mysql mysql -uroot -proot -N \
  -e "SELECT schema_name, default_collation_name FROM information_schema.schemata \
      WHERE schema_name IN ('yunshu','yunshu_log')"

# Redis AOF
docker exec -i yunshu-redis redis-cli CONFIG GET appendonly appendfsync
# 期望：appendonly yes / appendfsync everysec
```

## 常见故障

| 现象 | 原因 | 处理 |
| :--- | :--- | :--- |
| `port is already allocated` / `bind: address already in use` | 3306 或 6379 被本机服务或其他容器占用 | 加 `-f deploy/docker-compose.portmap.yml` 换宿主端口（或 `scripts/dev.sh up` 自动处理）；**不得**把 DSN 改指本机 MySQL 8.4 |
| MySQL 健康检查一直 `starting` | 首次初始化需要 20–60s | 看 `docker logs yunshu-mysql`；`start_period` 已设为 40s |
| `Access denied for user 'root'` | `.env` 的 `MYSQL_ROOT_PASSWORD` 与卷内旧密码不一致 | 删除卷重建（见下）或改回旧密码 |
| 建库脚本未执行 | `mysql_data` 卷已存在，初始化脚本只在空卷首次启动时运行 | `docker compose -f deploy/docker-compose.yml down -v` 后重新 `make up` |
| Redis 重启后数据仍在 | AOF everysec 正常行为 | 不需要处理；Redis 全丢也不影响资金正确性 |

清空本地数据：

```bash
docker compose -f deploy/docker-compose.yml down -v
```

该命令删除 `mysql_data` 与 `redis_data`，不可恢复；只用于本地开发。

---

# 全栈容器化部署

`docker-compose.stack.yml` 把整个项目装进容器：MySQL 8.0、Redis 7、三个 Go 进程
（`gatewayd` / `controld` / `jobsd`）、一次性迁移任务，以及 nginx 托管的前端站点。

project name 是 `yunshu-stack`，与开发用的 `yunshu` 不同，两套可以同时跑。

## 拓扑

```
                    ┌──────────── edge 网络（发布宿主端口）────────────┐
浏览器 ──:8088──▶ web (nginx)
                    │   /api /console /admin /healthz /readyz
                    │        └──────────────▶ controld:8081  ──:18081
公网 ───:18080──▶ gatewayd:8080  (/v1/ 数据面)
                    └───────────────────────────────────────────────┘
                                     │
                    ┌──────────── backend 网络（不发布端口）──────────┐
                    │  mysql:3306      redis:6379      jobsd（无端口） │
                    └───────────────────────────────────────────────┘
```

关键约束：

- **MySQL / Redis 不发布宿主端口。** 这与开发 compose 相反——开发时宿主上的
  `go run` 必须能连 `13306/16379`，容器里则没有这个需求，不发布就是最小的暴露面。
- **站点与控制面同源。** nginx 把 `/api`、`/console`、`/admin` 反代到 `controld:8081`，
  浏览器只看到一个 origin，构建时不需要 `VITE_API_BASE_URL`。
- **迁移是独立的一次性任务。** 三个进程都用
  `depends_on: migrate: service_completed_successfully` 等它退出 0 才启动；
  没有待应用迁移时它同样退出 0，可以反复执行。
- **`gatewayd` 的排水期是 120s**，所以 `stop_grace_period: 130s`。
  短于这个值，`docker compose down` 会在在途请求结算到一半时把容器 SIGKILL 掉。

## 文件

| 文件 | 作用 |
| :--- | :--- |
| `deploy/Dockerfile.app` | 多阶段构建出 `gatewayd` / `controld` / `jobsd` / `migrate` 四个静态二进制，运行在 alpine 非 root 用户下 |
| `deploy/Dockerfile.web` | node 阶段 `npm ci && npm run build`，nginx 阶段托管 `dist` 并反代控制面 |
| `deploy/nginx/default.conf` | SPA 兜底、静态资源长缓存、控制面反代、`resolver 127.0.0.11` 延迟解析 |
| `deploy/docker-compose.stack.yml` | 全栈编排 |
| `deploy/docker-compose.stack.debug.yml` | 排障覆盖：把 MySQL / Redis 绑到宿主回环 |
| `deploy/.env.stack.example` | 环境模板，复制成 `deploy/.env.stack` 使用（后者已 gitignore） |
| `scripts/stack.sh` | 一键入口（本机没有 GNU make，与 Makefile 的 `stack-*` 等价） |
| `scripts/smoke.sh` | 部署后冒烟自检：容器/迁移/探针/前端路由/内网门逐条断言，失败非 0 退出 |
| `.dockerignore` | 构建上下文排除 `.git` / `node_modules` / 密钥文件 |

## 起停

```bash
scripts/stack.sh env      # 生成 deploy/.env.stack：随机密钥 + 自动挑空闲端口
scripts/stack.sh up       # 构建镜像并启动，等全部 healthcheck 通过
scripts/stack.sh smoke    # 冒烟：迁移版本 / 表数量 / 探针 / 站点（转发 scripts/smoke.sh）
scripts/stack.sh logs gatewayd
scripts/stack.sh down     # 停服务，保留数据卷
scripts/stack.sh reset    # 连数据卷一起删（需输入 yes 确认，不可恢复）
```

没装脚本环境时，等价的 compose 命令：

```bash
docker compose --env-file deploy/.env.stack -f deploy/docker-compose.stack.yml up -d --build --wait
```

`deploy/.env.stack` 里只有 `MYSQL_ROOT_PASSWORD` 需要人工确认；`SESSION_SECRET`、
`CHANNEL_KEY_KEK`、`GATEWAY_METRICS_TOKEN` 由 `env` 子命令随机生成。

### 首次部署：登录 → 改密

`controld` 启动时会把内置超级管理员建出来（幂等，已存在就跳过，不改角色也不重置口令）。两个步骤不能省：

1. **登录**：打开 `http://<宿主>:8088/admin`，账号 `ADMIN_BOOTSTRAP_USERNAME`（默认 `admin`），
   口令 `ADMIN_BOOTSTRAP_PASSWORD`（默认 `12345678`，**公开弱口令**）。登录页需要勾选用户协议。
2. **改密**：登录后 `POST /api/v1/auth/password`（`{old_password, new_password}`，带 `X-CSRF-Token`），
   成功后该账号的全部会话会被吊销，重新登录一次确认。

相关的三个变量（`ADMIN_BOOTSTRAP_USERNAME` / `ADMIN_BOOTSTRAP_PASSWORD` / `ADMIN_BOOTSTRAP_DISABLED`）
在 `deploy/.env.stack.example` 里有注释；它们只被 `controld` 读取，但因为在
`docker-compose.stack.yml` 的 `x-app-env` 白名单里，改 `.env.stack` 才会生效。
存量环境已经有超管、不想再跑引导时设 `ADMIN_BOOTSTRAP_DISABLED=true`。
k8s 部署同理：`deploy/k8s/configmap.yaml` 只放非密配置，带口令的
`ADMIN_BOOTSTRAP_PASSWORD` 应进凭据资源；不设置时用的是代码里的公开默认值，
所以那条路径上线后**必须先改密**。

## 端口

| 服务 | 宿主端口 | 说明 |
| :--- | :--- | :--- |
| web | `8088` | 站点入口（nginx） |
| gatewayd | `18080` | 数据面 `/v1/`，对外 API |
| controld | `18081` | 控制面，同时被 web 内网反代 |
| mysql | 不发布 | 仅 backend 网络 |
| redis | 不发布 | 仅 backend 网络 |
| gatewayd metrics | 不发布 | 容器内 `:9090`；要抓取时加端口映射**并**设 `GATEWAY_METRICS_TOKEN` |

端口已被占用时 `scripts/stack.sh env` 会从默认值顺延（最多试 30 个），把实际值
写回 `deploy/.env.stack`。已生成的 env 文件不会被静默覆盖，要重来用 `env --force`。

### 需要从宿主连数据库排障时

默认不发布存储层端口是刻意的。真要连进去看数据，套调试覆盖文件——只绑回环，
端口沿用开发编排的 `13306 / 16379`：

```bash
docker compose --env-file deploy/.env.stack \
  -f deploy/docker-compose.stack.yml \
  -f deploy/docker-compose.stack.debug.yml up -d

mysql -h127.0.0.1 -P13306 -uroot -proot yunshu
redis-cli -h 127.0.0.1 -p 16379
```

覆盖文件用 `ports: !override`：普通写法是**追加**，会在发布回环端口的同时保留
原配置，语义不明确。排障完把 `-f ...debug.yml` 去掉重新 `up -d` 即可收回端口。

## 生产接入：TLS 与健康端点

### TLS 的边界在容器之外

`web` 容器**只监听 80，不持有证书**。生产环境由前置网关 / SLB 终止 TLS，再把
明文请求转给容器。这样做的理由：证书续期、加密套件、HTTP/2 与 HSTS 都在那一层
统一管，换证书不需要重启应用，也不需要把私钥塞进镜像。

由此带来一个必须处理的细节：TLS 终止之后，nginx 看到的 `$scheme` 永远是 `http`。
如果直接把它传给应用，应用会认为请求是明文的，进而漏掉 `Secure` cookie、
生成 `http://` 的绝对 URL（重定向、回调地址都会错）。

`default.conf` 里的 `map $http_x_forwarded_proto $forwarded_proto` 就是为此存在：
优先采信上游给的 `X-Forwarded-Proto`，没有才回落到 `$scheme`。**前置网关必须
带上这个头**，否则会退化成 http 语义。

| 层 | 监听 | 证书 | 责任 |
| :--- | :--- | :--- | :--- |
| SLB / 网关 | 443 | 有 | TLS 终止、HSTS、限速、注入 `X-Forwarded-Proto` |
| `web` 容器 | 80 | 无 | 静态资源、SPA 兜底、反代控制面 |

### `/healthz` 与 `/readyz` 已限制为私网可达

这两个端点默认只对回环与私网（`127.0.0.0/8`、`10/8`、`172.16/12`、`192.168/16`）
放行，其余来源一律 403：

```nginx
location = /readyz {
    allow 127.0.0.1;
    allow 10.0.0.0/8;
    allow 172.16.0.0/12;
    allow 192.168.0.0/16;
    deny all;
    ...
}
```

理由不只是「习惯」：`/readyz` 的响应体会逐个列出 `redis` / `mysql` / `config`
的状态，等于把内部依赖拓扑和故障面公开给任何能访问它的人。

**探针怎么配**：

- 容器自身的 healthcheck 不走这两个 location。`web` 探的是 `/index.html`，
  `gatewayd` / `controld` 直接探各自的 `8080` / `8081`，所以收紧这段不影响编排。
- K8s / Compose 探针在私网内，直接落在放行网段里，无需改动。
- 如果 SLB 用**公网 IP 回源**，把那个 IP 单独加一行 `allow`，
  不要图省事改 `allow all`。
- 更干净的做法是让探针绕开 nginx，直连 `controld:8081/readyz`，容器间流量不出网。

## 验证

```bash
# 迁移是否落库
docker exec yunshu-stack-mysql mysql -uroot -proot -N \
  -e "SELECT version FROM yunshu.schema_migrations ORDER BY version DESC LIMIT 5"

# 探针：/healthz 只报进程活着，/readyz 会逐个检查 redis / mysql / config 快照
curl -s http://127.0.0.1:18080/readyz
curl -s http://127.0.0.1:18081/readyz

# 站点与同源反代
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8088/
curl -s http://127.0.0.1:8088/healthz
```

`readyz` 返回 503 时看 body 里的 `checks`，哪个依赖 `down` 就是哪个没起来，
比翻日志快。

## 上线前必须处理

这几项在本地跑通没问题，直接对外就是事故：

1. **`MYSQL_ROOT_PASSWORD` 仍是 `root`，且 DSN 用 root 连库。** 生产必须换成
   受限账号 + 密钥管理注入，不能复用 compose 里的默认值。具体做法见
   `deploy/.env.stack.example` 顶部的告示（含建账号与授权的 SQL）。
2. **没有 TLS。** `web` 容器只监听 80，站点与 API 都是明文；必须在前面挂反向
   代理或负载均衡终止 TLS，并且**必须带 `X-Forwarded-Proto`**。详见
   「生产接入：TLS 与健康端点」。
3. **`/healthz`、`/readyz` 已默认只对私网放行**（`deny all` + 四个私网 allow）。
   如果 SLB 用公网 IP 回源，需要单独加一行 allow；不要改成 `allow all`。
4. **`CHANNEL_KEY_KEK` 决定上游密钥能否解密。** 空值时进程照常启动，但每次
   上游分发都返回 `503 config_unavailable`——没有明文回退。轮换 KEK 需要重加密
   已存的渠道密钥，不能直接替换。
5. **单副本。** `jobsd` 靠 `t_job_lock` 选主，多副本本身是安全的；但 `mysql` /
   `redis` 都是单点，没有主从。上生产要换成托管实例或补上高可用。
6. **`gatewayd` 的 metrics 没有认证就不该出网。** 空 token 时 `/metrics` 谁都能读。
7. **内置超管的初始口令必须改。** `ADMIN_BOOTSTRAP_PASSWORD` 的默认值 `12345678`
   是公开弱口令；不改密等于把运营后台的钥匙挂在门上。改法见「首次部署：登录 → 改密」，
   检查项见 `docs/go-live-checklist.md` 第 27 项。

## 当前阶段：站点数据为什么是空的

这一条不是部署问题，写在这里免得后来者反复排查。

前端的模型 / 定价 / 公告 / 接入点区块走的是 `/api/public/models`、`/api/public/plans`
等公开接口，这些接口属于 **T12 / T15**，controld 目前还没注册，请求返回 **404**。
所以页面上这些区块显示的是错误态（带重试按钮），而不是数据——无论库里有没有数据。

已经验证的事实：`ENV=local` 跑迁移会写入 0019 的种子（1 渠道 / 2 模型 / 2 别名 /
2 路由 / 2 价目规则），数据确实在 `t_model` 等表里，中文编码完好。**缺的是读取
这些数据的 HTTP 接口，不是数据本身。**

在接口补齐之前要预览渲染效果，只有一条合规路径：本地 dev server 开 fixture

```bash
cd web
VITE_USE_FIXTURES=1 npm run dev      # 端口 5180
```

fixture 分支由 `import.meta.env.DEV` 守护，生产构建里会被 Vite 静态消除，且
`vite.config.ts` 的 `assertNoFixtures` 会断言产物中不含 fixture 标记——所以
**不能**靠改构建参数把一个带假数据的镜像发出去，那条路是被刻意堵死的。

## 常见故障

| 现象 | 原因 | 处理 |
| :--- | :--- | :--- |
| `migrate` 容器 exited 非 0，三个进程不启动 | 迁移 SQL 失败，或 MySQL 还没真正可写 | `docker compose logs migrate`；MySQL 首次初始化要 20–60s，`start_period` 已设 40s |
| `readyz` 返回 503 且 `config: down` | 配置快照没加载，通常是没有迁移数据或 Redis 里没有快照键 | 确认 `migrate` 已成功、`redis` 健康；看 gatewayd 日志里的 `config snapshot unavailable` |
| `upstream` 调用全部 503 `config_unavailable` | `CHANNEL_KEY_KEK` 为空或不合法（必须是 base64 的 32 字节） | 重新生成后 `scripts/stack.sh up` |
| nginx 启动即退出，日志 `host not found in upstream` | 用了字面量上游地址 | 配置里已改成 `set $control` + `resolver 127.0.0.11`；自行改配置时别退回字面量 |
| 页面刷新 404 | SPA 兜底被改掉 | `location /` 必须保留 `try_files $uri $uri/ /index.html` |
| 构建卡在 `go mod download` | 容器内出网慢 | `docker build --build-arg GOPROXY=https://goproxy.cn,direct ...`，Dockerfile 已是该默认值 |
| 构建卡在 `npm ci` | 同上 | `--build-arg NPM_REGISTRY=https://registry.npmmirror.com`（已是默认） |

## 与开发编排的关系

`deploy/docker-compose.yml`（本地依赖）和 `deploy/docker-compose.stack.yml`（全栈）
用不同 project name，各自有独立的 `mysql_data` / `redis_data` 卷：

- 两套数据**不共享**。开发时灌的测试数据不会出现在全栈环境里，反之亦然。
- 不要在 3306 上同时起两套——开发编排会发布 3306/13306，全栈不发布，所以冲突
  只会发生在开发编排和其他占用 3306 的服务之间。

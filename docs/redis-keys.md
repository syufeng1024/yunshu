# Redis 键空间运维手册（T06）

30 个键（K1–K30）与验证码键族（K-VC，见下文）的唯一构造函数都在 `internal/redisx/keys.go`。业务代码不得自行拼接键名；同一键的两种拼写会把一个计数器拆成两个，等于放大限额。

Redis 版本前提 5.0.9：只用 `EVAL`、`PEXPIRE`、`EXPIRE`、`SET NX PX`、`ZADD`、`ZREMRANGEBYSCORE`、`ZCARD`、`TIME`、`PUBLISH`、`HGET`/`HSET`/`HINCRBY`、`INCR`/`INCRBY`、`DEL`、`EXISTS`、`TTL`/`PTTL`、`MGET`、`SADD`/`SREM`/`SMEMBERS`、`WATCH`、`XADD`/`XREADGROUP`/`XACK`/`XPENDING`/`XCLAIM`。`XAUTOCLAIM`、`ZADD GT/LT`、`FUNCTION`、`GETDEL`、`GETEX`、`COPY` 不可用。

`maxmemory-policy` 必须是 `noeviction`。`internal/redisx.Client.CheckPolicy` 在启动时读取 `CONFIG GET maxmemory-policy`，不是 `noeviction` 即失败。被驱逐的限流计数器会静默放大额度，被驱逐的预扣哨兵会让重放看起来像新请求。

## 键清单

| # | 键名模板 | 构造函数 | 类型 | TTL | 写入方 | 失效 / 清理 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| K1 | `rl:rpm:{key_id}:{min_epoch}` | `RPMLKey` | STRING | 130s | L1 通过后 `INCRBY 1` | 到期；L1 内首次写入设 TTL |
| K2 | `rl:tpm:{key_id}:{min_epoch}` | `TPMKey` | STRING | 130s | L1 占位、L2 结算差额 | 到期；跨分钟不补偿 |
| K3 | `rl:cc:{key_id}` | `ConcurrencyKey` | ZSET（member=`request_id`，score=租约到期毫秒） | 无键 TTL | L1 `ZADD`、L2 `ZREM` | 每次 L1 `ZREMRANGEBYSCORE -inf now_ms` |
| K4 | `rl:dq:{user_id}:{yyyymmdd_cst}` | `DailyQuotaKey` | STRING（微积分） | 到次日 00:10（UTC+8）+ 600s | L1 预扣、L2 结算/释放 | 到期；旧日补偿只写仍存在的键 |
| K5 | `rl:dqr:{user_id}:{yyyymmdd_cst}` | `DailyQuotaRejectedKey` | STRING | 同 K4 | 日额度拦截后 `Limiter.RecordDailyRejection` `INCRBY 1` | 到期 |
| K6 | `idem:{user_id}:{sha1(scope\|api_key_id\|idem_key)}` | `IdemKey` / `IdemHash` | STRING（`state\|request_id\|created_ms\|body_hash\|fence`） | 24h + 0~300s 抖动 | `idemstore.Store.Begin` / `Complete` | 到期；DB 是权威 |
| K7 | `idem:lock:{user_id}:{sha1(...)}` | `IdemLockKey` | STRING | 15s，处理中每 5s 续约 | `Begin` 的 `SET NX PX`、`Renew` | `Complete` 成功或失败时 `DEL` |
| K8 | `cfg:ver` | `ConfigVersionKey` | STRING | 无 | `internal/config` 发布时 `INCR` | 不失效 |
| K9 | `cfg:snap:{namespace}` | `ConfigSnapshotKey` | STRING | 300s | `internal/config` 发布时 `SET` | 版本号 + Pub/Sub + TTL |
| K10 | `cfg:invalidate` | `ConfigInvalidateChannel` | Pub/Sub channel | 不适用 | `internal/config` 发布时 `PUBLISH` | 不适用 |
| K11 | `hold:{request_id}` | `HoldKey` | STRING（微积分） | 非流式 120s / 流式 600s | 预扣（P10）Redis 侧 | 到期；DB `t_prehold_hold` 为真相 |
| K12 | `hold:recover:{request_id}` | `HoldRecoverKey` | STRING | 300s | 对账任务 `SET NX` | 到期 |
| K13 | `ak:{sha256_prefix_16}` | `APIKeyHashKey` | HASH（`uid`/`kid`/`st`/`ipm`/`exp`/`nt`/`kn`/`grp`/`gp`） | **无** | `KeyAuthWriter.Upsert` / `Delete` / `BulkUpdateGroupRatio` | 控制面写穿；缺失即 401，无主库回落 |
| K14 | `m:{scope_type}:{scope_id}:{min_epoch}` | `MetricKey` | HASH | 300s | 请求结束 `HINCRBY` | 到期；权威值从 `t_usage_record` 重算 |
| K15 | `m:chan:{channel_id}:{min_epoch}` | `ChannelMetricKey` | HASH | 300s | 每次上游尝试 `HINCRBY` | 到期 |
| K16 | `up:cb:{channel_id}` | `BreakerKey` | HASH（`st`/`fc`/`open_until`/`bo`/`ps`） | 无 | `breaker.Record` | `open_until` 逻辑恢复；半开成功 3 次回 `CLOSED` |
| K17 | `up:probe:{channel_id}` | `ProbeKey` | STRING | 30s（仅崩溃兜底） | `breaker.TryProbe` `SET NX PX` | **探测结束必须 `ReleaseProbe` → `DEL`** |
| K18 | `node:set` | `NodeSetKey` | ZSET（member=`node_id`，score=心跳毫秒） | 无 | 节点启动与每 20s `ZADD` | `ZREMRANGEBYSCORE` 清理 60s 无心跳节点 |
| K19 | `price:{model_id}` | `PriceKey` | STRING（标准价） | 120s | `internal/config.PriceCache` | 主动失效；**只存标准价，不乘分组倍率** |
| K20 | `sess:admin:{session_id}` | `AdminSessionKey` | STRING | 2h | 运营后台登录 | 到期 |
| K21 | `lock:recon:{yyyymmdd}` | `ReconLockKey` | STRING | 2h | 日对账 `SET NX` | 到期 |
| K22 | `lock:expire_batch` | `ExpireBatchLockKey` | STRING | 300s | 批次清零 `SET NX` | `RenewExpireBatch` 仅续期当前 owner |
| K23 | `up:cd:{channel_id}` | `CooldownKey` | STRING | ≤60s（缺省 5s） | 上游 429 时 `SET` | 到期；**不参与 `fc`，不触发熔断** |
| K24 | `log:stream` | `LogStreamKey` | STREAM（`v`、`p`） | `MAXLEN ~ 200000` | `logq.Producer.Add` `XADD` | 近似裁剪；消费端 `XACK` 移出 PEL |
| K25 | `job:heartbeat:{instance}` | `JobHeartbeatKey` | STRING | 60s | jobsd 每 20s `SET` | 到期；`Alive` 用 `EXISTS` |
| K26 | `bal:{userID}` | `BalanceKey` | STRING（微积分） | 60s | 镜像缺失时回源后回填 | **失效只用 `DEL`**；再用一次 `SET` 覆盖会盖掉并发结算 |
| K27 | `exp:user:{user_id}` | `ExportLockKey` | STRING（固定 `1`） | 1h | `usage.StartExport` `SET NX` | 到期；导出完成 `DEL`。同一账号同时只允许一个导出 |
| K28 | `auth:fail:acct:{sha256(identifier)}` | `LoginAccountFailKey` | STRING（失败次数） | 15m | `auth.recordLoginFailure` `INCR`；首次失败与达到阈值时 `EXPIRE` | 到期；登录成功 `DEL` |
| K29 | `auth:fail:ip:{sha256(ip)}` | `LoginIPFailKey` | STRING（失败次数） | 10m | `auth.recordLoginFailure` `INCR`；首次失败与达到阈值时 `EXPIRE` | 到期；登录成功不清（IP 预算跨账号，不能被一次正确密码重置） |
| K30 | `sess:admin:uid:{user_id}` | `AdminUserSessionSetKey` | SET（member=sid） | 2h 滑动，与 K20 同步 | `SessionManager.Issue` `SADD`；`Load` 每次 `SADD` 自愈并续期 | 登出 `SREM`；改密 `DEL`（同时逐个 `DEL` K20） |

## 验证码键族（K-VC）

验证码的键不在 K1–K30 里，它们是独立的一族，构造函数同样在 `internal/redisx/keys.go`。
**`purpose` 必须进键名**：一个渠道为 A 用途发的码不能被重放到 B 用途（`bind` 与 `reset`
共用键会直通式账号接管，见 `docs/tasks/INC-password-recovery-design.md` §12 R3）。

| 键名模板 | 构造函数 | 类型 | TTL | 写入方 | 说明 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `vc:code:<purpose>:<sha256(identifier)[:32]>` | `VerifyCodeKey` | STRING（SHA-256 摘要） | 600s | `auth.issueVerificationCode` | 只存摘要，Redis dump 不等于可用凭据；一次一用 |
| `vc:code:<purpose>:<hash>:n` | 同上 + `":n"` | STRING（错误次数） | 600s | `auth.consumeVerificationCode` `INCR` | ≥ 5 时与码一起 `DEL` |
| `vc:cool:<purpose>:<hash>` | `VerifyCodeCooldownKey` | STRING（固定 `1`） | 60s | 发码 `SETNX` | 并发双击只有一次能过 |
| `vc:quota:<purpose>:<hash>` | `VerifyCodeQuotaKey` | STRING（次数） | 24h | 发码 `INCR` | 地址日配额：`login`/`register` 20，`reset` 10，`bind` 10 |
| `vc:quota:<purpose>:uid:<uid>` | `VerifyCodeUIDQuotaKey` | STRING（次数） | 24h | `auth.consumeUIDQuota` | **账号**日配额，目前只有 `bind` 用（5/天）：一个已登录账号不能变成向任意邮箱投递的免费发信器 |
| `vc:ipq:<sha256(ip)[:32]>:<YYYYMMDDHH>` | `VerifyCodeIPQuotaKey` | STRING（次数） | 2h | 发码 `INCR` | 单 IP 每小时 10 次。**故意不按 purpose 拆分**：新增用途不得把单 IP 的发信上限从 10 抬到 40 |

`purpose ∈ {login, register, reset, bind}`。查某个用途：`redis-cli --scan --pattern 'vc:code:reset:*'`；
查某个地址被发过几种码：先算 `sha256(归一化后的地址)[:32]`，再
`redis-cli --scan --pattern 'vc:*:*:<hash>'`。

## 时间与日界

`{min_epoch}` = `floor((now_ms + redis_offset_ms) / 60000)`。`redisx.Clock.Sync` 每分钟发一次 `TIME`，偏移 >±1s 打 WARN、>±5s 打 ERROR 并调用 `OffsetObserver`。两副本落进不同分钟桶会让实际限额翻倍，所以 `MinEpoch` 不得直接取本地时钟。

`{yyyymmdd_cst}` 用 `Asia/Shanghai`（`ratelimit.Day`），不是 UTC 日期。日额度重置时刻是次日 00:00（UTC+8）；K4/K5 的 TTL 到次日 00:10 + 600s，给跨日结算留补偿窗口。

## 限流降级与失败取向

| 维度 | Redis 可用 | Redis 不可用（`concurrency_only`） |
| :--- | :--- | :--- |
| 并发（K3） | ZSET 全局精确 | 节点内 `atomic.Int64`，per-Key 上限 `ceil(quota / ZCARD(node:set))` |
| RPM（K1） | 全局精确 | fail-close |
| TPM（K2） | 全局精确 | fail-close |
| 日额度（K4） | 全局精确 | fail-close |

L1 执行失败（连接失败、`EVALSHA` 与 `EVAL` 均失败）返回 `ratelimit.ErrUnavailable`，上层转 503 `rate_limiter_unavailable`，不得放行。L2 执行失败不 fail-close：进入 10,000 条 `pending_release` 缓冲，由 `ReplayPending` 每 5s 重放；缓冲满丢弃最旧并由日对账修正。

## 返回值约定

`rl_acquire.lua` 固定返回 8 个下标：`allowed / code / limit / total / remaining / reset_at_sec / all_hit / reset_at_max`。`code` 是优先级最高的触顶维度（并发 > RPM > TPM > 日额度）；`all_hit` 是全部触顶维度；`reset_at_max` 是恢复最晚的时刻。对外 429 的 `code` 由 `ratelimit.Verdict.OutwardCode()` 按「日额度 > RPM/TPM > 并发」选取，`limit`/`remaining`/`reset_at` 只描述主维度。

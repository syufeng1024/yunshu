# 分级降级矩阵（T22 交付物 17）

> **适用对象**：gatewayd 的四维限流（P11）与资金预扣（P09/P10）在依赖（Redis、配置中心）不可用时的行为。
> **一句话结论**：**能证明「多放一个请求也不会造成不可逆损失」的维度才降级放行；金额与滥用防护相关的一律 fail-close。**
>
> 口径来源：PRD §9.8（降级行为必须可解释）、PRD §9.5（分档与误杀容忍度）、TDS §2.3（降级矩阵）。
> 值班处置见 [ops-runbook.md](ops-runbook.md) §8「容量与限流告警」；本文只讲**行为**，不讲怎么修。

---

## 1 矩阵

三列不是三套策略，而是同一套优先级在两个依赖状态下的取值：**Redis 正常** → 全局精确；**Redis 不可用** → 按维度决定放行或拒绝。
下表的「Redis 不可用」两列是 PRD §9.8 / TDS §2.3 的**规范要求**；本仓当前的实际接线见 §1.1，两者有一处差距，不要混着读。

| 维度 | Redis 正常 | Redis 不可用（默认模式） | Redis 不可用（`concurrency_only`） |
| :--- | :--- | :--- | :--- |
| **并发（K3）** | 全局精确（ZSET，名义上限） | **放行**：退化为节点内计数，实际上限 = 名义值 ÷ 副本数 | **放行**：同左（节点内计数） |
| **RPM（K1）** | 全局精确 | **拒绝** 503 `rate_limiter_unavailable` | **拒绝** 503 `rate_limiter_unavailable` |
| **TPM（K2）** | 全局精确 | **拒绝** 503 `rate_limiter_unavailable` | **拒绝** 503 `rate_limiter_unavailable` |
| **账户日额度（K4）** | 全局精确 | **拒绝** 503 `rate_limiter_unavailable` | **拒绝** 503 `rate_limiter_unavailable` |
| **余额预扣（P10，非四维）** | 正常预扣 | **拒绝** 503 `settlement_unavailable` | **拒绝** 503 `settlement_unavailable` |

> **实现现状注记（G31 已修）**：表中「Redis 不可用 → 并发节点内计数」这一格已可达——`Degrader.Replicas()` 拆成「热路径读缓存」+「周期刷新」，Redis 不可用时用缓存副本数算本地上限：本地满 → 429 `concurrency_limit_exceeded`、未满 → 503 `rate_limiter_unavailable`；RPM/TPM/日额度 fail-close 不变。`GW_RATELIMIT_DEGRADE_MODE` 仍**默认 `off`**，详见 §1.1。

矩阵的读法：

- **「放行」不等于「不限制」**。并发维度退化为节点内计数，上限按副本数等分：`ceil(名义值 ÷ 副本数)`（`ratelimit.PerKeyLocalLimit`，D-09；用 ceil 不用 floor——floor 会让「配额 5、3 副本」只能放进 3 个请求，等于一扩容就悄悄缩容）。单副本部署时与原值相同；扩缩容时上限随副本数变化，这是已知代价——它换来的是 Redis 故障期间数据面不完全停摆。
- **日额度这一整列对免费档「不适用」，不是「放行」**。免费档（`t_user.plan_id IS NULL`）本来就没有账户日额度（PRD §3.2.1），因此该维对免费档既不会拒绝也不会放行。**不要把它写成「免费档在降级时不受限」**——那会把 PRD §7 第 32 项的滥用防护缺口说成一种能力。
- **`concurrency_only` 模式只影响「哪些维度允许降级」的判定入口，不改变 RPM/TPM/日额度的 fail-close**。它的存在是为了在压测或容量事件里显式声明「只保留并发维度」，而不是一个可以放行金额维度的开关。
- **余额预扣不在四维里，但它同样 fail-close**：`bal:{userID}` 镜像读不到时不能猜余额（T22 §4 第 5 条）。

### 1.1 实现现状（与上表对照，必须一起读）

**① 开关已经接通（此前是规格偏差，已修）**
降级姿态由启动参数 `GW_RATELIMIT_DEGRADE_MODE` 决定（`ratelimit.ModeFromEnv`），取值 `off` / `concurrency_only`，**其它值启动失败**（不静默回落），启动日志打印 `ratelimit degrade mode=<姿态>`。在此之前该值是硬编码的 `ModeOff`，连启动参数都没读，属于 E-12 裁定「T07 从启动参数读取」未落实。

**② 默认 `off`，理由与代价**
TDS :3073 写「V1 唯一取值 `concurrency_only`」。G31 修好后这条路径已可达（§③），但默认仍 `off`，理由有二：(1) `concurrency_only` 只放宽并发一维，RPM/TPM/日额度仍 fail-close，可用性收益本就是局部的；(2) 开启会把 `gw_ratelimit_degraded_total{dim="concurrency"}` 的语义从「降级事件」改成「放行次数」，降级信号反而更模糊。
默认 `off` 下该计数器**只反映真实降级**，不再被放行路径污染（有用例 `TestDegradedCounterStaysQuietInOffMode` 钉住）。

**③ 已修（G31）：副本数改为缓存 + 周期刷新**
`Replicas()` 已拆成「热路径读缓存」+「后台 `RefreshReplicas`」，Redis 不可用时并发维度用上次已知值算本地上限，不再因为读不到 K18' 而直接不可达。RPM/TPM/日额度 fail-close 不变：Redis 完全不可用时仍是「本地满 → 429 `concurrency_limit_exceeded`、未满 → 503 `rate_limiter_unavailable`」。`TestDegradedModeRejectsLocallyWhenRedisIsDown` 钉住这两条分支。

| 项 | 规范要求 | 本仓现状 |
| :--- | :--- | :--- |
| RPM / TPM / 日额度 | 任何模式下 fail-close | 满足：`callAcquire` 读三个维度的键，Redis 不可用即 `ErrUnavailable` → 503 `rate_limiter_unavailable` |
| Redis 完全不可用不得放行 | 不得放行 | 满足：`Allowed` 恒为 false；并发本地满 → 429，RPM/TPM/日额度与并发未满 → 503 |
| 并发维度降级为节点内计数 | `concurrency_only` 时放行 | ⚠️ **已修（G31）**：副本数缓存 + 周期刷新，并发维度可达（本地满 → 429）；RPM/TPM/日额度仍 fail-close；默认仍 `off` |
| 恢复状态机（3 次心跳 + `drain_window` 150s） | 自动切回 | **未自动化**：代码只有原语 `Degrader.ResetLocal()`，按 §4 人工执行 |

**对值班的含义**：默认 `off` 下 Redis 故障时**没有任何维度会放行**，四个维度全 503（`gw_ratelimit_unavailable_total` 是可靠信号）。显式开启 `concurrency_only` 时，并发维度满会走 429 `concurrency_limit_exceeded`（见 `gw_ratelimit_rejected_total{dim="concurrency"}`），不计入 `unavailable_total`；其余三维（RPM/TPM/日额度）仍 503。`docs/ops-runbook.md` §8 按这个前提写。

---

## 2 为什么这样分

| 维度 | 降级放行的代价 | 结论 |
| :--- | :--- | :--- |
| 并发 | 短时间多跑几个请求，最坏是上游轻微过载；请求仍受 P10 预扣与 P15/P16 超时保护 | 可降级 |
| RPM / TPM | 放行即等于**取消滥用防护**：一个脚本可以在窗口内打满上游额度，把成本转移到平台 | fail-close |
| 账户日额度 | 这是**金额维度**：日额度打满的账户继续放行，等于平台先垫钱。钱花出去收不回，属于不可逆损失 | fail-close |
| 余额预扣 | 同上，且会直接产生负余额与对账差异 | fail-close |

一句话记忆：**可以「慢一点」，不可以「多花钱」，不可以「不知道是谁花了钱」。**

---

## 3 对外如何解释（客服 / 文档话术）

降级期间对外的解释只有两种，不允许出现第三种「静默变慢」：

**A. 被拒绝（RPM / TPM / 并发 / 日额度 / 平台故障 503）**——直接使用错误码自带的 `hint`，不要自行改写数字：

| 情形 | 错误码 | 用户可见文案（`message` / `hint` 原文） |
| :--- | :--- | :--- |
| 分钟请求数触顶 | `rpm_limit_exceeded` | 「本分钟请求数已达上限 N，请 N 秒后重试」／「按 {retry} 秒等待；持续触顶 → 控制台申请提额」 |
| 分钟 token 触顶 | `tpm_limit_exceeded` | 「本分钟 token 用量已达上限 N，请 N 秒后重试」／「按 {retry} 秒等待；持续触顶 → 控制台申请提额」 |
| 并发触顶 | `concurrency_limit_exceeded` | 「并发已达上限 N，请 N 秒后重试」／「按 3 秒退避重试；持续触顶 → 控制台申请提额」 |
| 日额度用尽 | `daily_quota_exceeded` | 「账户日额度已用尽（今日已用 X / 上限 Y），请次日 00:00 后重试」／「控制台 → 申请提额，或次日 00:00 后再试」 |
| 限流服务不可用（降级 fail-close） | `rate_limiter_unavailable` | 「限流服务暂时不可用，请稍后重试」／「按 {retry} 秒退避重试；持续出现 → 携 request_id 联系客服。平台侧故障，不必调整你的请求频率」 |
| 结算/预扣不可用 | `settlement_unavailable` | 「结算服务暂时不可用」／「按 {retry} 秒退避重试。不要以为是余额不足」 |

**B. 客服口头口径**（客户问「是不是你们平台出问题了」）：

> 你好，这段时间我们的一项目标保护机制（请求频率校验）在自动切换中，为了防止你的额度被误算，系统对你的请求做了短暂的拒绝。**你的余额没有变化，也不会因为这次重试产生额外费用**。请按返回头里的 `Retry-After` 秒数重试；如果连续出现，请把 `X-Request-Id` 发给我们，我们会核对这一次请求的计费记录。

客服口径的三条红线：

1. **不得承诺可用性百分比、P99 延迟、恢复时长**。这些是内部目标与设计假设（PRD §9.2 / §9.3 / T22 硬约束 3、15），一旦写进对外口径就变成对外承诺，平台既无法核算也无法举证。
2. **不得说「我们已经修复」**，只能说「正在处理并已通告」。
3. **不得解释内部机制细节**（ZSET、节点内计数、副本数、drain_window）。用户只需要知道「为什么被拒」「钱有没有动」「怎么办」。

---

## 4 恢复

降级退出是两段式的，不是一次性切回：

1. **判定**：连续 **3 次** Redis 心跳成功才退出降级（单次心跳成功可能是抖动）。
2. **清账**：退出降级时清空节点内计数器，避免本地残留计数叠加全局计数造成重复限制。
3. **RPM / TPM / 日额度立即恢复**：fail-close 解除，回到全局精确判定。
4. **并发维度再等一个 `drain_window`（默认 150s，等于并发租约的 TTL）** 才切回 ZSET。

**现状提醒**：上面四步里，第 1、2 步是人工判定（代码里没有心跳计数器），第 4 步依赖 `Degrader.ResetLocal()`；在 `ModeOff` 前提下（§1.1）降级根本不会发生，本节按「接线打开后」的规范写。

**为什么第 4 步不能省**：并发维度用的是「租约」语义——本地计数里可能还有若干在途请求，而 ZSET 里此刻一个都没有。立刻切回会让这批在途请求在新旧两套体系里各被放行一次，瞬时并发上限变成两倍。等 `drain_window` 让本地租约全部自然过期，两套计数才对齐。

**观察恢复是否完成**：`gw_ratelimit_degraded_total{dim}` 的窗口增量归零（`increase(...[5m]) == 0`），同时 `gw_ratelimit_unavailable_total` 停止增长。150s 内如果再次判失败，会重新进入降级，不会「半恢复」。

---

## 5 观测点

| 现象 | 指标 / 日志 | 说明 |
| :--- | :--- | :--- |
| 某维度正在降级 | `gw_ratelimit_degraded_total{dim}` + `WARN ratelimit_degraded` | `dim ∈ {rpm,tpm,concurrency,daily}` |
| fail-close 已发生 | `gw_ratelimit_unavailable_total{dim}` | 每一次都是 503，对应 PRD §9.3 可用性分子 |
| 降级期拒绝趋势 | `gw_ratelimit_rejected_total{dim}` | 按维度看降级是不是把拒绝推高了；告警见 `RatelimitRejectRatioHigh`（PRD §9.5，全局拒绝率） |

告警规则见 `internal/obs/alert/rules.yaml`（`RatelimitUnavailable` 是降级/故障信号本身；降级把拒绝推高时由 `RatelimitRejectRatioHigh` / `RatelimitRejectRatioHighByDim` 反映）。

---

## 6 不在本文范围

- **熔断器**（K18）的降级方向相反：Redis 不可读时按 **CLOSED 放行**（TDS §2.2 失败分支）。理由不是「金额无所谓」，而是熔断保护的是上游资源、不是用户额度，误判 OPEN 会把全渠道摘光，造成的可用性损失大于上游过载风险。该分支见 TDS §2.2，不在本矩阵内。
- **配置中心不可用**：用本地快照继续服务（E-60），属于配置通道的降级，不涉及限流维度放行与否。
- **上游渠道全失败**：按 TDS §3.4.1 处理，不在本矩阵内。

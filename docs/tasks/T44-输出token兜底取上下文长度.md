# T44 · 输出 token 兜底取 `context_length` 导致大上下文模型恒 429

| 项 | 值 |
| :--- | :--- |
| 卡片编号 | T44 |
| 阶段 | 6 · 交付（主流程 E2E 实测拦截） |
| 优先级 | **P0**（上下文 > TPM 上限的模型 100% 不可调用，属可用性阻断） |
| 依赖 | 无 |
| 交付根 | `internal/gw/p09_estimate.go`、`internal/gw/p09_estimate_test.go`（或同包测试） |
| 状态 | ✅ 已收口（未提交，见 §8） |
| 分段 | **单段**（预计 ≤ 20 分钟） |

## 1. 事实（已核，勿重复调查）

| 事实 | 证据 |
| :--- | :--- |
| 平台对客户端 `max_tokens` 的硬上限是 **32768** | `internal/gw/p04_parse.go:14` `PlatformMaxOutputTokens = 32_768`；`:359-361` 超过即拒绝 |
| 输出 token 预估的三级兜底**最后一级是 `context_length`** | `internal/gw/p09_estimate.go:192-203` `outputTokens()`：`IR.MaxTokens` → `Model.MaxOutputTokens` → **`Model.ContextLength`** → `PlatformMaxOutputTokens` |
| 本地联调种子的 `max_output_tokens` 是 **0** | `internal/platform/migrate/sql/mysql/0019_seed_local.sql`，`t_model` 插入未写 `max_output_tokens`，列默认 0 |
| 于是 `local-deepseek-chat`（context 65536）预估输出 = **65536** | 上一行 + 兜底第三级 |
| TPM 判定是 `tpm_total + est_tokens > tpm_limit` | `internal/ratelimit/scripts/rl_acquire.lua:108` |
| 默认 TPM = **60000**，故 65536 > 60000 → 每次必拒 | 实测 429 报文 `limit: 60000` |
| 实测反证：`local-qwen-plus`（**32768**）非流式 + 流式全 200 | 本文末尾 §6 |
| 拒绝时计数器实际为 **0**，`remaining` 却是 60000，报文自相矛盾 | Redis `rl:tpm:*` = `0`；429 body `remaining: 60000` |

**结论**：平台自己把输出上限定在 32768，兜底却按「上下文长度」预留输出；`context_length = 输入 + 输出`，把它当输出上限等于**重复预留一次上下文**。只要 `context_length > tpm_limit`，该模型的任何请求都会被判超额——不是模型问题、不是配额问题，是兜底取值错了。

## 2. 要做（两处，按序）

1. **兜底改为受平台上限约束**
   `internal/gw/p09_estimate.go` 的 `outputTokens()` 第三级，把「直接返回 `c.Model.ContextLength`」改为「返回 `min(c.Model.ContextLength, PlatformMaxOutputTokens)`」：

   ```go
   if c.Model.ContextLength > 0 {
       if c.Model.ContextLength < PlatformMaxOutputTokens {
           return c.Model.ContextLength
       }
       return PlatformMaxOutputTokens
   }
   ```

   语义：上下文比平台输出上限小的小模型仍按上下文收窄（更保守）；比它大的按平台真实输出上限预留。**不要**引入新的魔法数字，用的是同包已有常量。

2. **补断言（正反两个方向）**
   在 `internal/gw` 包内新增或补充用例，直接调 `EstimateStage.outputTokens`（或走 `Estimate` 阶段可得等价可观测量）：

   - `MaxOutputTokens=0, ContextLength=65536, MaxTokens=nil` → 返回值 **≤ 32768**（当前实现返回 65536，会红）。
   - `MaxOutputTokens=0, ContextLength=4096, MaxTokens=nil` → 返回值 **== 4096**（保证小模型不被抬到 32768）。
   - `MaxTokens=&2000` → 返回值 **== 2000**（优先级不变）。
   - `MaxOutputTokens=8192` → 返回值 **== 8192`（模型自带上限优先于兜底）。

## 3. 硬约束

1. **不改 `rl_acquire.lua`**、不改 `internal/ratelimit/` 任何文件。限流器逻辑本身没错，错的是喂给它的预估值。
2. **不改默认 TPM 60000**，不改任何配额配置与套餐表。拒绝「把上限调大」这种掩盖式修法。
3. **不改 `PlatformMaxOutputTokens` 的值**（32768 是客户端 `max_tokens` 的契约上限，改它会动 P04 错误码语义）。
4. **不加迁移、不改表结构**：`max_output_tokens=0` 是历史数据的合法状态，本卡只让代码在 0 时兜底正确。
5. **不动 `0019_seed_local.sql`**（种子补 `max_output_tokens` 属数据修正，见 T43 的范围说明；本卡不碰）。
6. `p09_estimate.go:105-115` 既有的「输出裁剪到 `context_length - in` 剩余空间」逻辑**保持不动**，它与本卡正交。
7. 不在日志/错误信息里打印正文或密钥（沿用既有约束）。
8. `bash scripts/check-eol.sh` 退出码 0；`go build ./...`、`go vet ./...` 干净。
9. **不 commit、不 push、不建分支**。交付总监复核后提交。

## 4. 自检命令（本机有 Go 1.27.1，开发库已起）

```bash
cd /d/github_projects/yunshu-api
go build ./... && go vet ./...
go test ./internal/gw/ -run 'Estim|OutputTokens' -count=1 -v
go test ./internal/gw/ ./internal/dispatch/ ./internal/ratelimit/ -count=1
gofmt -l internal/gw
bash scripts/check-eol.sh; echo "eol_exit=$?"
```

跑 `internal/control/**` 等需要 DB 的包前，先确认 `docker ps --filter name=yunshu-mysql` 是 healthy（本机端口 13306）。

回执里请贴 **`go test -run 'Estim|OutputTokens' -v` 的真实输出**，不要只写「已通过」。

## 5. 验收口径（交付总监独立复跑，不采信转述）

| # | 查什么 | 通过标准 |
| :--- | :--- | :--- |
| 1 | 兜底真的受上限约束 | `outputTokens()` 第三级不是裸返回 `ContextLength`；`ContextLength=65536` 时返回值 ≤ 32768 |
| 2 | 小模型不被抬升 | `ContextLength=4096` 仍返回 4096 |
| 3 | 优先级未被打乱 | 显式 `max_tokens` 与 `MaxOutputTokens` 仍优先 |
| 4 | 反证有效 | 把第三级改回裸 `return c.Model.ContextLength`，新增用例必须红；恢复后绿 |
| 5 | 端到端 | 重启 gatewayd 后：`local-deepseek-chat`（65536）非流式返回 **200**；`local-qwen-plus` 仍 200；两者流式均 200 |
| 6 | 回归 | `internal/gw`、`internal/dispatch`、`internal/ratelimit` 全绿；build / vet / gofmt / check-eol 干净 |

第 5 条的实测口径（假上游已在 `:18090`）：

```bash
KEY=<重跑时用 /api/console/keys 新建一把>
curl -s --noproxy '*' -X POST http://127.0.0.1:8082/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer $KEY" \
  -d '{"model":"local-deepseek-chat","messages":[{"role":"user","content":"t44"}]}' \
  -w '\n[%{http_code}]\n'
```

修复前对照：同一命令回 `429 tpm_limit_exceeded`，而 `redis-cli GET rl:tpm:*` 为 0。

## 6. 实测原始证据（2026-11-05，交付总监跑）

```
# local-deepseek-chat / context 65536 / max_output_tokens=0 / 未传 max_tokens
{"error":{"type":"rate_limit_exceeded","code":"tpm_limit_exceeded",
 "message":"本分钟 token 用量已达上限 60000，请 36 秒后重试",
 "limit":60000,"remaining":60000}}

# 同 key、同分钟、换 local-qwen-plus / context 32768
data: {"id":"chatcmpl-fake","object":"chat.completion.chunk",...}   ← 200 流式正常

# 计数器
rl:tpm:1:29898165 = 0
rl:rpm:1:29898165 = 1
```

## 7. 不在本卡范围（写了会被打回）

- `.env.example` 的 `LOG_SQL_DSN`（T43）。
- 429 报文文案优化（`remaining` 与「已达上限」自相矛盾）：属错误码/文案收口，本卡只要求回执记一行，**不要顺手改** `internal/apierr/`。
- 种子 `t_model.max_output_tokens` 补真实值（T43 讨论范围）。
- 真实上游渠道接入、真实模型价目配置。

## 8. 完成回执（已验收，未提交）

**结果**：✅ 通过。改动 1 个生产文件 **+7 / -1**，新增 1 个测试文件（5 个用例）。工作区待提交。

### 8.1 实现落点

| 文件:行 | 内容 |
| :--- | :--- |
| `internal/gw/p09_estimate.go:198-207` | `ContextLength > 0` 分支由「直接返回 `ContextLength`」改为「`ContextLength < PlatformMaxOutputTokens` 时返回它，否则返回 `PlatformMaxOutputTokens`」 |
| `internal/gw/t44_output_tokens_test.go` | 新增 5 个用例（见 8.2） |

### 8.2 新增用例

| 用例 | 断言 |
| :--- | :--- |
| `TestOutputTokensFallbackIsCappedByPlatformCeiling` | `context=65536, max_output=0, 无 max_tokens` → `<= 32768`（实测 32768）|
| `TestOutputTokensFallbackKeepsSmallContext` | `context=4096` → 恰为 `4096`（不被抬升）|
| `TestOutputTokensExplicitMaxTokensWins` | 显式 `max_tokens=2000` → `2000` |
| `TestOutputTokensModelMaxOutputWinsOverFallback` | `max_output=8192` → `8192` |
| `TestEstimateOutputTokensCappedAtPlatformCeiling` | 走完整 `Estimate.Run`，`Est.Tokens.OutputTokens <= 32768`（实测 32768）|

### 8.3 交付总监独立复跑（不采信转述）

| 命令 | 结果 |
| :--- | :--- |
| `go build ./...` / `go vet ./internal/gw/` | 均干净 |
| `go test ./internal/gw/ -run 'Estim\|OutputTokens' -count=1 -v` | 6 个用例全 PASS（含既有 `TestA7G02...`），`outputTokens=32768` |
| **反证**（把兜底改回裸 `return c.Model.ContextLength`） | 2 个用例变红：`outputTokens() = 65536` 与 `est.OutputTokens = 65535`；恢复后转绿 |
| `go test ./internal/gw/ ./internal/dispatch/ ./internal/ratelimit/ -count=1` | 三包全 ok |
| `gofmt -l internal/gw` / `scripts/check-eol.sh` | 无输出 / 退出 0 |
| 端到端（重启 gatewayd 后） | `local-deepseek-chat`（context 65536）非流式 **200**、流式 **200**；`local-qwen-plus` 仍 **200** |

### 8.4 一处需要记录的观测（非缺陷）

端到端中途出现过一次 `402 insufficient_credits`，并非本卡引入：验收脚本用 `curl ... | head -3` 掐断了流式响应，客户端中断后预扣的 lease 未即时结算，`rl:cc:*` 租约到期回收后立即恢复 200。**结论：客户端中途断开 → 预扣短暂占用余额 → lease 到期自愈**，属既定保护，不改。

### 8.5 遗留（不在本卡）

- 429 报文 `remaining` 与「已达上限」自相矛盾的文案，仍未修（错误码收口范畴）。


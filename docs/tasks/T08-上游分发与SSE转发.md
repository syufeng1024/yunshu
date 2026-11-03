# T08 · 上游分发·协议转换·SSE（P15–P25）

| 项 | 值 |
| :--- | :--- |
| 卡片编号 | T08 |
| 阶段 | 3 · 网关数据面 |
| 优先级 | P0 |
| 依赖 | T07 |
| 可并行 | 否（关键路径） |
| 交付根 | `relaykit/`（独立 module）、`internal/dispatch/`、`internal/stream/` |
| 状态 | ✅ 已交付（2026-10-26） |

## 1. 目标

打通「真正打到上游并原样回来」：协议转换（两段式 IR）、上游连接与重试降级、SSE 逐帧解析与重发、usage 与首 token 时间提取、断连处置。本卡是唯一允许接触上游 HTTP 的地方，也是「换 base_url 就能用」这句话能否成立的地方。

## 2. 必读

| 文档 | 区间 | 要点 |
| :--- | :--- | :--- |
| TDS §3.1 P15–P25 | 3247–3263 | 阶段输入输出与超时 |
| TDS §3.3 | 3433–3612 | **异常路径全矩阵**（含 E-06 等），重试判据与退避的唯一权威 |
| TDS §3.4.1 | 385–399 | 对外可观测结论：可重试仅 5xx+超时；上游 429 不重试、不换候选、转 503 |
| TDS §4.1–4.5 | 3614–3722 | 逐帧状态机、无 usage 三模式、首 token 口径、断连部分结算、心跳与背压 |
| TDS AD-05 / AD-11 / AD-12 | 111、117、118 | SSE 解析后重发；IR 两段式；对外 2 种协议，百炼是上游接入方式 |
| TDS ADR-002 | 946 | `relaykit` 独立 module，`GOWORK=off go build ./...` 验证 |
| PRD §3.4 | 364–383 | 模型三态、别名、定价层级、来源地 |
| PRD 附录 E.3 | 2023–2046 | 重试与降级策略（对外承诺） |
| PRD 附录 E.4 | 2046–2091 | SSE 错误帧契约 |

## 3. 交付物清单

| # | 路径 | 职责 |
| :--- | :--- | :--- |
| 1 | `relaykit/go.mod` | 独立 module `yunshu.dev/relaykit`；**只含协议 DTO 与转换，不含业务**；`GOWORK=off go build ./...` 必须通过 |
| 2 | `relaykit/ir/ir.go` | IR 数据结构：`IRRequest` / `IRResponse` / `IRChunk` / `IRUsage` / `IRError` |
| 3 | `relaykit/openai/dto.go` + `in.go` + `out.go` | OpenAI 兼容 → IR、IR → OpenAI 兼容 |
| 4 | `relaykit/anthropic/dto.go` + `in.go` + `out.go` | Anthropic 兼容（`x-api-key`、`system`、`stop_sequences`、`tool_choice`） |
| 5 | `relaykit/bailian/in.go` + `out.go` | 阿里云百炼（**仅作上游接入方式**，对外不暴露） |
| 6 | `relaykit/convert_test.go` | 三协议 × 双向 的黄金用例（fixture 驱动）；未知字段必须**保留**（`Extra` 透传） |
| 7 | `internal/dispatch/dispatch.go` | P15 上游连接与发送：连接 3s、`http.Transport` 复用、每渠道独立 `MaxConnsPerHost` |
| 8 | `internal/dispatch/firstbyte.go` | P16 首字节等待 30s（`gw.stream_first_byte_ms`） |
| 9 | `internal/dispatch/retry.go` | 重试判据：**仅 5xx + 连接/首字节/读超时**；`gw.retry_max=1`；门槛 `gw.retry_threshold_ms`（非流式 65s / 流式 45s）；**不追加预算** |
| 10 | `internal/dispatch/upstream429.go` | 上游 429 → 对外 **503 `upstream_capacity_exceeded`**，`retry_after` = 上游 `Retry-After`，缺省 30；**必须**：不重试、不换候选、**不转成客户端 429** |
| 11 | `internal/dispatch/attempt.go` | 每次尝试产生一条 `t_upstream_attempt`（供 T09 落库）；含 `channel_id`、`route_id`、`status`、`latency_ms`、`error_class` |
| 12 | `internal/stream/parser.go` | P18 SSE 逐帧解析状态机：`data:` / `event:` / 注释帧 / `[DONE]` / 半帧缓冲；单帧 30s 无数据即超时 |
| 13 | `internal/stream/relay.go` | P19/P20/P22：解析 → IRChunk → 对外协议重组 → `data:` 帧；每帧 flush + `SetWriteDeadline(5s)` |
| 14 | `internal/stream/usage.go` | P25 usage 提取；`stream_usage_mode` 三档（有 usage / 末帧 usage / 无 usage 需探测） |
| 15 | `internal/stream/heartbeat.go` | P23 心跳 `: keep-alive`，15s 间隔（仅在无数据时发） |
| 16 | `internal/stream/firsttoken.go` | 首 token 时间提取（T-26 口径）：从**上游首帧含内容**起算，不含 `role` 空帧 |
| 17 | `internal/stream/abort.go` | P24 客户端断连检测：`ctx.Done()` → 停止读上游、记录部分产出、走部分结算 |
| 18 | `internal/stream/*_test.go` | 逐字节喂入的半帧用例、乱序帧、双 `data:` 行、无 `[DONE]`、上游中途断开 |
| 19 | `docs/relay-contract.md` | 三协议字段映射矩阵（对外字段 → IR → 上游字段），以及**不透传字段清单** |
| 20 | `internal/cryptobox/cryptobox.go` + `cmd/keytool/` | AES-256-GCM 信封（`v1:` + nonce + ciphertext）；KEK 取 env `CHANNEL_KEY_KEK`（base64 32B）。`keytool encrypt \| seed-channel` 供本地把明文 Key 加密写库（`upstream_key_enc` **始终密文**，代码里**不得**有明文回退分支） |
| 21 | `cmd/fakeupstream/` | 开发态 OpenAI 兼容假上游（流式/非流式；可模拟 429 / 500 / 无 `[DONE]` / 半帧）。仅用于 A13 端到端联调，**不得**进生产构建路径 |

## 4. 硬约束（违反即驳回）

1. **`relaykit` 独立可构建**：`cd relaykit && GOWORK=off go build ./...` 必须成功；`relaykit` **不得** import `internal/` 或 `cmd/`。
2. **两段式转换**：`对外请求 → IR → 上游请求`，反向亦然。**禁止**协议间直转（OpenAI → 百炼 直接改字段），否则加第三种协议成本指数上升。
3. **未知字段必须保留**：两个方向的 DTO 都要有 `Extra map[string]json.RawMessage`，未知键原样透传。丢字段会让客户端用高级参数时静默失效。
4. **上游 429 绝不转成客户端 429**：必须 503 + `upstream_capacity_exceeded` + 上游的等待秒数。转 429 会让用户去自查自己的额度（PRD §3.4.1）。
5. **上游 429 不重试、不换候选**：429 是配额问题不是故障。重试是白等，换候选通常仍是同一上游。
6. **可重试集合严格限定**：`500/502/503/504` 与「连接超时 / 首字节超时 / 读超时」。**DNS 失败、TLS 握手失败、上游空响应、上游 4xx 一律不重试。**
7. **重试次数 = 1**（共 2 次尝试），且**不追加总预算**：非流式 120s、流式 600s 为硬上限，重试必须在剩余预算内完成。
8. **重试门槛**：若剩余预算不足 `gw.retry_threshold_ms`（65s / 45s），**放弃重试直接返回**。避免「重试到一半必超时」。
9. **SSE 必须解析后重发**，不得字节透传。理由：不解析就拿不到 `usage` 与首 token 时间，也无法统一错误帧。
10. **首 token 口径**：从上游**首个含内容增量**的帧起算；`role`/空 delta 帧不计。该值写入 `t_usage_record.first_token_ms`。
11. **无 usage 兜底**：按 `stream_usage_mode` 三档处理；**不得**因为无 usage 就退化为「不计费」或「释放占位」。
12. **客户端断连的部分结算**：已产出 token 计费、未产出部分释放预扣；`hold` 全额退回的路径只允许在「零产出」时走。断连不计为平台失败（不计入成功率分母），但必须落 `t_usage_record`。
13. **心跳只在静默期发**：15s 无数据才发 `: keep-alive`；有数据时插心跳会污染客户端解析器。
14. **每帧写超时 5s**（`ResponseController.SetWriteDeadline`），慢客户端不得拖住 goroutine。
15. **流式不返回 `X-Credits-Consumed`**：只返回 `X-Request-Id` / `X-Credits-Preheld` / `X-Credits-Remaining`。这是物理约束（头在首帧前定型），**不得**为了"对账方便"擅自加。
16. **不得缓存或落盘上游响应体全文**（除 `full_log_enabled` 用户且经信封加密，属 T22 范围）。
17. **渠道 Key 只从配置快照解密取用**，禁止出现在日志、`t_upstream_attempt`、错误体。
18. **`relaykit` 与 `internal/` 的 `.go`/`.sql` 不得出现 `QuantumNous`，也不得成段照搬 new-api 的代码**。理由不是"不允许二次开发"，而是本项目闭源、并入 AGPL 代码即成为衍生作品（errata E-23）；**借鉴其 SSE / 转发思路不需要署名**，注释里可以写来源。
19. **单一 IR（D-15）**：`relaykit/ir` 是唯一 IR 定义。`internal/gw/ir.go` 的 `IRRequest` / `IRMessage` / `IRTool` **必须删除**，`Ctx.IR` 改为 `*ir.IRRequest`，P04/P09 与相关测试同步调整。**禁止**保留两套 IR——两套结构下「未知字段透传」守不住。
20. **渠道载荷按 D-14 补齐**：`ChannelEntry` 增加 `base_url`、`upstream_key_enc`(base64)、`timeout_connect_ms`/`timeout_first_byte_ms`/`timeout_total_ms`、`retry_max`、`stream_usage_mode`、`upstream_model_name_map`，与 TDS §1.4 选路 SQL 的列一致。缺字段时按最安全默认拒绝（503 `config_unavailable`），**不得**猜默认值。
21. **Key 解密只在发送前一刻**：`upstream_key_enc` 解出明文后只活在本次请求的局部变量里，**不得**进 `Ctx`、日志、`t_upstream_attempt`、错误体、metric 标签。KEK 缺失或密文不合法 → 503 `config_unavailable`，不得回退明文。
22. **必须替换并删除 `NotWiredTerminal`**：T07 的临时终端（返 503 + `Settle(consumed=0)` 释放预扣）**不得保留**，`cmd/gatewayd/main.go` 的 `NewHandler(deps, nil)` 改为传入真实 dispatcher。若保留任何回到临时终端的开关，视为未完成。

## 5. 验收标准

| # | 判定 | 方式 |
| :--- | :--- | :--- |
| A1 | `cd relaykit && GOWORK=off go build ./... && go test ./...` 通过 | 复跑 |
| A2 | 黄金转换：OpenAI ⇄ IR ⇄ Anthropic 三轮往返后语义等价；未知字段 `logprobs` 保留 | 单测 |
| A3 | 上游 429（带 `Retry-After: 30`）→ 对外 503 `upstream_capacity_exceeded`、`retry_after_seconds=30`、`t_upstream_attempt` 只有 1 行 | 单测（mock 上游） |
| A4 | 上游 500 → 重试 1 次（共 2 行 attempt），第二次成功 → 对外 200 且**只结算一次** | 单测 |
| A5 | 上游 DNS 失败 → **不重试**（1 行 attempt），对外 502 | 单测 |
| A6 | 预算不足门槛：剩余 40s 且非流式 → 不重试，直接 504 | 单测 |
| A7 | SSE 半帧：把 `data: {"cho` 与 `ices":[]}` 分两次喂入 → 正确拼成一帧 | 单测 |
| A8 | 上游无 `[DONE]` 直接断 → 已产出部分正常收尾，`usage` 走兜底，结算不为 0 | 单测 |
| A9 | 首 token：`role` 帧在前、内容帧在后 → `first_token_ms` 取内容帧时刻 | 单测 |
| A10 | 客户端断连：产出 200 token 后断开 → 结算 200 token 对应费用，剩余预扣释放；成功率不计失败 | 单测 |
| A11 | 心跳：静默 15s 后出现 `: keep-alive`；有数据流时不出现 | 单测 |
| A12 | 流式响应头三件套齐备，**无** `X-Credits-Consumed` | 手工 curl |
| A13 | **端到端联调（对 `cmd/fakeupstream`）**：`/v1/chat/completions` 非流式与流式各 1 次成功，流式须验证「解析后重发」而非字节透传（假上游注入一个非标准帧，客户端不应收到原样字节）。**真实境内上游本次无法联调**（`0019` 的渠道 `base_url=https://local.invalid/v1` 是占位、Key 是假值），登记为遗留，**不得**用假上游冒充真实上游宣称通过 | 手工 |
| A15 | `grep -rn "v1:" internal/ --include=*.go` 之外不得出现明文 Key 回退分支；`keytool` 加密后库内 `upstream_key_enc` 以 `v1:` 开头 | 复跑 |
| A14 | `grep -rn "QuantumNous" relaykit internal` 为空 | grep |

## 6. 自检命令

```bash
cd relaykit && GOWORK=off go build ./... && go test ./... && cd ..
go test ./internal/dispatch/... ./internal/stream/... -race -cover
grep -rn "QuantumNous" relaykit/ internal/ || echo "OK"
curl -N -s -D - localhost:8082/v1/chat/completions -H "Authorization: Bearer $KEY" \
  -d '{"model":"qwen-max","messages":[{"role":"user","content":"hi"}],"stream":true}' | head -20
```

## 7. 禁止事项

- 不得把 SSE 直接 `io.Copy` 给客户端。
- 不得在 `relaykit` 里放限流、计费、日志逻辑。
- 不得对上游 429 做退避重试（TDS E-06）。
- 不得用「客户端 429」表示上游限流。
- 不得为了首字节更快而跳过协议解析（AD-05 已定案）。

## 8. 完成回执（已归档 · 2026-10-26）

| 项 | 内容 |
| :--- | :--- |
| 卡号 | T08 |
| 完成状态 | 完成（21 项交付物；A13 改为对假上游，真实上游见遗留） |
| 验收结论 | ✅ **通过**（验收时修掉 1 处真缺陷：环境代理污染） |
| 复核人 | 交付总监 |

**交付物**：`relaykit/`（独立 module：`ir/` + `openai/` + `anthropic/` + `bailian/` + 15 个 fixture）、`internal/dispatch/`、`internal/stream/`、`internal/cryptobox/`、`cmd/keytool/`、`cmd/fakeupstream/`、`docs/relay-contract.md`（128 行）。

**自检结果**

```bash
cd relaykit && GOWORK=off go build ./... && GOWORK=off go test ./...
ok  yunshu.dev/relaykit  0.029s      # 57 条 RUN 全 PASS，仅标准库
go test ./... -count=1               # 21 个包全绿

A3 429   attempts=1 outward=503 upstream_capacity_exceeded retry_after=30 cooldown=30s
A4 500   attempt1 500 RETRY_SAME_ROUTE → attempt2 200 SUCCESS；settle 恰 1 次
A5 DNS   attempts=1 outward=502 upstream_dns_failure class=DNS（不重试）
A6 预算   attempts=1 outward=504 upstream_timeout（剩余 40s < 65s 门槛）
A7 半帧   `data: {"cho` + `ices":[]}` → 正确拼成一帧
A8 无DONE 部分收尾，usage 走兜底，结算不为 0
A9 首token role 帧在前 → first_token_ms 取内容帧
A10 断连  consumed=405 released=595
A11 心跳  静默 15s 出 `: keep-alive`；有数据时不发
A12 流式头 preheld=1000 remaining=9000，**无 X-Credits-Consumed**
A13 联调  非流式 consumed=13 remaining=9987；流式 preheld=1000 remaining=9000
A15 加密  库内 upstream_key_enc = `v1:4YbYKwZ…`（59 字节），mask = sk-****-key

覆盖率   dispatch 67.5% / stream 73.6% / cryptobox 93.0%
-race    容器内 dispatch / stream / cryptobox / gw 四包全 PASS
grep     QuantumNous 0；internal/stream 内 io.Copy 0；:8080 仅 env 默认值
```

**启动装配实测**：`gatewayd` 带真实 dispatcher 起来后 `/healthz` 200、`/readyz` 三依赖 ok、无鉴权请求返 401 `invalid_api_key` 且带 `request_id`。

**验收时我修掉的一处真缺陷（不是实现方漏写）**

`dispatch` 用 `Proxy: http.ProxyFromEnvironment` 建 Transport。本机环境有 `HTTP_PROXY=127.0.0.1:62934`，于是：

1. `no-such-host.invalid` **根本没做本地 DNS 解析**，被送去代理，代理返 5xx → A5 判成 `upstream_all_failed`（用例失败，card A5 要求 `upstream_dns_failure`）；
2. 更要紧的是生产语义：**所有客户请求体都会经过运维环境里随手 export 的代理**——多一跳、延迟不可控、per-channel 超时预算失效、第三方能看到请求内容，而 V1 上游全是 `region=DOMESTIC`，根本不需要代理。

已改为 `Proxy: nil` 并在代码注释写明理由；将来要按渠道走代理必须是显式配置。改后 A5 通过，dispatch 全绿。

**偏离与遗留**

| # | 项 | 处置 |
| :--- | :--- | :--- |
| 1 | **真实境内上游未联调** | `0019` 的渠道 `base_url=https://local.invalid/v1` 是占位、Key 是假值，A13 只能对 `cmd/fakeupstream` 做。**待有真实渠道后必须补**，不得以假上游充当验收 |
| 2 | **ROUTING 快照仍无生产方**（T07 遗留 #1，D-14 已定载荷） | 网关能起、能鉴权失败，但**无法走到 P12 选路**——没有快照、也没有 API Key。真实端到端要等 T11 |
| 3 | 实现方越界 | 本轮只派了「第一段 relaykit」，实际交付了整张卡。内容我逐项复核通过，但**回执与派单范围不一致**；后续派单会明确「只做本段、超范围的部分留待下一段」，避免验收口径与工作量对不上 |
| 4 | Anthropic `error` 对象内部非标准成员只留顶层 Extra | 影响面极小，T10 的 SSE 错误帧已覆盖对外契约 |
| 5 | `image_url.detail` / `source.cache_control` 上提到 `ContentPart.Extra` | IR 规范化选择，已在 `relay-contract.md` 表 1 标注 |

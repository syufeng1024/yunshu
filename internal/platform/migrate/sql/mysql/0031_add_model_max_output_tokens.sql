-- 0031 t_model.max_output_tokens：让「模型自身的输出上限」成为可配置项。
--
-- 为什么需要它：internal/boot 的模型快照结构体（modelEntry）早就声明了
-- max_output_tokens 字段，但 t_model 没有对应列，loadModels 的 SELECT 也从未
-- 选过它，于是快照里该字段恒为 0；网关估算的第二级兜底
-- （`if model.MaxOutputTokens > 0`）永远不命中，只能退到
-- min(context_length, 平台输出上限)。结果是运营无法按模型真实能力配置输出
-- 上限，每个模型都被按上下文长度保守裁剪后的值估算。
--
-- 为什么是 int unsigned NOT NULL DEFAULT 0 而不是可空列：与相邻的
-- context_length 同一风格，0 表示「未配置」，沿用既有语义；可空列会让快照
-- 装配、后台 DTO、前端表单三处都多一层判空。
--
-- 0 不代表「不能输出」：它是哨兵值，消费方按 min(context_length, 32768) 兜底
-- （见 internal/gw/p09_estimate.go）。
--
-- 为什么只加列不改网关：平台输出上限（32768，客户端 max_tokens 的硬上限）与
-- 模型自身能力是两个不同的量。本迁移只负责让模型能力可配置，不做两者联动。
--
-- 存量行不回填：0 即「未配置」，语义正确；逐条订正需要上游能力清单，
-- 不在本迁移范围内。
--
-- 回滚见 0031_add_model_max_output_tokens.down.sql：只 DROP 列。网关读的是
-- Redis 里的快照 JSON，不直连该列，掉列不会让正在服务的请求失败；快照里的值
-- 会在下一次 tick 后回到 0（未配置）。

ALTER TABLE t_model
  ADD COLUMN max_output_tokens int unsigned NOT NULL DEFAULT 0
    COMMENT '模型自身输出上限（tokens）；0 = 未配置，按 min(context_length, 32768) 兜底'
    AFTER context_length;

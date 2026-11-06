-- 0031 回滚：只 DROP 列。
--
-- 该列只被两处读写：快照装配（internal/boot/route_snapshot.go 的 loadModels）
-- 与后台模型接口（internal/control/ops/model.go）。回滚到旧版本二进制后这两处
-- 都不再引用该列，所以下面是安全的逆操作。
--
-- 网关不直连数据库：它读 Redis 里的 cfg:snap:ROUTING JSON，掉列不会让任何
-- 正在服务的请求失败——快照重发后 max_output_tokens 回到 0（未配置），
-- 估算退回 min(context_length, 32768) 兜底。
--
-- 回滚会丢掉已配置的值，且无法从旧快照还原成列数据；这是可逆迁移的正常代价。

ALTER TABLE t_model DROP COLUMN max_output_tokens;

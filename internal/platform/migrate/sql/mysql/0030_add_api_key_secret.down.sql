-- 0030 回滚：只 DROP 列。
--
-- key_enc 是明文的副本，鉴权链路（K13 + t_api_key.key_hash）不读它，
-- 掉列不会让任何正在使用的密钥失效。已经被用户复制走的明文本来就收不回来，
-- 所以这里不需要（也无法）做数据层的补救。

ALTER TABLE t_api_key DROP COLUMN key_enc;

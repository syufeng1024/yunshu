-- 0029 回滚：只 DROP 表。
--
-- 台账里的 trial_granted_at 是 90 天窗口的唯一记忆，表一掉窗口就没有数据源
-- （代码侧 fail-closed 会直接 500，不会静默放行）；而 t_user.phone / phone_hmac
-- 一旦被释放，明文已不可恢复（hmac 可由明文重算，明文不能由 hmac 还原）。
-- 所以生产回滚口径是「前滚修复」，不是本脚本；本脚本只用于本地与 CI。

DROP TABLE t_phone_claim;

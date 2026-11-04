-- 0027 回滚：删除 username 列与 uk_user_username。
--
-- 先 DROP INDEX 再 DROP COLUMN：DROP COLUMN 会隐式删掉只含该列的唯一索引，
-- 但依赖隐式行为写回滚脚本会让读者无法判断「索引到底有没有被删」，
-- 而且 MySQL 在多列索引场景下并不会自动删除整条索引。这里显式两步。
--
-- 回滚的代价：所有 username 值随之丢失，内置超管将无法用用户名登录
-- （只能靠 email/phone）。回滚前请确认没有环境依赖 username 登录。

ALTER TABLE t_user
  DROP INDEX uk_user_username,
  DROP COLUMN username;

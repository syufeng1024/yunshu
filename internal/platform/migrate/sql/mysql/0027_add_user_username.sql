-- 0027 t_user.username：内置超级管理员与运营账号的登录用户名。
--
-- 为什么单独加一列而不是复用 email/phone：
--   内置超管没有任何真实邮箱/手机号可以收验证码，也不该占用一个虚构地址
--   （那会让「找回密码」把信发到一个不存在的信箱）。登录标识三列
--   （user_no / username / email）排在一起，读到 SHOW CREATE 输出的人一眼
--   能看出 username 是登录标识而不是展示名，因此位置写 AFTER user_no。
--   （本节刻意不写建表语句的全大写词形：guard_test 按该词形计数，
--   注释里出现一次就会被判成一次真实建表。）
--
-- 为什么 DEFAULT NULL 而不是 NOT NULL DEFAULT ''：
--   1) 存量行不写回填、不锁表；
--   2) 只有运营账号有用户名，全部客户账号保持 NULL；
--   3) InnoDB 的唯一索引把 NULL 当作互不相等，任意多行 username=NULL 都允许，
--      所以已有客户账号不会因为「NULL 重复」撞上 uk_user_username
--      （与 0011 把手机号唯一性挪到可空列的判据一致）。
--   本迁移不更新任何存量行。
--
-- username 由应用层归一化为小写后写入（唯一实现见 internal/control/auth
-- 的 usernameRE：^[a-z][a-z0-9._-]{2,31}$）。排序规则是
-- utf8mb4_0900_as_cs（大小写敏感），不归一化就会允许 Admin 与 admin 双份账号，
-- 所以列宽 32 与正则的 32 字节上限严格对齐。
--
-- t_user 没有配套视图、没有触发器；deploy/seed/*.sql 不直接 INSERT INTO t_user
-- （@uid 由调用方给出），新列不影响种子脚本。
--
-- 回滚见 0027_add_user_username.down.sql：先 DROP INDEX 再 DROP COLUMN。
-- 单列索引会被 DROP COLUMN 隐式删除，显式写出来才是可读的回滚脚本。

ALTER TABLE t_user
  ADD COLUMN username varchar(32) DEFAULT NULL
    COMMENT '运营/超管登录用户名；小写归一，仅运营账号有值，普通用户为 NULL' AFTER user_no,
  ADD UNIQUE KEY uk_user_username (username);

-- 0028 t_user.password_is_initial：标记"仍在使用开通时下发的初始口令"。
--
-- 为什么需要一列，而不是在登录时拿 hash 去和默认口令比对：
--   1) 比对要付一次 bcrypt（cost 12，约 100ms），而这是每次登录都要付的；
--   2) 默认口令是公开值，任何账号都可能恰好用它；只有"开通时由系统写入"这个
--      事实能区分"初始口令"与"用户自己选的同一串字符"。
--   因此由写入方打标、由改密方清零，登录只读标记，不做任何推断。
--
-- 语义：
--   * 只有内置超级管理员引导（internal/control/bootstrap）在插入时置 1；
--   * POST /api/v1/auth/password 成功后在同一事务里置 0；
--   * 其余所有路径（注册、运营建号）保持 DEFAULT 0。
--
-- 这一列只驱动"登录后提醒"，不参与任何鉴权或拦截：置 1 的账号照常登录、
-- 照常访问全部接口。是否提醒由前端按 /auth/me 的 password_warning 决定。

ALTER TABLE t_user
  ADD COLUMN password_is_initial tinyint(1) NOT NULL DEFAULT 0
    COMMENT '1=仍用开通时下发的初始口令（仅内置超管置 1，改密清零）；只用于提醒，不拦截'
    AFTER password_updated_at;

-- 存量行默认 0，但**升级前的内置超管要补标**，否则这次改动在已有环境里等于
-- 不存在：那些行是 0027 时代由引导插入的，密码从来没被改过，却没有标记。
--
-- 判据是"口令时间戳等于建号时间戳"，即插入后没有发生过 POST /auth/password
-- （该接口会把 password_updated_at 更新到改密时刻）。再叠加引导账号的三个
-- 结构特征（SUPER_ADMIN + 有用户名 + 无邮箱/手机号），把误伤面收到最小：
-- 运营用邮箱/手机号创建的 SUPER_ADMIN 不在此列，客户账号也不在此列。
--
-- 误判代价是把"用户自己设的口令"错标成初始口令 → 多显示一条提醒，改密后即
-- 消失；不涉及任何拦截，可接受。
UPDATE t_user
   SET password_is_initial = 1
 WHERE role = 'SUPER_ADMIN'
   AND username IS NOT NULL
   AND email IS NULL
   AND phone IS NULL
   AND password_updated_at = created_at;

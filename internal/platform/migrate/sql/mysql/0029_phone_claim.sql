-- 0029 t_phone_claim：手机号台账（一号码一行，无状态机）。
--
-- 为什么需要它：0011 把唯一性挪到 t_user.phone_hmac 之后，「注销 = 占号」，
-- 用户被自己的号码永久锁死（同号永远 409）；而「同号 90 天内不得再领体验版」
-- 从未实现（fund/trial.go 只做跨账号等值判重，无时间窗）。本轮注销即释放号段，
-- 释放后 t_user 行上不再有号码，唯一的记忆只能落在号码维度的台账上。
--
-- 为什么没有 state 列：占用状态的唯一权威是「是否存在一行 t_user.phone_hmac = H」；
-- 台账只记事实（首登、最近注销、注销次数、最近发放）。UNIQUE(phone_hmac, state)
-- 会让同一号码第二次 releasing 撞 1062，绕开它的手段（release_seq / RELEASED 存
-- NULL）都是隐式约定，且 state 会与 t_user 漂移。
--
-- 排序规则必须与 t_user.phone_hmac 完全一致（utf8mb4_0900_as_cs）：回填/校验/客服
-- 查询都要 JOIN ON u.phone_hmac = c.phone_hmac，两边 charset 不同会退化成隐式
-- 转换 + 无法用索引。
--
-- 不设清理 Job：一行一号码（上界 = 出现过手机号的号码数），hmac 是伪名化标识
-- 不含明文，且它是「该号码是否领过体验版」的唯一记忆——清掉它等于把 90 天窗口
-- 一起废止。清理的前提只能是窗口改由别处承载。
--
-- 回滚见 0029_phone_claim.down.sql：只 DROP 表；已释放的号码不可回滚（见设计 §9）。

CREATE TABLE t_phone_claim (
  phone_hmac         char(64)        NOT NULL COMMENT 'HMAC-SHA256(手机号, PHONE_HMAC_KEY) hex；一号码一行，台账主键',
  first_user_id      bigint unsigned NOT NULL COMMENT '首次认领该号码的账号 id；不随注销/重注册变化',
  last_claim_user_id bigint unsigned DEFAULT NULL COMMENT '最近一次注册占用该号码的账号 id',
  last_claim_at      datetime(3)     DEFAULT NULL COMMENT '最近一次被注册占用的时点',
  last_cancel_at     datetime(3)     DEFAULT NULL COMMENT '最近一次因注销释放的时点；存量释放取原 cancel_apply_at',
  cancel_count       int unsigned    NOT NULL DEFAULT 0 COMMENT '该号码被注销释放的累计次数',
  trial_granted_at   datetime(3)     DEFAULT NULL COMMENT '该号码最近一次发放体验版的时点；90 天窗口的唯一锚点',
  last_trial_user_id bigint unsigned DEFAULT NULL COMMENT '最近一次领到体验版的账号 id',
  created_at         datetime(3)     NOT NULL COMMENT '台账建立时点（= 号码首次被认领时间）',
  updated_at         datetime(3)     NOT NULL,
  PRIMARY KEY (phone_hmac)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

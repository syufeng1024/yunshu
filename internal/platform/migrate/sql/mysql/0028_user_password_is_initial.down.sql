-- 0028 回滚：删除"初始口令"标记列。
--
-- 回滚后前端不再收到 password_warning，所有账号都不再显示改密提醒；
-- 已经改过密码的账号不受影响（该列本来就只是提示位，不参与鉴权）。

ALTER TABLE t_user
  DROP COLUMN password_is_initial;

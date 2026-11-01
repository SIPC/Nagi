-- 多用户：角色（admin、operator、viewer）。已有的账号都是管理员。
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'admin';
-- 会话管理：登录时的客户端地址。
ALTER TABLE sessions ADD COLUMN ip TEXT NOT NULL DEFAULT '';

-- 高权限确认的有效期：打开节点终端、访问节点文件前要再验证一次（见 docs/node-access.md）。
ALTER TABLE sessions ADD COLUMN elevated_until INTEGER NOT NULL DEFAULT 0;

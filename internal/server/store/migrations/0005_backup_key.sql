-- 项目快照仓库的密码（config/backup.key），用面板主密钥加密。节点坏了只剩备份文件时，用它打开仓库。
ALTER TABLE project_backups ADD COLUMN backup_key BLOB;

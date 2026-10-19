-- 节点加入时提交的 WireGuard 公钥。mesh 里以它为准：节点之后上报的公钥不一致就不采用，
-- 防止一个节点冒用别的节点的公钥。
ALTER TABLE nodes ADD COLUMN mesh_key BLOB;
-- 用接入码加入的节点要管理员批准：批准前不进 mesh、不下发告警配置、不能对它操作。
ALTER TABLE nodes ADD COLUMN pending INTEGER NOT NULL DEFAULT 0;

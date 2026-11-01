-- 跨节点入口：其他入口节点（节点 ID 的 JSON 数组），以及是否不从容器所在节点进来。
ALTER TABLE routes ADD COLUMN entry_nodes TEXT NOT NULL DEFAULT '[]';
ALTER TABLE routes ADD COLUMN no_local_entry INTEGER NOT NULL DEFAULT 0;

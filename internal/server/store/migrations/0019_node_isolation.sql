-- 被隔离的节点（见 docs/mesh.md#隔离）：从所有节点的 mesh 成员里去掉，撤销项目放行，停止为它中转。
ALTER TABLE nodes ADD COLUMN isolated INTEGER NOT NULL DEFAULT 0;

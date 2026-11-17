-- 容器在某个备份节点上单独指定的副本存放位置（见 docs/snapshot.md#副本的存放位置）。没有记录的用节点的默认目录。
-- 节点自己记着副本实际在哪里（是准的），这里只用于界面显示。
CREATE TABLE backup_replica_dirs (
    project_id TEXT NOT NULL,
    container  TEXT NOT NULL,
    node_id    TEXT NOT NULL,
    dir        TEXT NOT NULL,
    PRIMARY KEY (project_id, container, node_id),
    FOREIGN KEY (project_id, container) REFERENCES containers (project_id, name) ON DELETE CASCADE
);

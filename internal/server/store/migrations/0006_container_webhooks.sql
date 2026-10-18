-- webhook 改为每个容器一个（项目由多个容器组成）。令牌由节点每次上报时重新登记，直接重建表即可。
DROP TABLE webhooks;

CREATE TABLE webhooks (
    token_hash TEXT PRIMARY KEY,
    node_id    TEXT NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    project_id TEXT NOT NULL,
    container  TEXT NOT NULL,
    UNIQUE (node_id, project_id, container)
);

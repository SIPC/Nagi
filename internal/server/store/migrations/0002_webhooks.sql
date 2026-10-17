-- webhook 令牌到项目的映射，由节点上报的项目信息维护。
CREATE TABLE webhooks (
    token_hash TEXT PRIMARY KEY,
    node_id    TEXT NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    project_id TEXT NOT NULL,
    UNIQUE (node_id, project_id)
);
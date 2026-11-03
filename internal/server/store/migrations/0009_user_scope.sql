-- 访问范围（见 docs/panel.md#访问范围）：操作员和只读成员可以只访问指定的项目、节点。默认全部，已有的账号不受影响。
ALTER TABLE users ADD COLUMN all_projects INTEGER NOT NULL DEFAULT 1;
ALTER TABLE users ADD COLUMN all_nodes INTEGER NOT NULL DEFAULT 1;

CREATE TABLE user_projects (
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    project_id TEXT    NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, project_id)
);

-- 节点移除时只是标记 revoked，所以移除时要另外删掉（DeleteNodeFromScopes）。
CREATE TABLE user_nodes (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    node_id TEXT    NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, node_id)
);

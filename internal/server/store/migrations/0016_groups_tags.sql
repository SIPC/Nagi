-- 项目分组和节点标签（见 docs/panel.md#分组和标签）：只在面板上，不下发给节点。
-- 项目最多属于一个分组，没有记录表示未分组；分组没有单独的表，有项目在里面就存在。
CREATE TABLE project_groups (
    project_id TEXT NOT NULL PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    name       TEXT NOT NULL
);
CREATE INDEX project_groups_name ON project_groups (name);

-- 节点移除时只是标记 revoked，所以移除时要另外删掉（DeleteNodeFromScopes）。
CREATE TABLE node_tags (
    node_id TEXT NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    tag     TEXT NOT NULL,
    PRIMARY KEY (node_id, tag)
);
CREATE INDEX node_tags_tag ON node_tags (tag);

-- 访问范围里的分组和标签：分组里的项目、带这些标签的节点都在范围内，以后加进来的也算。
CREATE TABLE user_project_groups (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name    TEXT    NOT NULL,
    PRIMARY KEY (user_id, name)
);

CREATE TABLE user_node_tags (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    tag     TEXT    NOT NULL,
    PRIMARY KEY (user_id, tag)
);

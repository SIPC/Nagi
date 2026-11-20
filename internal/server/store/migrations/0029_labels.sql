-- 节点分组（见 docs/panel.md#分组和标签）：只用来整理节点页，不影响访问范围（访问范围按标签），不下发给节点。
-- 节点最多属于一个分组，没有记录表示未分组。节点移除时只是标记 revoked，所以移除时要另外删掉。
CREATE TABLE node_groups (
    node_id TEXT NOT NULL PRIMARY KEY REFERENCES nodes (id) ON DELETE CASCADE,
    name    TEXT NOT NULL
);
CREATE INDEX node_groups_name ON node_groups (name);

-- 节点页的排序，同 user_project_order：每个成员各自一份。
-- sort：排序方式，空是加入时间，name 按名称，custom 自定义。groups、nodes：自定义顺序里的分组名、节点 ID，一行一个。
CREATE TABLE user_node_order (
    user_id INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    sort    TEXT NOT NULL DEFAULT '',
    groups  TEXT NOT NULL DEFAULT '',
    nodes   TEXT NOT NULL DEFAULT ''
);

-- 项目标签（见 docs/panel.md#分组和标签）：只用来整理项目页，不影响访问范围（访问范围按分组），不下发给节点。
CREATE TABLE project_tags (
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    tag        TEXT NOT NULL,
    PRIMARY KEY (project_id, tag)
);
CREATE INDEX project_tags_tag ON project_tags (tag);
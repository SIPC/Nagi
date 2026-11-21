-- 告警规则的覆盖（见 docs/monitoring.md#告警）：节点、项目、容器上改过的规则字段。rules 是 AlertOverride.rules 的
-- protobuf（只有改过的字段），revision 防止两个人同时改时互相覆盖。全局默认值在 settings 的 alert_policy 里。
-- 旧的 nodes.alert_rules、containers.alert_rules 和项目设置里的 alert_rules 保留（回滚到旧版本时还在），升级后不再使用。
CREATE TABLE alert_node_overrides (
    node_id  TEXT    NOT NULL PRIMARY KEY,
    rules    BLOB    NOT NULL,
    revision INTEGER NOT NULL
);

CREATE TABLE alert_project_overrides (
    project_id TEXT    NOT NULL PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    rules      BLOB    NOT NULL,
    revision   INTEGER NOT NULL
);

CREATE TABLE alert_container_overrides (
    project_id TEXT    NOT NULL,
    container  TEXT    NOT NULL,
    rules      BLOB    NOT NULL,
    revision   INTEGER NOT NULL,
    PRIMARY KEY (project_id, container),
    FOREIGN KEY (project_id, container) REFERENCES containers (project_id, name) ON DELETE CASCADE
);

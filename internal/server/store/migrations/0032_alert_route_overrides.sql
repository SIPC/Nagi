-- 映射层的告警规则覆盖（见 docs/monitoring.md#告警）：全局 → 项目 → 容器 → 映射。只对按映射判断的规则有效
-- （HTTP 的 5xx、4xx、请求突增和 L4 规则）。删除映射时一起删掉。
CREATE TABLE alert_route_overrides (
    project_id TEXT    NOT NULL,
    route      TEXT    NOT NULL,
    rules      BLOB    NOT NULL,
    revision   INTEGER NOT NULL,
    PRIMARY KEY (project_id, route),
    FOREIGN KEY (project_id, route) REFERENCES routes (project_id, name) ON DELETE CASCADE
);

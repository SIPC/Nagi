-- 活动记录：用户在面板上的操作和节点、面板发出的告警。只保留最近的一部分（见 PruneActivity）。
CREATE TABLE activity (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at INTEGER NOT NULL,
    kind       TEXT NOT NULL,              -- operation：操作；alert：告警
    actor      TEXT NOT NULL DEFAULT '',   -- 操作的用户，或发出告警的节点
    action     TEXT NOT NULL,              -- 接口方法名；告警是 firing 或 resolved
    project_id TEXT NOT NULL DEFAULT '',
    target     TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    failed     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX activity_kind_id ON activity (kind, id);

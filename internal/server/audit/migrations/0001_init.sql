-- 审计记录（见 docs/audit.md）：只追加，每条带哈希链。
CREATE TABLE events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    time         INTEGER NOT NULL,              -- 毫秒
    category     TEXT NOT NULL,                 -- operation、read、mcp、session、login、alert、security
    action       TEXT NOT NULL,
    actor_type   TEXT NOT NULL DEFAULT '',      -- user、token、node、panel、system
    actor_id     TEXT NOT NULL DEFAULT '',
    actor_name   TEXT NOT NULL DEFAULT '',
    via          TEXT NOT NULL DEFAULT '',      -- web、mcp、cli、system、node
    token_id     INTEGER NOT NULL DEFAULT 0,
    client       TEXT NOT NULL DEFAULT '',
    session      TEXT NOT NULL DEFAULT '',
    ip           TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT '',
    project_id   TEXT NOT NULL DEFAULT '',
    target_type  TEXT NOT NULL DEFAULT '',
    target_id    TEXT NOT NULL DEFAULT '',
    target_name  TEXT NOT NULL DEFAULT '',
    params       TEXT NOT NULL DEFAULT '',
    result       TEXT NOT NULL DEFAULT 'ok',    -- ok、failed、denied
    error        TEXT NOT NULL DEFAULT '',
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    parent_id    INTEGER NOT NULL DEFAULT 0,
    recording_id TEXT NOT NULL DEFAULT '',
    prev_hash    TEXT NOT NULL DEFAULT '',
    hash         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX events_time ON events (time);
CREATE INDEX events_category ON events (category, id);
CREATE INDEX events_project ON events (project_id, id);
CREATE INDEX events_actor ON events (actor_id, id);
CREATE INDEX events_target ON events (target_type, target_id, id);
CREATE INDEX events_parent ON events (parent_id) WHERE parent_id <> 0;

-- 审计库自己的状态：迁移进度、最近一次校验等。
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

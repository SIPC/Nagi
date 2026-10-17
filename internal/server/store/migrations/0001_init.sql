CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    created_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
    token_hash TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE TABLE enroll_tokens (
    token_hash TEXT    PRIMARY KEY,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
);

CREATE TABLE nodes (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL DEFAULT 0,
    -- 节点上报的 NodeInfo，protobuf 二进制
    info       BLOB,
    revoked    INTEGER NOT NULL DEFAULT 0
);

-- 节点上报的项目配置备份，以节点为准
CREATE TABLE project_backups (
    node_id    TEXT    NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    project_id TEXT    NOT NULL,
    revision   INTEGER NOT NULL,
    -- Project，protobuf 二进制
    data       BLOB    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (node_id, project_id)
);

-- 项目自身的数据，存在项目目录的 config/project.db 里，随项目目录一起走。
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- 每个版本（一次部署）：配置（ProjectConfig，protobuf 二进制）、文件快照、镜像摘要。版本不可变。
CREATE TABLE versions (
    version        INTEGER PRIMARY KEY,
    config         BLOB    NOT NULL,
    created_at     INTEGER NOT NULL,
    deployed_at    INTEGER,
    -- 版本文件的快照 ID（kopia manifest），没有时为空
    files_snapshot TEXT    NOT NULL DEFAULT '',
    -- 服务名 → 镜像摘要（JSON）
    images         TEXT    NOT NULL DEFAULT '{}',
    rollback_of    INTEGER NOT NULL DEFAULT 0,
    git_commit     TEXT    NOT NULL DEFAULT ''
);

-- 未部署的草稿，最多一份。
CREATE TABLE draft (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    config     BLOB    NOT NULL,
    updated_at INTEGER NOT NULL
);
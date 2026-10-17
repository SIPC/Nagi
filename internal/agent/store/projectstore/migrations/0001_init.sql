-- 项目自身的数据，存在项目目录的 config/project.db 里，随项目目录一起走。
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE versions (
    version     INTEGER PRIMARY KEY,
    compose     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    deployed_at INTEGER
);

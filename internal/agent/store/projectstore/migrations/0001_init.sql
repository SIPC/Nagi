-- 项目自身的数据，存在项目目录的 config/project.db 里，随项目目录一起走。
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- 每个版本的完整配置（ProjectConfig，protobuf 二进制），版本不可变。
CREATE TABLE versions (
    version     INTEGER PRIMARY KEY,
    config      BLOB    NOT NULL,
    created_at  INTEGER NOT NULL,
    deployed_at INTEGER
);

-- 未部署的草稿，最多一份。
CREATE TABLE draft (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    config     BLOB    NOT NULL,
    updated_at INTEGER NOT NULL
);
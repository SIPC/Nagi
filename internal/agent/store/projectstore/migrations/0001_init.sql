-- 项目自身的数据，存在项目目录的 config/project.db 里，随项目目录一起走（同步给顶替节点和备份节点时也带上）。
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- 容器：项目里独立部署的单位，单个容器（镜像表单）或容器组（一份 compose）。容器名创建后不能改。
CREATE TABLE containers (
    name             TEXT    PRIMARY KEY,
    -- 创建顺序
    position         INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    -- 修订号：保存草稿、部署都加一，用于多面板冲突检测
    revision         INTEGER NOT NULL DEFAULT 1,
    -- 最近一次部署的版本，0 表示从未部署
    deployed_version INTEGER NOT NULL DEFAULT 0,
    webhook_token    TEXT    NOT NULL,
    last_commit      TEXT    NOT NULL DEFAULT '',
    -- 已经从 compose 导入过的端口（JSON 数组），用户删掉的映射不会再导入
    imported_ports   TEXT    NOT NULL DEFAULT '[]',
    -- 上次部署时版本文件的根对象 ID，用来判断文件有没有变
    last_files_root  TEXT    NOT NULL DEFAULT ''
);

-- 容器的每个版本：配置（ContainerConfig，protobuf 二进制）、部署时用的共享变量（EnvList）、文件快照、镜像摘要。版本不可变。
CREATE TABLE versions (
    container      TEXT    NOT NULL,
    version        INTEGER NOT NULL,
    config         BLOB    NOT NULL,
    shared_env     BLOB    NOT NULL DEFAULT x'',
    created_at     INTEGER NOT NULL,
    deployed_at    INTEGER,
    -- 版本文件的快照 ID（kopia manifest），没有时为空
    files_snapshot TEXT    NOT NULL DEFAULT '',
    -- 服务名 → 镜像摘要（JSON）
    images         TEXT    NOT NULL DEFAULT '{}',
    rollback_of    INTEGER NOT NULL DEFAULT 0,
    git_commit     TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (container, version)
);

-- 容器未部署的草稿，每个容器最多一份。
CREATE TABLE drafts (
    container  TEXT    PRIMARY KEY,
    config     BLOB    NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 部署记录：每次部署（一个或多个容器）一条，记下部署完成后所有容器各自的版本。
CREATE TABLE deployments (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at INTEGER NOT NULL,
    source     TEXT    NOT NULL,
    -- 这次动到的容器（JSON 数组）
    containers TEXT    NOT NULL,
    -- 容器名 → 版本（JSON）
    versions   TEXT    NOT NULL,
    success    INTEGER NOT NULL,
    error      TEXT    NOT NULL DEFAULT '',
    restore_of INTEGER NOT NULL DEFAULT 0
);

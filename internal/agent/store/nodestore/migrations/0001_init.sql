-- 分配给本节点的容器和它们的配置缓存（面板下发的 ContainerSpec，protobuf）。配置以面板为准，
-- 面板不可达时节点用缓存重启、恢复、顶替；面板丢失时新面板用它把项目拼回来。
CREATE TABLE containers (
    project_id       TEXT    NOT NULL,
    name             TEXT    NOT NULL,
    dir              TEXT    NOT NULL UNIQUE,
    spec             BLOB    NOT NULL,
    -- 最近一次部署时 data/ 的内容（版本文件快照的根对象），文件变了要重建挂载 data/ 的服务
    files_root       TEXT    NOT NULL DEFAULT '',
    last_snapshot    INTEGER NOT NULL DEFAULT 0,
    last_maintenance INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    PRIMARY KEY (project_id, name)
);

-- 容器网段的分配。owner：p:<项目 ID>（项目内网，/24）、g:<项目 ID>/<容器组>（容器组自己的网络，/26）。
CREATE TABLE subnets (
    subnet TEXT PRIMARY KEY,
    owner  TEXT NOT NULL UNIQUE
);

-- 监控数据：最近 24 小时每 10 秒一个点（raw），30 天内每 5 分钟一个点（m5）。
CREATE TABLE metrics_raw (
    series TEXT    NOT NULL,
    ts     INTEGER NOT NULL,
    value  REAL    NOT NULL,
    PRIMARY KEY (series, ts)
) WITHOUT ROWID;

CREATE TABLE metrics_m5 (
    series TEXT    NOT NULL,
    ts     INTEGER NOT NULL,
    value  REAL    NOT NULL,
    PRIMARY KEY (series, ts)
) WITHOUT ROWID;

CREATE INDEX metrics_raw_ts ON metrics_raw (ts);
CREATE INDEX metrics_m5_ts ON metrics_m5 (ts);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE users (
    id             INTEGER PRIMARY KEY,
    username       TEXT    NOT NULL UNIQUE,
    password_hash  TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    -- 两步验证：密钥用面板主密钥加密后存（base64），为空表示没开启；last_step 防止同一个验证码用两次。
    totp_secret    TEXT    NOT NULL DEFAULT '',
    totp_last_step INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE sessions (
    token_hash TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

-- 加入令牌：安装命令用的一次性令牌；multi_use 是接入码（1 小时内可以给多个节点用）。
CREATE TABLE enroll_tokens (
    token_hash TEXT    PRIMARY KEY,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER,
    multi_use  INTEGER NOT NULL DEFAULT 0,
    uses       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE nodes (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL DEFAULT 0,
    -- 节点上报的 NodeInfo，protobuf 二进制
    info       BLOB,
    revoked    INTEGER NOT NULL DEFAULT 0,
    -- 节点加入时提交的 WireGuard 公钥。mesh 里以它为准：节点之后上报的公钥不一致就不采用。
    mesh_key   BLOB,
    -- 用接入码加入的节点要管理员批准：批准前不进 mesh、不下发告警配置、不能对它操作。
    pending    INTEGER NOT NULL DEFAULT 0
);

-- 项目和容器的配置以面板为准（见 docs/panel.md#项目数据）。带密钥的字段用面板主密钥加密。
CREATE TABLE projects (
    id               TEXT    PRIMARY KEY,
    name             TEXT    NOT NULL,
    network_name     TEXT    NOT NULL UNIQUE,
    revision         INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    -- ProjectSettings（protobuf，加密）：共享变量是已经部署过的那份
    settings         BLOB    NOT NULL,
    -- 共享变量的草稿（EnvList，protobuf，加密），NULL 表示没有草稿
    shared_env_draft BLOB,
    -- 登录过的私有仓库（docker 的 config.json，加密），NULL 表示没有
    docker_config    BLOB
);

CREATE TABLE containers (
    project_id       TEXT    NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name             TEXT    NOT NULL,
    -- 创建顺序
    position         INTEGER NOT NULL,
    node_id          TEXT    NOT NULL,
    dir              TEXT    NOT NULL,
    revision         INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    deployed_version INTEGER NOT NULL DEFAULT 0,
    -- 草稿（ContainerConfig，protobuf，加密），NULL 表示没有草稿
    draft            BLOB,
    -- webhook 令牌的哈希（POST /hooks/<令牌> 拉取并部署这个容器）
    webhook_hash     TEXT    NOT NULL UNIQUE,
    -- webhook 令牌、deploy key 私钥、快照仓库密码（JSON，加密）
    secrets          BLOB    NOT NULL,
    deploy_key_pub   TEXT    NOT NULL,
    -- 从 compose 导入过的端口（服务/容器端口/协议，JSON 数组）：用户改过或删掉的映射不会被覆盖或加回来
    imported_ports   TEXT    NOT NULL DEFAULT '[]',
    last_commit      TEXT    NOT NULL DEFAULT '',
    -- 备份节点（见 docs/snapshot.md#异地副本），空表示没有设置
    backup_node_id   TEXT    NOT NULL DEFAULT '',
    -- 顶替设置（FailoverConfig，protobuf），NULL 表示没有设置（见 docs/failover.md）
    failover         BLOB,
    -- 面板知道的最新租约轮次：持有者上报更高的轮次时，面板据此改容器的所在节点
    failover_epoch   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, name)
);

-- 容器的版本：每次部署一个容器生成一个。版本文件（data/ 的内容）在容器所在节点的快照仓库里。
CREATE TABLE versions (
    project_id  TEXT    NOT NULL,
    container   TEXT    NOT NULL,
    version     INTEGER NOT NULL,
    -- ContainerConfig（protobuf，加密）
    config      BLOB    NOT NULL,
    -- 部署时用的共享变量（EnvList，protobuf，加密）
    shared_env  BLOB    NOT NULL,
    created_at  INTEGER NOT NULL,
    -- 部署成功的时间，NULL 表示还没部署成功
    deployed_at INTEGER,
    -- 服务名 → 实际使用的镜像（带摘要），JSON
    images      TEXT    NOT NULL DEFAULT '{}',
    git_commit  TEXT    NOT NULL DEFAULT '',
    rollback_of INTEGER NOT NULL DEFAULT 0,
    has_files   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, container, version),
    FOREIGN KEY (project_id, container) REFERENCES containers (project_id, name) ON DELETE CASCADE
);

-- 项目的部署记录：每次部署（一个或多个容器）一条。
CREATE TABLE deployments (
    id          INTEGER PRIMARY KEY,
    project_id  TEXT    NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    created_at  INTEGER NOT NULL,
    source      TEXT    NOT NULL,
    -- 这次动到的容器（JSON 数组）
    containers  TEXT    NOT NULL,
    -- 部署完成后所有容器各自的版本（JSON 对象）
    versions    TEXT    NOT NULL,
    success     INTEGER NOT NULL,
    error       TEXT    NOT NULL DEFAULT '',
    restore_of  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX deployments_project ON deployments (project_id, id);

-- 端口映射：属于容器，开在容器所在的节点上。
CREATE TABLE routes (
    project_id TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    container  TEXT    NOT NULL,
    service    TEXT    NOT NULL,
    target     INTEGER NOT NULL,
    protocol   TEXT    NOT NULL,
    published  INTEGER NOT NULL DEFAULT 0,
    -- 绑定的域名（JSON 数组）
    domains    TEXT    NOT NULL DEFAULT '[]',
    http_only  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, name),
    FOREIGN KEY (project_id, container) REFERENCES containers (project_id, name) ON DELETE CASCADE
);

-- 项目连接（见 docs/agent.md#项目连接）：双向，a < b。
CREATE TABLE project_links (
    a          TEXT    NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    b          TEXT    NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (a, b)
);

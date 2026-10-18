-- 项目由多个容器组成：容器网段不再一个项目一个，改为单独的分配表。
-- owner：p:<项目 ID>（项目内网）、g:<项目 ID>/<容器组>（容器组自己的网络）、s:<名字>（共享网络）。
CREATE TABLE subnets (
    subnet TEXT PRIMARY KEY,
    owner  TEXT NOT NULL UNIQUE
);

INSERT INTO subnets (subnet, owner) SELECT subnet, 'p:' || id FROM projects;

CREATE TABLE projects_new (
    id         TEXT    PRIMARY KEY,
    dir        TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);
INSERT INTO projects_new (id, dir, created_at) SELECT id, dir, created_at FROM projects;
DROP TABLE projects;
ALTER TABLE projects_new RENAME TO projects;

-- 节点上的共享网络，加入的项目记在各自的项目设置里。
CREATE TABLE shared_networks (
    name       TEXT    PRIMARY KEY,
    created_at INTEGER NOT NULL
);

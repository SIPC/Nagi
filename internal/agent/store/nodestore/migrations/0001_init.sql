-- 节点上有哪些项目、各自在哪个目录。项目自身的数据在项目目录的 config/project.db 里。
CREATE TABLE projects (
    id         TEXT    PRIMARY KEY,
    dir        TEXT    NOT NULL UNIQUE,
    -- 项目默认网络的网段，从节点本地的容器网段池分配
    subnet     TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);

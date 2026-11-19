-- 角色和权限（见 docs/panel.md#成员和角色）：成员选一个角色，角色勾选权限。
-- builtin：admin（管理员，固定，拥有全部权限，不存权限）、operator、viewer（预置，可以编辑）；自定义角色为空。
-- 预置角色的 name 为空，界面显示本地化的名称；改过名称后显示改的。
CREATE TABLE roles (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL DEFAULT '',
    builtin    TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX roles_name ON roles (name) WHERE name != '';

CREATE TABLE role_permissions (
    role_id    INTEGER NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    permission TEXT    NOT NULL,
    PRIMARY KEY (role_id, permission)
);

INSERT INTO roles (id, builtin) VALUES (1, 'admin'), (2, 'operator'), (3, 'viewer');

-- 预置的操作员：查看、查看配置、日志、控制、部署、终端、文件查看、文件修改、快照、查看节点（和 auth.PresetOperator 一致）。
-- 升级前的操作员因此不再有备份、高可用、迁移、网络和域名、告警规则、镜像仓库、项目管理。
INSERT INTO role_permissions (role_id, permission) VALUES
    (2, 'project.view'), (2, 'project.config'), (2, 'project.logs'), (2, 'project.control'), (2, 'project.deploy'),
    (2, 'project.terminal'), (2, 'project.files.read'), (2, 'project.files.write'), (2, 'project.snapshots'), (2, 'node.view');
-- 预置的只读：查看、日志、查看节点（和 auth.PresetViewer 一致）。
INSERT INTO role_permissions (role_id, permission) VALUES (3, 'project.view'), (3, 'project.logs'), (3, 'node.view');

-- 成员的角色。默认是只读：漏写 role_id 的插入不会得到更多权限。
ALTER TABLE users ADD COLUMN role_id INTEGER NOT NULL DEFAULT 3;
UPDATE users SET role_id = CASE role WHEN 'admin' THEN 1 WHEN 'operator' THEN 2 ELSE 3 END;
UPDATE users SET role = 'member' WHERE role != 'admin';

-- 节点范围「跟随项目」（见 docs/panel.md#访问范围）：只能看到范围内项目的容器所在的节点。设置时 all_nodes 为 0。
-- 升级前节点范围是全部、项目范围是指定的成员改成跟随项目；项目范围也是全部的保持全部。
ALTER TABLE users ADD COLUMN nodes_follow INTEGER NOT NULL DEFAULT 0;
UPDATE users SET nodes_follow = 1, all_nodes = 0 WHERE role != 'admin' AND all_nodes = 1 AND all_projects = 0;

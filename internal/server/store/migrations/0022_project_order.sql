-- 项目页的排序（见 docs/panel.md#分组和标签）：每个成员各自一份，换浏览器、换设备都一样。
-- sort：排序方式，空是最近创建，name 按名称，custom 自定义。
-- groups、projects：自定义顺序里的分组名、项目 ID，一行一个。
CREATE TABLE user_project_order (
    user_id  INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    sort     TEXT NOT NULL DEFAULT '',
    groups   TEXT NOT NULL DEFAULT '',
    projects TEXT NOT NULL DEFAULT ''
);

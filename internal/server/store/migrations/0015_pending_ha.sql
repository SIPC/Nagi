-- 从节点缓存拼回、等管理员确认的顶替设置和备份节点（PendingHA 的 protobuf 二进制，见 docs/panel.md#项目数据）。
ALTER TABLE containers ADD COLUMN pending_ha BLOB;

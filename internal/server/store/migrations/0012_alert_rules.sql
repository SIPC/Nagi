-- 告警规则按节点和容器设置（见 docs/monitoring.md#告警）。AlertRules 的 protobuf 二进制，为空表示用默认值；
-- 容器的为空表示用项目的（项目的在 projects.settings 里）。
ALTER TABLE nodes ADD COLUMN alert_rules BLOB;
ALTER TABLE containers ADD COLUMN alert_rules BLOB;

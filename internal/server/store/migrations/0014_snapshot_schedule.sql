-- 容器单独设置的定时快照计划（见 docs/snapshot.md#定时快照）。SnapshotSchedule 的 protobuf 二进制，为空表示用项目的
-- （项目的在 projects.settings 里）。
ALTER TABLE containers ADD COLUMN snapshot_schedule BLOB;

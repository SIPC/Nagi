-- 定时快照失败后的退避（见 docs/snapshot.md#失败与退避）：连续失败次数和下次重试的时间。成功后清零。
-- 写在数据库里，节点重启后照样按退避等待。
ALTER TABLE containers ADD COLUMN snapshot_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE containers ADD COLUMN snapshot_retry_at INTEGER NOT NULL DEFAULT 0;

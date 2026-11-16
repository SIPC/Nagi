-- 按保留时间删掉的记录留下 ID 和哈希（墓碑），校验哈希链时用它接上，删改别的记录仍然能发现。
-- 比所有剩下的记录都旧的墓碑没有用了，清理时一起删掉。
CREATE TABLE tombstones (
    id   INTEGER PRIMARY KEY,
    hash TEXT NOT NULL
);

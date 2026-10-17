-- 监控数据：最近 24 小时每 10 秒一个点（raw），30 天内每 5 分钟一个点（m5）。
CREATE TABLE metrics_raw (
    series TEXT    NOT NULL,
    ts     INTEGER NOT NULL,
    value  REAL    NOT NULL,
    PRIMARY KEY (series, ts)
) WITHOUT ROWID;

CREATE TABLE metrics_m5 (
    series TEXT    NOT NULL,
    ts     INTEGER NOT NULL,
    value  REAL    NOT NULL,
    PRIMARY KEY (series, ts)
) WITHOUT ROWID;

CREATE INDEX metrics_raw_ts ON metrics_raw (ts);
CREATE INDEX metrics_m5_ts ON metrics_m5 (ts);
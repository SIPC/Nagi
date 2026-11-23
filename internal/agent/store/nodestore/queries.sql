-- name: UpsertContainer :exec
INSERT INTO containers (project_id, name, dir, spec, created_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (project_id, name) DO UPDATE SET dir = excluded.dir, spec = excluded.spec;

-- name: GetContainer :one
SELECT * FROM containers WHERE project_id = ? AND name = ?;

-- name: ListContainers :many
SELECT * FROM containers ORDER BY created_at, project_id, name;

-- name: ListProjectContainers :many
SELECT * FROM containers WHERE project_id = ? ORDER BY created_at, name;

-- name: DeleteContainer :exec
DELETE FROM containers WHERE project_id = ? AND name = ?;

-- name: SetContainerFilesRoot :exec
UPDATE containers SET files_root = ? WHERE project_id = ? AND name = ?;

-- name: SetContainerSnapshotTime :exec
UPDATE containers SET last_snapshot = ?, snapshot_failures = 0, snapshot_retry_at = 0 WHERE project_id = ? AND name = ?;

-- name: SetContainerSnapshotRetry :exec
UPDATE containers SET snapshot_failures = ?, snapshot_retry_at = ? WHERE project_id = ? AND name = ?;

-- name: SetContainerMaintenanceTime :exec
UPDATE containers SET last_maintenance = ? WHERE project_id = ? AND name = ?;

-- name: ListSubnets :many
SELECT subnet FROM subnets;

-- name: GetSubnet :one
SELECT subnet FROM subnets WHERE owner = ?;

-- name: InsertSubnet :exec
INSERT INTO subnets (subnet, owner) VALUES (?, ?);

-- name: DeleteSubnet :exec
DELETE FROM subnets WHERE owner = ?;

-- name: InsertMetric :exec
INSERT OR REPLACE INTO metrics_raw (series, ts, value) VALUES (?, ?, ?);

-- name: AverageRaw :many
SELECT series, CAST(avg(value) AS REAL) AS value, count(*) AS samples FROM metrics_raw
WHERE ts >= @from_ts AND ts < @to_ts GROUP BY series;

-- name: InsertM5 :exec
INSERT OR REPLACE INTO metrics_m5 (series, ts, value) VALUES (?, ?, ?);

-- name: DeleteRawBefore :exec
DELETE FROM metrics_raw WHERE ts < ?;

-- name: DeleteM5Before :exec
DELETE FROM metrics_m5 WHERE ts < ?;

-- QueryRaw/QueryM5 visit each matching series through the (series, ts) primary
-- key: the recursive part steps to the next series name, so neither a long time
-- range nor many other series makes SQLite fall back to the ts index. Pages
-- continue after (after_series, after_ts); the first page passes an empty
-- after_series and after_ts = from_ts - 1.

-- name: QueryRaw :many
WITH RECURSIVE s(name) AS (
  SELECT (SELECT min(x.series) FROM metrics_raw x WHERE x.series >= max(@prefix, CAST(@after_series AS TEXT)) AND x.series < @prefix_end)
  UNION ALL
  SELECT (SELECT min(x.series) FROM metrics_raw x WHERE x.series > s.name AND x.series < @prefix_end) FROM s WHERE s.name IS NOT NULL
)
SELECT m.series, m.ts, m.value FROM s JOIN metrics_raw m ON m.series = s.name
WHERE instr(m.series, @prefix) = 1 AND m.ts >= @from_ts AND m.ts <= @to_ts
AND (m.series, m.ts) > (CAST(@after_series AS TEXT), CAST(@after_ts AS INTEGER)) ORDER BY m.series, m.ts LIMIT 2048;

-- name: QueryM5 :many
WITH RECURSIVE s(name) AS (
  SELECT (SELECT min(x.series) FROM metrics_m5 x WHERE x.series >= max(@prefix, CAST(@after_series AS TEXT)) AND x.series < @prefix_end)
  UNION ALL
  SELECT (SELECT min(x.series) FROM metrics_m5 x WHERE x.series > s.name AND x.series < @prefix_end) FROM s WHERE s.name IS NOT NULL
)
SELECT m.series, m.ts, m.value FROM s JOIN metrics_m5 m ON m.series = s.name
WHERE instr(m.series, @prefix) = 1 AND m.ts >= @from_ts AND m.ts <= @to_ts
AND (m.series, m.ts) > (CAST(@after_series AS TEXT), CAST(@after_ts AS INTEGER)) ORDER BY m.series, m.ts LIMIT 2048;

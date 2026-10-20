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
UPDATE containers SET last_snapshot = ? WHERE project_id = ? AND name = ?;

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
SELECT series, CAST(avg(value) AS REAL) AS value FROM metrics_raw
WHERE ts >= @from_ts AND ts < @to_ts GROUP BY series;

-- name: InsertM5 :exec
INSERT OR REPLACE INTO metrics_m5 (series, ts, value) VALUES (?, ?, ?);

-- name: DeleteRawBefore :exec
DELETE FROM metrics_raw WHERE ts < ?;

-- name: DeleteM5Before :exec
DELETE FROM metrics_m5 WHERE ts < ?;

-- name: QueryRaw :many
SELECT series, ts, value FROM metrics_raw
WHERE instr(series, @prefix) = 1 AND ts >= @from_ts AND ts <= @to_ts ORDER BY series, ts;

-- name: QueryM5 :many
SELECT series, ts, value FROM metrics_m5
WHERE instr(series, @prefix) = 1 AND ts >= @from_ts AND ts <= @to_ts ORDER BY series, ts;

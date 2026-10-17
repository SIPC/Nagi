-- name: InsertProject :exec
INSERT INTO projects (id, dir, subnet, created_at) VALUES (?, ?, ?, ?);

-- name: GetProject :one
SELECT * FROM projects WHERE id = ?;

-- name: ListProjects :many
SELECT * FROM projects ORDER BY created_at;

-- name: ListSubnets :many
SELECT subnet FROM projects;


-- name: DeleteProject :exec
DELETE FROM projects WHERE id = ?;

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
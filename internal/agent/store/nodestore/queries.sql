-- name: InsertProject :exec
INSERT INTO projects (id, dir, created_at) VALUES (?, ?, ?);

-- name: GetProject :one
SELECT * FROM projects WHERE id = ?;

-- name: ListProjects :many
SELECT * FROM projects ORDER BY created_at;

-- name: ListSubnets :many
SELECT subnet FROM subnets;

-- name: GetSubnet :one
SELECT subnet FROM subnets WHERE owner = ?;

-- name: InsertSubnet :exec
INSERT INTO subnets (subnet, owner) VALUES (?, ?);

-- name: DeleteSubnet :exec
DELETE FROM subnets WHERE owner = ?;

-- name: DeleteProjectSubnets :exec
DELETE FROM subnets WHERE owner = @project_owner OR owner LIKE @group_prefix;

-- name: InsertSharedNetwork :exec
INSERT INTO shared_networks (name, created_at) VALUES (?, ?);

-- name: ListSharedNetworks :many
SELECT * FROM shared_networks ORDER BY name;

-- name: DeleteSharedNetwork :exec
DELETE FROM shared_networks WHERE name = ?;


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
-- name: ListSharedNetworkLinks :many
SELECT * FROM shared_network_links ORDER BY network, peer_name;

-- name: InsertSharedNetworkLink :exec
INSERT INTO shared_network_links (network, peer_key, peer_name, peer_node) VALUES (?, ?, ?, ?);

-- name: DeleteSharedNetworkLinks :exec
DELETE FROM shared_network_links WHERE network = ?;

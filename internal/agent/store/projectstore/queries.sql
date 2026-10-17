-- name: GetMeta :one
SELECT value FROM meta WHERE key = ?;

-- name: SetMeta :exec
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: InsertVersion :exec
INSERT INTO versions (version, compose, created_at) VALUES (?, ?, ?);

-- name: LatestVersion :one
SELECT * FROM versions ORDER BY version DESC LIMIT 1;

-- name: GetVersion :one
SELECT * FROM versions WHERE version = ?;

-- name: MarkDeployed :exec
UPDATE versions SET deployed_at = ? WHERE version = ?;

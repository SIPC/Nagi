-- name: GetMeta :one
SELECT value FROM meta WHERE key = ?;

-- name: SetMeta :exec
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: InsertVersion :exec
INSERT INTO versions (version, config, created_at) VALUES (?, ?, ?);

-- name: LatestVersion :one
SELECT * FROM versions ORDER BY version DESC LIMIT 1;

-- name: GetVersion :one
SELECT * FROM versions WHERE version = ?;

-- name: MarkDeployed :exec
UPDATE versions SET deployed_at = ? WHERE version = ?;

-- name: GetDraft :one
SELECT * FROM draft WHERE id = 1;

-- name: SetDraft :exec
INSERT INTO draft (id, config, updated_at) VALUES (1, ?, ?)
ON CONFLICT (id) DO UPDATE SET config = excluded.config, updated_at = excluded.updated_at;

-- name: DeleteDraft :exec
DELETE FROM draft WHERE id = 1;
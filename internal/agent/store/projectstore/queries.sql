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

-- name: ListVersions :many
SELECT * FROM versions ORDER BY version DESC;

-- name: SetVersionFiles :exec
UPDATE versions SET files_snapshot = ? WHERE version = ?;

-- name: SetVersionDeployed :exec
UPDATE versions SET deployed_at = ?, images = ?, git_commit = ? WHERE version = ?;

-- name: InsertRollbackVersion :exec
INSERT INTO versions (version, config, created_at, rollback_of) VALUES (?, ?, ?, ?);

-- name: DeleteVersionsBefore :exec
DELETE FROM versions WHERE version < ?;

-- name: ReplaceUndeployedVersion :exec
UPDATE versions SET config = ?, created_at = ?, rollback_of = ?, files_snapshot = '', images = '{}', git_commit = ''
WHERE version = ? AND deployed_at IS NULL;
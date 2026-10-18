-- name: GetMeta :one
SELECT value FROM meta WHERE key = ?;

-- name: SetMeta :exec
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;


-- name: InsertContainer :exec
INSERT INTO containers (name, position, created_at, webhook_token)
VALUES (?, (SELECT coalesce(max(position), 0) + 1 FROM containers), ?, ?);

-- name: GetContainer :one
SELECT * FROM containers WHERE name = ?;

-- name: ListContainers :many
SELECT * FROM containers ORDER BY position;

-- name: DeleteContainer :exec
DELETE FROM containers WHERE name = ?;

-- name: SetContainerRevision :exec
UPDATE containers SET revision = ? WHERE name = ?;

-- name: SetContainerDeployed :exec
UPDATE containers SET deployed_version = ? WHERE name = ?;

-- name: SetContainerCommit :exec
UPDATE containers SET last_commit = ? WHERE name = ?;

-- name: SetContainerImportedPorts :exec
UPDATE containers SET imported_ports = ? WHERE name = ?;

-- name: SetContainerFilesRoot :exec
UPDATE containers SET last_files_root = ? WHERE name = ?;


-- name: InsertVersion :exec
INSERT INTO versions (container, version, config, shared_env, created_at, rollback_of) VALUES (?, ?, ?, ?, ?, ?);

-- name: ReplaceUndeployedVersion :exec
UPDATE versions SET config = ?, shared_env = ?, created_at = ?, rollback_of = ?, files_snapshot = '', images = '{}', git_commit = ''
WHERE container = ? AND version = ? AND deployed_at IS NULL;

-- name: LatestVersion :one
SELECT * FROM versions WHERE container = ? ORDER BY version DESC LIMIT 1;

-- name: GetVersion :one
SELECT * FROM versions WHERE container = ? AND version = ?;

-- name: ListVersions :many
SELECT * FROM versions WHERE container = ? ORDER BY version DESC;

-- name: SetVersionFiles :exec
UPDATE versions SET files_snapshot = ? WHERE container = ? AND version = ?;

-- name: SetVersionDeployed :exec
UPDATE versions SET deployed_at = ?, images = ?, git_commit = ? WHERE container = ? AND version = ?;

-- name: DeleteVersionsBefore :exec
DELETE FROM versions WHERE container = ? AND version < ?;

-- name: DeleteContainerVersions :exec
DELETE FROM versions WHERE container = ?;


-- name: GetDraft :one
SELECT * FROM drafts WHERE container = ?;

-- name: SetDraft :exec
INSERT INTO drafts (container, config, updated_at) VALUES (?, ?, ?)
ON CONFLICT (container) DO UPDATE SET config = excluded.config, updated_at = excluded.updated_at;

-- name: DeleteDraft :exec
DELETE FROM drafts WHERE container = ?;


-- name: InsertDeployment :one
INSERT INTO deployments (created_at, source, containers, versions, success, error, restore_of)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetDeployment :one
SELECT * FROM deployments WHERE id = ?;

-- name: ListDeployments :many
SELECT * FROM deployments ORDER BY id DESC LIMIT ?;

-- name: DeleteDeploymentsBefore :exec
DELETE FROM deployments WHERE id < ?;

-- name: GetMeta :one
SELECT value FROM meta WHERE key = ?;

-- name: SetMeta :exec
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;


-- name: InsertMember :exec
INSERT INTO members (name, position, created_at, webhook_token)
VALUES (?, (SELECT coalesce(max(position), 0) + 1 FROM members), ?, ?);

-- name: GetMember :one
SELECT * FROM members WHERE name = ?;

-- name: ListMembers :many
SELECT * FROM members ORDER BY position;

-- name: DeleteMember :exec
DELETE FROM members WHERE name = ?;

-- name: SetMemberRevision :exec
UPDATE members SET revision = ? WHERE name = ?;

-- name: SetMemberDeployed :exec
UPDATE members SET deployed_version = ? WHERE name = ?;

-- name: SetMemberCommit :exec
UPDATE members SET last_commit = ? WHERE name = ?;

-- name: SetMemberImportedPorts :exec
UPDATE members SET imported_ports = ? WHERE name = ?;

-- name: SetMemberFilesRoot :exec
UPDATE members SET last_files_root = ? WHERE name = ?;


-- name: InsertVersion :exec
INSERT INTO versions (member, version, config, shared_env, created_at, rollback_of) VALUES (?, ?, ?, ?, ?, ?);

-- name: ReplaceUndeployedVersion :exec
UPDATE versions SET config = ?, shared_env = ?, created_at = ?, rollback_of = ?, files_snapshot = '', images = '{}', git_commit = ''
WHERE member = ? AND version = ? AND deployed_at IS NULL;

-- name: LatestVersion :one
SELECT * FROM versions WHERE member = ? ORDER BY version DESC LIMIT 1;

-- name: GetVersion :one
SELECT * FROM versions WHERE member = ? AND version = ?;

-- name: ListVersions :many
SELECT * FROM versions WHERE member = ? ORDER BY version DESC;

-- name: SetVersionFiles :exec
UPDATE versions SET files_snapshot = ? WHERE member = ? AND version = ?;

-- name: SetVersionDeployed :exec
UPDATE versions SET deployed_at = ?, images = ?, git_commit = ? WHERE member = ? AND version = ?;

-- name: DeleteVersionsBefore :exec
DELETE FROM versions WHERE member = ? AND version < ?;

-- name: DeleteMemberVersions :exec
DELETE FROM versions WHERE member = ?;


-- name: GetDraft :one
SELECT * FROM drafts WHERE member = ?;

-- name: SetDraft :exec
INSERT INTO drafts (member, config, updated_at) VALUES (?, ?, ?)
ON CONFLICT (member) DO UPDATE SET config = excluded.config, updated_at = excluded.updated_at;

-- name: DeleteDraft :exec
DELETE FROM drafts WHERE member = ?;


-- name: InsertDeployment :one
INSERT INTO deployments (created_at, source, members, versions, success, error, restore_of)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetDeployment :one
SELECT * FROM deployments WHERE id = ?;

-- name: ListDeployments :many
SELECT * FROM deployments ORDER BY id DESC LIMIT ?;

-- name: DeleteDeploymentsBefore :exec
DELETE FROM deployments WHERE id < ?;

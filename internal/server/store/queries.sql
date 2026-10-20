-- name: GetSetting :one
SELECT value FROM settings WHERE key = ?;

-- name: SetSetting :exec
INSERT INTO settings (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: DeleteSetting :exec
DELETE FROM settings WHERE key = ?;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)
RETURNING id;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = ?;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?);

-- name: GetSession :one
SELECT s.user_id, s.expires_at, u.username
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < ?;

-- name: CreateEnrollToken :exec
INSERT INTO enroll_tokens (token_hash, created_at, expires_at) VALUES (?, ?, ?);

-- name: UseEnrollToken :one
UPDATE enroll_tokens SET used_at = ?, uses = uses + 1
WHERE token_hash = ? AND expires_at > ? AND (used_at IS NULL OR multi_use = 1)
RETURNING multi_use;

-- name: CreateInvite :exec
INSERT INTO enroll_tokens (token_hash, created_at, expires_at, multi_use) VALUES (?, ?, ?, 1);

-- name: ListInvites :many
SELECT token_hash, created_at, expires_at, uses FROM enroll_tokens
WHERE multi_use = 1 AND expires_at > ? ORDER BY created_at DESC;

-- name: RevokeInvite :execrows
UPDATE enroll_tokens SET expires_at = 0 WHERE multi_use = 1 AND substr(token_hash, 1, 12) = ?;

-- name: CreateNode :exec
INSERT INTO nodes (id, name, created_at, mesh_key, pending) VALUES (?, ?, ?, ?, ?);

-- name: SetNodeEnrollment :exec
UPDATE nodes SET revoked = 0, mesh_key = ?, pending = ? WHERE id = ?;

-- name: MeshKeyOwner :one
SELECT id FROM nodes WHERE mesh_key = ? AND id != ? AND revoked = 0;

-- name: ApproveNode :execrows
UPDATE nodes SET pending = 0 WHERE id = ? AND revoked = 0;

-- name: GetNode :one
SELECT * FROM nodes WHERE id = ?;

-- name: ListNodes :many
SELECT * FROM nodes WHERE revoked = 0 ORDER BY created_at;

-- name: UpdateNodeSeen :exec
UPDATE nodes SET last_seen = ?, info = ? WHERE id = ?;

-- name: RevokeNode :exec
UPDATE nodes SET revoked = 1 WHERE id = ?;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ?;

-- name: ListUsers :many
SELECT * FROM users ORDER BY id;

-- name: UpdatePassword :exec
UPDATE users SET password_hash = ? WHERE id = ?;

-- name: SetTOTP :exec
UPDATE users SET totp_secret = ?, totp_last_step = 0 WHERE id = ?;

-- name: UseTOTPStep :execrows
UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?;

-- name: DeleteUserSessions :exec
DELETE FROM sessions WHERE user_id = ?;

-- name: DeleteOtherSessions :exec
DELETE FROM sessions WHERE user_id = ? AND token_hash != ?;

-- name: CountSealedData :one
SELECT (SELECT count(*) FROM users WHERE totp_secret != '')
     + (SELECT count(*) FROM settings WHERE key = 'alert_config')
     + (SELECT count(*) FROM projects) AS n;

-- name: ClearTOTPSecrets :exec
UPDATE users SET totp_secret = '', totp_last_step = 0 WHERE totp_secret != '';

-- name: CountActiveEnrollTokens :one
SELECT count(*) FROM enroll_tokens WHERE expires_at > ? AND (used_at IS NULL OR multi_use = 1);


-- name: InsertProject :exec
INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES (?, ?, ?, 1, ?, ?);

-- name: GetProject :one
SELECT * FROM projects WHERE id = ?;

-- name: ListProjects :many
SELECT * FROM projects ORDER BY created_at;

-- name: ProjectByNetworkName :one
SELECT id FROM projects WHERE network_name = ?;

-- name: UpdateProject :exec
UPDATE projects SET name = ?, network_name = ?, settings = ?, revision = revision + 1 WHERE id = ?;

-- name: SetProjectSettings :exec
UPDATE projects SET settings = ?, revision = revision + 1 WHERE id = ?;

-- name: SetSharedEnvDraft :exec
UPDATE projects SET shared_env_draft = ?, revision = revision + 1 WHERE id = ?;

-- name: SetProjectDockerConfig :exec
UPDATE projects SET docker_config = ?, revision = revision + 1 WHERE id = ?;

-- name: DeleteProject :exec
DELETE FROM projects WHERE id = ?;


-- name: InsertContainer :exec
INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?);

-- name: GetContainer :one
SELECT * FROM containers WHERE project_id = ? AND name = ?;

-- name: ListContainers :many
SELECT * FROM containers WHERE project_id = ? ORDER BY position;

-- name: ListAllContainers :many
SELECT * FROM containers ORDER BY project_id, position;

-- name: ListNodeContainers :many
SELECT * FROM containers WHERE node_id = ? ORDER BY project_id, position;

-- name: CountNodeContainers :one
SELECT count(*) FROM containers WHERE node_id = ?;

-- name: ContainerByWebhook :one
SELECT project_id, name FROM containers WHERE webhook_hash = ?;

-- name: ContainerDirOwner :one
SELECT project_id, name FROM containers WHERE node_id = ? AND dir = ?;

-- name: SetContainerDraft :exec
UPDATE containers SET draft = ?, revision = revision + 1 WHERE project_id = ? AND name = ?;

-- name: SetContainerDeployed :exec
UPDATE containers SET deployed_version = ?, revision = revision + 1 WHERE project_id = ? AND name = ?;

-- name: BumpContainer :exec
UPDATE containers SET revision = revision + 1 WHERE project_id = ? AND name = ?;

-- name: SetContainerCommit :exec
UPDATE containers SET last_commit = ? WHERE project_id = ? AND name = ?;

-- name: SetContainerImportedPorts :exec
UPDATE containers SET imported_ports = ? WHERE project_id = ? AND name = ?;

-- name: DeleteContainer :exec
DELETE FROM containers WHERE project_id = ? AND name = ?;


-- name: InsertVersion :exec
INSERT INTO versions (project_id, container, version, config, shared_env, created_at, rollback_of)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ReplaceUndeployedVersion :exec
UPDATE versions SET config = ?, shared_env = ?, created_at = ?, rollback_of = ?, images = '{}', git_commit = '', has_files = 0
WHERE project_id = ? AND container = ? AND version = ? AND deployed_at IS NULL;

-- name: SetVersionDeployed :exec
UPDATE versions SET deployed_at = ?, images = ?, git_commit = ?, has_files = ?
WHERE project_id = ? AND container = ? AND version = ?;

-- name: SetVersionFiles :exec
UPDATE versions SET has_files = ? WHERE project_id = ? AND container = ? AND version = ?;

-- name: GetVersion :one
SELECT * FROM versions WHERE project_id = ? AND container = ? AND version = ?;

-- name: LatestVersion :one
SELECT * FROM versions WHERE project_id = ? AND container = ? ORDER BY version DESC LIMIT 1;

-- name: ListVersions :many
SELECT * FROM versions WHERE project_id = ? AND container = ? ORDER BY version DESC;

-- name: DeleteVersionsBefore :exec
DELETE FROM versions WHERE project_id = ? AND container = ? AND version < ?;

-- name: ClearVersionFiles :exec
UPDATE versions SET has_files = 0 WHERE project_id = ? AND container = ?;


-- name: InsertDeployment :one
INSERT INTO deployments (project_id, created_at, source, containers, versions, success, error, restore_of)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetDeployment :one
SELECT * FROM deployments WHERE project_id = ? AND id = ?;

-- name: ListDeployments :many
SELECT * FROM deployments WHERE project_id = ? ORDER BY id DESC LIMIT ?;

-- name: DeleteDeploymentsBefore :exec
DELETE FROM deployments WHERE project_id = ? AND id < ?;


-- name: UpsertRoute :exec
INSERT INTO routes (project_id, name, container, service, target, protocol, published, domains, http_only)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project_id, name) DO UPDATE SET
    container = excluded.container, service = excluded.service, target = excluded.target, protocol = excluded.protocol,
    published = excluded.published, domains = excluded.domains, http_only = excluded.http_only;

-- name: ListRoutes :many
SELECT * FROM routes WHERE project_id = ? ORDER BY name;

-- name: ListAllRoutes :many
SELECT * FROM routes ORDER BY project_id, name;

-- name: DeleteRoute :exec
DELETE FROM routes WHERE project_id = ? AND name = ?;

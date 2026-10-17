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

-- name: UseEnrollToken :execrows
UPDATE enroll_tokens SET used_at = ?, uses = uses + 1
WHERE token_hash = ? AND expires_at > ? AND (used_at IS NULL OR multi_use = 1);

-- name: CreateInvite :exec
INSERT INTO enroll_tokens (token_hash, created_at, expires_at, multi_use) VALUES (?, ?, ?, 1);

-- name: ListInvites :many
SELECT token_hash, created_at, expires_at, uses FROM enroll_tokens
WHERE multi_use = 1 AND expires_at > ? ORDER BY created_at DESC;

-- name: RevokeInvite :execrows
UPDATE enroll_tokens SET expires_at = 0 WHERE multi_use = 1 AND substr(token_hash, 1, 12) = ?;

-- name: CreateNode :exec
INSERT INTO nodes (id, name, created_at) VALUES (?, ?, ?);

-- name: GetNode :one
SELECT * FROM nodes WHERE id = ?;

-- name: ListNodes :many
SELECT * FROM nodes WHERE revoked = 0 ORDER BY created_at;

-- name: UpdateNodeSeen :exec
UPDATE nodes SET last_seen = ?, info = ? WHERE id = ?;

-- name: UpsertProjectBackup :exec
INSERT INTO project_backups (node_id, project_id, revision, data, updated_at, backup_key) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (node_id, project_id) DO UPDATE SET
    revision = excluded.revision,
    data = excluded.data,
    updated_at = excluded.updated_at,
    backup_key = coalesce(excluded.backup_key, project_backups.backup_key);

-- name: GetProjectBackupKey :one
SELECT backup_key FROM project_backups WHERE node_id = ? AND project_id = ?;

-- name: ListProjectBackups :many
SELECT * FROM project_backups WHERE node_id = ? ORDER BY project_id;

-- name: UpsertWebhook :exec
INSERT OR REPLACE INTO webhooks (token_hash, node_id, project_id, member) VALUES (?, ?, ?, ?);

-- name: GetWebhook :one
SELECT * FROM webhooks WHERE token_hash = ?;

-- name: RevokeNode :exec
UPDATE nodes SET revoked = 1 WHERE id = ?;

-- name: UnrevokeNode :exec
UPDATE nodes SET revoked = 0 WHERE id = ?;

-- name: DeleteNodeData :exec
DELETE FROM project_backups WHERE node_id = ?;

-- name: DeleteNodeWebhooks :exec
DELETE FROM webhooks WHERE node_id = ?;

-- name: DeleteProjectBackup :exec
DELETE FROM project_backups WHERE node_id = ? AND project_id = ?;

-- name: DeleteProjectWebhooks :exec
DELETE FROM webhooks WHERE node_id = ? AND project_id = ?;

-- name: ListNodeWebhooks :many
SELECT * FROM webhooks WHERE node_id = ?;

-- name: DeleteWebhook :exec
DELETE FROM webhooks WHERE token_hash = ?;
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

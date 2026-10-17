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
UPDATE enroll_tokens SET used_at = ?
WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?;

-- name: CreateNode :exec
INSERT INTO nodes (id, name, created_at) VALUES (?, ?, ?);

-- name: GetNode :one
SELECT * FROM nodes WHERE id = ?;

-- name: ListNodes :many
SELECT * FROM nodes WHERE revoked = 0 ORDER BY created_at;

-- name: UpdateNodeSeen :exec
UPDATE nodes SET last_seen = ?, info = ? WHERE id = ?;

-- name: UpsertProjectBackup :exec
INSERT INTO project_backups (node_id, project_id, revision, data, updated_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (node_id, project_id) DO UPDATE SET
    revision = excluded.revision,
    data = excluded.data,
    updated_at = excluded.updated_at;

-- name: ListProjectBackups :many
SELECT * FROM project_backups WHERE node_id = ? ORDER BY project_id;
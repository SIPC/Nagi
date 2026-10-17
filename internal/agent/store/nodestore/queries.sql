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
package store

import (
	"context"
	"strings"
)

// 角色和权限的查询（见 docs/panel.md#成员和角色）。手写，不放进 queries.sql。

// Role 是一个角色和它的权限、成员数。
type Role struct {
	ID          int64
	Name        string
	Builtin     string
	CreatedAt   int64
	Permissions []string
	Members     int64
}

const roleSelect = `SELECT r.id, r.name, r.builtin, r.created_at,
	COALESCE((SELECT group_concat(permission) FROM role_permissions WHERE role_id = r.id), ''),
	(SELECT count(*) FROM users WHERE role_id = r.id) FROM roles r`

func scanRole(r interface{ Scan(...any) error }) (Role, error) {
	var x Role
	var perms string
	err := r.Scan(&x.ID, &x.Name, &x.Builtin, &x.CreatedAt, &perms, &x.Members)
	if perms != "" {
		x.Permissions = strings.Split(perms, ",")
	}
	return x, err
}

// ListRoles 列出全部角色：预置的在前，自定义的按创建顺序。
func (q *Queries) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := q.db.QueryContext(ctx, roleSelect+` ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Role{}
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRole 按 ID 找角色。
func (q *Queries) GetRole(ctx context.Context, id int64) (Role, error) {
	return scanRole(q.db.QueryRowContext(ctx, roleSelect+` WHERE r.id = ?`, id))
}

// RoleNameTaken 判断名称是不是已经被别的角色用了（不区分大小写）。
func (q *Queries) RoleNameTaken(ctx context.Context, name string, except int64) (bool, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, `SELECT count(*) FROM roles WHERE lower(name) = lower(?) AND id != ?`, name, except).Scan(&n)
	return n > 0, err
}

// CreateRole 新建自定义角色。
func (q *Queries) CreateRole(ctx context.Context, name string, createdAt int64) (int64, error) {
	var id int64
	err := q.db.QueryRowContext(ctx, `INSERT INTO roles (name, created_at) VALUES (?, ?) RETURNING id`, name, createdAt).Scan(&id)
	return id, err
}

// RenameRole 改角色的名称。
func (q *Queries) RenameRole(ctx context.Context, id int64, name string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE roles SET name = ? WHERE id = ?`, name, id)
	return err
}

// SetRolePermissions 换掉角色的全部权限。
// 在一个事务里：中途出错时不留下只有一部分权限的角色。
func (q *Queries) SetRolePermissions(ctx context.Context, id int64, perms []string) error {
	return q.inTx(ctx, func(q *Queries) error {
		if _, err := q.db.ExecContext(ctx, `DELETE FROM role_permissions WHERE role_id = ?`, id); err != nil {
			return err
		}
		for _, p := range perms {
			if _, err := q.db.ExecContext(ctx, `INSERT OR IGNORE INTO role_permissions (role_id, permission) VALUES (?, ?)`, id, p); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteRole 删除没有成员在用的自定义角色，返回删掉的行数（有成员在用、是预置角色时为 0）。
func (q *Queries) DeleteRole(ctx context.Context, id int64) (int64, error) {
	res, err := q.db.ExecContext(ctx, `DELETE FROM roles WHERE id = ? AND builtin = '' AND NOT EXISTS (SELECT 1 FROM users WHERE role_id = ?)`, id, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetUserNodesFollow 设置成员的节点范围是不是「跟随项目」。
func (q *Queries) SetUserNodesFollow(ctx context.Context, id int64, follow bool) error {
	v := 0
	if follow {
		v = 1
	}
	_, err := q.db.ExecContext(ctx, `UPDATE users SET nodes_follow = ? WHERE id = ?`, v, id)
	return err
}

// RolePerms 返回角色的权限名称，逗号分隔。
func (q *Queries) RolePerms(ctx context.Context, roleID int64) (string, error) {
	var s string
	err := q.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT group_concat(permission) FROM role_permissions WHERE role_id = ?), '')`, roleID).Scan(&s)
	return s, err
}

// ContainerNode 是一个容器所在的节点。
type ContainerNode struct {
	ProjectID string
	NodeID    string
}

// ContainerNodes 列出所有容器所在的节点（节点范围「跟随项目」用）。
func (q *Queries) ContainerNodes(ctx context.Context) ([]ContainerNode, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT project_id, node_id FROM containers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContainerNode{}
	for rows.Next() {
		var c ContainerNode
		if err := rows.Scan(&c.ProjectID, &c.NodeID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

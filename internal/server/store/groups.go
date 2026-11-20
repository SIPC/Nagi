package store

// 项目分组、节点标签，以及访问范围里的分组和标签（见 docs/panel.md#分组和标签）。手写的，理由同 tokens.go。

import (
	"context"
	"database/sql"
	"strings"
)

// inTx 在事务里执行 f。已经在事务里（q.db 不是 *sql.DB）时直接执行。
func (q *Queries) inTx(ctx context.Context, f func(*Queries) error) error {
	d, ok := q.db.(*sql.DB)
	if !ok {
		return f(q)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := f(q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

func (q *Queries) strings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anyArgs(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// ---- 项目分组 ----

// ProjectGroup 返回项目所在的分组，未分组时为空。
func (q *Queries) ProjectGroup(ctx context.Context, projectID string) (string, error) {
	var name string
	err := q.db.QueryRowContext(ctx, `SELECT name FROM project_groups WHERE project_id = ?`, projectID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

// SetProjectGroup 把项目放进分组，name 为空表示移出分组。
func (q *Queries) SetProjectGroup(ctx context.Context, projectID, name string) error {
	if name == "" {
		_, err := q.db.ExecContext(ctx, `DELETE FROM project_groups WHERE project_id = ?`, projectID)
		return err
	}
	_, err := q.db.ExecContext(ctx, `INSERT INTO project_groups (project_id, name) VALUES (?, ?)
		ON CONFLICT (project_id) DO UPDATE SET name = excluded.name`, projectID, name)
	return err
}

// RenameProjectGroup 重命名分组（新名称已存在时合并），访问范围里的这个分组一起改。to 为空表示删除分组：
// 里面的项目变成未分组，访问范围里去掉这个分组。
func (q *Queries) RenameProjectGroup(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	return q.inTx(ctx, func(q *Queries) error {
		if to == "" {
			if _, err := q.db.ExecContext(ctx, `DELETE FROM project_groups WHERE name = ?`, from); err != nil {
				return err
			}
			_, err := q.db.ExecContext(ctx, `DELETE FROM user_project_groups WHERE name = ?`, from)
			return err
		}
		if _, err := q.db.ExecContext(ctx, `UPDATE project_groups SET name = ? WHERE name = ?`, to, from); err != nil {
			return err
		}
		if _, err := q.db.ExecContext(ctx, `UPDATE OR IGNORE user_project_groups SET name = ? WHERE name = ?`, to, from); err != nil {
			return err
		}
		_, err := q.db.ExecContext(ctx, `DELETE FROM user_project_groups WHERE name = ?`, from)
		return err
	})
}

// ProjectGroupExists 判断分组是否存在：有项目在里面，或者在成员的访问范围里。
func (q *Queries) ProjectGroupExists(ctx context.Context, name string) (bool, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM project_groups WHERE name = ?) + (SELECT COUNT(*) FROM user_project_groups WHERE name = ?)`, name, name).Scan(&n)
	return n > 0, err
}

// ProjectsInGroups 返回这些分组里的项目。
func (q *Queries) ProjectsInGroups(ctx context.Context, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	return q.strings(ctx, `SELECT project_id FROM project_groups WHERE name IN (`+placeholders(len(names))+`)`, anyArgs(names)...)
}

// ---- 节点标签 ----

// NodeTags 返回节点的标签，按名称排序。
func (q *Queries) NodeTags(ctx context.Context, nodeID string) ([]string, error) {
	return q.strings(ctx, `SELECT tag FROM node_tags WHERE node_id = ? ORDER BY tag`, nodeID)
}

// SetNodeTags 替换节点的标签。
func (q *Queries) SetNodeTags(ctx context.Context, nodeID string, tags []string) error {
	return q.inTx(ctx, func(q *Queries) error {
		if err := q.DeleteNodeTags(ctx, nodeID); err != nil {
			return err
		}
		for _, t := range tags {
			if _, err := q.db.ExecContext(ctx, `INSERT OR IGNORE INTO node_tags (node_id, tag) VALUES (?, ?)`, nodeID, t); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNodeTags 删掉节点的所有标签（移除节点时）。
func (q *Queries) DeleteNodeTags(ctx context.Context, nodeID string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM node_tags WHERE node_id = ?`, nodeID)
	return err
}

// RenameNodeTag 重命名标签（新名称已存在时合并），访问范围里的这个标签一起改。to 为空表示删除标签：
// 从所有节点和访问范围里去掉。
func (q *Queries) RenameNodeTag(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	return q.inTx(ctx, func(q *Queries) error {
		if to != "" {
			if _, err := q.db.ExecContext(ctx, `UPDATE OR IGNORE node_tags SET tag = ? WHERE tag = ?`, to, from); err != nil {
				return err
			}
			if _, err := q.db.ExecContext(ctx, `UPDATE OR IGNORE user_node_tags SET tag = ? WHERE tag = ?`, to, from); err != nil {
				return err
			}
		}
		// 新名称已经有了的（UPDATE OR IGNORE 跳过的）和删除标签时，删掉旧的。
		if _, err := q.db.ExecContext(ctx, `DELETE FROM node_tags WHERE tag = ?`, from); err != nil {
			return err
		}
		_, err := q.db.ExecContext(ctx, `DELETE FROM user_node_tags WHERE tag = ?`, from)
		return err
	})
}

// NodeTagExists 判断标签是否存在：有节点带着它，或者在成员的访问范围里。
func (q *Queries) NodeTagExists(ctx context.Context, tag string) (bool, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM node_tags WHERE tag = ?) + (SELECT COUNT(*) FROM user_node_tags WHERE tag = ?)`, tag, tag).Scan(&n)
	return n > 0, err
}

// NodesWithTags 返回带有这些标签之一的节点。
func (q *Queries) NodesWithTags(ctx context.Context, tags []string) ([]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	return q.strings(ctx, `SELECT DISTINCT node_id FROM node_tags WHERE tag IN (`+placeholders(len(tags))+`)`, anyArgs(tags)...)
}

// ---- 项目标签 ----

// ProjectTags 返回项目的标签，按名称排序。
func (q *Queries) ProjectTags(ctx context.Context, projectID string) ([]string, error) {
	return q.strings(ctx, `SELECT tag FROM project_tags WHERE project_id = ? ORDER BY tag`, projectID)
}

// SetProjectTags 替换项目的标签。
func (q *Queries) SetProjectTags(ctx context.Context, projectID string, tags []string) error {
	return q.inTx(ctx, func(q *Queries) error {
		if _, err := q.db.ExecContext(ctx, `DELETE FROM project_tags WHERE project_id = ?`, projectID); err != nil {
			return err
		}
		for _, t := range tags {
			if _, err := q.db.ExecContext(ctx, `INSERT OR IGNORE INTO project_tags (project_id, tag) VALUES (?, ?)`, projectID, t); err != nil {
				return err
			}
		}
		return nil
	})
}

// RenameProjectTag 重命名项目标签（新名称已存在时合并）。to 为空表示删除标签，从所有项目上去掉。
func (q *Queries) RenameProjectTag(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	return q.inTx(ctx, func(q *Queries) error {
		if to != "" {
			if _, err := q.db.ExecContext(ctx, `UPDATE OR IGNORE project_tags SET tag = ? WHERE tag = ?`, to, from); err != nil {
				return err
			}
		}
		// 新名称已经有了的（UPDATE OR IGNORE 跳过的）和删除标签时，删掉旧的。
		_, err := q.db.ExecContext(ctx, `DELETE FROM project_tags WHERE tag = ?`, from)
		return err
	})
}

// ProjectTagExists 判断项目标签是否存在（有项目带着它）。
func (q *Queries) ProjectTagExists(ctx context.Context, tag string) (bool, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_tags WHERE tag = ?`, tag).Scan(&n)
	return n > 0, err
}

// ---- 节点分组 ----

// NodeGroup 返回节点所在的分组，未分组时为空。
func (q *Queries) NodeGroup(ctx context.Context, nodeID string) (string, error) {
	var name string
	err := q.db.QueryRowContext(ctx, `SELECT name FROM node_groups WHERE node_id = ?`, nodeID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

// SetNodeGroup 把节点放进分组，name 为空表示移出分组（也用于移除节点时）。
func (q *Queries) SetNodeGroup(ctx context.Context, nodeID, name string) error {
	if name == "" {
		_, err := q.db.ExecContext(ctx, `DELETE FROM node_groups WHERE node_id = ?`, nodeID)
		return err
	}
	_, err := q.db.ExecContext(ctx, `INSERT INTO node_groups (node_id, name) VALUES (?, ?)
		ON CONFLICT (node_id) DO UPDATE SET name = excluded.name`, nodeID, name)
	return err
}

// RenameNodeGroup 重命名节点分组（新名称已存在时合并）。to 为空表示删除分组，里面的节点变成未分组。
func (q *Queries) RenameNodeGroup(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	if to == "" {
		_, err := q.db.ExecContext(ctx, `DELETE FROM node_groups WHERE name = ?`, from)
		return err
	}
	_, err := q.db.ExecContext(ctx, `UPDATE node_groups SET name = ? WHERE name = ?`, to, from)
	return err
}

// NodeGroupExists 判断节点分组是否存在（有节点在里面）。
func (q *Queries) NodeGroupExists(ctx context.Context, name string) (bool, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_groups WHERE name = ?`, name).Scan(&n)
	return n > 0, err
}

// ---- 访问范围里的分组和标签 ----

func (q *Queries) ListUserProjectGroups(ctx context.Context, userID int64) ([]string, error) {
	return q.strings(ctx, `SELECT name FROM user_project_groups WHERE user_id = ? ORDER BY name`, userID)
}

func (q *Queries) ListUserNodeTags(ctx context.Context, userID int64) ([]string, error) {
	return q.strings(ctx, `SELECT tag FROM user_node_tags WHERE user_id = ? ORDER BY tag`, userID)
}

// SetUserProjectGroups 替换成员访问范围里的分组。
func (q *Queries) SetUserProjectGroups(ctx context.Context, userID int64, names []string) error {
	if _, err := q.db.ExecContext(ctx, `DELETE FROM user_project_groups WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, n := range names {
		if _, err := q.db.ExecContext(ctx, `INSERT OR IGNORE INTO user_project_groups (user_id, name) VALUES (?, ?)`, userID, n); err != nil {
			return err
		}
	}
	return nil
}

// SetUserNodeTags 替换成员访问范围里的标签。
func (q *Queries) SetUserNodeTags(ctx context.Context, userID int64, tags []string) error {
	if _, err := q.db.ExecContext(ctx, `DELETE FROM user_node_tags WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, t := range tags {
		if _, err := q.db.ExecContext(ctx, `INSERT OR IGNORE INTO user_node_tags (user_id, tag) VALUES (?, ?)`, userID, t); err != nil {
			return err
		}
	}
	return nil
}

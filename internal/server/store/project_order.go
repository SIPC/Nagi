package store

// 成员自己的项目页、节点页排序（见 docs/panel.md#分组和标签）。手写的，理由同 tokens.go。

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ProjectOrder 是一个成员的项目页排序：排序方式，以及自定义顺序里的分组名和项目 ID。
type ProjectOrder struct {
	Sort     string
	Groups   []string
	Projects []string
}

// NodeOrder 是一个成员的节点页排序：排序方式，以及自定义顺序里的分组名和节点 ID。
type NodeOrder struct {
	Sort   string
	Groups []string
	Nodes  []string
}

func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}

// getOrder 读取一份排序（表名和列名是常量），没有保存过时返回空的。
func (q *Queries) getOrder(ctx context.Context, table, itemsCol string, userID int64) (sort string, groups, items []string, err error) {
	var g, i string
	err = q.db.QueryRowContext(ctx, `SELECT sort, groups, `+itemsCol+` FROM `+table+` WHERE user_id = ?`, userID).Scan(&sort, &g, &i)
	if errors.Is(err, sql.ErrNoRows) {
		return "", []string{}, []string{}, nil
	}
	if err != nil {
		return "", nil, nil, err
	}
	return sort, splitLines(g), splitLines(i), nil
}

// setOrder 整份覆盖一份排序。名称和 ID 里不能有换行（调用方检查）。
func (q *Queries) setOrder(ctx context.Context, table, itemsCol string, userID int64, sort string, groups, items []string) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO `+table+` (user_id, sort, groups, `+itemsCol+`) VALUES (?, ?, ?, ?)
ON CONFLICT (user_id) DO UPDATE SET sort = excluded.sort, groups = excluded.groups, `+itemsCol+` = excluded.`+itemsCol,
		userID, sort, strings.Join(groups, "\n"), strings.Join(items, "\n"))
	return err
}

// GetProjectOrder 读取成员的项目页排序，没有保存过时返回空的。
func (q *Queries) GetProjectOrder(ctx context.Context, userID int64) (ProjectOrder, error) {
	sort, groups, projects, err := q.getOrder(ctx, "user_project_order", "projects", userID)
	return ProjectOrder{Sort: sort, Groups: groups, Projects: projects}, err
}

// SetProjectOrder 整份覆盖成员的项目页排序。
func (q *Queries) SetProjectOrder(ctx context.Context, userID int64, o ProjectOrder) error {
	return q.setOrder(ctx, "user_project_order", "projects", userID, o.Sort, o.Groups, o.Projects)
}

// GetNodeOrder 读取成员的节点页排序，没有保存过时返回空的。
func (q *Queries) GetNodeOrder(ctx context.Context, userID int64) (NodeOrder, error) {
	sort, groups, nodes, err := q.getOrder(ctx, "user_node_order", "nodes", userID)
	return NodeOrder{Sort: sort, Groups: groups, Nodes: nodes}, err
}

// SetNodeOrder 整份覆盖成员的节点页排序。
func (q *Queries) SetNodeOrder(ctx context.Context, userID int64, o NodeOrder) error {
	return q.setOrder(ctx, "user_node_order", "nodes", userID, o.Sort, o.Groups, o.Nodes)
}

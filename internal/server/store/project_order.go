package store

// 成员自己的项目页排序（见 docs/panel.md#分组和标签）。手写的，理由同 tokens.go。

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

func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}

// GetProjectOrder 读取成员的排序，没有保存过时返回空的。
func (q *Queries) GetProjectOrder(ctx context.Context, userID int64) (ProjectOrder, error) {
	var sort, groups, projects string
	err := q.db.QueryRowContext(ctx, `SELECT sort, groups, projects FROM user_project_order WHERE user_id = ?`, userID).Scan(&sort, &groups, &projects)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectOrder{Groups: []string{}, Projects: []string{}}, nil
	}
	if err != nil {
		return ProjectOrder{}, err
	}
	return ProjectOrder{Sort: sort, Groups: splitLines(groups), Projects: splitLines(projects)}, nil
}

// SetProjectOrder 整份覆盖成员的排序。名称和 ID 里不能有换行（调用方检查）。
func (q *Queries) SetProjectOrder(ctx context.Context, userID int64, o ProjectOrder) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO user_project_order (user_id, sort, groups, projects) VALUES (?, ?, ?, ?)
ON CONFLICT (user_id) DO UPDATE SET sort = excluded.sort, groups = excluded.groups, projects = excluded.projects`,
		userID, o.Sort, strings.Join(o.Groups, "\n"), strings.Join(o.Projects, "\n"))
	return err
}

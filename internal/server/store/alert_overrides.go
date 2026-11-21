package store

// 告警规则的覆盖（迁移 0031，见 docs/monitoring.md#告警）。手写的，理由同 tokens.go。

import (
	"context"
	"database/sql"
	"errors"
)

// AlertOverride 是一条覆盖：Scope 是 node、project 或 container。Rules 是 protobuf（map<string, AlertRule> 包在 AlertOverride 里）。
type AlertOverride struct {
	Scope     string
	NodeID    string
	ProjectID string
	Container string
	Rules     []byte
	Revision  int64
}

// ErrAlertOverrideConflict：覆盖被别人改过（修订号不对）。
var ErrAlertOverrideConflict = errors.New("alert override revision conflict")

func overrideWhere(o AlertOverride) (table, where string, args []any) {
	switch o.Scope {
	case "node":
		return "alert_node_overrides", "node_id = ?", []any{o.NodeID}
	case "project":
		return "alert_project_overrides", "project_id = ?", []any{o.ProjectID}
	default:
		return "alert_container_overrides", "project_id = ? AND container = ?", []any{o.ProjectID, o.Container}
	}
}

// ListAlertOverrides 返回全部覆盖。
func (q *Queries) ListAlertOverrides(ctx context.Context) ([]AlertOverride, error) {
	var out []AlertOverride
	for _, part := range []struct{ scope, query string }{
		{"node", "SELECT node_id, '', '', rules, revision FROM alert_node_overrides ORDER BY node_id"},
		{"project", "SELECT '', project_id, '', rules, revision FROM alert_project_overrides ORDER BY project_id"},
		{"container", "SELECT '', project_id, container, rules, revision FROM alert_container_overrides ORDER BY project_id, container"},
	} {
		rows, err := q.db.QueryContext(ctx, part.query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			o := AlertOverride{Scope: part.scope}
			if err := rows.Scan(&o.NodeID, &o.ProjectID, &o.Container, &o.Rules, &o.Revision); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, o)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetAlertOverride 返回一条覆盖，没有时 Revision 为 0、Rules 为空。
func (q *Queries) GetAlertOverride(ctx context.Context, o AlertOverride) (AlertOverride, error) {
	table, where, args := overrideWhere(o)
	err := q.db.QueryRowContext(ctx, "SELECT rules, revision FROM "+table+" WHERE "+where, args...).Scan(&o.Rules, &o.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		o.Rules, o.Revision = nil, 0
		return o, nil
	}
	return o, err
}

// SetAlertOverride 保存一条覆盖（rules 为空时删除），base 是读到的修订号，被别人改过时返回 ErrAlertOverrideConflict。
// 返回新的修订号。
func (q *Queries) SetAlertOverride(ctx context.Context, o AlertOverride, base int64) (int64, error) {
	var next int64
	err := q.inTx(ctx, func(q *Queries) error {
		cur, err := q.GetAlertOverride(ctx, o)
		if err != nil {
			return err
		}
		if cur.Revision != base {
			return ErrAlertOverrideConflict
		}
		table, where, args := overrideWhere(o)
		if len(o.Rules) == 0 {
			_, err = q.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+where, args...)
			next = 0
			return err
		}
		next = cur.Revision + 1
		switch o.Scope {
		case "node":
			_, err = q.db.ExecContext(ctx, "INSERT OR REPLACE INTO alert_node_overrides (node_id, rules, revision) VALUES (?, ?, ?)", o.NodeID, o.Rules, next)
		case "project":
			_, err = q.db.ExecContext(ctx, "INSERT OR REPLACE INTO alert_project_overrides (project_id, rules, revision) VALUES (?, ?, ?)", o.ProjectID, o.Rules, next)
		default:
			_, err = q.db.ExecContext(ctx, "INSERT OR REPLACE INTO alert_container_overrides (project_id, container, rules, revision) VALUES (?, ?, ?, ?)", o.ProjectID, o.Container, o.Rules, next)
		}
		return err
	})
	return next, err
}

// DeleteNodeAlertOverride 删除节点的覆盖（节点被移除时）。项目、容器的随外键一起删除。
func (q *Queries) DeleteNodeAlertOverride(ctx context.Context, nodeID string) error {
	_, err := q.db.ExecContext(ctx, "DELETE FROM alert_node_overrides WHERE node_id = ?", nodeID)
	return err
}

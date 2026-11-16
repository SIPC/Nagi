package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// metaActivityMigrated 记下面板库 activity 表已经复制到哪一条，复制完以后是 "done"。
const metaActivityMigrated = "activity_migrated"

// sessionActions、readActions 是原来活动记录里属于这两类的动作。
var (
	sessionActions = map[string]bool{"OpenNodeShell": true, "CloseNodeShell": true, "OpenTerminal": true, "ExecCommand": true, "NodeExec": true}
	readActions    = map[string]bool{"DownloadVolumeFile": true, "DownloadNodeFile": true, "ReadNodeFile": true, "GetBackupKey": true}
	nodeActions    = map[string]bool{"OpenNodeShell": true, "CloseNodeShell": true, "NodeExec": true, "DownloadNodeFile": true, "UploadNodeFile": true}
)

// MigrateActivity 把面板库 activity 表里的记录复制进审计库（见 docs/audit.md#存储保留与同步），缺少的字段留空。
// 只做一次；中途中断的话下次从断点接着复制。
func (l *Log) MigrateActivity(ctx context.Context, panel *sql.DB) (int, error) {
	state := l.Meta(ctx, metaActivityMigrated)
	if state == "done" {
		return 0, nil
	}
	after, _ := strconv.ParseInt(state, 10, 64)
	var exists int
	if err := panel.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'activity'`).Scan(&exists); err != nil {
		return 0, err
	}
	n := 0
	if exists > 0 {
		for {
			rows, err := panel.QueryContext(ctx, `SELECT id, created_at, kind, actor, action, project_id, target, detail, failed, via
				FROM activity WHERE id > ? ORDER BY id LIMIT 1000`, after)
			if err != nil {
				return n, err
			}
			var batch []Event
			for rows.Next() {
				var id, created, failed int64
				var kind, actor, action, project, target, detail, via string
				if err := rows.Scan(&id, &created, &kind, &actor, &action, &project, &target, &detail, &failed, &via); err != nil {
					rows.Close()
					return n, err
				}
				after = id
				batch = append(batch, legacyEvent(created, kind, actor, action, project, target, detail, failed != 0, via))
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return n, err
			}
			if len(batch) == 0 {
				break
			}
			for _, e := range batch {
				normalize(&e)
				if _, err := l.insert(ctx, &e); err != nil {
					return n, err
				}
				n++
			}
			if err := l.SetMeta(ctx, metaActivityMigrated, strconv.FormatInt(after, 10)); err != nil {
				return n, err
			}
		}
	}
	return n, l.SetMeta(ctx, metaActivityMigrated, "done")
}

// legacyEvent 把一条原来的活动记录转成审计记录。
func legacyEvent(created int64, kind, actor, action, project, target, detail string, failed bool, via string) Event {
	e := Event{Time: time.Unix(created, 0), Action: action, ActorName: actor, ProjectID: project, TargetName: target, Result: ResultOK}
	if kind == "alert" {
		e.Category, e.ActorType, e.Via = Alert, ActorNode, ViaNode
		b, _ := json.Marshal(map[string]string{"title": detail})
		e.Params = string(b)
		if project != "" && target != "" {
			e.TargetType, e.TargetID = "container", target
		}
		return e
	}
	e.ActorType, e.Via = ActorUser, ViaWeb
	if name, ok := strings.CutPrefix(via, "mcp:"); ok {
		e.Via, e.Client = ViaMCP, name
	}
	switch {
	case action == "Login" || action == "Logout" || action == "Setup":
		e.Category = Login
	case action == "MCPToolDenied":
		e.Category, e.Action, e.Result = MCP, "mcp:"+target, ResultDenied
		e.TargetType, e.TargetID = "tool", target
	case sessionActions[action]:
		e.Category = Session
	case readActions[action]:
		e.Category = Read
	default:
		e.Category = Operation
	}
	if nodeActions[action] {
		e.TargetType = "node"
	} else if project != "" && target != "" && e.TargetType == "" {
		e.TargetType, e.TargetID = "container", target
	}
	if failed {
		if e.Result == ResultOK {
			e.Result = ResultFailed
		}
		e.Error = detail
	} else if detail != "" {
		b, _ := json.Marshal(map[string]string{"detail": detail})
		e.Params = string(b)
	}
	return e
}

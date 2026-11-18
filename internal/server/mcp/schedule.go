package mcp

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 定时快照计划（见 docs/snapshot.md#定时快照）：项目设置里一份（update_project），容器可以单独设置（set_snapshot_schedule）。

// ScheduleArgs 是修改定时快照计划的参数，没给的保持不变。
type ScheduleArgs struct {
	SnapshotsEnabled        *bool    `json:"snapshots_enabled,omitempty" jsonschema:"turn scheduled snapshots on or off. Containers with failover still take one every 15 minutes; containers with a backup node stop syncing to it while off"`
	SnapshotIntervalMinutes *uint32  `json:"snapshot_interval_minutes,omitempty" jsonschema:"take a snapshot every this many minutes, at most 10080; 0 means the default (720). Switches from daily times back to an interval"`
	SnapshotTimes           []string `json:"snapshot_times,omitempty" jsonschema:"take a snapshot every day at these times instead of at an interval, as 24-hour HH:MM (at most 24), e.g. 03:00 and 15:00; an empty list switches back to the interval"`
	SnapshotTimeZone        *string  `json:"snapshot_time_zone,omitempty" jsonschema:"IANA time zone for snapshot_times, e.g. Asia/Shanghai; ask the user if unsure. Defaults to the one already set"`
}

func (a ScheduleArgs) empty() bool {
	return a.SnapshotsEnabled == nil && a.SnapshotIntervalMinutes == nil && a.SnapshotTimes == nil && a.SnapshotTimeZone == nil
}

// apply 在 base 上改出新的计划，返回改了什么。
func (a ScheduleArgs) apply(base *nagiv1.SnapshotSchedule) (*nagiv1.SnapshotSchedule, []string) {
	sch := &nagiv1.SnapshotSchedule{}
	if base != nil {
		sch = proto.CloneOf(base)
	}
	var changed []string
	if a.SnapshotsEnabled != nil && *a.SnapshotsEnabled == sch.GetDisabled() {
		sch.Disabled = !*a.SnapshotsEnabled
		changed = append(changed, fmt.Sprintf("snapshots_enabled → %t", *a.SnapshotsEnabled))
	}
	if a.SnapshotIntervalMinutes != nil && (*a.SnapshotIntervalMinutes != sch.GetIntervalMinutes() || len(sch.GetTimes()) > 0) {
		sch.IntervalMinutes = *a.SnapshotIntervalMinutes
		if a.SnapshotTimes == nil && len(sch.GetTimes()) > 0 {
			sch.Times = nil
			changed = append(changed, "snapshot_times → none (interval)")
		}
		changed = append(changed, fmt.Sprintf("snapshot_interval_minutes → %d", spec.SnapshotIntervalMinutes(sch.GetIntervalMinutes())))
	}
	if a.SnapshotTimes != nil && !slices.Equal(a.SnapshotTimes, sch.GetTimes()) {
		sch.Times = a.SnapshotTimes
		if len(sch.Times) == 0 {
			changed = append(changed, "snapshot_times → none (interval)")
		} else {
			changed = append(changed, "snapshot_times → "+strings.Join(sch.Times, ", "))
		}
	}
	if a.SnapshotTimeZone != nil && *a.SnapshotTimeZone != sch.GetTimeZone() {
		sch.TimeZone = *a.SnapshotTimeZone
		changed = append(changed, "snapshot_time_zone → "+sch.TimeZone)
	}
	return sch, changed
}

// errNeedTimeZone：设置了每天的时刻但不知道时区。
var errNeedTimeZone = errors.New("snapshot_time_zone is required with snapshot_times (ask the user for their time zone, e.g. Asia/Shanghai)")

// scheduleView 是定时快照计划给 agent 看的样子。
type scheduleView struct {
	Enabled         bool     `json:"enabled"`
	IntervalMinutes uint32   `json:"interval_minutes,omitempty"`
	Times           []string `json:"daily_times,omitempty"`
	TimeZone        string   `json:"time_zone,omitempty"`
	// Own：容器单独设置的（不是用项目的）。只用于容器。
	Own bool `json:"own,omitempty"`
}

func scheduleOf(sch *nagiv1.SnapshotSchedule) scheduleView {
	v := scheduleView{Enabled: !sch.GetDisabled(), Times: sch.GetTimes()}
	if len(v.Times) > 0 {
		v.TimeZone = sch.GetTimeZone()
		if v.TimeZone == "" {
			v.TimeZone = "UTC"
		}
	} else {
		v.IntervalMinutes = spec.SnapshotIntervalMinutes(sch.GetIntervalMinutes())
	}
	return v
}

// containerSchedule 返回容器生效的计划。
func containerSchedule(p *nagiv1.Project, m *nagiv1.Container) scheduleView {
	if sch := m.GetSnapshotSchedule(); sch != nil {
		v := scheduleOf(sch)
		v.Own = true
		return v
	}
	return scheduleOf(spec.ProjectSnapshotSchedule(p.GetSettings()))
}

type ContainerScheduleArgs struct {
	ContainerArg
	ScheduleArgs
	UseProject bool `json:"use_project,omitempty" jsonschema:"drop the container's own schedule and follow the project's again; the other fields are ignored"`
}

func toolSetSnapshotSchedule(c *call, in ContainerScheduleArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	req := &nagiv1.SetContainerSnapshotScheduleRequest{ProjectId: p.GetId(), Container: m.GetName()}
	changed := []string{"follows the project's schedule"}
	if in.UseProject {
		if m.GetSnapshotSchedule() == nil {
			return map[string]any{"changed": []string{}, "note": "the container already follows the project's schedule"}, nil
		}
	} else {
		if in.ScheduleArgs.empty() {
			return nil, errors.New("give snapshots_enabled, snapshot_interval_minutes, snapshot_times or snapshot_time_zone, or use_project")
		}
		base := m.GetSnapshotSchedule()
		if base == nil {
			base = spec.ProjectSnapshotSchedule(p.GetSettings())
		}
		req.Schedule, changed = in.ScheduleArgs.apply(base)
		if len(req.Schedule.GetTimes()) > 0 && req.Schedule.GetTimeZone() == "" {
			return nil, errNeedTimeZone
		}
		if len(changed) == 0 && m.GetSnapshotSchedule() != nil {
			return map[string]any{"changed": []string{}, "note": "nothing changed"}, nil
		}
		if len(changed) == 0 {
			changed = []string{"own schedule, same as the project's for now"}
		}
	}
	res, err := c.h.clients.projects.SetContainerSnapshotSchedule(c.ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"changed": changed, "note": "takes effect immediately"}
	if m2, err := findContainer(res.Msg, m.GetName()); err == nil {
		sch := containerSchedule(res.Msg, m2)
		out["schedule"] = sch
		if w := backupScheduleWarning(m2, sch); w != "" {
			out["warnings"] = []string{w}
		}
	}
	return out, nil
}

// backupScheduleWarning：设了备份节点或顶替的容器关掉了定时快照。备份节点在每次定时快照后同步，关掉以后副本不再更新。
func backupScheduleWarning(m *nagiv1.Container, sch scheduleView) string {
	if sch.Enabled || len(m.GetBackupNodeIds()) == 0 && m.GetFailover() == nil {
		return ""
	}
	return "Scheduled snapshots are off for a container with backup nodes or failover. Backup nodes sync after each scheduled snapshot, so their copy stops being updated (failover still takes a snapshot every 15 minutes). Keep scheduled snapshots on, and use a short interval for databases."
}

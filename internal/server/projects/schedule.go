package projects

import (
	"context"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 定时快照的计划（见 docs/snapshot.md#定时快照）：项目设置里一份，容器可以单独设置一份整个替换掉项目的。
// 面板把生效的那份随容器配置推给节点（ContainerSpec.snapshot_schedule），旧版本的节点按换算出的间隔。

// containerSnapshotSchedule 返回容器单独设置的计划，没有设置时为 nil。
func containerSnapshotSchedule(c store.Container) *nagiv1.SnapshotSchedule {
	if len(c.SnapshotSchedule) == 0 {
		return nil
	}
	sch := &nagiv1.SnapshotSchedule{}
	if proto.Unmarshal(c.SnapshotSchedule, sch) != nil {
		return nil
	}
	return sch
}

// effectiveSnapshotSchedule 返回容器生效的计划：容器单独设置了就用它的，否则用项目的。
func effectiveSnapshotSchedule(settings *nagiv1.ProjectSettings, c store.Container) *nagiv1.SnapshotSchedule {
	if sch := containerSnapshotSchedule(c); sch != nil {
		return sch
	}
	return spec.ProjectSnapshotSchedule(settings)
}

// SetContainerSnapshotSchedule 让容器单独设置定时快照计划，sch 为 nil 时改回用项目的。推给节点缓存，节点离线时由 Sync 补推。
func (s *Service) SetContainerSnapshotSchedule(ctx context.Context, projectID, name string, sch *nagiv1.SnapshotSchedule) (*nagiv1.Project, error) {
	var b []byte
	if sch != nil {
		clean, err := spec.CleanSnapshotSchedule(sch)
		if err != nil {
			return nil, err
		}
		b = marshalSchedule(clean)
	}
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	if _, err := s.container(ctx, projectID, name); err != nil {
		return nil, err
	}
	if err := s.Q.SetContainerSnapshotSchedule(ctx, store.SetContainerSnapshotScheduleParams{SnapshotSchedule: b, ProjectID: projectID, Name: name}); err != nil {
		return nil, err
	}
	_ = s.push(context.WithoutCancel(ctx), projectID, name)
	return s.reload(ctx, projectID)
}

// marshalSchedule 编码容器单独设置的计划。全是默认值时编码是空的，写上一个值为 0 的间隔字段，
// 和"没有单独设置"（NULL）区分开；解码时没有区别。
func marshalSchedule(sch *nagiv1.SnapshotSchedule) []byte {
	b, _ := proto.Marshal(sch)
	if len(b) == 0 {
		b = []byte{0x10, 0x00}
	}
	return b
}

// mergeSnapshotSchedule 整理项目设置里的计划：带了计划的按计划（旧的间隔字段跟着改）；没带计划只改了间隔的
// （旧版本的客户端），在原来的计划上换掉间隔。
func mergeSnapshotSchedule(next, old *nagiv1.ProjectSettings) {
	if next.GetSnapshotSchedule() != nil {
		next.SnapshotIntervalMinutes = next.GetSnapshotSchedule().GetIntervalMinutes()
		return
	}
	if old.GetSnapshotSchedule() == nil {
		return
	}
	sch := proto.CloneOf(old.GetSnapshotSchedule())
	if next.GetSnapshotIntervalMinutes() != old.GetSnapshotIntervalMinutes() {
		sch.IntervalMinutes = next.GetSnapshotIntervalMinutes()
	}
	next.SnapshotSchedule = sch
	next.SnapshotIntervalMinutes = sch.GetIntervalMinutes()
}

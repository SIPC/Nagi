package projects

import (
	"context"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 容器单独设置的计划存进数据库能读回来；全是默认值时也和"没有单独设置"区分开；清空后改回用项目的。
func TestContainerSnapshotScheduleStored(t *testing.T) {
	ctx := context.Background()
	s := entryTestService(t)
	const project = "aaaaaaaaaaaaaaaa"
	settings := &nagiv1.ProjectSettings{SnapshotSchedule: &nagiv1.SnapshotSchedule{IntervalMinutes: 60}}
	load := func() store.Container {
		c, err := s.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: project, Name: "web"})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got := effectiveSnapshotSchedule(settings, load()); got.GetIntervalMinutes() != 60 {
		t.Fatalf("没有单独设置时应用项目的：%v", got)
	}
	for _, sch := range []*nagiv1.SnapshotSchedule{{}, {Disabled: true}, {Times: []string{"03:00"}, TimeZone: "Asia/Shanghai"}} {
		if err := s.Q.SetContainerSnapshotSchedule(ctx, store.SetContainerSnapshotScheduleParams{SnapshotSchedule: marshalSchedule(sch), ProjectID: project, Name: "web"}); err != nil {
			t.Fatal(err)
		}
		got := containerSnapshotSchedule(load())
		if got == nil || !proto.Equal(got, sch) {
			t.Errorf("读回来是 %v，应为 %v", got, sch)
		}
	}
	if err := s.Q.SetContainerSnapshotSchedule(ctx, store.SetContainerSnapshotScheduleParams{ProjectID: project, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if got := containerSnapshotSchedule(load()); got != nil {
		t.Errorf("清空后应改回用项目的：%v", got)
	}
}

// 只改了间隔的旧客户端：在原来的计划上换掉间隔；什么都没带：保留原来的计划。
func TestMergeSnapshotSchedule(t *testing.T) {
	old := &nagiv1.ProjectSettings{SnapshotIntervalMinutes: 0, SnapshotSchedule: &nagiv1.SnapshotSchedule{Disabled: true, Times: []string{"03:00"}, TimeZone: "UTC"}}
	next := &nagiv1.ProjectSettings{}
	mergeSnapshotSchedule(next, old)
	if !proto.Equal(next.GetSnapshotSchedule(), old.GetSnapshotSchedule()) {
		t.Errorf("没带计划时应保留：%v", next.GetSnapshotSchedule())
	}
	next = &nagiv1.ProjectSettings{SnapshotIntervalMinutes: 30}
	mergeSnapshotSchedule(next, old)
	if next.GetSnapshotSchedule().GetIntervalMinutes() != 30 || !next.GetSnapshotSchedule().GetDisabled() {
		t.Errorf("只改间隔：%v", next.GetSnapshotSchedule())
	}
	next = &nagiv1.ProjectSettings{SnapshotSchedule: &nagiv1.SnapshotSchedule{IntervalMinutes: 90}}
	mergeSnapshotSchedule(next, old)
	if next.GetSnapshotIntervalMinutes() != 90 {
		t.Errorf("旧的间隔字段应跟着计划：%d", next.GetSnapshotIntervalMinutes())
	}
}

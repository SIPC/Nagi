package agent

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

const testProject = "p000000000000001"

// 放行按两条路算：项目放行（端口要开放）或入口放行（只限那个端口），撤销一条时另一条还在就不断开。
func TestPortOpen(t *testing.T) {
	var k mesh.Key
	k[0] = 1
	n := &intranet{plan: &nagiv1.LinkPlan{
		Allows: []*nagiv1.LinkAllow{{ProjectId: testProject, Callers: [][]byte{k[:]}, InternalPorts: []string{"db:5432", "dns:53/udp"}}},
		EntryAllows: []*nagiv1.LinkEntryAllow{
			{ProjectId: testProject, TargetName: "web", Port: 8080, Protocol: "tcp", Callers: [][]byte{k[:]}},
		},
	}}
	cases := []struct {
		name     string
		port     uint16
		protocol string
		declared []uint32
		want     bool
	}{
		{"web", 80, spec.ProtoTCP, []uint32{80}, true},
		{"web", 81, spec.ProtoTCP, []uint32{80}, false},
		{"web", 8080, spec.ProtoTCP, nil, true},
		{"db", 5432, spec.ProtoTCP, nil, true},
		{"db", 5432, spec.ProtoUDP, nil, false},
		{"dns", 53, spec.ProtoUDP, nil, true},
	}
	for _, c := range cases {
		if got := n.portOpen(testProject, c.name, c.port, c.protocol, k, c.declared); got != c.want {
			t.Errorf("%s %d/%s: %v，应为 %v", c.name, c.port, c.protocol, got, c.want)
		}
	}
	var other mesh.Key
	if n.portOpen(testProject, "web", 80, spec.ProtoTCP, other, []uint32{80}) {
		t.Error("没有放行的节点不应该能连")
	}

	// 撤销项目放行：入口放行的端口还能连，其他的不能。
	n.plan = &nagiv1.LinkPlan{EntryAllows: n.plan.GetEntryAllows()}
	if !n.portOpen(testProject, "web", 8080, spec.ProtoTCP, k, nil) || n.portOpen(testProject, "web", 80, spec.ProtoTCP, k, []uint32{80}) {
		t.Error("撤销项目放行后应该只剩入口放行的端口")
	}
}

// 计划换掉时立刻重新检查，不用等 accessRecheck。
func TestWatchAccessWakesOnPlanChange(t *testing.T) {
	var k mesh.Key
	k[0] = 1
	n := &intranet{file: filepath.Join(t.TempDir(), "links.pb"), kick: make(chan struct{}, 1)}
	if err := n.set(&nagiv1.LinkPlan{Allows: []*nagiv1.LinkAllow{{ProjectId: testProject, Callers: [][]byte{k[:]}}}}); err != nil {
		t.Fatal(err)
	}
	dropped := make(chan struct{})
	stop := n.watchAccess(func() { close(dropped) }, func() bool {
		return n.portOpen(testProject, "web", 80, spec.ProtoTCP, k, []uint32{80})
	})
	defer stop()

	// 换成同样放行的计划：不断开。
	if err := n.set(&nagiv1.LinkPlan{Allows: []*nagiv1.LinkAllow{{ProjectId: testProject, Callers: [][]byte{k[:]}}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dropped:
		t.Fatal("放行还在，不应该断开")
	case <-time.After(200 * time.Millisecond):
	}

	if err := n.set(&nagiv1.LinkPlan{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dropped:
	case <-time.After(accessRecheck / 2):
		t.Fatal("撤销放行后应该立刻断开")
	}
}

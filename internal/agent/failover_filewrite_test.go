package agent

import (
	"strings"
	"testing"
)

func TestCheckFileWrite(t *testing.T) {
	a := &agent{failovers: newFailovers(t.TempDir())}
	const project, container = "p", "c"
	if err := a.checkFileWrite(project, container); err != nil {
		t.Fatalf("没有顶替的容器应该能写：%v", err)
	}

	// 测试里的 agent 没有 mesh，selfKey 是 nil，所以持有者 nil 就是本节点。
	cases := []struct {
		name  string
		st    failoverState
		allow bool
	}{
		{"持有租约", failoverState{}, true},
		{"租约在别的节点", failoverState{holder: []byte("other")}, false},
		{"续不上租约", failoverState{fenced: true}, false},
		{"正在恢复", failoverState{restoring: true}, false},
		{"正在切回", failoverState{failingBack: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := c.st
			a.failovers.m[project+"-"+container] = &st
			err := a.checkFileWrite(project, container)
			if c.allow && err != nil {
				t.Fatalf("应该能写：%v", err)
			}
			if !c.allow && (err == nil || !strings.Contains(err.Error(), "无法在此节点上编辑文件")) {
				t.Fatalf("应该拒绝并说明无法编辑文件，实际：%v", err)
			}
		})
	}
}

package mcp

import (
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 按名称选节点：有不止一台叫这个名称时不猜，要求用 ID（2026-11-19-834f027#3）。
func TestFindNodeAmbiguous(t *testing.T) {
	list := &nagiv1.ListNodesResponse{Nodes: []*nagiv1.Node{
		{Id: "aaaa", Name: "prod"}, {Id: "bbbb", Name: "Prod"}, {Id: "cccc", Name: "edge"},
	}}
	if n, err := findNode(list, "edge"); err != nil || n.GetId() != "cccc" {
		t.Errorf("唯一的名称：%v %v", n, err)
	}
	if n, err := findNode(list, "bbbb"); err != nil || n.GetId() != "bbbb" {
		t.Errorf("按 ID：%v %v", n, err)
	}
	if _, err := findNode(list, "prod"); err == nil || !strings.Contains(err.Error(), "aaaa") || !strings.Contains(err.Error(), "bbbb") {
		t.Errorf("重名时应该报错并列出 ID：%v", err)
	}
	if _, err := findNode(list, "nope"); err == nil {
		t.Error("找不到应该报错")
	}
}

// 重名的报错会记进审计的失败详情：名称和各台的 ID 都是引用，查看的人看不到的那台显示为「其他节点」，
// 一台都看不到时名称也不出现。
func TestFindNodeAmbiguousRefs(t *testing.T) {
	list := &nagiv1.ListNodesResponse{Nodes: []*nagiv1.Node{{Id: "aaaa", Name: "prod"}, {Id: "bbbb", Name: "Prod"}}}
	_, err := findNode(list, "prod")
	if err == nil || err.Error() != `2 nodes are named "prod"; pass the node ID instead (aaaa, bbbb)` {
		t.Fatalf("调用者看到的报错：%v", err)
	}
	hide := func(hidden ...string) string {
		out, _ := i18n.Redact(err, func(r i18n.Ref) (string, bool) {
			for _, id := range hidden {
				if r.Kind == "node" && r.ID == id {
					return "other node", true
				}
			}
			return "", false
		})
		return out.Error()
	}
	if got := hide("bbbb"); got != `2 nodes are named "prod"; pass the node ID instead (aaaa, other node)` {
		t.Errorf("看不到 bbbb：%s", got)
	}
	if got := hide("aaaa", "bbbb"); strings.Contains(got, "prod") || strings.Contains(got, "aaaa") || strings.Contains(got, "bbbb") {
		t.Errorf("一台都看不到：%s", got)
	}
	// 记进审计的那一份带着引用标记，按 ID 换得回来。
	marked := i18n.Marked(func(h func(i18n.Ref) (string, bool)) string {
		out, _ := i18n.Redact(err, h)
		return out.Error()
	})
	if got := i18n.RenderRefs(marked, func(r i18n.Ref) string {
		if r.ID == "bbbb" {
			return "other node"
		}
		return r.Name
	}); got != `2 nodes are named "prod"; pass the node ID instead (aaaa, other node)` {
		t.Errorf("审计里显示：%q", got)
	}
}

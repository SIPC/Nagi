package mcp

import (
	"strings"
	"testing"

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

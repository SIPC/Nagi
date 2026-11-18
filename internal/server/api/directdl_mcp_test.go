package api

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// MCP 的 get_node_download_link：和 read_node_file 一样只给管理员，令牌要勾「节点终端」，网页的会话要做过高权限确认；
// 通过后节点不能直连时返回 ErrNoDirect（不悄悄改成经面板）。
func TestNodeDirectLinkPermissions(t *testing.T) {
	h := newAuditHarness(t)
	h.a.Nodes = nodes.NewRegistry(h.a.Q, slog.New(slog.DiscardHandler), h.a.Sealer, nil)
	full := auth.User{ID: 1, Name: "admin", TokenID: 7, Role: auth.RoleAdmin, AllowExec: true, AllowNodeExec: true}
	noNode := full
	noNode.AllowNodeExec = false
	operator := full
	operator.Role = auth.RoleOperator
	session := auth.User{ID: 1, Name: "admin", Role: auth.RoleAdmin} // 没做过高权限确认
	for name, u := range map[string]auth.User{"没勾节点终端": noNode, "不是管理员": operator, "会话没有确认身份": session} {
		_, err := h.a.NodeDirectLink(auth.WithUser(context.Background(), u), "n1", "/etc/hostname")
		if err == nil || errors.Is(err, ErrNoDirect) {
			t.Errorf("%s：应该拒绝，实际 %v", name, err)
		}
	}
	if _, err := h.a.NodeDirectLink(auth.WithUser(context.Background(), full), "n1", "/etc/hostname"); !errors.Is(err, ErrNoDirect) {
		t.Errorf("节点不在线时应该说不能直连：%v", err)
	}
}

// MCP 的 get_download_link：权限和 read_file 一样（能操作容器，令牌要允许执行命令），在查项目之前就拒绝。
func TestVolumeDirectLinkPermissions(t *testing.T) {
	h := newAuditHarness(t)
	ref := &nagiv1.VolumeFileRef{ProjectId: "p", Container: "web", Volume: "data", Path: "a.txt"}
	for name, u := range map[string]auth.User{
		"令牌不能执行命令": {ID: 1, TokenID: 7, Role: auth.RoleAdmin},
		"只读角色":     {ID: 2, Role: auth.RoleViewer},
	} {
		if _, err := h.a.VolumeDirectLink(auth.WithUser(context.Background(), u), ref); err == nil || errors.Is(err, ErrNoDirect) {
			t.Errorf("%s：应该拒绝，实际 %v", name, err)
		}
	}
}

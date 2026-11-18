package mcp

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"

	"github.com/SIPC/nagi/internal/server/api"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// 下载链接的工具：get_download_link 和 read_file 一样要「执行命令」，另外只给管理员；get_node_download_link 和
// read_node_file 一样要「节点终端」。都不在备用面板上。
func TestDownloadLinkToolsAllowed(t *testing.T) {
	byName := map[string]tool{}
	for _, tl := range allTools {
		byName[tl.name] = tl
	}
	vol, node := byName["get_download_link"], byName["get_node_download_link"]
	if vol.name == "" || node.name == "" {
		t.Fatal("工具没有登记")
	}
	admin := auth.User{TokenID: 1, Role: auth.RoleAdmin, AllowExec: true, AllowNodeExec: true}
	noExec := admin
	noExec.AllowExec = false
	noNode := admin
	noNode.AllowNodeExec = false
	operator := admin
	operator.Role = auth.RoleOperator
	for i, c := range []struct {
		tl      tool
		u       auth.User
		standby bool
		want    bool
	}{
		{vol, admin, false, true}, {vol, noNode, false, true}, {vol, noExec, false, false}, {vol, operator, false, false}, {vol, admin, true, false},
		{node, admin, false, true}, {node, noNode, false, false}, {node, noExec, false, false}, {node, operator, false, false}, {node, admin, true, false},
	} {
		if got := c.tl.allowedFor(c.u, c.standby); got != c.want {
			t.Errorf("case %d %s：allowedFor = %v，想要 %v", i, c.tl.name, got, c.want)
		}
	}
	if vol.scope() != "nagi:exec" || node.scope() != "nagi:node" {
		t.Errorf("OAuth 范围：%s %s", vol.scope(), node.scope())
	}
	// 描述写明命令里有密钥、会出现在对话里。
	for _, tl := range []tool{vol, node} {
		if !strings.Contains(tl.description, "key") || !strings.Contains(tl.description, "conversation") {
			t.Errorf("%s 的描述要说明命令里有密钥：%s", tl.name, tl.description)
		}
	}
}

// 返回结果：HTTPS 给链接；加密 HTTP 给命令，并提醒密钥；没有 SHA-256 时写明原因。
func TestDirectView(t *testing.T) {
	v := directView(api.DirectResult{URL: "https://203.0.113.5/.nagi/download/x"}, "a.txt")
	if v["method"] != "https" || v["url"] == "" || v["command_posix"] != nil {
		t.Errorf("链接：%v", v)
	}
	v = directView(api.DirectResult{Command: &nodes.DirectHTTPCommand{Command: "curl …", CommandWindows: "& { … }", Name: "a.txt", Size: 3, HashSkipped: "too_large"}}, "a.txt")
	if v["method"] != "encrypted_http" || v["command_posix"] != "curl …" || v["command_windows"] != "& { … }" || v["not_verified"] != "too_large" || !strings.Contains(v["note"].(string), "key") {
		t.Errorf("命令：%v", v)
	}
}

// listOnlyNodes 只回答节点列表。
type listOnlyNodes struct {
	nagiv1connect.NodeServiceClient
}

func (listOnlyNodes) ListNodes(context.Context, *connect.Request[nagiv1.ListNodesRequest]) (*connect.Response[nagiv1.ListNodesResponse], error) {
	return connect.NewResponse(&nagiv1.ListNodesResponse{Nodes: []*nagiv1.Node{{Id: "n1", Name: "edge"}}}), nil
}

// 工具把令牌的用户放进 ctx 再调用 api（它不经过接口的拦截器，按 auth.CurrentUser 检查权限）。
func TestNodeDownloadLinkPassesUser(t *testing.T) {
	u := auth.User{ID: 3, Name: "admin", TokenID: 9, Role: auth.RoleAdmin, AllowExec: true, AllowNodeExec: true}
	var got auth.User
	h := &handler{clients: clients{nodes: listOnlyNodes{}}}
	h.o.Direct.Node = func(ctx context.Context, nodeID, path string) (api.DirectResult, error) {
		got = auth.CurrentUser(ctx)
		return api.DirectResult{URL: "https://203.0.113.5/.nagi/download/x"}, nil
	}
	c := &call{ctx: auth.WithInternalUser(context.Background(), u), u: u, h: h}
	v, err := toolGetNodeDownloadLink(c, NodePathArgs{Node: "edge", Path: "/srv/a"})
	if err != nil || got.ID != 3 || got.TokenID != 9 || !got.AllowNodeExec {
		t.Fatalf("api 拿到的用户：%+v，%v %v", got, v, err)
	}
}

package mcp

import (
	"strings"
	"testing"

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
	if v["method"] != "https" || v["url"] == "" || v["command"] != nil {
		t.Errorf("链接：%v", v)
	}
	v = directView(api.DirectResult{Command: &nodes.DirectHTTPCommand{Command: "curl …", Name: "a.txt", Size: 3, HashSkipped: "too_large"}}, "a.txt")
	if v["method"] != "encrypted_http" || v["command"] != "curl …" || v["not_verified"] != "too_large" || !strings.Contains(v["note"].(string), "key") {
		t.Errorf("命令：%v", v)
	}
}

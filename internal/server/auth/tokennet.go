package auth

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/store"
)

// 访问令牌的网段（见 docs/mcp.md#网段）：生成时可以填写允许使用的网段；能执行命令、能用节点终端的令牌
// 第一次从新网段使用时告警。令牌存在 MCP 客户端的配置文件里，被偷走后别人能在任何地方用它。

const maxSeenNetworks = 20

// TokenNetworks 校验并规范化令牌允许使用的网段，返回存进数据库的写法（一行一个，空表示不限制）。
func TokenNetworks(list []string) (string, error) {
	prefixes, err := ParseNetworks(list)
	if err != nil {
		return "", err
	}
	var out []string
	for _, p := range prefixes {
		v := p.String()
		if p.Bits() == p.Addr().BitLen() {
			v = p.Addr().String()
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return strings.Join(out, "\n"), nil
}

func tokenActor(t store.AccessToken, username string) audit.Event {
	at, aid, an := userActor(t.UserID, username)
	return audit.Event{ActorType: at, ActorID: aid, ActorName: an, Via: "mcp:" + t.Name, TokenID: t.ID,
		TargetType: "token", TargetID: strconv.FormatInt(t.ID, 10), TargetName: t.Name}
}

// tokenNetworkDenied 记下令牌从允许范围以外的地址使用，并告警。rule 是哪一项限制：token（令牌自己的地址）
// 或 admin（管理员登录地址）。
func (s *Service) tokenNetworkDenied(ctx context.Context, t store.AccessToken, username, ip, rule string) {
	e := tokenActor(t, username)
	e.Action, e.IP, e.Result, e.Client, e.Category = "token_network_denied", ip, audit.ResultDenied, clipClient(RequestMeta(ctx).Client), audit.Security
	e.Params = `{"rule":"` + rule + `"}`
	s.loginEvent(ctx, e)
	if s.Alert != nil {
		s.Alert(ctx, "token-denied:"+strconv.FormatInt(t.ID, 10),
			i18n.Errorf("访问令牌 %s 从允许范围以外的地址使用，已拒绝", t.Name).Error(),
			i18n.Errorf("成员：%s；来源：%s。令牌可能已经泄露。", username, ip).Error())
	}
}

// checkTokenNetwork 记下令牌用过的网段；能执行命令或用节点终端的令牌第一次从新网段使用时告警。第一次使用不告警。
func (s *Service) checkTokenNetwork(ctx context.Context, t store.AccessToken, username, ip, client string) {
	_, network := Network(ip)
	if network == "" {
		return
	}
	var seen []string
	if t.SeenNetworks != "" {
		seen = strings.Split(t.SeenNetworks, "\n")
	}
	if slices.Contains(seen, network) {
		return
	}
	first := len(seen) == 0
	seen = append(seen, network)
	if len(seen) > maxSeenNetworks {
		seen = seen[len(seen)-maxSeenNetworks:]
	}
	_ = s.q.SetTokenSeenNetworks(ctx, t.ID, strings.Join(seen, "\n"))
	if first || !(t.AllowExec || t.AllowNodeExec) {
		return
	}
	e := tokenActor(t, username)
	e.Action, e.IP, e.Client, e.Category = "token_new_network", ip, clipClient(client), audit.Security
	s.loginEvent(ctx, e)
	if s.Alert != nil {
		s.Alert(ctx, "token-network:"+strconv.FormatInt(t.ID, 10)+":"+network,
			i18n.Errorf("访问令牌 %s 第一次从网段 %s 使用", t.Name, network).Error(),
			i18n.Errorf("成员：%s；来源：%s；客户端：%s。这个令牌能执行命令。", username, ip, client).Error())
	}
}

func clipClient(v string) string {
	r := []rune(v)
	if len(r) > 200 {
		return string(r[:200])
	}
	return v
}

package auth

import (
	"context"
	"net/netip"
	"strings"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/store"
)

// 管理员登录地址（见 docs/panel.md#管理员登录地址）：填写后管理员只能从这些地址登录和使用面板，其他角色不受影响。
// 填错了把自己锁在外面时，在面板机器上执行 nagi reset admin-networks。

// SettingAdminNetworks 是允许的地址在 settings 表里的键，一行一个；没有这一项表示不限制。
const SettingAdminNetworks = "admin_networks"

const maxAdminNetworks = 50

// ParseNetworks 解析网段列表：每一项是 IP 或 CIDR，返回规范化后的写法。空行忽略。
func ParseNetworks(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range list {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(v, "/") {
			x, err := netip.ParsePrefix(v)
			if err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("地址格式不正确：%s", v))
			}
			p = x.Masked()
		} else {
			a, err := netip.ParseAddr(v)
			if err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("地址格式不正确：%s", v))
			}
			a = a.Unmap()
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p)
	}
	if len(out) > maxAdminNetworks {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("最多填写 %d 项", maxAdminNetworks))
	}
	return out, nil
}

// prefixesContain 判断 IP 是否在任一网段里。
func prefixesContain(list []netip.Prefix, ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// AdminNetworks 返回管理员登录地址，没有限制时为空。
func (s *Service) AdminNetworks(ctx context.Context) []string {
	v, err := s.q.GetSetting(ctx, SettingAdminNetworks)
	if err != nil || strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.Split(v, "\n")
}

// adminAllowed 判断管理员能不能从这个 IP 登录和使用面板。
func (s *Service) adminAllowed(ctx context.Context, ip string) bool {
	list := s.AdminNetworks(ctx)
	if len(list) == 0 {
		return true
	}
	prefixes, err := ParseNetworks(list)
	if err != nil || len(prefixes) == 0 {
		// 设置被手动改坏了：不限制，免得所有管理员都进不来。
		return true
	}
	return prefixesContain(prefixes, ip)
}

// errAdminNetwork 是管理员从允许范围以外的地址登录或使用面板时的错误。
func errAdminNetwork() error {
	return connect.NewError(connect.CodePermissionDenied, i18n.New("管理员只能从允许的地址登录"))
}

// SetAdminNetworks 保存管理员登录地址，返回规范化后的列表。不能把当前的地址排除在外，否则保存后自己就被锁在外面了。
func (s *Service) SetAdminNetworks(ctx context.Context, list []string, currentIP string) ([]string, error) {
	prefixes, err := ParseNetworks(list)
	if err != nil {
		return nil, err
	}
	if len(prefixes) == 0 {
		return nil, s.q.DeleteSetting(ctx, SettingAdminNetworks)
	}
	if !prefixesContain(prefixes, currentIP) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.Errorf("当前地址 %s 不在列表里，保存后将无法使用面板", currentIP))
	}
	out := make([]string, 0, len(prefixes))
	seen := map[string]bool{}
	for _, p := range prefixes {
		v := p.String()
		if p.Bits() == p.Addr().BitLen() {
			v = p.Addr().String()
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out, s.q.SetSetting(ctx, store.SetSettingParams{Key: SettingAdminNetworks, Value: strings.Join(out, "\n")})
}

// adminNetworkDenied 记下管理员从允许范围以外的地址登录（密码或通行密钥已经对了），并告警：凭据很可能已经泄露。
func (s *Service) adminNetworkDenied(ctx context.Context, userID int64, username, ip, method string) {
	at, aid, an := userActor(userID, username)
	s.loginEvent(ctx, audit.Event{Action: "admin_network_denied", ActorType: at, ActorID: aid, ActorName: an, IP: ip,
		Result: audit.ResultDenied, Params: `{"method":"` + method + `"}`})
	if s.Alert != nil {
		s.Alert(ctx, "admin-network:"+username, i18n.Errorf("管理员 %s 从允许范围以外的地址登录，已拒绝", username).Error(),
			i18n.Errorf("来源：%s。登录凭据可能已经泄露。", ip).Error())
	}
}

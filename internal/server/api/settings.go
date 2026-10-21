package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SettingPublicAddress 是面板对外地址在 settings 表里的键。
const SettingPublicAddress = "public_address"

// PanelInfo 是接口需要的面板自身信息。
type PanelInfo struct {
	Q           *store.Queries
	DataDir     string
	Port        int
	Fingerprint string
	Version     string
	// OnDomainChange 在面板域名改变时调用（纯面板自己申请证书时用）。
	OnDomainChange func(domain string)
	// NewEnrollToken 生成一次性加入令牌，返回令牌和过期时间。
	NewEnrollToken func(ctx context.Context) (string, time.Time, error)
	// 主备面板（见 docs/standby.md）：本面板是主面板时 Primary 不为空，是备用面板时 Standby 不为空。
	Primary *standby.Primary
	Standby *standby.Client
	// Promote 把本面板（备用面板）提升为主面板。
	Promote func(ctx context.Context) (uint64, error)
}

// PublicAddress 返回面板对外地址（不含端口）。
func (p *PanelInfo) PublicAddress(ctx context.Context) string {
	v, err := p.Q.GetSetting(ctx, SettingPublicAddress)
	if err != nil || v == "" {
		return "127.0.0.1"
	}
	return v
}

// SettingDomain 是面板绑定的域名在 settings 表里的键。
const SettingDomain = "panel_domain"

// Domain 返回面板绑定的域名，没绑定时为空。
func (p *PanelInfo) Domain(ctx context.Context) string {
	v, _ := p.Q.GetSetting(ctx, SettingDomain)
	return v
}

// PublicHostPort 返回 "对外地址:端口"。
func (p *PanelInfo) PublicHostPort(ctx context.Context) string {
	return net.JoinHostPort(p.PublicAddress(ctx), strconv.Itoa(p.Port))
}

func (p *PanelInfo) settings(ctx context.Context) *nagiv1.PanelSettings {
	return &nagiv1.PanelSettings{
		PublicAddress: p.PublicAddress(ctx),
		Port:          uint32(p.Port),
		Fingerprint:   p.Fingerprint,
		Version:       p.Version,
		Domain:        p.Domain(ctx),
		MasterKeyPath: masterKeyPath(p.DataDir),
	}
}

var hostnameRe = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

type settingsService struct{ *API }

func (s *settingsService) GetSettings(ctx context.Context, _ *connect.Request[nagiv1.GetSettingsRequest]) (*connect.Response[nagiv1.GetSettingsResponse], error) {
	return connect.NewResponse(&nagiv1.GetSettingsResponse{Settings: s.Panel.settings(ctx)}), nil
}

func (s *settingsService) UpdateSettings(ctx context.Context, req *connect.Request[nagiv1.UpdateSettingsRequest]) (*connect.Response[nagiv1.UpdateSettingsResponse], error) {
	addr := strings.TrimSpace(req.Msg.GetPublicAddress())
	if net.ParseIP(addr) == nil && !hostnameRe.MatchString(addr) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("对外地址必须是 IP 或域名，不含端口"))
	}
	old := s.Panel.PublicAddress(ctx)
	if err := s.Q.SetSetting(ctx, store.SetSettingParams{Key: SettingPublicAddress, Value: addr}); err != nil {
		return nil, err
	}
	if addr != old {
		s.Nodes.PushAddress(s.Panel.PublicHostPort(ctx), s.Panel.Fingerprint)
		s.Log.Info("面板对外地址已更新，已通知在线节点", "address", addr)
	}
	return connect.NewResponse(&nagiv1.UpdateSettingsResponse{Settings: s.Panel.settings(ctx)}), nil
}

func (s *settingsService) SetPanelDomain(ctx context.Context, req *connect.Request[nagiv1.SetPanelDomainApiRequest]) (*connect.Response[nagiv1.UpdateSettingsResponse], error) {
	d := strings.ToLower(strings.TrimSpace(req.Msg.GetDomain()))
	if d != "" && (net.ParseIP(d) != nil || !hostnameRe.MatchString(d) || !strings.Contains(d, ".")) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("请填写域名，例如 panel.example.com"))
	}
	var err error
	if d == "" {
		err = s.Q.DeleteSetting(ctx, SettingDomain)
	} else {
		err = s.Q.SetSetting(ctx, store.SetSettingParams{Key: SettingDomain, Value: d})
	}
	if err != nil {
		return nil, err
	}
	s.Nodes.PushPanelDomain(d)
	if s.Panel.OnDomainChange != nil {
		s.Panel.OnDomainChange(d)
	}
	s.Log.Info("面板域名已更新", "domain", d)
	return connect.NewResponse(&nagiv1.UpdateSettingsResponse{Settings: s.Panel.settings(ctx)}), nil
}

// CreateEnrollCommand 生成添加节点的安装命令。
func (s *nodeService) CreateEnrollCommand(ctx context.Context, _ *connect.Request[nagiv1.CreateEnrollCommandRequest]) (*connect.Response[nagiv1.CreateEnrollCommandResponse], error) {
	token, expires, err := s.Panel.NewEnrollToken(ctx)
	if err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf(`curl -fsS --pinnedpubkey "%s" -k https://%s/install.sh | sh -s -- --token %s`,
		s.Panel.Fingerprint, s.Panel.PublicHostPort(ctx), token)
	return connect.NewResponse(&nagiv1.CreateEnrollCommandResponse{Command: cmd, ExpiresAt: timestamppb.New(expires)}), nil
}

// ---- 告警 ----

func (s *settingsService) GetAlertConfig(ctx context.Context, _ *connect.Request[nagiv1.GetAlertConfigRequest]) (*connect.Response[nagiv1.GetAlertConfigResponse], error) {
	cfg, err := s.Nodes.AlertConfig(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.GetAlertConfigResponse{Config: cfg, Lost: s.Nodes.AlertConfigLost(ctx)}), nil
}

func (s *settingsService) UpdateAlertConfig(ctx context.Context, req *connect.Request[nagiv1.UpdateAlertConfigRequest]) (*connect.Response[nagiv1.UpdateAlertConfigResponse], error) {
	cfg := req.Msg.GetConfig()
	if cfg == nil {
		cfg = &nagiv1.AlertConfig{}
	}
	ids := map[string]bool{}
	for _, ch := range cfg.GetChannels() {
		ch.Name = strings.TrimSpace(ch.GetName())
		ch.Url = strings.TrimSpace(ch.GetUrl())
		ch.TelegramToken = strings.TrimSpace(ch.GetTelegramToken())
		ch.TelegramChatId = strings.TrimSpace(ch.GetTelegramChatId())
		if err := alert.Validate(ch); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("渠道 %q：%w", ch.GetName(), err))
		}
		if ch.GetName() == "" {
			ch.Name = ch.GetType()
		}
		if ch.GetId() == "" || ids[ch.GetId()] {
			ch.Id = randomID()
		}
		ids[ch.GetId()] = true
	}
	if r := cfg.GetRules(); r != nil && (r.GetDiskPercent() > 100 || r.GetMemPercent() > 100) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("百分比不能超过 100"))
	}
	if err := s.Nodes.SetAlertConfig(ctx, cfg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.UpdateAlertConfigResponse{Config: cfg}), nil
}

func (s *settingsService) TestAlertChannel(ctx context.Context, req *connect.Request[nagiv1.TestAlertChannelRequest]) (*connect.Response[nagiv1.TestAlertChannelResponse], error) {
	ch := req.Msg.GetChannel()
	if err := alert.Validate(ch); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	a := &nagiv1.Alert{Key: "test", Status: "firing", Title: "Nagi 测试消息", Message: "告警渠道配置成功。",
		Node: "面板", Time: timestamppb.Now()}
	if err := alert.Send(ctx, ch, a); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&nagiv1.TestAlertChannelResponse{}), nil
}

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- 账号安全 ----

func (s *settingsService) security(ctx context.Context) *nagiv1.Security {
	return &nagiv1.Security{TotpEnabled: s.Auth.TOTPEnabled(ctx, auth.CurrentUser(ctx).ID), Entrance: s.Auth.Entrance(ctx)}
}

func (s *settingsService) GetSecurity(ctx context.Context, _ *connect.Request[nagiv1.GetSecurityRequest]) (*connect.Response[nagiv1.Security], error) {
	return connect.NewResponse(s.security(ctx)), nil
}

func (s *settingsService) BeginTOTP(ctx context.Context, _ *connect.Request[nagiv1.BeginTOTPRequest]) (*connect.Response[nagiv1.BeginTOTPResponse], error) {
	u := auth.CurrentUser(ctx)
	secret, err := s.Auth.BeginTOTP(u.ID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.BeginTOTPResponse{Secret: secret, Uri: auth.TOTPURI(secret, u.Name, "Nagi")}), nil
}

func (s *settingsService) EnableTOTP(ctx context.Context, req *connect.Request[nagiv1.EnableTOTPRequest]) (*connect.Response[nagiv1.Security], error) {
	u := auth.CurrentUser(ctx)
	if err := s.Auth.EnableTOTP(ctx, u.ID, auth.SessionToken(req.Header()), req.Msg.GetCode()); err != nil {
		return nil, err
	}
	s.Log.Info("已开启两步验证", "user", u.Name)
	return connect.NewResponse(s.security(ctx)), nil
}

func (s *settingsService) DisableTOTP(ctx context.Context, req *connect.Request[nagiv1.DisableTOTPRequest]) (*connect.Response[nagiv1.Security], error) {
	u := auth.CurrentUser(ctx)
	if err := s.Auth.DisableTOTP(ctx, u.ID, req.Msg.GetCode(), req.Msg.GetPassword()); err != nil {
		return nil, err
	}
	s.Log.Info("已关闭两步验证", "user", u.Name)
	return connect.NewResponse(s.security(ctx)), nil
}

func (s *settingsService) SetEntrance(ctx context.Context, req *connect.Request[nagiv1.SetEntranceRequest]) (*connect.Response[nagiv1.Security], error) {
	if _, err := s.Auth.SetEntrance(ctx, req.Msg.GetEnabled()); err != nil {
		return nil, err
	}
	return connect.NewResponse(s.security(ctx)), nil
}

// masterKeyPath 返回面板主密钥文件的绝对路径（和 server 里 sealer.Load 用的一致）。
func masterKeyPath(dataDir string) string {
	path := filepath.Join(dataDir, "pki", "master.key")
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// ---- 主备面板 ----

func (s *settingsService) CreateStandbyCode(ctx context.Context, _ *connect.Request[nagiv1.CreateStandbyCodeRequest]) (*connect.Response[nagiv1.CreateStandbyCodeResponse], error) {
	if s.Panel.Primary == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("这是备用面板，请在主面板上添加备用面板"))
	}
	addr := net.JoinHostPort(s.Panel.PublicAddress(ctx), strconv.Itoa(s.Panel.Port))
	code, expires, err := s.Panel.Primary.NewCode(ctx, addr, s.Panel.Fingerprint)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.CreateStandbyCodeResponse{Code: code, ExpiresAt: timestamppb.New(expires)}), nil
}

func (s *settingsService) GetStandbys(ctx context.Context, _ *connect.Request[nagiv1.GetStandbysRequest]) (*connect.Response[nagiv1.GetStandbysResponse], error) {
	out := &nagiv1.GetStandbysResponse{}
	if c := s.Panel.Standby; c != nil {
		st := c.Status()
		out.IsStandby, out.PrimaryAddress, out.Connected, out.LastError = true, c.Info.PrimaryAddress, st.Connected, st.LastError
		if !st.LastSync.IsZero() {
			out.LastSync = timestamppb.New(st.LastSync)
		}
		return connect.NewResponse(out), nil
	}
	rows, err := s.Q.ListStandbyPanels(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		sp := &nagiv1.StandbyPanel{Id: r.ID, Name: r.Name, Address: r.Address, CreatedAt: timestamppb.New(time.Unix(r.CreatedAt, 0))}
		if s.Panel.Primary != nil {
			st := s.Panel.Primary.StatusOf(r.ID)
			sp.Connected = st.Connected
			if !st.LastSync.IsZero() {
				sp.LastSync = timestamppb.New(st.LastSync)
			}
		}
		out.Standbys = append(out.Standbys, sp)
	}
	return connect.NewResponse(out), nil
}

func (s *settingsService) PromoteStandby(ctx context.Context, _ *connect.Request[nagiv1.PromoteStandbyRequest]) (*connect.Response[nagiv1.PromoteStandbyResponse], error) {
	if s.Panel.Standby == nil || s.Panel.Promote == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("本面板已经是主面板"))
	}
	term, err := s.Panel.Promote(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&nagiv1.PromoteStandbyResponse{Term: term}), nil
}

func (s *settingsService) RemoveStandby(ctx context.Context, req *connect.Request[nagiv1.RemoveStandbyRequest]) (*connect.Response[nagiv1.RemoveStandbyResponse], error) {
	if s.Panel.Primary == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("这是备用面板，请在主面板上操作"))
	}
	n, err := s.Q.DeleteStandbyPanel(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("备用面板不存在"))
	}
	s.Log.Info("已移除备用面板", "standby", req.Msg.GetId())
	return connect.NewResponse(&nagiv1.RemoveStandbyResponse{}), nil
}

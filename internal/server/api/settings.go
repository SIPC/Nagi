package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
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
	// NewEnrollToken 生成一次性加入令牌，返回令牌和过期时间。
	NewEnrollToken func(ctx context.Context) (string, time.Time, error)
}

// PublicAddress 返回面板对外地址（不含端口）。
func (p *PanelInfo) PublicAddress(ctx context.Context) string {
	v, err := p.Q.GetSetting(ctx, SettingPublicAddress)
	if err != nil || v == "" {
		return "127.0.0.1"
	}
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
	if err := s.Q.SetSetting(ctx, store.SetSettingParams{Key: SettingPublicAddress, Value: addr}); err != nil {
		return nil, err
	}
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

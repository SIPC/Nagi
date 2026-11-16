package api

import (
	"context"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 管理员登录地址（见 docs/panel.md#管理员登录地址）。校验和保存在 auth/adminnet.go。

func (s *settingsService) GetAdminNetworks(ctx context.Context, _ *connect.Request[nagiv1.GetAdminNetworksRequest]) (*connect.Response[nagiv1.AdminNetworks], error) {
	return connect.NewResponse(&nagiv1.AdminNetworks{Networks: s.Auth.AdminNetworks(ctx), CurrentIp: auth.RequestMeta(ctx).IP}), nil
}

func (s *settingsService) SetAdminNetworks(ctx context.Context, req *connect.Request[nagiv1.AdminNetworks]) (*connect.Response[nagiv1.AdminNetworks], error) {
	ip := auth.RequestMeta(ctx).IP
	list, err := s.Auth.SetAdminNetworks(ctx, req.Msg.GetNetworks(), ip)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.AdminNetworks{Networks: list, CurrentIp: ip}), nil
}

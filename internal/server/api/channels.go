package api

import (
	"context"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/release"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
)

// 更新渠道（见 docs/development.md#预发布版本）：面板和每个节点各有一个，正式版（默认）或预发布。
//   - 面板的存在 settings 表的 update_channel，每个面板自己一份（standby.LocalSettings），决定一键更新面板时找哪个渠道的最新版本。
//   - 节点的存在 settings 表的 node_update_channel:<节点 ID>，随备用面板同步，决定这个节点能不能升级到面板的版本：
//     节点升级的目标永远是面板正在运行的版本，面板是预发布版而节点是正式版渠道时，这个节点不升级（nodeUpgradeTarget）。
// 没有记录、记的值认不出时都按正式版处理。

// nodeChannelPrefix 是节点更新渠道在 settings 表里的键前缀。
const nodeChannelPrefix = "node_update_channel:"

func channelOrStable(v string) string {
	if v == release.ChannelPre {
		return v
	}
	return release.ChannelStable
}

// UpdateChannel 返回本面板的更新渠道。
func (p *PanelInfo) UpdateChannel(ctx context.Context) string {
	v, _ := p.Q.GetSetting(ctx, standby.SettingUpdateChannel)
	return channelOrStable(v)
}

// nodeChannel 返回节点的更新渠道。
func (a *API) nodeChannel(ctx context.Context, id string) string {
	v, _ := a.Q.GetSetting(ctx, nodeChannelPrefix+id)
	return channelOrStable(v)
}

// setChannel 存渠道：正式版就删掉记录，和没有设置一样。
func setChannel(ctx context.Context, q *store.Queries, key, channel string) error {
	if !release.IsChannel(channel) {
		return connect.NewError(connect.CodeInvalidArgument, i18n.New("更新渠道须为 stable 或 pre"))
	}
	if channel == release.ChannelStable {
		return q.DeleteSetting(ctx, key)
	}
	return q.SetSetting(ctx, store.SetSettingParams{Key: key, Value: channel})
}

// nodeUpgradeTarget 返回节点一键升级能升到的版本：面板的版本 panel，且节点的渠道允许、比节点的版本 current 新；不能升级时为空。
// 单个升级和批量升级都按它判断，正式版渠道的节点不会被升级到预发布版。
func nodeUpgradeTarget(panel, channel, current string) string {
	if !release.Allowed(channel, panel) || !release.Older(current, panel) {
		return ""
	}
	return panel
}

// SetPanelUpdateChannel 设置本面板的更新渠道。换渠道后清掉缓存的最新版本，下次检查按新渠道查。
func (s *settingsService) SetPanelUpdateChannel(ctx context.Context, req *connect.Request[nagiv1.SetPanelUpdateChannelRequest]) (*connect.Response[nagiv1.UpdateSettingsResponse], error) {
	if err := setChannel(ctx, s.Q, standby.SettingUpdateChannel, req.Msg.GetChannel()); err != nil {
		return nil, err
	}
	s.updates.forget()
	return connect.NewResponse(&nagiv1.UpdateSettingsResponse{Settings: s.Panel.settings(ctx)}), nil
}

// SetNodeUpdateChannel 设置节点的更新渠道。节点离线也能改，只存在面板上，节点不知道自己的渠道。
func (s *nodeService) SetNodeUpdateChannel(ctx context.Context, req *connect.Request[nagiv1.SetNodeUpdateChannelRequest]) (*connect.Response[nagiv1.SetNodeUpdateChannelResponse], error) {
	id := req.Msg.GetNodeId()
	if n, err := s.Q.GetNode(ctx, id); err != nil || n.Revoked != 0 {
		return nil, nodeNotFound()
	}
	if err := setChannel(ctx, s.Q, nodeChannelPrefix+id, req.Msg.GetChannel()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.SetNodeUpdateChannelResponse{}), nil
}

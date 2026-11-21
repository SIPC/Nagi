package api

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/projects"
	"github.com/SIPC/nagi/internal/server/standby"
)

// 节点一键升级的目标：面板的版本，节点的渠道允许、且比节点旧。正式版渠道的节点永远不升到预发布版，
// 面板是预发布版、节点是上一个正式版时也一样（面板 pre 管理正式版节点）。
func TestNodeUpgradeTarget(t *testing.T) {
	cases := []struct {
		panel, channel, node, want string
	}{
		{"v0.38.0", "stable", "v0.37.0", "v0.38.0"},
		{"v0.38.0", "pre", "v0.37.0", "v0.38.0"},
		{"v0.38.0-pre.2", "stable", "v0.37.0", ""},
		{"v0.38.0-pre.2", "", "v0.37.0", ""},
		{"v0.38.0-pre.2", "pre", "v0.37.0", "v0.38.0-pre.2"},
		{"v0.38.0-pre.2", "pre", "v0.38.0-pre.1", "v0.38.0-pre.2"},
		// 从预发布版升到同号正式版；同号或更新的不再升级（只升不降）。
		{"v0.38.0", "pre", "v0.38.0-pre.2", "v0.38.0"},
		{"v0.38.0", "stable", "v0.38.0-pre.2", "v0.38.0"},
		{"v0.38.0-pre.2", "pre", "v0.38.0-pre.10", ""},
		{"v0.38.0-pre.2", "pre", "v0.38.0", ""},
		{"v0.38.0", "stable", "v0.38.0", ""},
		{"dev", "pre", "v0.37.0", ""},
	}
	for _, c := range cases {
		if got := nodeUpgradeTarget(c.panel, c.channel, c.node); got != c.want {
			t.Errorf("nodeUpgradeTarget(%q, %q, %q) = %q，应为 %q", c.panel, c.channel, c.node, got, c.want)
		}
	}
}

// 面板渠道每个面板一份，不随备用面板同步；节点渠道随同步，移除节点后回到正式版。
func TestUpdateChannels(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, slog.Default(), h.a.Sealer, nil)
	h.a.Projects = &projects.Service{Q: h.a.Q, Sealer: h.a.Sealer, Log: slog.Default(), Nodes: h.a.Nodes}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO nodes (id, name, created_at, mesh_key) VALUES ('n1', 'web', 0, x'01')`); err != nil {
		t.Fatal(err)
	}
	s, ns := &settingsService{h.a}, &nodeService{h.a}

	if got := h.a.Panel.UpdateChannel(ctx); got != "stable" {
		t.Fatalf("默认渠道：%q", got)
	}
	res, err := s.SetPanelUpdateChannel(ctx, connect.NewRequest(&nagiv1.SetPanelUpdateChannelRequest{Channel: "pre"}))
	if err != nil || res.Msg.GetSettings().GetUpdateChannel() != "pre" {
		t.Fatalf("设置面板渠道：%v %v", res, err)
	}
	if _, err := s.SetPanelUpdateChannel(ctx, connect.NewRequest(&nagiv1.SetPanelUpdateChannelRequest{Channel: "beta"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("认不出的渠道应该拒绝：%v", err)
	}
	found := false
	for _, k := range standby.LocalSettings {
		found = found || k == standby.SettingUpdateChannel
	}
	if !found {
		t.Error("面板渠道应该是每个面板自己的设置，不随同步覆盖")
	}

	if _, err := ns.SetNodeUpdateChannel(ctx, connect.NewRequest(&nagiv1.SetNodeUpdateChannelRequest{NodeId: "n1", Channel: "pre"})); err != nil {
		t.Fatal(err)
	}
	if got := h.a.nodeChannel(ctx, "n1"); got != "pre" {
		t.Fatalf("节点渠道：%q", got)
	}
	for _, k := range standby.LocalSettings {
		if k == nodeChannelPrefix+"n1" {
			t.Error("节点渠道应该随备用面板同步")
		}
	}
	if _, err := ns.SetNodeUpdateChannel(ctx, connect.NewRequest(&nagiv1.SetNodeUpdateChannelRequest{NodeId: "missing", Channel: "pre"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("不存在的节点：%v", err)
	}
	if err := ns.forgetNode(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	if got := h.a.nodeChannel(ctx, "n1"); got != "stable" {
		t.Fatalf("移除后应回到正式版：%q", got)
	}
}

// 面板是预发布版时，正式版渠道的节点在接口上也升不上去，批量升级的 channel 只接受认得的值。
func TestUpgradeRejectsStableNodeToPre(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, slog.Default(), h.a.Sealer, nil)
	h.a.Panel.Version = "v0.38.0-pre.1"
	ns := &nodeService{h.a}
	if _, err := ns.UpgradeAllNodes(ctx, connect.NewRequest(&nagiv1.UpgradeAllNodesRequest{Channel: "beta"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("认不出的渠道：%v", err)
	}
	if err := h.a.runNodeUpgrade(ctx, "n1", h.a.Panel.Version); err != errUpgradeCanceled {
		t.Fatalf("没有升级记录时不应该开始：%v", err)
	}
	// 排队的记录：开始下载前按渠道再核对一次。
	h.a.withUpdates(ctx, func(r *updateRecords) { r.Nodes["n1"] = newUpdateState(h.a.Panel.Version, "v0.37.0", updateInstalling) })
	err := h.a.runNodeUpgrade(ctx, "n1", h.a.Panel.Version)
	if err == nil || !strings.Contains(err.Error(), "正式版更新渠道") {
		t.Fatalf("正式版渠道的节点不应该升级到预发布版：%v", err)
	}
}

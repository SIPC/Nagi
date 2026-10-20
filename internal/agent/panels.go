package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 信任列表的修改：由已信任的面板发指令（AgentService），或者在节点上执行本地命令（nagi panel）。
// 两条路都只改 identity/panels.json，运行中的节点在 10 秒内按新列表增减连接。

func (s *agentService) ListPanels(context.Context, *nagiv1.ListPanelsRequest) (*nagiv1.ListPanelsResponse, error) {
	a := s.node
	a.mu.Lock()
	connected := map[string]bool{}
	for fp := range a.panels {
		connected[fp] = true
	}
	a.mu.Unlock()
	out := &nagiv1.ListPanelsResponse{}
	for _, p := range a.id.Panels() {
		out.Panels = append(out.Panels, &nagiv1.TrustedPanel{
			Address: p.Address, Fingerprint: p.Fingerprint, Connected: connected[p.Fingerprint], Role: a.role(p),
		})
	}
	return out, nil
}

func (s *agentService) AddPanel(ctx context.Context, req *nagiv1.AddPanelRequest) (*nagiv1.AddPanelResponse, error) {
	a := s.node
	if req.GetAddress() == "" || req.GetFingerprint() == "" || req.GetToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "需要面板地址、公钥指纹和令牌")
	}
	if p, ok := a.id.panel(req.GetFingerprint()); ok {
		// 已经信任这个面板，只更新地址，不消耗令牌。只能改自己的地址：
		// 否则一个面板可以把另一个面板的地址改成连不上的，悄悄把它锁在外面。
		if p.Address != req.GetAddress() && req.GetFingerprint() != s.panel {
			return nil, status.Error(codes.PermissionDenied, "已经信任这个面板了，地址不一样；面板的地址只能由它自己修改")
		}
		if p.Address != req.GetAddress() {
			p.Address = req.GetAddress()
			if err := a.id.addPanel(p); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			a.reloadPanels()
		}
		return &nagiv1.AddPanelResponse{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := a.id.enroll(ctx, req.GetAddress(), req.GetFingerprint(), req.GetToken(), a.hostname); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	a.log.Info("已加入面板", "panel", req.GetAddress(), "by", s.panel)
	a.reloadPanels()
	return &nagiv1.AddPanelResponse{}, nil
}

func (s *agentService) RemovePanel(_ context.Context, req *nagiv1.RemovePanelRequest) (*nagiv1.RemovePanelResponse, error) {
	a := s.node
	removed, err := a.id.removePanel(req.GetFingerprint(), false, nil)
	if errors.Is(err, errLastPanel) {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	a.log.Info("已从信任列表移除面板", "panel", removed.Address, "by", s.panel)
	if req.GetFingerprint() != s.panel && a.alerts != nil {
		// 一个面板移除了另一个面板：被移除的面板之后就连不上这个节点了，发一条告警让管理员知道。
		by := s.panel
		if p, ok := a.id.panel(s.panel); ok {
			by = p.Address
		}
		a.alerts.Fire(context.Background(), "panel-removed:"+req.GetFingerprint(),
			fmt.Sprintf("面板 %s 把面板 %s 从本节点的信任列表里移除了", by, removed.Address), "", "")
	}
	// 移除的可能就是调用者自己：先把响应发回去，再断开连接。
	go func() {
		time.Sleep(time.Second)
		a.reloadPanels()
	}()
	return &nagiv1.RemovePanelResponse{}, nil
}

// ---- 节点本地命令 ----

// PanelInfo 是信任列表里的一个面板。
type PanelInfo struct {
	Address     string
	Fingerprint string
	NodeID      string
}

func openIdentity(dataDir string) (*identity, error) {
	dir := filepath.Join(dataDir, "identity")
	if _, err := os.Stat(dir); err != nil {
		return nil, errors.New("找不到节点数据，请检查 --data-dir（面板自带的节点在 <面板数据目录>/agent）")
	}
	return loadIdentity(dir)
}

// ListPanels 返回节点信任的面板。
func ListPanels(dataDir string) ([]PanelInfo, error) {
	id, err := openIdentity(dataDir)
	if err != nil {
		return nil, err
	}
	var out []PanelInfo
	for _, p := range id.Panels() {
		out = append(out, PanelInfo{Address: p.Address, Fingerprint: p.Fingerprint, NodeID: p.NodeID})
	}
	return out, nil
}

// HasPanels 判断节点是否信任了至少一个面板。
func HasPanels(dataDir string) bool {
	panels, err := ListPanels(dataDir)
	return err == nil && len(panels) > 0
}

// AddPanel 用令牌加入一个面板。节点已经信任它时只更新地址。
func AddPanel(ctx context.Context, dataDir, address, fingerprint, token string) error {
	id, err := loadIdentity(filepath.Join(dataDir, "identity"))
	if err != nil {
		return err
	}
	if p, ok := id.panel(fingerprint); ok {
		p.Address = address
		return id.addPanel(p)
	}
	hostname, _ := os.Hostname()
	return id.enroll(ctx, address, fingerprint, token, hostname)
}

// RemovePanel 从信任列表移除面板。which 是公钥指纹或地址。force 时允许移除最后一个面板（旧面板已经挂了时只能这样）。
// 第二个返回值是受影响的顶替（按容器的顶替改造完成前为空）。
func RemovePanel(dataDir, which string, force bool) (PanelInfo, []string, error) {
	id, err := openIdentity(dataDir)
	if err != nil {
		return PanelInfo{}, nil, err
	}
	p, err := id.removePanel(which, force, nil)
	if errors.Is(err, errLastPanel) {
		return PanelInfo{}, nil, errors.New(err.Error() + "（确实要移除请加 --force）")
	}
	return PanelInfo{Address: p.Address, Fingerprint: p.Fingerprint, NodeID: p.NodeID}, nil, err
}

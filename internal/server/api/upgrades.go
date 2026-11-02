package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/release"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 更新状态（见 docs/panel.md#更新状态）：面板和节点的更新都在后台进行，进度记在面板的数据库里。
// 刷新页面、换一个浏览器、面板重启以后都能接着看，不会把进行中的更新显示成可以再点一次。

const (
	updateQueued     = "queued"     // 一键升级时排队等前面的节点
	updateInstalling = "installing" // 正在下载、核对、替换
	updateRestarting = "restarting" // 已经替换，等它用新版本回来
	updateFailed     = "failed"

	// updateInstallTimeout 是下载和替换的时间上限。
	updateInstallTimeout = 15 * time.Minute
	// updateRestartTimeout：替换好以后等这么久还没回来就算失败。
	updateRestartTimeout = 3 * time.Minute
)

type updateState struct {
	Target  string `json:"target"`
	From    string `json:"from"`
	Phase   string `json:"phase"`
	Error   string `json:"error,omitempty"`
	Started int64  `json:"started"`
	Changed int64  `json:"changed"`
}

func (s *updateState) active() bool { return s != nil && s.Phase != updateFailed }

func (s *updateState) fail(message string, now time.Time) {
	s.Phase, s.Error, s.Changed = updateFailed, message, now.Unix()
}

// updateRecords 是一个面板记下的更新状态，存在 settings 表，不同步给其他面板。更新成功的记录直接删掉。
type updateRecords struct {
	Panel *updateState            `json:"panel,omitempty"`
	Nodes map[string]*updateState `json:"nodes,omitempty"`
}

// resume 在进程启动后第一次读取时调用：上一个进程的后台任务已经没了。
func (r *updateRecords) resume(lang i18n.Lang, version string, now time.Time) {
	if p := r.Panel; p != nil {
		switch {
		case p.Phase == updateFailed:
		case p.Target == version:
			r.Panel = nil
		case p.Phase == updateRestarting:
			p.fail(i18n.Localize(lang, i18n.Errorf("面板重启后版本还是 %s，没有更新到 %s", version, p.Target)), now)
		default:
			p.fail(i18n.Localize(lang, i18n.New("面板重启打断了更新，请重试")), now)
		}
	}
	for id, n := range r.Nodes {
		switch n.Phase {
		case updateQueued:
			delete(r.Nodes, id)
		case updateInstalling:
			n.fail(i18n.Localize(lang, i18n.New("面板重启打断了升级，请重试")), now)
		}
	}
}

// reconcile 按现在的版本更新记录。nodeVersion 返回节点上报的版本，known 判断节点还在不在。
func (r *updateRecords) reconcile(lang i18n.Lang, version string, nodeVersion func(string) string, known func(string) bool, now time.Time) {
	if p := r.Panel; p != nil {
		switch {
		case !release.Older(version, p.Target):
			r.Panel = nil
		case p.Phase == updateRestarting && now.Unix()-p.Changed > int64(updateRestartTimeout/time.Second):
			p.fail(i18n.Localize(lang, i18n.Errorf("面板已替换为 %s，但 3 分钟内没有恢复", p.Target)), now)
		}
	}
	for id, n := range r.Nodes {
		switch {
		case !known(id):
			delete(r.Nodes, id)
		case nodeVersion(id) != "" && !release.Older(nodeVersion(id), n.Target):
			delete(r.Nodes, id)
		case n.Phase == updateRestarting && now.Unix()-n.Changed > int64(updateRestartTimeout/time.Second):
			n.fail(i18n.Localize(lang, i18n.Errorf("节点已替换为 %s，但 3 分钟内没有恢复", n.Target)), now)
		}
	}
}

// withUpdates 在锁里读写更新状态，有变化时存回去。
func (a *API) withUpdates(ctx context.Context, fn func(*updateRecords)) {
	u := &a.updates
	u.stateMu.Lock()
	defer u.stateMu.Unlock()
	ctx = context.WithoutCancel(ctx)
	if !u.loaded {
		u.loaded = true
		if raw, err := a.Q.GetSetting(ctx, standby.SettingUpdateState); err == nil {
			u.saved = raw
			if err := json.Unmarshal([]byte(raw), &u.state); err != nil {
				a.Log.Warn("读取更新状态失败", "err", err)
				u.state = updateRecords{}
			}
			u.state.resume(i18n.FromContext(ctx), a.Panel.Version, time.Now())
		}
	}
	if u.state.Nodes == nil {
		u.state.Nodes = map[string]*updateState{}
	}
	fn(&u.state)
	raw, err := json.Marshal(u.state)
	if err != nil || string(raw) == u.saved {
		return
	}
	if err := a.Q.SetSetting(ctx, store.SetSettingParams{Key: standby.SettingUpdateState, Value: string(raw)}); err != nil {
		a.Log.Warn("保存更新状态失败", "err", err)
		return
	}
	u.saved = string(raw)
}

func (a *API) nodeVersion(id string) string {
	conn, err := a.Nodes.Peek(id)
	if err != nil {
		return ""
	}
	return conn.LastReport().GetNode().GetVersion()
}

// reconcileUpdates 用节点现在的版本刷新状态。
func (a *API) reconcileUpdates(ctx context.Context) {
	rows, err := a.Q.ListNodes(ctx)
	if err != nil {
		a.Log.Warn("读取节点失败", "err", err)
		return
	}
	known := map[string]bool{}
	for _, row := range rows {
		known[row.ID] = true
	}
	a.withUpdates(ctx, func(r *updateRecords) {
		r.reconcile(i18n.FromContext(ctx), a.Panel.Version, a.nodeVersion, func(id string) bool { return known[id] }, time.Now())
	})
}

func updateStateProto(s *updateState) *nagiv1.UpdateState {
	return &nagiv1.UpdateState{Target: s.Target, From: s.From, Phase: s.Phase, Error: s.Error, StartedAt: timestamppb.New(time.Unix(s.Started, 0))}
}

func (s *settingsService) GetUpdateStatus(ctx context.Context, _ *connect.Request[nagiv1.GetUpdateStatusRequest]) (*connect.Response[nagiv1.GetUpdateStatusResponse], error) {
	s.reconcileUpdates(ctx)
	out := &nagiv1.GetUpdateStatusResponse{Version: s.Panel.Version, Nodes: map[string]*nagiv1.UpdateState{}}
	s.withUpdates(ctx, func(r *updateRecords) {
		if r.Panel != nil {
			out.Panel = updateStateProto(r.Panel)
		}
		for id, n := range r.Nodes {
			out.Nodes[id] = updateStateProto(n)
		}
	})
	return connect.NewResponse(out), nil
}

// ---- 面板 ----

// runPanelUpdate 在后台下载并替换面板，成功后原地重启。
func (a *API) runPanelUpdate(ctx context.Context, latest string) {
	fail := func(err error) {
		a.updates.running.Store(false)
		message := i18n.Localize(i18n.FromContext(ctx), err)
		a.withUpdates(ctx, func(r *updateRecords) {
			if r.Panel != nil {
				r.Panel.fail(message, time.Now())
			}
		})
		a.recordOperation(ctx, nagiv1connect.SettingsServiceUpdatePanelProcedure, &nagiv1.UpdatePanelRequest{}, message)
	}
	installCtx, cancel := context.WithTimeout(ctx, updateInstallTimeout)
	defer cancel()
	exe, err := release.Install(installCtx, release.BaseFor(a.Panel.DataDir, latest), latest)
	if err != nil {
		fail(i18n.Errorf("更新失败：%w", err))
		return
	}
	a.withUpdates(ctx, func(r *updateRecords) {
		if r.Panel != nil {
			r.Panel.Phase, r.Panel.Changed = updateRestarting, time.Now().Unix()
		}
	})
	a.Log.Info("面板已替换为新版本，重启", "from", a.Panel.Version, "to", latest)
	time.Sleep(time.Second) // 让界面看到状态
	if err := release.Reexec(exe); err != nil {
		a.Log.Error("重启失败，请手动重启面板", "err", err)
		fail(i18n.New("重启失败，请手动重启面板"))
	}
}

// ---- 节点 ----

// upgradableNode 判断节点能不能一键升级，能时返回 nil。
func (a *API) upgradableNode(id string) error {
	conn, err := a.Nodes.Get(id)
	if err != nil {
		return err
	}
	info := conn.LastReport().GetNode()
	if info.GetOs() != "linux" {
		return i18n.New("只能升级 Linux 节点")
	}
	if info.GetEmbedded() {
		return i18n.New("面板自带的节点随面板一起升级，不能单独升级")
	}
	return nil
}

// runNodeUpgrade 让节点下载并替换新版本，返回后节点正在重启。失败时记下原因。
func (a *API) runNodeUpgrade(ctx context.Context, id, target string) error {
	err := func() error {
		if err := a.upgradableNode(id); err != nil {
			return err
		}
		conn, err := a.Nodes.Get(id)
		if err != nil {
			return err
		}
		installCtx, cancel := context.WithTimeout(ctx, updateInstallTimeout)
		defer cancel()
		_, err = conn.Agent.Upgrade(installCtx, &nagiv1.UpgradeRequest{Version: target})
		if err != nil {
			return fromGRPC(err)
		}
		return nil
	}()
	now := time.Now()
	message := ""
	if err != nil {
		message = i18n.Localize(i18n.FromContext(ctx), err)
		var ce *connect.Error
		if errors.As(err, &ce) {
			message = ce.Message() // 不要 unavailable: 这样的前缀。
		}
		a.recordOperation(ctx, nagiv1connect.NodeServiceUpgradeNodeProcedure, &nagiv1.UpgradeNodeRequest{NodeId: id}, message)
	}
	a.withUpdates(ctx, func(r *updateRecords) {
		n := r.Nodes[id]
		if n == nil {
			return
		}
		if err != nil {
			n.fail(message, now)
		} else {
			n.Phase, n.Changed = updateRestarting, now.Unix()
		}
	})
	if err == nil {
		a.Log.Info("节点正在升级", "node", id, "to", target)
	}
	return err
}

// waitNodeUpgrade 等节点用新版本回来。回来了返回 true；失败或超时返回 false。
func (a *API) waitNodeUpgrade(ctx context.Context, id string) bool {
	for {
		a.reconcileUpdates(ctx)
		var state *updateState
		a.withUpdates(ctx, func(r *updateRecords) { state = r.Nodes[id] })
		if !state.active() {
			return state == nil
		}
		time.Sleep(2 * time.Second)
	}
}

func (s *nodeService) UpgradeNode(ctx context.Context, req *connect.Request[nagiv1.UpgradeNodeRequest]) (*connect.Response[nagiv1.UpgradeResponse], error) {
	id := req.Msg.GetNodeId()
	if err := s.upgradableNode(id); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	// 节点从 GitHub Release（或安装时指定的镜像）下载和面板同一个版本，面板不提供二进制。
	target := s.Panel.Version
	if !release.IsVersion(target) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("面板不是正式发布的版本，不能一键升级节点"))
	}
	busy := false
	s.withUpdates(ctx, func(r *updateRecords) {
		if busy = r.Nodes[id].active(); !busy {
			r.Nodes[id] = newUpdateState(target, s.nodeVersion(id), updateInstalling)
		}
	})
	if busy {
		return nil, connect.NewError(connect.CodeAborted, i18n.New("节点正在升级，请稍候"))
	}
	go func() { _ = s.runNodeUpgrade(context.WithoutCancel(ctx), id, target) }()
	return connect.NewResponse(&nagiv1.UpgradeResponse{Version: target}), nil
}

// UpgradeAllNodes 把所有落后于面板的节点升级到面板的版本。一个一个来：同时重启可能让顶替的租约续不上，
// 先升级热备和备份节点（见 docs/control-plane.md）。某个节点失败就停下，后面的不动。
func (s *nodeService) UpgradeAllNodes(ctx context.Context, _ *connect.Request[nagiv1.UpgradeAllNodesRequest]) (*connect.Response[nagiv1.UpgradeAllNodesResponse], error) {
	target := s.Panel.Version
	if !release.IsVersion(target) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("面板不是正式发布的版本，不能一键升级节点"))
	}
	rows, err := s.Q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, row := range rows {
		if s.upgradableNode(row.ID) == nil && release.Older(s.nodeVersion(row.ID), target) {
			ids = append(ids, row.ID)
		}
	}
	if len(ids) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("没有可以升级的节点"))
	}
	ids, err = s.Projects.UpgradeOrder(ctx, ids)
	if err != nil {
		return nil, err
	}
	busy := false
	s.withUpdates(ctx, func(r *updateRecords) {
		for _, n := range r.Nodes {
			busy = busy || n.active()
		}
		if busy {
			return
		}
		for _, id := range ids {
			r.Nodes[id] = newUpdateState(target, s.nodeVersion(id), updateQueued)
		}
	})
	if busy {
		return nil, connect.NewError(connect.CodeAborted, i18n.New("有节点正在升级，请稍候"))
	}
	go s.runNodeUpgrades(context.WithoutCancel(ctx), ids, target)
	return connect.NewResponse(&nagiv1.UpgradeAllNodesResponse{NodeIds: ids, Version: target}), nil
}

func (a *API) runNodeUpgrades(ctx context.Context, ids []string, target string) {
	for i, id := range ids {
		a.withUpdates(ctx, func(r *updateRecords) {
			if n := r.Nodes[id]; n != nil {
				n.Phase, n.Changed = updateInstalling, time.Now().Unix()
			}
		})
		if a.runNodeUpgrade(ctx, id, target) != nil || !a.waitNodeUpgrade(ctx, id) {
			a.withUpdates(ctx, func(r *updateRecords) {
				for _, rest := range ids[i+1:] {
					delete(r.Nodes, rest)
				}
			})
			return
		}
	}
}

func newUpdateState(target, from, phase string) *updateState {
	now := time.Now().Unix()
	return &updateState{Target: target, From: from, Phase: phase, Started: now, Changed: now}
}

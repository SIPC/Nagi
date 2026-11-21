package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/release"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 更新状态（见 docs/panel.md#更新状态）：面板和节点的更新都在后台进行，进度记在面板的数据库里。
// 刷新页面、换一个浏览器、面板重启以后都能接着看，不会把进行中的更新显示成可以再点一次。

const (
	updateQueued     = "queued"     // 一键升级时排队等前面的节点
	updateInstalling = "installing" // 正在下载、核对、替换
	updateRestarting = "restarting" // 已经替换，等它用新版本回来
	updateFailed     = "failed"

	// updateInstallTimeout 是下载和替换的时间上限，和 release.Download 的上限一致。
	updateInstallTimeout = 10 * time.Minute
	// updateRestartTimeout：替换好以后等这么久还没回来就算失败。
	updateRestartTimeout = 3 * time.Minute
)

type updateState struct {
	Target  string `json:"target"`
	From    string `json:"from"`
	Phase   string `json:"phase"`
	Error   string `json:"error,omitempty"` // 中文；显示时按请求的语言翻译（见 updateStateProto）
	Started int64  `json:"started"`
	Changed int64  `json:"changed"`
	Source  string `json:"source,omitempty"` // 下载用上的地址（主机名），替换后才有
}

func (s *updateState) active() bool { return s != nil && s.Phase != updateFailed }

// fail 记下失败的原因。存的是中文，读取时按请求的语言翻译：看的人不一定和触发的人用同一种语言。
func (s *updateState) fail(err error, now time.Time) {
	s.Phase, s.Error, s.Changed = updateFailed, errorText(i18n.Zh, err), now.Unix()
}

// updateRecords 是一个面板记下的更新状态，存在 settings 表，不同步给其他面板。更新成功的记录直接删掉。
type updateRecords struct {
	Panel *updateState            `json:"panel,omitempty"`
	Nodes map[string]*updateState `json:"nodes,omitempty"`
}

// resume 在进程启动后第一次读取时调用：上一个进程的后台任务已经没了。
func (r *updateRecords) resume(version string, now time.Time) {
	if p := r.Panel; p != nil {
		switch {
		case p.Phase == updateFailed:
		case p.Target == version:
			r.Panel = nil
		case p.Phase == updateRestarting:
			p.fail(i18n.Errorf("面板重启后版本仍是 %s，未能更新到 %s", version, p.Target), now)
		default:
			p.fail(i18n.New("面板重启中断了更新，请重试"), now)
		}
	}
	for id, n := range r.Nodes {
		switch n.Phase {
		case updateQueued:
			delete(r.Nodes, id)
		case updateInstalling:
			n.fail(i18n.New("面板重启中断了升级，请重试"), now)
		}
	}
}

// reconcile 按现在的版本更新记录。nodeVersion 返回节点上报的版本，nodeStarted 返回节点进程启动的时间（旧版本不报，为 0），
// known 判断节点还在不在。
func (r *updateRecords) reconcile(version string, nodeVersion func(string) string, nodeStarted func(string) int64, known func(string) bool, now time.Time) {
	if p := r.Panel; p != nil {
		switch {
		case !release.Older(version, p.Target):
			r.Panel = nil
		case p.Phase == updateRestarting && now.Unix()-p.Changed > int64(updateRestartTimeout/time.Second):
			p.fail(i18n.Errorf("面板已替换为 %s，但 3 分钟内没有恢复，请查看面板日志", p.Target), now)
		}
	}
	for id, n := range r.Nodes {
		switch {
		case !known(id):
			delete(r.Nodes, id)
		case nodeVersion(id) != "" && !release.Older(nodeVersion(id), n.Target):
			delete(r.Nodes, id)
		case n.Phase == updateRestarting && nodeVersion(id) != "" && nodeStarted(id) >= n.Changed:
			// 替换以后节点又起来了，版本却还是旧的：新版本没能运行，节点自动换回去了。
			n.fail(i18n.Errorf("新版本没能在节点上正常运行，节点已自动回到 %s，请在节点上查看日志", nodeVersion(id)), now)
		case n.Phase == updateRestarting && now.Unix()-n.Changed > int64(updateRestartTimeout/time.Second):
			n.fail(i18n.Errorf("节点已替换为 %s，但 3 分钟内没有恢复，请在节点上查看日志", n.Target), now)
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
			u.state.resume(a.Panel.Version, time.Now())
		}
	}
	if u.state.Nodes == nil {
		u.state.Nodes = map[string]*updateState{}
	}
	if u.cancels == nil {
		u.cancels = map[*updateState]context.CancelFunc{}
	}
	fn(&u.state)
	a.upgradeAlerts(ctx, &u.state)
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

func (a *API) nodeStarted(id string) int64 {
	conn, err := a.Nodes.Peek(id)
	if err != nil {
		return 0
	}
	return conn.LastReport().GetNode().GetStartedAt()
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
		r.reconcile(a.Panel.Version, a.nodeVersion, a.nodeStarted, func(id string) bool { return known[id] }, time.Now())
	})
}

func updateStateProto(lang i18n.Lang, s *updateState) *nagiv1.UpdateState {
	return &nagiv1.UpdateState{Target: s.Target, From: s.From, Phase: s.Phase, Error: i18n.Text(lang, s.Error), StartedAt: timestamppb.New(time.Unix(s.Started, 0)), Source: s.Source}
}

func (s *settingsService) GetUpdateStatus(ctx context.Context, _ *connect.Request[nagiv1.GetUpdateStatusRequest]) (*connect.Response[nagiv1.GetUpdateStatusResponse], error) {
	s.reconcileUpdates(ctx)
	out := &nagiv1.GetUpdateStatusResponse{Version: s.Panel.Version, Nodes: map[string]*nagiv1.UpdateState{}}
	s.withUpdates(ctx, func(r *updateRecords) {
		if r.Panel != nil {
			out.Panel = updateStateProto(i18n.FromContext(ctx), r.Panel)
			s.updates.fillProgress(r.Panel, out.Panel)
		}
		for id, n := range r.Nodes {
			out.Nodes[id] = updateStateProto(i18n.FromContext(ctx), n)
			s.updates.fillProgress(n, out.Nodes[id])
		}
	})
	return connect.NewResponse(out), nil
}

// ---- 面板 ----

// runPanelUpdate 在后台下载并替换面板，成功后原地重启。
func (a *API) runPanelUpdate(ctx context.Context, latest string) {
	finish := func(string, error) {}
	fail := func(err error) {
		finish("failed", err)
		a.updates.running.Store(false)
		a.withUpdates(ctx, func(r *updateRecords) {
			if r.Panel != nil {
				r.Panel.fail(err, time.Now())
			}
		})
		a.recordOperation(ctx, nagiv1connect.SettingsServiceUpdatePanelProcedure, &nagiv1.UpdatePanelRequest{}, err.Error())
	}
	installCtx, cancel := context.WithTimeout(ctx, updateInstallTimeout)
	defer cancel()
	var mine *updateState
	a.withUpdates(ctx, func(r *updateRecords) { mine = r.Panel })
	if mine != nil {
		installCtx = release.WithProgress(installCtx, func(p release.Progress) { a.updates.setProgress(mine, p, time.Now()) })
		defer a.updates.clearProgress(mine)
	}
	installCtx, finish = release.Journal(installCtx, a.Logs, time.Duration(a.LogConfig.UpgradeProgressSeconds)*time.Second, a.Panel.Version, latest)
	exe, src, err := release.Install(installCtx, a.Panel.DataDir, latest, a.Log)
	if errors.Is(err, context.DeadlineExceeded) {
		err = i18n.New("10 分钟内没有完成下载和校验，已停止更新")
	}
	if err != nil {
		fail(err)
		return
	}
	a.withUpdates(ctx, func(r *updateRecords) {
		if r.Panel != nil {
			r.Panel.Phase, r.Panel.Changed, r.Panel.Source = updateRestarting, time.Now().Unix(), src.Name()
		}
	})
	a.Log.Info("面板已替换为新版本，重启", "from", a.Panel.Version, "to", latest, "source", src.Name())
	finish("restarting", nil)
	time.Sleep(time.Second) // 让界面看到状态
	if err := release.Reexec(exe); err != nil {
		a.Log.Error("重启失败，请手动重启面板", "err", err)
		fail(i18n.New("面板未能自动重启，请手动重启"))
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

// errUpgradeCanceled：升级被取消了（CancelNodeUpgrade 已经删掉记录）。
var errUpgradeCanceled = errors.New("upgrade canceled")

// runNodeUpgrade 让节点下载并替换新版本，返回后节点正在重启。失败时记下原因；被取消时返回 errUpgradeCanceled。
// 返回错误时节点还是原来的版本。
func (a *API) runNodeUpgrade(ctx context.Context, id, target string) error {
	installCtx, cancel := context.WithTimeout(ctx, updateInstallTimeout)
	defer cancel()
	// 记录换过（取消后又点了升级）时不动新的记录。
	var mine *updateState
	a.withUpdates(ctx, func(r *updateRecords) {
		if mine = r.Nodes[id]; mine != nil {
			a.updates.cancels[mine] = cancel
		}
	})
	if mine == nil {
		return errUpgradeCanceled
	}
	var source string
	err := func() error {
		// 排队期间渠道可能被改回正式版：开始下载前再核对一次，正式版渠道的节点不会被升级到预发布版。
		if !release.Allowed(a.nodeChannel(ctx, id), target) {
			return i18n.Errorf("节点 %s 使用正式版更新渠道，不能升级到预发布版 %s", a.nodeRef(ctx, id), target)
		}
		if err := a.upgradableNode(id); err != nil {
			return err
		}
		conn, err := a.Nodes.Get(id)
		if err != nil {
			return err
		}
		// 等 Upgrade 返回期间每秒问一次进度（见 upgrade_progress.go）。
		pollCtx, stopPoll := context.WithCancel(installCtx)
		polled := make(chan struct{})
		go func() {
			defer close(polled)
			a.pollUpgradeProgress(pollCtx, conn.Agent, mine)
		}()
		defer func() {
			stopPoll()
			<-polled
			a.updates.clearProgress(mine)
		}()
		resp, err := conn.Agent.Upgrade(installCtx, &nagiv1.UpgradeRequest{Version: target})
		source = nodeSource(resp.GetSource()) // 来自节点，存进 settings 表，显示给界面和 MCP
		switch {
		case err == nil:
			return nil
		case errors.Is(installCtx.Err(), context.Canceled) || status.Code(err) == codes.Canceled:
			return errUpgradeCanceled
		case errors.Is(installCtx.Err(), context.DeadlineExceeded):
			return i18n.New("10 分钟内没有完成下载和校验，已停止升级")
		}
		return fromGRPC(err)
	}()
	now := time.Now()
	if err != nil && !errors.Is(err, errUpgradeCanceled) {
		a.recordOperation(ctx, nagiv1connect.NodeServiceUpgradeNodeProcedure, &nagiv1.UpgradeNodeRequest{NodeId: id}, errorText(i18n.Zh, err))
	}
	a.withUpdates(ctx, func(r *updateRecords) {
		delete(a.updates.cancels, mine)
		if r.Nodes[id] != mine {
			return
		}
		switch {
		case err == nil:
			mine.Phase, mine.Changed, mine.Source = updateRestarting, now.Unix(), source
		case errors.Is(err, errUpgradeCanceled):
			delete(r.Nodes, id)
		default:
			mine.fail(err, now)
		}
	})
	if err == nil {
		a.Log.Info("节点正在升级", "node", id, "to", target, "source", source)
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
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("面板不是正式发布的版本，无法一键升级节点"))
	}
	// 渠道不允许（正式版渠道的节点、面板是预发布版）或者节点已经不旧了都不升级（见 channels.go）。
	if !release.Allowed(s.nodeChannel(ctx, id), target) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.Errorf("节点 %s 使用正式版更新渠道，不能升级到预发布版 %s", s.nodeRef(ctx, id), target))
	}
	// 只升不降：预发布渠道的节点可能比面板新（面板退回了正式版）。节点是本地编译的版本时照旧可以升级。
	if v := s.nodeVersion(id); release.IsVersion(v) && !release.Older(v, target) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.Errorf("节点 %s 已是 %s，不比面板的版本旧", s.nodeRef(ctx, id), v))
	}
	busy := false
	s.withUpdates(ctx, func(r *updateRecords) {
		if busy = r.Nodes[id].active(); !busy {
			r.Nodes[id] = newUpdateState(target, s.nodeVersion(id), updateInstalling)
		}
	})
	if busy {
		return nil, connect.NewError(connect.CodeAborted, i18n.New("节点正在升级，请在完成后重试"))
	}
	go func() { _ = s.runNodeUpgrade(context.WithoutCancel(ctx), id, target) }()
	return connect.NewResponse(&nagiv1.UpgradeResponse{Version: target}), nil
}

// UpgradeAllNodes 把所有落后于面板的节点升级到面板的版本。一个一个来：同时重启可能让顶替的租约续不上，
// 先升级热备和备份节点（见 docs/control-plane.md）。下载阶段失败（包括超时）时节点还是原来的版本，记下原因接着升下一个；
// 替换后没有用新版本回来就停下，后面的不动：可能是新版本在这些机器上有问题。
// 每个节点都按自己的更新渠道判断（nodeUpgradeTarget）：正式版渠道的节点不会被升级到预发布版，请求里的 channel 只能再缩小范围。
func (s *nodeService) UpgradeAllNodes(ctx context.Context, req *connect.Request[nagiv1.UpgradeAllNodesRequest]) (*connect.Response[nagiv1.UpgradeAllNodesResponse], error) {
	target := s.Panel.Version
	if !release.IsVersion(target) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("面板不是正式发布的版本，无法一键升级节点"))
	}
	only := req.Msg.GetChannel()
	if only != "" && !release.IsChannel(only) {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("更新渠道须为 stable 或 pre"))
	}
	rows, err := s.Q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, row := range rows {
		channel := s.nodeChannel(ctx, row.ID)
		if (only == "" || only == channel) && s.upgradableNode(row.ID) == nil && nodeUpgradeTarget(target, channel, s.nodeVersion(row.ID)) != "" {
			ids = append(ids, row.ID)
		}
	}
	if len(ids) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("没有需要升级的节点"))
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
		return nil, connect.NewError(connect.CodeAborted, i18n.New("有节点正在升级，请在完成后重试"))
	}
	go s.runNodeUpgrades(context.WithoutCancel(ctx), ids, target)
	return connect.NewResponse(&nagiv1.UpgradeAllNodesResponse{NodeIds: ids, Version: target}), nil
}

func (a *API) runNodeUpgrades(ctx context.Context, ids []string, target string) {
	for i, id := range ids {
		queued := false
		a.withUpdates(ctx, func(r *updateRecords) {
			if n := r.Nodes[id]; n != nil && n.Phase == updateQueued {
				n.Phase, n.Changed, queued = updateInstalling, time.Now().Unix(), true
			}
		})
		if !queued {
			continue // 取消了
		}
		if a.runNodeUpgrade(ctx, id, target) != nil {
			continue
		}
		if !a.waitNodeUpgrade(ctx, id) {
			a.withUpdates(ctx, func(r *updateRecords) {
				for _, rest := range ids[i+1:] {
					if n := r.Nodes[rest]; n != nil && n.Phase == updateQueued {
						delete(r.Nodes, rest)
					}
				}
			})
			return
		}
	}
}

// CancelNodeUpgrade 取消一个节点或者全部的升级：排队的删掉记录；下载中的让节点停止下载（CancelUpgrade），
// 节点还是原来的版本。已经替换、正在重启的不能取消。v0.20.0 以前的节点不支持停止下载，只能等它完成或者超时。
func (s *nodeService) CancelNodeUpgrade(ctx context.Context, req *connect.Request[nagiv1.CancelNodeUpgradeRequest]) (*connect.Response[nagiv1.CancelNodeUpgradeResponse], error) {
	id := req.Msg.GetNodeId()
	type install struct {
		id  string
		rec *updateState
	}
	var installing []install
	var err error
	canceled := 0
	s.withUpdates(ctx, func(r *updateRecords) {
		ids := []string{id}
		if id == "" {
			ids = slices.Collect(maps.Keys(r.Nodes))
		}
		for _, nid := range ids {
			switch n := r.Nodes[nid]; {
			case n != nil && n.Phase == updateQueued:
				delete(r.Nodes, nid)
				canceled++
			case n != nil && n.Phase == updateInstalling:
				installing = append(installing, install{nid, n})
			case id == "":
			case n != nil && n.Phase == updateRestarting:
				err = i18n.New("节点已替换为新版本，正在重启，无法取消")
			default:
				err = i18n.New("节点没有在升级")
			}
		}
	})
	// 问节点的时候不拿着锁：节点可能要等一会儿才回答。
	for _, in := range installing {
		if e := s.cancelNodeInstall(ctx, in.id); e != nil {
			err = e
			continue
		}
		s.withUpdates(ctx, func(r *updateRecords) {
			if r.Nodes[in.id] == in.rec {
				if c := s.updates.cancels[in.rec]; c != nil {
					c()
				}
				delete(r.Nodes, in.id)
			}
		})
		canceled++
	}
	if err == nil && canceled == 0 {
		err = i18n.New("没有可以取消的升级")
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&nagiv1.CancelNodeUpgradeResponse{}), nil
}

// cancelNodeInstall 让节点停止正在下载、校验的升级。节点说没有在升级时也算成功：请求还没到，或者已经失败了。
func (a *API) cancelNodeInstall(ctx context.Context, id string) error {
	conn, err := a.Nodes.Get(id)
	if err != nil {
		return i18n.Errorf("节点 %s 离线，无法取消升级", a.nodeRef(ctx, id))
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = conn.Agent.CancelUpgrade(cctx, &nagiv1.CancelUpgradeRequest{})
	switch status.Code(err) {
	case codes.OK, codes.NotFound:
		return nil
	case codes.Unimplemented:
		return i18n.Errorf("节点 %s 版本过低，无法取消正在进行的下载，请等待完成或超时", a.nodeRef(ctx, id))
	case codes.FailedPrecondition:
		return i18n.Errorf("节点 %s 已替换为新版本，正在重启，无法取消", a.nodeRef(ctx, id))
	}
	return i18n.Detail(i18n.Errorf("无法连接节点 %s，未能取消升级，请稍后重试", a.nodeRef(ctx, id)), err)
}

func newUpdateState(target, from, phase string) *updateState {
	now := time.Now().Unix()
	return &updateState{Target: target, From: from, Phase: phase, Started: now, Changed: now}
}

// upgradeAlerts 按更新记录发出「节点升级失败或回退」告警（见 docs/monitoring.md#告警）：节点的记录变成失败时告警
// （包括节点自动换回旧版本），记录被删掉（升级成功、节点移除）时恢复或结束。只在主面板上。调用方持有 stateMu。
func (a *API) upgradeAlerts(ctx context.Context, r *updateRecords) {
	if a.Nodes == nil || a.Panel == nil || a.Panel.Standby != nil {
		return
	}
	now := time.Now()
	seen := map[string]bool{}
	for id, n := range r.Nodes {
		key := "upgrade:" + id
		seen[key] = true
		if n.Phase != updateFailed {
			continue
		}
		go a.Nodes.EvaluatePanelRule(context.WithoutCancel(ctx), nodes.PanelObservation{Key: key, Rule: alert.RuleUpgrade, NodeID: id, Node: a.nodeName(ctx, id),
			Subject: n.Target, Bad: true, Message: n.Error}, now)
	}
	for _, key := range a.Nodes.PanelRuleKeys("upgrade:") {
		if !seen[key] {
			id := strings.TrimPrefix(key, "upgrade:")
			go a.Nodes.EvaluatePanelRule(context.WithoutCancel(ctx), nodes.PanelObservation{Key: key, Rule: alert.RuleUpgrade, NodeID: id, Node: a.nodeName(ctx, id), Good: true}, now)
		}
	}
}

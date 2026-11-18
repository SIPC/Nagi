package api

import (
	"context"
	"encoding/hex"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 首页（见 docs/android.md#首页）：需要处理、关键数字、对外流量和流量最大的映射，按成员的访问范围算。
// 网页的全局概览还是自己在浏览器里算（pages/overview.tsx），算法相同：对外 = 网卡 − 节点网络（不小于 0）。

const (
	// overviewMerge：变化通知 1 秒内的多次合并成一次。
	overviewMerge = time.Second
	// overviewHeartbeat：这么久没有变化时发一次心跳。
	overviewHeartbeat = 30 * time.Second
	// trafficCache：流量要问每个节点，结果缓存这么久，几条流共用。
	trafficCache = 10 * time.Second
	// topRoutes 是首页列出的端口映射数。
	topRoutes = 5
)

// ---- 需要处理 ----

// 严重程度，排序用。
var severityRank = map[string]int{"critical": 0, "warning": 1, "info": 2}

// criticalAlert 判断告警算不算严重：节点失联、顶替、安全（见 docs/android.md#通知）。
func criticalAlert(key string) bool {
	for _, p := range []string{"offline", "lease", "failover", "security"} {
		if key == p || strings.HasPrefix(key, p+":") {
			return true
		}
	}
	return false
}

// attention 算需要处理的事。projects、rows 已按访问范围过滤。
func (a *API) attention(ctx context.Context, u auth.User, x *access, projects []*nagiv1.ProjectSummary, rows []store.Node) []*nagiv1.AttentionItem {
	var items []*nagiv1.AttentionItem
	nodeName := map[string]string{}
	offline := map[string]bool{}
	for _, n := range rows {
		nodeName[n.ID] = n.Name
		if n.Pending != 0 {
			it := &nagiv1.AttentionItem{Kind: "node_pending", Severity: "warning", NodeId: n.ID,
				Title: i18n.T(ctx, "节点 %s 等待批准", n.Name), Since: timestamppb.New(time.Unix(n.CreatedAt, 0))}
			if u.Role == auth.RoleAdmin {
				it.Action = "approve"
			}
			items = append(items, it)
			continue
		}
		if !a.Nodes.Online(n.ID) {
			offline[n.ID] = true
			it := &nagiv1.AttentionItem{Kind: "node_offline", Severity: "critical", NodeId: n.ID, Title: i18n.T(ctx, "节点 %s 离线", n.Name)}
			if n.LastSeen > 0 {
				it.Since = timestamppb.New(time.Unix(n.LastSeen, 0))
			}
			items = append(items, it)
		}
	}
	abnormal := map[string]bool{}
	for _, s := range projects {
		p := s.GetProject()
		for _, c := range p.GetContainers() {
			if offline[c.GetNodeId()] || c.GetDeployedVersion() == 0 {
				continue
			}
			var svcs []*nagiv1.ServiceStatus
			for _, v := range s.GetStatus().GetServices() {
				if v.GetContainer() == c.GetName() {
					svcs = append(svcs, v)
				}
			}
			title := ""
			for _, v := range svcs {
				switch {
				case v.GetHealth() == "unhealthy":
					title = i18n.T(ctx, "%s / %s 健康检查失败", p.GetName(), c.GetName())
				case v.GetState() == "dead" && title == "":
					title = i18n.T(ctx, "%s / %s 异常", p.GetName(), c.GetName())
				}
			}
			if title != "" {
				abnormal[p.GetId()+"/"+c.GetName()] = true
				items = append(items, &nagiv1.AttentionItem{Kind: "container", Severity: "critical", Title: title, ProjectId: p.GetId(),
					Container: c.GetName(), NodeId: c.GetNodeId(), Detail: nodeName[c.GetNodeId()]})
			}
		}
		updated := map[string]bool{}
		for _, up := range s.GetStatus().GetImageUpdates() {
			c := up.GetContainer()
			if updated[c] {
				continue
			}
			updated[c] = true
			it := &nagiv1.AttentionItem{Kind: "image_update", Severity: "info", Title: i18n.T(ctx, "%s / %s 镜像可更新", p.GetName(), c),
				Detail: up.GetImage(), ProjectId: p.GetId(), Container: c}
			if u.Role != auth.RoleViewer {
				it.Action = "update"
			}
			items = append(items, it)
		}
	}
	for _, al := range a.Nodes.ActiveAlerts() {
		if !x.node(al.NodeID) || al.ProjectID != "" && !x.project(al.ProjectID) || offline[al.NodeID] {
			continue
		}
		if al.Container != "" && abnormal[al.ProjectID+"/"+al.Container] {
			continue // 容器异常已经有一行
		}
		sev := "warning"
		if criticalAlert(al.Key) {
			sev = "critical"
		}
		detail := al.NodeName
		if al.ProjectID != "" && al.Container != "" {
			for _, s := range projects {
				if s.GetProject().GetId() == al.ProjectID {
					detail = s.GetProject().GetName() + " / " + al.Container + " · " + al.NodeName
				}
			}
		}
		items = append(items, &nagiv1.AttentionItem{Kind: "alert", Severity: sev, Title: al.Title, Detail: detail, NodeId: al.NodeID,
			ProjectId: al.ProjectID, Container: al.Container, Since: timestamppb.New(al.Since)})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if ri, rj := severityRank[items[i].GetSeverity()], severityRank[items[j].GetSeverity()]; ri != rj {
			return ri < rj
		}
		return items[i].GetSince().AsTime().Before(items[j].GetSince().AsTime())
	})
	return items
}

// scoped 读出访问范围内的项目和节点。
func (a *API) scoped(ctx context.Context) (*access, []*nagiv1.ProjectSummary, []store.Node, error) {
	u := auth.CurrentUser(ctx)
	x, err := a.loadAccess(ctx, u)
	if err != nil {
		return nil, nil, nil, err
	}
	list, err := a.Projects.List(ctx)
	if err != nil {
		return nil, nil, nil, toConnect(err)
	}
	var projects []*nagiv1.ProjectSummary
	for _, s := range list {
		if x.project(s.GetProject().GetId()) {
			projects = append(projects, s)
		}
	}
	all, err := a.Q.ListNodes(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var rows []store.Node
	for _, n := range all {
		if x.node(n.ID) && n.Revoked == 0 {
			rows = append(rows, n)
		}
	}
	return x, projects, rows, nil
}

func (s *projectService) GetAttention(ctx context.Context, _ *connect.Request[nagiv1.GetAttentionRequest]) (*connect.Response[nagiv1.Attention], error) {
	x, projects, rows, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.Attention{Items: s.attention(ctx, auth.CurrentUser(ctx), x, projects, rows)}), nil
}

// ---- 流量 ----

// trafficNow 是各节点当前的速率：对外入站、出站（按节点）和端口映射（序列名 → 字节/秒）。
type trafficNow struct {
	at     time.Time
	ready  bool
	extRx  map[string]float64
	extTx  map[string]float64
	routes map[string]float64
}

type trafficCacheState struct {
	mu         sync.Mutex
	cur        *trafficNow
	refreshing bool
}

var (
	meshSeries  = regexp.MustCompile(`^m:([0-9a-f]+):(rx|tx)$`)
	relaySeries = regexp.MustCompile(`^m:relay:(rx|tx)$`)
)

// latest 是序列最后一个点的值。
func latest(s *nagiv1.MetricSeries) (float64, bool) {
	if n := len(s.GetValues()); n > 0 {
		return s.GetValues()[n-1], true
	}
	return 0, false
}

// meshDeduct 是节点网卡里属于节点网络的那部分（从对外流量里扣掉）。自带节点和面板之间走本机回环，不扣。
func meshDeduct(series []*nagiv1.MetricSeries, panelKey string, embedded bool) (rx, tx float64) {
	for _, s := range series {
		v, ok := latest(s)
		if !ok {
			continue
		}
		name := s.GetName()
		dir := ""
		if m := relaySeries.FindStringSubmatch(name); m != nil {
			dir = m[1]
		} else if m := meshSeries.FindStringSubmatch(name); m != nil {
			if embedded && m[1] == panelKey {
				continue
			}
			dir = m[2]
		}
		switch dir {
		case "rx":
			rx += v
		case "tx":
			tx += v
		}
	}
	return rx, tx
}

func (a *API) panelMeshKey() string {
	if m := a.Nodes.Mesh(); m != nil {
		k := m.PublicKey()
		return hex.EncodeToString(k[:])
	}
	return ""
}

// liveTraffic 返回缓存的各节点当前速率，不等节点：缓存超过 trafficCache 时在后台重新问（同一时间只问一次），
// 问完通知订阅流再算一次。还没有结果时返回空的（ready 为 false），首页先发别的内容，流量随下一份到达。
// 以前在这里同步问每个节点，最慢的节点（最多 5 秒）会拖住首页的第一份数据，等待时还锁着别的流。
func (a *API) liveTraffic(ctx context.Context) *trafficNow {
	a.traffic.mu.Lock()
	defer a.traffic.mu.Unlock()
	c := a.traffic.cur
	if (c == nil || time.Since(c.at) >= trafficCache) && !a.traffic.refreshing {
		a.traffic.refreshing = true
		go a.refreshTraffic()
	}
	if c == nil {
		return &trafficNow{extRx: map[string]float64{}, extTx: map[string]float64{}, routes: map[string]float64{}}
	}
	return c
}

// refreshTraffic 问一遍节点，存进缓存，通知订阅流。
func (a *API) refreshTraffic() {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out := a.queryTraffic(ctx)
	a.traffic.mu.Lock()
	a.traffic.cur = out
	a.traffic.refreshing = false
	a.traffic.mu.Unlock()
	a.Nodes.Changes.Bump()
}

// queryTraffic 问每个在线节点最近 1 分钟的网卡、节点网络和端口映射速率，各节点同时问，每个最多等 5 秒。
func (a *API) queryTraffic(ctx context.Context) *trafficNow {
	out := &trafficNow{at: time.Now(), extRx: map[string]float64{}, extTx: map[string]float64{}, routes: map[string]float64{}}
	conns := a.Nodes.Conns()
	panelKey := a.panelMeshKey()
	from := timestamppb.New(time.Now().Add(-time.Minute))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range conns {
		if c.Pending() {
			continue
		}
		wg.Add(1)
		go func(c *nodes.Conn) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			// 三类数据同时问。
			var res [3]*nagiv1.QueryMetricsResponse
			var errs [3]error
			var qwg sync.WaitGroup
			for i, prefix := range []string{"node:net_", "m:", "r:"} {
				qwg.Add(1)
				go func() {
					defer qwg.Done()
					res[i], errs[i] = c.Agent.QueryMetrics(qctx, &nagiv1.QueryMetricsRequest{Prefix: prefix, From: from})
				}()
			}
			qwg.Wait()
			for _, err := range errs {
				if err != nil {
					return
				}
			}
			var rx, tx float64
			for _, s := range res[0].GetSeries() {
				if v, ok := latest(s); ok {
					switch s.GetName() {
					case "node:net_rx":
						rx = v
					case "node:net_tx":
						tx = v
					}
				}
			}
			mrx, mtx := meshDeduct(res[1].GetSeries(), panelKey, c.OwnEmbedded())
			mu.Lock()
			defer mu.Unlock()
			out.ready = true
			out.extRx[c.NodeID] = max(0, rx-mrx)
			out.extTx[c.NodeID] = max(0, tx-mtx)
			for _, s := range res[2].GetSeries() {
				if v, ok := latest(s); ok {
					out.routes[s.GetName()] += v
				}
			}
		}(c)
	}
	wg.Wait()
	return out
}

// routeAddress 是端口映射的访问地址：绑了域名的写域名，否则是节点地址和端口。
func (a *API) routeAddress(ctx context.Context, r *nagiv1.Exposure, nodeID string) string {
	if len(r.GetDomains()) > 0 {
		scheme := "https://"
		if r.GetHttpOnly() {
			scheme = "http://"
		}
		return scheme + r.GetDomains()[0]
	}
	host := a.Panel.PublicAddress(ctx)
	if c, err := a.Nodes.Peek(nodeID); err == nil && c.LastReport() != nil {
		host = a.nodeAddress(ctx, c)
	}
	if r.GetPublished() == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(int(r.GetPublished())))
}

// overview 算首页的一份完整状态。
func (a *API) overview(ctx context.Context) (*nagiv1.Overview, error) {
	u := auth.CurrentUser(ctx)
	x, projects, rows, err := a.scoped(ctx)
	if err != nil {
		return nil, err
	}
	out := &nagiv1.Overview{PanelName: a.Panel.Name(ctx)}
	out.Attention = &nagiv1.Attention{Items: a.attention(ctx, u, x, projects, rows)}
	offline := map[string]bool{}
	for _, n := range rows {
		if n.Pending != 0 {
			continue
		}
		out.NodesTotal++
		if a.Nodes.Online(n.ID) {
			out.NodesOnline++
		} else {
			offline[n.ID] = true
		}
	}
	containerNode := map[string]string{}
	for _, s := range projects {
		for _, c := range s.GetProject().GetContainers() {
			containerNode[s.GetProject().GetId()+"/"+c.GetName()] = c.GetNodeId()
			out.ContainersTotal++
			if offline[c.GetNodeId()] || c.GetDeployedVersion() == 0 {
				continue
			}
			total, running := 0, 0
			ok := true
			for _, v := range s.GetStatus().GetServices() {
				if v.GetContainer() != c.GetName() {
					continue
				}
				total++
				if v.GetState() == "running" {
					running++
				}
				if v.GetHealth() == "unhealthy" || v.GetHealth() == "starting" {
					ok = false
				}
			}
			if ok && total > 0 && running == total {
				out.ContainersRunning++
			}
		}
	}
	t := a.liveTraffic(ctx)
	out.TrafficReady = t.ready
	for id, v := range t.extRx {
		if x.node(id) {
			out.ExternalRx += v
		}
	}
	for id, v := range t.extTx {
		if x.node(id) {
			out.ExternalTx += v
		}
	}
	var routes []*nagiv1.RouteTraffic
	for _, s := range projects {
		p := s.GetProject()
		for _, r := range p.GetRoutes() {
			key := "r:" + p.GetId() + "/" + r.GetName() + ":"
			rx, tx := t.routes[key+"in"], t.routes[key+"out"]
			if rx+tx <= 0 {
				continue
			}
			routes = append(routes, &nagiv1.RouteTraffic{ProjectId: p.GetId(), ProjectName: p.GetName(), Route: r.GetName(),
				Address: a.routeAddress(ctx, r, containerNode[p.GetId()+"/"+r.GetContainer()]), Rx: rx, Tx: tx})
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Rx+routes[i].Tx > routes[j].Rx+routes[j].Tx })
	if len(routes) > topRoutes {
		routes = routes[:topRoutes]
	}
	out.TopRoutes = routes
	return out, nil
}

// WatchOverview 是首页的订阅流：先发一份完整的当前状态，之后有变化（节点上报、上下线、告警、修改）时重新算，
// 和上一次的不同才发；30 秒没有发过时发心跳。访问范围每次重新计算。
func (s *projectService) WatchOverview(ctx context.Context, _ *connect.Request[nagiv1.WatchOverviewRequest], stream *connect.ServerStream[nagiv1.Overview]) error {
	var last *nagiv1.Overview
	heartbeat := time.NewTimer(overviewHeartbeat)
	defer heartbeat.Stop()
	for {
		changed := s.Nodes.Changes.Wait()
		// 会话退出、手机被退出时流立即结束（退出是一次修改，会通知到这里）。
		if err := s.stillSignedIn(ctx); err != nil {
			return err
		}
		cur, err := s.overview(ctx)
		if err != nil {
			return err
		}
		if last == nil || !proto.Equal(cur, last) {
			if err := stream.Send(cur); err != nil {
				return err
			}
			last = cur
			heartbeat.Reset(overviewHeartbeat)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeat.C:
			if err := stream.Send(&nagiv1.Overview{Heartbeat: true}); err != nil {
				return err
			}
			heartbeat.Reset(overviewHeartbeat)
			continue
		case <-changed:
		}
		// 合并 1 秒内的多次变化；流量最多每 trafficCache 重新问一次节点。
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(overviewMerge):
		}
	}
}

// stillSignedIn 检查订阅流的调用者还登录着：手机的设备记录、网页的会话还在。
func (a *API) stillSignedIn(ctx context.Context) error {
	u := auth.CurrentUser(ctx)
	switch {
	case u.DeviceID != "":
		if _, err := a.Q.AppDeviceByID(ctx, u.DeviceID); err != nil {
			return connect.NewError(connect.CodeUnauthenticated, i18n.New("这台手机已退出，请重新添加"))
		}
	case u.SessionID != "":
		if _, err := a.Auth.SessionByID(ctx, u.SessionID); err != nil {
			return connect.NewError(connect.CodeUnauthenticated, err)
		}
	}
	return nil
}

// ---- 流量趋势 ----

// QueryTraffic 是首页的流量趋势：访问范围内各在线节点的对外流量（网卡减去节点网络）和节点之间的流量（按发送方合计）。
func (s *nodeService) QueryTraffic(ctx context.Context, req *connect.Request[nagiv1.QueryTrafficRequest]) (*connect.Response[nagiv1.TrafficHistory], error) {
	x, err := s.loadAccess(ctx, auth.CurrentUser(ctx))
	if err != nil {
		return nil, err
	}
	step := req.Msg.GetStep()
	if step < 10 {
		step = 10
	}
	from := req.Msg.GetFrom()
	if from == nil {
		from = timestamppb.New(time.Now().Add(-time.Hour))
	}
	panelKey := s.panelMeshKey()
	type point struct{ rx, tx, mesh float64 }
	var mu sync.Mutex
	points := map[int64]*point{}
	counts := map[int64]int{}
	var wg sync.WaitGroup
	for _, c := range s.Nodes.Conns() {
		if c.Pending() || !x.node(c.NodeID) {
			continue
		}
		wg.Add(1)
		go func(c *nodes.Conn) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			netRes, err := c.Agent.QueryMetrics(qctx, &nagiv1.QueryMetricsRequest{Prefix: "node:net_", From: from, Step: step})
			if err != nil {
				return
			}
			meshRes, _ := c.Agent.QueryMetrics(qctx, &nagiv1.QueryMetricsRequest{Prefix: "m:", From: from, Step: step})
			embedded := c.OwnEmbedded()
			// 每个时刻的节点网络：扣除量（rx、tx）和发出量（mesh）。
			type dm struct{ rx, tx, sent float64 }
			deduct := map[int64]*dm{}
			for _, ser := range meshRes.GetSeries() {
				name := ser.GetName()
				dir, isMesh := "", false
				if m := relaySeries.FindStringSubmatch(name); m != nil {
					dir = m[1]
				} else if m := meshSeries.FindStringSubmatch(name); m != nil {
					if embedded && m[1] == panelKey {
						continue
					}
					dir, isMesh = m[2], true
				}
				for i, v := range ser.GetValues() {
					t := ser.GetTimes()[i]
					d := deduct[t]
					if d == nil {
						d = &dm{}
						deduct[t] = d
					}
					if dir == "rx" {
						d.rx += v
					} else if dir == "tx" {
						d.tx += v
						if isMesh {
							d.sent += v
						}
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			seen := map[int64]bool{}
			for _, ser := range netRes.GetSeries() {
				for i, v := range ser.GetValues() {
					t := ser.GetTimes()[i]
					p := points[t]
					if p == nil {
						p = &point{}
						points[t] = p
					}
					d := deduct[t]
					if d == nil {
						d = &dm{}
					}
					switch ser.GetName() {
					case "node:net_rx":
						p.rx += max(0, v-d.rx)
					case "node:net_tx":
						p.tx += max(0, v-d.tx)
					}
					if !seen[t] {
						seen[t] = true
						p.mesh += d.sent
						counts[t]++
					}
				}
			}
		}(c)
	}
	wg.Wait()
	times := make([]int64, 0, len(points))
	for t := range points {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	// 最后一个时段可能只有部分节点上报了，合计会突然变小，当作还没有数据（和网页一样）。
	if n := len(times); n > 1 && counts[times[n-1]] < counts[times[n-2]] {
		times = times[:n-1]
	}
	out := &nagiv1.TrafficHistory{}
	for _, t := range times {
		p := points[t]
		out.Times = append(out.Times, t)
		out.ExternalRx = append(out.ExternalRx, p.rx)
		out.ExternalTx = append(out.ExternalTx, p.tx)
		out.Mesh = append(out.Mesh, p.mesh)
	}
	return connect.NewResponse(out), nil
}

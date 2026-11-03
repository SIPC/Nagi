package api

import (
	"context"
	"encoding/hex"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 访问范围（见 docs/panel.md#访问范围）：操作员和只读成员可以只访问指定的项目、节点。
// 请求里的项目、节点在这里统一核对，回复里范围外的内容在这里去掉，处理函数不用管。

// access 是一个成员能访问的项目和节点。nil 的 *access 和 nil 的集合都表示全部。
type access struct {
	projects map[string]bool
	nodes    map[string]bool
}

func (x *access) project(id string) bool { return x == nil || x.projects == nil || x.projects[id] }
func (x *access) node(id string) bool    { return x == nil || x.nodes == nil || x.nodes[id] }

func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// loadAccess 读出用户的访问范围。管理员和两项都是全部的成员返回 nil。
func (a *API) loadAccess(ctx context.Context, u auth.User) (*access, error) {
	if u.ID == 0 || u.Role == auth.RoleAdmin || !u.LimitProjects && !u.LimitNodes {
		return nil, nil
	}
	x := &access{}
	if u.LimitProjects {
		ids, err := a.Q.ListUserProjects(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		x.projects = toSet(ids)
	}
	if u.LimitNodes {
		ids, err := a.Q.ListUserNodes(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		x.nodes = toSet(ids)
	}
	return x, nil
}

func projectNotFound() error {
	return connect.NewError(connect.CodeNotFound, i18n.New("项目不存在"))
}

func nodeNotFound() error {
	return connect.NewError(connect.CodeNotFound, i18n.New("节点不存在"))
}

// 请求里按名字核对的字段（只看最外层）。范围外的项目一律当作不存在，不暴露它是否存在。
var (
	scopeProjectFields = map[protoreflect.Name]bool{"project_id": true, "other_project_id": true}
	scopeNodeFields    = map[protoreflect.Name]bool{"node_id": true, "node_ids": true, "standby_node_id": true, "witness_node_id": true}
)

// scopeFiltered 是请求里没有项目、节点，靠过滤回复（或者另外处理）的接口。新增这类接口时要在这里登记，
// 并在 filterReply 或 checkRequest 里处理，scope_test.go 会检查。
var scopeFiltered = map[string]bool{
	nagiv1connect.ProjectServiceListProjectsProcedure:  true,
	nagiv1connect.ProjectServiceCreateProjectProcedure: true, // 目标节点按 node_id 核对，新项目加进范围
	nagiv1connect.NodeServiceListNodesProcedure:        true,
	nagiv1connect.NodeServiceGetPanelMeshProcedure:     true,
}

func scopedService(procedure string) bool {
	return strings.HasPrefix(procedure, "/"+nagiv1connect.ProjectServiceName+"/") || strings.HasPrefix(procedure, "/"+nagiv1connect.NodeServiceName+"/")
}

// checkRequest 核对请求里的项目和节点。
func (a *API) checkRequest(ctx context.Context, x *access, procedure string, msg any) error {
	m, ok := msg.(proto.Message)
	if !ok || !scopedService(procedure) {
		return nil
	}
	if q, ok := msg.(*nagiv1.QueryNodeMetricsRequest); ok && projectMetrics(x, q.GetPrefix()) {
		return nil
	}
	r := m.ProtoReflect()
	var err error
	r.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.Kind() != protoreflect.StringKind:
		case scopeProjectFields[fd.Name()] && !fd.IsList():
			if !x.project(v.String()) {
				err = projectNotFound()
			}
		case scopeNodeFields[fd.Name()] && fd.IsList():
			for i := range v.List().Len() {
				if !x.node(v.List().Get(i).String()) {
					err = nodeNotFound()
				}
			}
		case scopeNodeFields[fd.Name()]:
			if !x.node(v.String()) {
				err = nodeNotFound()
			}
		}
		return err == nil
	})
	if err != nil {
		return err
	}
	switch m := msg.(type) {
	case *nagiv1.RestoreFromBackupRequest:
		// 在备份节点上恢复：备份节点也要在范围内。
		c, err := a.findContainer(ctx, m.GetProjectId(), m.GetContainer())
		if err != nil {
			return err
		}
		if c != nil && c.GetBackupNodeId() != "" && !x.node(c.GetBackupNodeId()) {
			return nodeNotFound()
		}
	case *nagiv1.PingNodeMeshRequest:
		if owner := a.meshNodes(ctx)[hex.EncodeToString(m.GetPublicKey())]; owner != "" && !x.node(owner) {
			return nodeNotFound()
		}
	}
	return nil
}

func (a *API) findContainer(ctx context.Context, projectID, name string) (*nagiv1.Container, error) {
	s, err := a.Projects.Get(ctx, projectID)
	if err != nil {
		return nil, toConnect(err)
	}
	for _, c := range s.GetProject().GetContainers() {
		if c.GetName() == name {
			return c, nil
		}
	}
	return nil, nil
}

// meshNodes 返回 mesh 公钥（hex）→ 节点 ID。面板不在里面。
func (a *API) meshNodes(ctx context.Context) map[string]string {
	rows, err := a.Q.ListNodes(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if len(r.MeshKey) > 0 {
			out[hex.EncodeToString(r.MeshKey)] = r.ID
		}
	}
	return out
}

// filterReply 去掉回复里范围外的内容。
func (a *API) filterReply(ctx context.Context, x *access, msg any) {
	switch m := msg.(type) {
	case *nagiv1.ListProjectsResponse:
		kept := m.Projects[:0]
		for _, s := range m.GetProjects() {
			if x.project(s.GetProject().GetId()) {
				hideLinks(x, s.GetProject())
				kept = append(kept, s)
			}
		}
		m.Projects = kept
	case *nagiv1.ProjectSummary:
		hideLinks(x, m.GetProject())
	case *nagiv1.Project:
		hideLinks(x, m)
	case *nagiv1.ListNodesResponse:
		a.filterNodes(ctx, x, m)
	case *nagiv1.MeshStatus:
		owners := a.meshNodes(ctx)
		kept := m.Peers[:0]
		for _, p := range m.GetPeers() {
			if id := owners[hex.EncodeToString(p.GetPublicKey())]; id == "" || x.node(id) {
				kept = append(kept, p)
			}
		}
		m.Peers = kept
	case *nagiv1.QueryMetricsResponse:
		a.filterMetrics(ctx, x, m)
	case *nagiv1.ListNodeReplicasResponse:
		kept := m.Replicas[:0]
		for _, r := range m.GetReplicas() {
			if x.project(r.GetReplica().GetProjectId()) {
				kept = append(kept, r)
			}
		}
		m.Replicas = kept
	}
}

// hideLinks：已连接的项目不在范围内时不显示名称，界面显示"另一个项目"。网络名留着，配置里要用它访问对方的容器。
func hideLinks(x *access, p *nagiv1.Project) {
	for _, l := range p.GetLinks() {
		if !x.project(l.GetProjectId()) {
			l.Name = ""
		}
	}
}

// filterNodes 只留范围内的节点。范围内项目的容器用到的范围外节点放进 other_nodes，只给名称和是否在线。
// 节点上的容器数只算范围内的项目；不认识的容器只有管理员能处理，不给。
func (a *API) filterNodes(ctx context.Context, x *access, m *nagiv1.ListNodesResponse) {
	summaries, _ := a.Projects.List(ctx)
	used := map[string]bool{}
	count := map[string]int32{}
	for _, s := range summaries {
		if !x.project(s.GetProject().GetId()) {
			continue
		}
		for _, c := range s.GetProject().GetContainers() {
			count[c.GetNodeId()]++
			used[c.GetNodeId()] = true
			used[c.GetBackupNodeId()] = true
			for _, p := range []*nagiv1.FailoverParticipant{c.GetFailover().GetA(), c.GetFailover().GetB(), c.GetFailover().GetWitness()} {
				used[p.GetNodeId()] = true
			}
		}
	}
	kept := m.Nodes[:0]
	for _, n := range m.GetNodes() {
		n.UnknownContainers = nil
		if x.projects != nil {
			n.Containers = count[n.GetId()]
		}
		if x.node(n.GetId()) {
			kept = append(kept, n)
		} else if used[n.GetId()] {
			m.OtherNodes = append(m.OtherNodes, &nagiv1.Node{Id: n.GetId(), Name: n.GetName(), Online: n.GetOnline()})
		}
	}
	m.Nodes = kept
}

// filterMetrics：节点整体的序列（node:）照常给。到范围外节点的 mesh 流量（m:）不显示是哪个节点，换成编号，
// 概览算对外流量时仍然能从网卡流量里减掉它。其余序列都以"<种类>:<项目 ID>/"开头（容器 c:、端口映射 r:、
// 日志 l: 等），只留范围内的项目；不是这种形式的也去掉，以后加了新的种类不会漏。
func (a *API) filterMetrics(ctx context.Context, x *access, m *nagiv1.QueryMetricsResponse) {
	var owners map[string]string
	hidden := map[string]string{}
	kept := m.Series[:0]
	for _, s := range m.GetSeries() {
		kind, rest, _ := strings.Cut(s.GetName(), ":")
		switch kind {
		case "node":
		case "m":
			key, metric, _ := strings.Cut(rest, ":")
			if owners == nil {
				owners = a.meshNodes(ctx)
			}
			if id := owners[key]; id != "" && !x.node(id) {
				if hidden[key] == "" {
					hidden[key] = strconv.Itoa(len(hidden))
				}
				s.Name = "m:" + hidden[key] + ":" + metric
			}
		default:
			if project, ok := metricProject(rest); !ok || !x.project(project) {
				continue
			}
		}
		kept = append(kept, s)
	}
	m.Series = kept
}

// metricProject 取"<项目 ID>/..."里的项目 ID。
func metricProject(rest string) (string, bool) {
	project, _, ok := strings.Cut(rest, "/")
	return project, ok && project != ""
}

// projectMetrics 判断是不是只查一个范围内项目的序列（前缀形如 c:<项目 ID>/）。项目页要看本项目容器的占用，
// 容器在范围外的节点上时也放行：回复经过 filterMetrics，只剩这个项目的序列。
func projectMetrics(x *access, prefix string) bool {
	kind, rest, _ := strings.Cut(prefix, ":")
	project, ok := metricProject(rest)
	return kind != "node" && kind != "m" && ok && x.project(project)
}

// afterCall 处理调用成功以后的事：限定了项目范围的成员新建的项目加进他的范围。
func (a *API) afterCall(ctx context.Context, x *access, procedure string, msg any) {
	if procedure != nagiv1connect.ProjectServiceCreateProjectProcedure || x.projects == nil {
		return
	}
	if p, ok := msg.(*nagiv1.Project); ok {
		if err := a.Q.AddUserProject(ctx, store.AddUserProjectParams{UserID: auth.CurrentUser(ctx).ID, ProjectID: p.GetId()}); err != nil {
			a.Log.Error("新项目加入成员的访问范围失败", "project", p.GetId(), "err", err)
		}
		x.projects[p.GetId()] = true
	}
}

// projectAllowed 给不走 Connect 的入口（终端、上传、卷文件）核对项目范围。
func (a *API) projectAllowed(ctx context.Context, u auth.User, projectID string) error {
	x, err := a.loadAccess(ctx, u)
	if err != nil {
		return err
	}
	if !x.project(projectID) {
		return i18n.New("项目不存在")
	}
	return nil
}

// scopeCheck 是访问范围的拦截器，放在角色检查之后。
type scopeCheck struct{ a *API }

func (s scopeCheck) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		x, err := s.a.loadAccess(ctx, auth.CurrentUser(ctx))
		if err != nil {
			return nil, err
		}
		if x == nil {
			return next(ctx, req)
		}
		procedure := req.Spec().Procedure
		if err := s.a.checkRequest(ctx, x, procedure, req.Any()); err != nil {
			return nil, err
		}
		res, err := next(ctx, req)
		if err == nil {
			s.a.afterCall(ctx, x, procedure, res.Any())
			s.a.filterReply(ctx, x, res.Any())
		}
		return res, err
	}
}

func (scopeCheck) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (s scopeCheck) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		x, err := s.a.loadAccess(ctx, auth.CurrentUser(ctx))
		if err != nil {
			return err
		}
		if x == nil {
			return next(ctx, conn)
		}
		return next(ctx, &scopedConn{StreamingHandlerConn: conn, ctx: ctx, a: s.a, x: x})
	}
}

// scopedConn 在流式接口收到请求时核对范围，发出的消息同样过滤。
type scopedConn struct {
	connect.StreamingHandlerConn
	ctx context.Context
	a   *API
	x   *access
}

func (c *scopedConn) Receive(msg any) error {
	if err := c.StreamingHandlerConn.Receive(msg); err != nil {
		return err
	}
	return c.a.checkRequest(c.ctx, c.x, c.Spec().Procedure, msg)
}

func (c *scopedConn) Send(msg any) error {
	c.a.filterReply(c.ctx, c.x, msg)
	return c.StreamingHandlerConn.Send(msg)
}

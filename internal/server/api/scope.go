package api

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/projects"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 访问范围和权限（见 docs/panel.md#访问范围、#成员和角色）：不是管理员的成员只能访问范围内的项目、节点，
// 回复里只留他有权限看的内容。请求里的项目、节点在这里统一核对，回复在 clean.go 里统一清理，处理函数不用管。

// access 是一个成员能访问的项目、节点和他的权限。nil 的 *access 表示管理员（全部）；nil 的集合表示全部。
type access struct {
	perms    auth.Perms
	projects map[string]bool
	nodes    map[string]bool
	// names 是全部节点的 ID → 名称（清理回复里的节点名用），按需读出。
	names map[string]string
}

func (x *access) project(id string) bool { return x == nil || x.projects == nil || x.projects[id] }

// nodeInScope 判断节点在不在访问范围内（不看「查看节点」权限）。
func (x *access) nodeInScope(id string) bool {
	return x == nil || id != "" && (x.nodes == nil || x.nodes[id])
}

// node 判断成员看不看得到这个节点：在范围内，并且有「查看节点」权限。看不到的节点在回复里一律是「其他节点」。
func (x *access) node(id string) bool {
	return x == nil || x.perms.Has(auth.PermNodeView) && x.nodeInScope(id)
}

// can 判断有没有一项权限。
func (x *access) can(p auth.Perm) bool { return x == nil || x.perms.Has(p) }

func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// loadAccess 读出成员的访问范围和权限。管理员和没登录的请求返回 nil。
func (a *API) loadAccess(ctx context.Context, u auth.User) (*access, error) {
	if u.ID == 0 || u.IsAdmin() {
		return nil, nil
	}
	x := &access{perms: u.Perms}
	if u.LimitProjects {
		ids, err := a.Q.ListUserProjects(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		// 范围里的分组：分组里的项目都算，以后放进来的也算。
		groups, err := a.Q.ListUserProjectGroups(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		more, err := a.Q.ProjectsInGroups(ctx, groups)
		if err != nil {
			return nil, err
		}
		x.projects = toSet(append(ids, more...))
	}
	switch {
	case u.NodesFollow:
		// 跟随项目：范围内项目的容器所在的节点，按当时的位置算，容器迁走后原节点就不在范围内。
		rows, err := a.Q.ContainerNodes(ctx)
		if err != nil {
			return nil, err
		}
		x.nodes = map[string]bool{}
		for _, r := range rows {
			if x.project(r.ProjectID) {
				x.nodes[r.NodeID] = true
			}
		}
	case u.LimitNodes:
		ids, err := a.Q.ListUserNodes(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		tags, err := a.Q.ListUserNodeTags(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		more, err := a.Q.NodesWithTags(ctx, tags)
		if err != nil {
			return nil, err
		}
		x.nodes = toSet(append(ids, more...))
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

// scopeFiltered 是请求里没有项目、节点，靠清理回复（或者另外处理）的项目和节点接口。新增这类接口时要在这里登记，
// 并在 clean.go 或 checkRequest 里处理，scope_test.go 会检查。
var scopeFiltered = map[string]bool{
	nagiv1connect.ProjectServiceListProjectsProcedure:  true,
	nagiv1connect.ProjectServiceCreateProjectProcedure: true, // 目标节点按 node_id 核对，新项目加进范围
	nagiv1connect.NodeServiceListNodesProcedure:        true,
	nagiv1connect.NodeServiceGetPanelMeshProcedure:     true, // 只给管理员
}

func scopedService(procedure string) bool {
	return strings.HasPrefix(procedure, "/"+nagiv1connect.ProjectServiceName+"/") || strings.HasPrefix(procedure, "/"+nagiv1connect.NodeServiceName+"/")
}

// checkRequest 核对请求里的项目和节点。范围外、或者没有「查看节点」权限时看不到的节点一律当作不存在。
// 成员看不到的、原来就有的节点（管理员设的备份节点、入口节点）在这里补回请求里，改别的不受影响。
func (a *API) checkRequest(ctx context.Context, x *access, procedure string, msg any) error {
	m, ok := msg.(proto.Message)
	if !ok || !scopedService(procedure) {
		return nil
	}
	switch m := msg.(type) {
	case *nagiv1.QueryNodeMetricsRequest:
		return a.checkMetricsRequest(ctx, x, m)
	case *nagiv1.SetBackupNodesRequest:
		if !x.project(m.GetProjectId()) {
			return projectNotFound()
		}
		// 看不到的备份节点在回复里是空的 ID：去掉空的，再把原来的补回原来的位置。新加的备份节点要看得到。
		old := a.backupNodesOf(ctx, m.GetProjectId(), m.GetContainer())
		m.BackupNodeIds = a.keepHidden(x, old, m.GetBackupNodeIds())
		for _, id := range m.GetBackupNodeIds() {
			if !x.node(id) && !slices.Contains(old, id) {
				return nodeNotFound()
			}
		}
		for id := range m.GetReplicaDirs() {
			if !x.node(id) {
				delete(m.ReplicaDirs, id)
			}
		}
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
		// 在备份节点上恢复：备份节点也要在范围内。指定了节点时上面已经核对过；没指定时由面板选，每个备份节点都要在范围内。
		if m.GetNodeId() == "" {
			for _, id := range a.backupNodesOf(ctx, m.GetProjectId(), m.GetContainer()) {
				if !x.node(id) {
					return nodeNotFound()
				}
			}
		}
	case *nagiv1.PingNodeMeshRequest:
		// 不属于任何节点的对端（面板、备用面板、已移除的节点）只给管理员。
		if owner := a.meshNodes(ctx)[hex.EncodeToString(m.GetPublicKey())]; owner == "" || !x.node(owner) {
			return nodeNotFound()
		}
	// 映射的入口节点在 route 里面，上面只看最外层，这里单独核对。
	case *nagiv1.SaveRouteRequest:
		return a.checkEntryNodes(ctx, x, m.GetProjectId(), m.GetRoute())
	case *nagiv1.CheckDomainsRequest:
		for _, r := range m.GetRoutes() {
			if err := a.checkEntryNodes(ctx, x, m.GetProjectId(), r); err != nil {
				return err
			}
		}
	}
	return nil
}

// keepHidden 把成员看不到的原有节点补回列表：看不到的留在原来的位置，看得到的按请求的顺序填进其余位置，多出来的放在最后。
// 请求里的空 ID（回复里看不到的节点）去掉。
func (a *API) keepHidden(x *access, old, req []string) []string {
	visible := make([]string, 0, len(req))
	for _, id := range req {
		if id != "" && !slices.Contains(visible, id) {
			visible = append(visible, id)
		}
	}
	out := make([]string, 0, len(old)+len(visible))
	for _, id := range old {
		if !x.node(id) {
			out = append(out, id)
		} else if len(visible) > 0 {
			out = append(out, visible[0])
			visible = visible[1:]
		}
	}
	return append(out, visible...)
}

// checkMetricsRequest：节点整体的序列要节点在范围内；项目页查本项目容器的占用（前缀 c:<项目 ID>/）时，
// 节点在范围外也放行，但容器要确实在这台节点上，不能借报错探出别的节点在不在（回复经过 filterMetrics，只剩这个项目的序列）。
func (a *API) checkMetricsRequest(ctx context.Context, x *access, m *nagiv1.QueryNodeMetricsRequest) error {
	if x.node(m.GetNodeId()) {
		return nil
	}
	kind, rest, _ := strings.Cut(m.GetPrefix(), ":")
	project, ok := metricProject(rest)
	if kind == "node" || kind == "m" || !ok || !x.project(project) {
		return nodeNotFound()
	}
	// 没填节点：查这个项目的容器所在的、成员看不到的节点（api.go 的 hiddenNodeMetrics）。
	if m.GetNodeId() == "" {
		return nil
	}
	rows, err := a.Q.ContainerNodes(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ProjectID == project && r.NodeID == m.GetNodeId() {
			return nil
		}
	}
	return nodeNotFound()
}

// checkEntryNodes 核对映射的入口节点。看不到的入口节点在回复里是空的 ID：去掉空的，把这个映射原来就有的
// 看不到的入口节点（管理员设的）补回去，改别的设置不受影响。
func (a *API) checkEntryNodes(ctx context.Context, x *access, projectID string, r *nagiv1.Exposure) error {
	if r == nil {
		return nil
	}
	var kept []string
	if r.GetName() != "" {
		routes, err := a.Q.ListRoutes(ctx, projectID)
		if err != nil {
			return err
		}
		for _, old := range routes {
			if old.Name == r.GetName() {
				kept = projects.RouteProto(old).GetEntryNodes()
			}
		}
	}
	nodes := make([]string, 0, len(r.GetEntryNodes()))
	for _, id := range r.GetEntryNodes() {
		switch {
		case id == "":
		case x.node(id):
			nodes = append(nodes, id)
		case slices.Contains(kept, id):
			nodes = append(nodes, id)
		default:
			return nodeNotFound()
		}
	}
	for _, id := range kept {
		if !x.node(id) && !slices.Contains(nodes, id) {
			nodes = append(nodes, id)
		}
	}
	r.EntryNodes = nodes
	return nil
}

// backupNodesOf 返回容器现在的备份节点，容器不存在时为空（由处理函数报错）。
func (a *API) backupNodesOf(ctx context.Context, projectID, name string) []string {
	c, err := a.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: projectID, Name: name})
	if err != nil {
		return nil
	}
	return store.BackupNodes(c)
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

// metricProject 取"<项目 ID>/..."里的项目 ID。
func metricProject(rest string) (string, bool) {
	project, _, ok := strings.Cut(rest, "/")
	return project, ok && project != ""
}

// afterCall 处理调用成功以后的事：限定了项目范围的成员新建的项目加进他的范围。
func (a *API) afterCall(ctx context.Context, x *access, procedure string, msg any) {
	if procedure != nagiv1connect.ProjectServiceCreateProjectProcedure || x == nil || x.projects == nil {
		return
	}
	if p, ok := msg.(*nagiv1.Project); ok {
		if err := a.Q.AddUserProject(ctx, store.AddUserProjectParams{UserID: auth.CurrentUser(ctx).ID, ProjectID: p.GetId()}); err != nil {
			a.Log.Error("新项目加入成员的访问范围失败", "project", p.GetId(), "err", err)
		}
		x.projects[p.GetId()] = true
	}
}

// projectAllowed 给不走 Connect 的入口（终端、上传、卷文件）核对项目范围和权限。
func (a *API) projectAllowed(ctx context.Context, u auth.User, projectID string, need ...auth.Perm) error {
	for _, p := range need {
		if !u.Can(p) {
			return errNoPermission
		}
	}
	x, err := a.loadAccess(ctx, u)
	if err != nil {
		return err
	}
	if !x.project(projectID) {
		return i18n.New("项目不存在")
	}
	return nil
}

var errNoPermission = i18n.New("当前账号没有权限执行这个操作")

// scopeCheck 是访问范围的拦截器，放在角色检查之后：核对请求，清理回复（clean.go）。
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
		return res, s.a.scrubError(ctx, x, err)
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
		return s.a.scrubError(ctx, x, next(ctx, &scopedConn{StreamingHandlerConn: conn, ctx: ctx, a: s.a, x: x}))
	}
}

// scopedConn 在流式接口收到请求时核对范围，发出的消息同样清理。
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

// scrubError 清理接口的报错：非管理员看不到的节点的名称和 ID 不能出现在报错里（「节点 xx 离线」之类，
// 2026-11-19-87c02b5#9）。没有要换的内容时原样返回（保留错误码、响应头和可以按语言翻译的原文）。
func (a *API) scrubError(ctx context.Context, x *access, err error) error {
	if x == nil || err == nil {
		return err
	}
	text := errorText(i18n.FromContext(ctx), err)
	clean := a.scrubNodes(ctx, x, text)
	if clean == text {
		return err
	}
	out := connect.NewError(connect.CodeOf(err), errors.New(clean))
	var ce *connect.Error
	if errors.As(err, &ce) {
		for k, v := range ce.Meta() {
			out.Meta()[k] = v
		}
	}
	return out
}

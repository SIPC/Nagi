package api

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/recording"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 审计日志的查询（见 docs/audit.md#界面、#权限）。

// auditViewer 是查看审计记录的人能看到什么。
type auditViewer struct {
	admin  bool
	viewer bool
	userID string
	// session 是查看者自己的会话，非管理员只看得到自己的会话标记。
	session string
	// projects 是访问范围内的项目，nil 表示全部项目。
	projects []string
	// x 是查看者的访问范围（管理员为 nil），scrub 去掉文字里他看不到的节点名（A7）。
	x     *access
	scrub func(string) string
	// other 是「其他节点」按界面语言的文字。
	other string
	// refs 把失败详情里的引用标记（见 auditFailure）换成名称，查看者看不到的节点换成「其他节点」。
	refs func(string) string
	// hidden 是他看不到的节点：ID → 名称。筛选和搜索不能借它们探出这些节点（2026-11-19-87c02b5#10）。
	hidden map[string]string
}

// auditViewer：管理员和有「审计日志」权限的成员看全部记录；其他成员只看范围内项目的记录，没有「查看配置」时不看参数和失败详情。
func (a *API) auditViewer(ctx context.Context) (auditViewer, error) {
	u := auth.CurrentUser(ctx)
	v := auditViewer{admin: u.Can(auth.PermAudit), viewer: !u.Can(auth.PermConfig), userID: strconv.FormatInt(u.ID, 10),
		session: audit.SessionTag(u.SessionID), scrub: func(s string) string { return s }, refs: i18n.PlainRefs}
	if v.admin {
		return v, nil
	}
	// 和其他页面用同一个访问范围：直接给的项目，加上范围里的分组中的项目（见 scope.go）。
	x, err := a.loadAccess(ctx, u)
	if err != nil {
		return v, err
	}
	v.x = x
	// 参数、报错里只去掉看不到的节点的 ID（随机的 32 位十六进制，猜不出来）。节点的名称不在文字里找着换：
	// 对象名称、参数是成员自己能写的，照着换的话，建一个名称是猜测词的容器、看审计里它有没有变成「其他节点」，
	// 就能确认有没有这台节点（2026-11-19-7b50402#3）。操作者、对象是节点的记录按类型和 ID 换（见 entry）。
	v.scrub = func(s string) string { return a.scrubNodeIDs(ctx, x, s) }
	v.other = otherNode(ctx)
	other := v.other
	v.refs = func(s string) string {
		return i18n.RenderRefs(s, func(r i18n.Ref) string {
			if r.Kind == "node" && !x.node(r.ID) {
				return other
			}
			return r.Name
		})
	}
	v.hidden = map[string]string{}
	for id, name := range a.nodeNames(ctx, x) {
		if !x.node(id) {
			v.hidden[id] = name
		}
	}
	if x != nil && x.projects != nil {
		ids := []string{}
		for id := range x.projects {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		v.projects = ids
	}
	return v, nil
}

// filter 把界面的筛选条件转成查询条件，并按查看者的权限收窄。
func (v auditViewer) filter(f *nagiv1.AuditFilter) audit.Filter {
	out := audit.Filter{
		ActorID: f.GetActorId(), Via: f.GetVia(), Categories: f.GetCategories(), ProjectID: f.GetProjectId(),
		NodeID: f.GetNodeId(), FailedOnly: f.GetFailedOnly(), Search: f.GetSearch(),
	}
	// 只能按看得到的节点筛（A7）：看不到的节点和不存在的一样查不到，借结果条数探不出节点在不在。
	if !v.admin && out.NodeID != "" && !v.x.node(out.NodeID) {
		out.NodeID = "-"
	}
	// 操作者是看不到的节点时同样按查不到处理。
	if _, ok := v.hidden[out.ActorID]; ok && !v.admin {
		out.ActorID = "-"
	}
	if f.GetFrom() != nil {
		out.From = f.GetFrom().AsTime()
	}
	if f.GetTo() != nil {
		out.To = f.GetTo().AsTime()
	}
	v.restrict(&out)
	return out
}

// restrict 让非管理员只能查到访问范围内项目的记录，搜索只匹配他看得到的字段。
func (v auditViewer) restrict(f *audit.Filter) {
	if v.admin {
		return
	}
	// 参数和失败详情显示前要去掉别人的 IP、看不到的节点（见 view），搜索匹配的却是原文：
	// 按有没有结果就能逐位试出被去掉的内容。所以非管理员的搜索不匹配这两列。
	f.SearchFields = audit.SearchViewer
	// 有看不到的节点时，操作者、对象是节点的记录不按名称匹配：名称显示时会换成「其他节点」，按原文匹配的话，
	// 用很短的词反复搜就能把名称一段一段拼出来。别的记录照常按名称匹配，不看搜索的词是什么。
	f.SearchSkipNodes = len(v.hidden) > 0
	// 文件操作的记录，看不到路径的（见 entry）名称只按容器匹配，否则同样能把路径一段一段拼出来。
	f.SearchFilePaths = v.fileActions()
	if v.projects != nil {
		f.Projects = v.projects
	} else {
		f.RequireProject = true
	}
}

// auditNames 是把 ID 换成名字用的缓存。
type auditNames struct {
	projects map[string]string
	tokens   map[int64]string
}

func (a *API) auditNames(ctx context.Context) auditNames {
	n := auditNames{projects: map[string]string{}}
	if rows, err := a.Q.ListProjects(ctx); err == nil {
		for _, p := range rows {
			n.projects[p.ID] = p.Name
		}
	}
	n.tokens, _ = a.Q.AccessTokenNames(ctx)
	return n
}

// auditEntry 把记录转成接口返回的格式，按查看者的权限隐去 IP、浏览器、别人的会话和失败详情。
func (v auditViewer) entry(e audit.Event, names auditNames) *nagiv1.AuditEvent {
	out := &nagiv1.AuditEvent{
		Id: e.ID, Time: timestamppb.New(e.Time), Category: e.Category, Action: e.Action, ActorType: e.ActorType,
		ActorId: e.ActorID, ActorName: e.ActorName, Via: e.Via, TokenId: e.TokenID, TokenName: names.tokens[e.TokenID],
		Client: e.Client, Session: e.Session, Ip: e.IP, UserAgent: e.UserAgent, ProjectId: e.ProjectID,
		ProjectName: names.projects[e.ProjectID], TargetType: e.TargetType, TargetId: e.TargetID, TargetName: e.TargetName,
		Params: e.Params, Result: e.Result, Error: v.refs(e.Error), DurationMs: e.DurationMS, ParentId: e.ParentID,
		RecordingId: e.RecordingID,
	}
	if !v.admin {
		out.Ip, out.UserAgent = "", ""
		if e.Session != v.session {
			out.Session = ""
		}
		out.RecordingId = ""
		if !v.detail(e) {
			// 操作失败的详情可能带着配置里的内容（compose 报错等），参数里也有配置，没有「查看配置」时只看结果；
			// 别的权限管着的内容（执行的命令、文件路径、域名等）还要相应的权限（见 detail）。
			out.Error, out.Params = "", ""
			if e.TargetType == "file" {
				out.TargetId, out.TargetName = "", fileContainer(e)
			}
		}
		if e.Category == audit.Alert && !v.x.can(auth.PermAddresses) {
			out.Params = hideEgressTarget(out.Params)
		}
		// 项目设置里别的权限管着的项（出站放行、内部端口、告警、快照）：参数里去掉，失败详情可能提到它们，也不给。
		if p, hid := hideSettingsParams(e.Action, out.Params, v.x); hid {
			out.Params, out.Error = p, ""
		}
		// 看不到的节点（A7）：对象、操作者（告警由节点发出）是节点的，按类型和 ID 判断，整个名称换成「其他节点」；
		// 别的名称、报错和参数里只去掉节点的 ID。参数里的地址去掉（B9）。
		if e.TargetType == "node" && !v.x.node(e.TargetID) {
			out.TargetId, out.TargetName = "", v.other
		}
		if e.ActorType == "node" && !v.x.node(e.ActorID) {
			out.ActorId, out.ActorName = "", v.other
		}
		out.Error = v.scrub(out.Error)
		out.Params = v.scrub(stripAddressParams(out.Params))
	}
	return out
}

// configLevel 是有「查看配置」就够看参数和失败详情的操作要的权限：这些操作的参数是配置里本来就看得到的内容。
var configLevel = auth.PermsOf(auth.PermView, auth.PermConfig, auth.PermControl, auth.PermDeploy)

// actionPerms 是不走 Connect 的入口记的动作要的权限，和入口自己检查的一样。
var actionPerms = map[string]auth.Perm{
	"ExecCommand": auth.PermTerminal, "OpenTerminal": auth.PermTerminal, "CloseTerminal": auth.PermTerminal,
	"DownloadVolumeFile": auth.PermFilesRead, "UploadVolumeFile": auth.PermFilesWrite,
}

// detailPerms 返回看一条记录的参数和失败详情，除了「查看配置」还要哪些权限：做这个操作要的权限。
// 参数里是那项权限管着的内容：执行的命令（终端）、文件路径（文件）、域名和端口（网络和域名）、备份和顶替的节点等，
// 只有「查看配置」的成员在项目页上看不到它们，也不该从记录里看到。ok 为 false 表示认不出是什么操作，只给管理员。
func detailPerms(e audit.Event) (need auth.Perms, ok bool) {
	switch e.Category {
	case audit.Alert:
		// 告警的参数只有标题和键。
		return 0, true
	case audit.MCP:
		return mcpPerms(e.Params)
	}
	if p, ok := actionPerms[e.Action]; ok {
		return auth.PermsOf(p), true
	}
	for _, svc := range []string{nagiv1connect.ProjectServiceName, nagiv1connect.NodeServiceName, nagiv1connect.SettingsServiceName} {
		if p, ok := auth.ProcedurePerm("/" + svc + "/" + e.Action); ok {
			return auth.PermsOf(p) &^ configLevel, true
		}
	}
	return 0, false
}

// mcpPerms 取出 MCP 调用的记录里记着的权限（工具要的权限，见 mcp 的 record）。记了权限的记录，节点参数是 ID、
// 失败详情带引用标记，可以按查看的人换；没记的是以前的记录，参数和失败详情是工具的原文，可能写着查看者看不到的
// 节点的名称，只给管理员（它下面的接口调用记录照常按各自的规则给）。
func mcpPerms(params string) (auth.Perms, bool) {
	var p struct {
		Perms *[]string `json:"perms"`
	}
	if json.Unmarshal([]byte(params), &p) != nil || p.Perms == nil {
		return 0, false
	}
	for _, name := range *p.Perms {
		if !auth.ValidPermName(name) {
			return 0, false
		}
	}
	return auth.ParsePerms(*p.Perms) &^ configLevel, true
}

// detail 判断查看者能不能看这条记录的参数和失败详情：要「查看配置」，加上做这个操作要的权限（见 detailPerms）。
func (v auditViewer) detail(e audit.Event) bool {
	if v.admin {
		return true
	}
	need, ok := detailPerms(e)
	return !v.viewer && ok && v.x != nil && v.x.perms&need == need
}

// settingsParams 是项目设置的记录里各项要的权限，和项目接口回复里去掉的一样（见 cleanProject）：
// 动作 → 参数里放设置的键 → 设置项 → 权限。MCP 的 update_project 把设置写在工具参数里（它调用的 UpdateProjectSettings 另记一条）。
var settingsParams = map[string]struct {
	key   string
	perms map[string]auth.Perm
}{
	"UpdateProjectSettings": {"settings", projectSettingPerms},
	"CreateProject":         {"settings", projectSettingPerms},
	"mcp:update_project": {"arguments", map[string]auth.Perm{
		"internal_ports": auth.PermNetwork, "egress_mode": auth.PermNetwork, "egress_allow": auth.PermNetwork,
		"snapshot_limit": auth.PermSnapshots, "snapshots_enabled": auth.PermSnapshots, "snapshot_interval_minutes": auth.PermSnapshots,
		"snapshot_times": auth.PermSnapshots, "snapshot_time_zone": auth.PermSnapshots,
	}},
}

var projectSettingPerms = map[string]auth.Perm{
	"internal_ports": auth.PermNetwork, "egress": auth.PermNetwork, "alert_rules": auth.PermAlerts,
	"snapshot_interval_minutes": auth.PermSnapshots, "snapshot_limit": auth.PermSnapshots, "snapshot_schedule": auth.PermSnapshots,
}

// hideSettingsParams 去掉项目设置的参数里查看者没有相应权限的项，返回去掉以后的参数，以及有没有去掉。
// 参数被截断过、读不出来时整个不给。不是项目设置的记录、查看者权限都有时原样返回。
func hideSettingsParams(action, params string, x *access) (string, bool) {
	s, ok := settingsParams[action]
	if !ok || params == "" {
		return params, false
	}
	all := true
	for _, p := range s.perms {
		all = all && x.can(p)
	}
	if all {
		return params, false
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(params))
	dec.UseNumber() // 修订号这类大整数不经过 float64
	if dec.Decode(&m) != nil || m["truncated"] == true {
		return "", true
	}
	sub, ok := m[s.key].(map[string]any)
	if !ok {
		return params, false
	}
	hid := false
	if sub["truncated"] == true {
		m[s.key], hid = map[string]any{"truncated": true}, true
	} else {
		for k, p := range s.perms {
			if _, ok := sub[k]; ok && !x.can(p) {
				delete(sub, k)
				hid = true
			}
		}
	}
	if !hid {
		return params, false
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", true
	}
	return string(b), true
}

// fileActions 返回查看者看得到路径的文件操作（对象是文件的记录的动作），不在其中的只看得到容器。
func (v auditViewer) fileActions() []string {
	out := []string{}
	for _, a := range fileActionNames() {
		if v.detail(audit.Event{Action: a}) {
			out = append(out, a)
		}
	}
	return out
}

// fileActionNames 是对象可能是文件的动作：请求里有 path 的接口（见 recordCallWith），加上不走 Connect 的下载和上传。
// 只列 detailPerms 认得的几个服务，别的动作不在里面，按看不到路径处理。
var fileActionNames = sync.OnceValue(func() []string {
	out := []string{"DownloadVolumeFile", "UploadVolumeFile"}
	for _, svc := range []string{nagiv1connect.ProjectServiceName, nagiv1connect.NodeServiceName, nagiv1connect.SettingsServiceName} {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
		sd, ok := d.(protoreflect.ServiceDescriptor)
		if err != nil || !ok {
			continue
		}
		for i := range sd.Methods().Len() {
			m := sd.Methods().Get(i)
			if m.Input().Fields().ByName("path") != nil {
				out = append(out, string(m.Name()))
			}
		}
	}
	return out
})

// fileContainer 是文件操作的记录里文件所在的容器（对象名称是 <容器>:<路径>，不在容器里的文件为空）。
func fileContainer(e audit.Event) string {
	if c, ok := strings.CutSuffix(e.TargetName, ":"+e.TargetID); ok {
		return c
	}
	return ""
}

// hideEgressTarget 去掉出站告警里容器尝试连接的地址和端口（没有「查看访问地址」的成员不该看到）：
// 标题里的换成「…」，键 egress:<项目>/<容器>/<目标> 只留到容器。不是出站告警时原样返回。
func hideEgressTarget(params string) string {
	var p map[string]any
	if json.Unmarshal([]byte(params), &p) != nil {
		return params
	}
	key, _ := p["key"].(string)
	rest, ok := strings.CutPrefix(key, "egress:")
	if !ok {
		return params
	}
	parts := strings.SplitN(rest, "/", 3)
	target := ""
	if len(parts) == 3 {
		target = parts[2]
		p["key"] = "egress:" + parts[0] + "/" + parts[1]
	}
	if title, ok := p["title"].(string); ok {
		if target != "" {
			title = strings.ReplaceAll(title, target, "…")
		}
		// 标题和键来自节点，对不上时（被截断等）至少把 IP 去掉。
		p["title"] = scrubIPs(title)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// addressParam 判断审计参数的键是不是地址类的（B9）：issued_ip、ip、address、endpoint 这些。
func addressParam(key string) bool {
	k := strings.ToLower(key)
	return k == "ip" || strings.HasSuffix(k, "_ip") || strings.HasPrefix(k, "ip_") || strings.Contains(k, "address") ||
		strings.Contains(k, "endpoint") || strings.HasSuffix(k, "_addr") || k == "addr" || k == "host"
}

// stripAddressParams 去掉 JSON 参数里地址类的键（包括嵌套的），不是 JSON 对象时原样返回。
func stripAddressParams(params string) string { return stripParams(params, addressParam) }

// stripParams 去掉 JSON 参数里 drop 认为要去掉的键（包括嵌套的），不是 JSON 对象时原样返回。
func stripParams(params string, drop func(key string) bool) string {
	if params == "" || params[0] != '{' {
		return params
	}
	var m map[string]any
	if json.Unmarshal([]byte(params), &m) != nil {
		return params
	}
	var strip func(v any) any
	strip = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			// 太长被截断的参数（audit.TruncateParams）：head 是原文的开头，是一段字符串，里面的键去不掉，整段不给。
			if t["truncated"] == true {
				if _, ok := t["head"].(string); ok {
					delete(t, "head")
				}
			}
			for k, x := range t {
				if drop(k) {
					delete(t, k)
				} else {
					t[k] = strip(x)
				}
			}
		case []any:
			for i, x := range t {
				t[i] = strip(x)
			}
		}
		return v
	}
	strip(m)
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func (v auditViewer) canSee(e audit.Event) bool {
	if v.admin {
		return true
	}
	if e.ProjectID == "" {
		return false
	}
	if v.projects == nil {
		return true
	}
	for _, p := range v.projects {
		if p == e.ProjectID {
			return true
		}
	}
	return false
}

// projectAlertsQuery 判断是不是只查一个项目的告警（项目设置里出站限制列出被记录的目标用）：没有「审计日志」时只能这样查，
// 要有「查看」，内容和项目的事件一样（范围、清理规则见 auditViewer），其余的审计查询一律拒绝。
func projectAlertsQuery(m *nagiv1.ListAuditEventsRequest) bool {
	f := m.GetFilter()
	return m.GetParentId() == 0 && f.GetProjectId() != "" && slices.Equal(f.GetCategories(), []string{audit.Alert})
}

// errNoAudit 是没有「审计日志」权限时审计接口的报错。
func errNoAudit() error {
	return connect.NewError(connect.CodePermissionDenied, i18n.New("没有「审计日志」权限"))
}

func (s *settingsService) ListAuditEvents(ctx context.Context, req *connect.Request[nagiv1.ListAuditEventsRequest]) (*connect.Response[nagiv1.ListAuditEventsResponse], error) {
	v, err := s.auditViewer(ctx)
	if err != nil {
		return nil, err
	}
	// 接口登记为所有成员都能调用（auth 的 perms.go），在这里按权限放行。
	if !v.admin && (!auth.CurrentUser(ctx).Can(auth.PermView) || !projectAlertsQuery(req.Msg)) {
		return nil, errNoAudit()
	}
	limit := int(req.Msg.GetLimit())
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var f audit.Filter
	if id := req.Msg.GetParentId(); id != 0 {
		f = audit.Filter{ParentID: id}
		v.restrict(&f)
	} else {
		f = v.filter(req.Msg.GetFilter())
		f.TopLevel = true
	}
	f.Before, f.Limit = req.Msg.GetBefore(), limit
	rows, err := s.view().List(ctx, f)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	counts, _ := s.view().ChildCounts(ctx, ids)
	names := s.auditNames(ctx)
	out := &nagiv1.ListAuditEventsResponse{}
	for _, r := range rows {
		e := v.entry(r, names)
		e.Children = int32(counts[r.ID])
		out.Events = append(out.Events, e)
	}
	return connect.NewResponse(out), nil
}

func (s *settingsService) GetAuditEvent(ctx context.Context, req *connect.Request[nagiv1.GetAuditEventRequest]) (*connect.Response[nagiv1.GetAuditEventResponse], error) {
	v, err := s.auditViewer(ctx)
	if err != nil {
		return nil, err
	}
	// 接口登记在「审计日志」下（auth 的 perms.go），这里再挡一次。
	if !v.admin {
		return nil, errNoAudit()
	}
	row, err := s.view().Get(ctx, req.Msg.GetId())
	if err != nil || !v.canSee(row) {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("记录不存在"))
	}
	names := s.auditNames(ctx)
	out := &nagiv1.GetAuditEventResponse{Event: v.entry(row, names)}
	if row.ParentID != 0 {
		if p, err := s.view().Get(ctx, row.ParentID); err == nil && v.canSee(p) {
			out.Parent = v.entry(p, names)
		}
	}
	f := audit.Filter{ParentID: row.ID, Limit: 200}
	v.restrict(&f)
	children, err := s.view().List(ctx, f)
	if err != nil {
		return nil, err
	}
	// 按发生的顺序排。
	for i := len(children) - 1; i >= 0; i-- {
		out.Children = append(out.Children, v.entry(children[i], names))
	}
	out.Event.Children = int32(len(children))
	return connect.NewResponse(out), nil
}

// exportChunk 是导出时每次发送的大小。
const exportChunk = 256 << 10

// ExportAuditEvents 按筛选条件导出 JSON Lines，要「审计日志」（角色检查）。导出本身记一条 read 记录。
func (s *settingsService) ExportAuditEvents(ctx context.Context, req *connect.Request[nagiv1.ExportAuditEventsRequest], stream *connect.ServerStream[nagiv1.ExportAuditEventsChunk]) error {
	v, err := s.auditViewer(ctx)
	if err != nil {
		return err
	}
	if !v.admin {
		return errNoAudit()
	}
	f := v.filter(req.Msg.GetFilter())
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	err = s.view().Each(ctx, f, func(e audit.Event) error {
		if err := enc.Encode(audit.ExportLine(e)); err != nil {
			return err
		}
		if buf.Len() >= exportChunk {
			if err := stream.Send(&nagiv1.ExportAuditEventsChunk{Data: bytes.Clone(buf.Bytes())}); err != nil {
				return err
			}
			buf.Reset()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if buf.Len() > 0 {
		return stream.Send(&nagiv1.ExportAuditEventsChunk{Data: buf.Bytes()})
	}
	return nil
}

// ---- 原来的活动记录接口：改为查询审计库，返回格式不变 ----

// activityEntry 把审计记录转成原来的活动记录格式。节点和审计页一样按引用换（见 entry）：操作者、对象是
// 查看者看不到的节点时换成「其他节点」，失败详情里的引用按标记换；不在文字里找名称。
func (v auditViewer) activityEntry(e audit.Event, names auditNames) *nagiv1.ActivityEntry {
	// 失败详情和文件路径的规则和审计页一样（见 detail）：没有「查看配置」、或者没有做这个操作要的权限时不给。
	detailed := v.detail(e)
	e.Error = v.refs(e.Error)
	if !v.admin {
		e.Error = v.scrub(e.Error)
		if e.Category == audit.Alert && !v.x.can(auth.PermAddresses) {
			e.Params = hideEgressTarget(e.Params)
		}
		if _, hid := hideSettingsParams(e.Action, e.Params, v.x); hid {
			e.Error = ""
		}
		if e.ActorType == "node" && !v.x.node(e.ActorID) {
			e.ActorName = v.other
		}
		if e.TargetType == "node" && !v.x.node(e.TargetID) {
			e.TargetName = v.other
		}
	}
	kind := "operation"
	if e.Category == audit.Alert {
		kind = "alert"
	}
	detail := e.Error
	if kind == "alert" {
		detail = alertTitle(e.Params)
	} else if !detailed {
		detail = ""
	} else if e.TargetType == "file" && e.TargetID != "" {
		detail = e.TargetID
		if e.Error != "" {
			detail += ": " + e.Error
		}
	}
	target := e.TargetName
	if e.TargetType == "file" {
		target = strings.TrimSuffix(e.TargetName, ":"+e.TargetID)
	}
	via := ""
	if e.Via == audit.ViaMCP {
		name := names.tokens[e.TokenID]
		if name == "" {
			name = e.Client
		}
		via = "mcp:" + name
	}
	return &nagiv1.ActivityEntry{
		Id: e.ID, Time: timestamppb.New(e.Time), Kind: kind, Actor: e.ActorName, Action: e.Action,
		ProjectId: e.ProjectID, Target: target, Detail: detail, Failed: e.Result != audit.ResultOK, Via: via,
	}
}

func alertTitle(params string) string {
	var p struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal([]byte(params), &p)
	return p.Title
}

// containerEventsLimit 是容器事件一次返回的条数。
const containerEventsLimit = 20

func (s *projectService) ListContainerEvents(ctx context.Context, req *connect.Request[nagiv1.ContainerRef]) (*connect.Response[nagiv1.ListActivityResponse], error) {
	return s.projectEvents(ctx, audit.Filter{ProjectID: req.Msg.GetProjectId(), TargetMatch: req.Msg.GetContainer(), Limit: containerEventsLimit})
}

// ListProjectEvents 是项目的活动记录。按项目核对访问范围（请求里有 project_id）；只读角色和容器事件一样看不到失败的详情。
func (s *projectService) ListProjectEvents(ctx context.Context, req *connect.Request[nagiv1.ListProjectEventsRequest]) (*connect.Response[nagiv1.ListActivityResponse], error) {
	m := req.Msg
	limit := int(m.GetLimit())
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.projectEvents(ctx, audit.Filter{ProjectID: m.GetProjectId(), TargetMatch: m.GetContainer(), Before: m.GetBefore(), FailedOnly: m.GetFailedOnly(), Limit: limit})
}

// projectEvents 查项目的事件：操作和告警，不含敏感查询（查看配置不算项目的事件）。
func (s *projectService) projectEvents(ctx context.Context, f audit.Filter) (*connect.Response[nagiv1.ListActivityResponse], error) {
	if f.ProjectID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("缺少项目"))
	}
	f.Categories = []string{audit.Operation, audit.Session, audit.Alert, audit.MCP, audit.Security}
	f.TopLevel = false
	v, err := s.auditViewer(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.view().List(ctx, f)
	if err != nil {
		return nil, err
	}
	names := s.auditNames(ctx)
	out := &nagiv1.ListActivityResponse{}
	for _, r := range rows {
		// 操作失败的详情可能带着配置里的内容（compose 报错等），只读角色只看结果（见 activityEntry）。
		out.Entries = append(out.Entries, v.activityEntry(r, names))
	}
	return connect.NewResponse(out), nil
}

func (s *settingsService) ListActivity(ctx context.Context, req *connect.Request[nagiv1.ListActivityRequest]) (*connect.Response[nagiv1.ListActivityResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	f := audit.Filter{Before: req.Msg.GetBefore(), Limit: limit}
	switch req.Msg.GetKind() {
	case "alert":
		f.Categories = []string{audit.Alert}
	case "operation":
		f.Categories = []string{audit.Operation, audit.Read, audit.MCP, audit.Session, audit.Login, audit.Security}
	}
	v, err := s.auditViewer(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.view().List(ctx, f)
	if err != nil {
		return nil, err
	}
	names := s.auditNames(ctx)
	out := &nagiv1.ListActivityResponse{}
	for _, r := range rows {
		out.Entries = append(out.Entries, v.activityEntry(r, names))
	}
	return connect.NewResponse(out), nil
}

// ---- 录制和审计设置 ----

// SettingRecording 是终端录制的开关，"0" 表示关闭，默认开启。
const SettingRecording = "audit_recording"

// RecordingEnabled 判断终端录制有没有开。
func (a *API) RecordingEnabled(ctx context.Context) bool {
	v, err := a.Q.GetSetting(ctx, SettingRecording)
	return err != nil || v != "0"
}

// GetRecording 回放终端录制。角色检查要「审计日志」，高权限确认由 elevationCheck 检查；回放本身作为敏感查询记一条。
func (s *settingsService) GetRecording(ctx context.Context, req *connect.Request[nagiv1.GetRecordingRequest], stream *connect.ServerStream[nagiv1.RecordingChunk]) error {
	if node, id, remote := recording.ParseReference(req.Msg.GetId()); remote {
		return s.readNodeRecording(ctx, node, id, func(body []byte) error {
			return stream.Send(&nagiv1.RecordingChunk{Data: body})
		})
	}
	if s.Recordings == nil {
		return connect.NewError(connect.CodeNotFound, i18n.New("录制不存在"))
	}
	err := s.Recordings.Read(req.Msg.GetId(), func(b []byte) error {
		return stream.Send(&nagiv1.RecordingChunk{Data: b})
	})
	if err != nil {
		return connect.NewError(connect.CodeNotFound, i18n.New("录制不存在或无法读取"))
	}
	return nil
}

// securityAlert 发一条面板的安全通知（操作人和来源写在消息里），发送在后台进行。
func (a *API) securityAlert(ctx context.Context, key, title string) {
	if a.Nodes == nil || a.Panel != nil && a.Panel.Standby != nil {
		return
	}
	a.notify(ctx, key, i18n.New(title).Error(), "")
}

// view 返回界面上查看的审计库（备用面板上是同步来的那份）。
func (a *API) view() *audit.Log {
	if a.AuditView != nil {
		return a.AuditView
	}
	return a.Audit
}

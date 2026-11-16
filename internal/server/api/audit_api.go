package api

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
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
}

func (a *API) auditViewer(ctx context.Context) (auditViewer, error) {
	u := auth.CurrentUser(ctx)
	v := auditViewer{admin: u.Role == auth.RoleAdmin, viewer: u.Role == auth.RoleViewer, userID: strconv.FormatInt(u.ID, 10), session: audit.SessionTag(u.SessionID)}
	if v.admin || !u.LimitProjects {
		return v, nil
	}
	ids, err := a.Q.ListUserProjects(ctx, u.ID)
	if err != nil {
		return v, err
	}
	if ids == nil {
		ids = []string{}
	}
	v.projects = ids
	return v, nil
}

// filter 把界面的筛选条件转成查询条件，并按查看者的权限收窄。
func (v auditViewer) filter(f *nagiv1.AuditFilter) audit.Filter {
	out := audit.Filter{
		ActorID: f.GetActorId(), Via: f.GetVia(), Categories: f.GetCategories(), ProjectID: f.GetProjectId(),
		NodeID: f.GetNodeId(), FailedOnly: f.GetFailedOnly(), Search: f.GetSearch(),
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

// restrict 让非管理员只能查到访问范围内项目的记录。
func (v auditViewer) restrict(f *audit.Filter) {
	if v.admin {
		return
	}
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
		Params: e.Params, Result: e.Result, Error: e.Error, DurationMs: e.DurationMS, ParentId: e.ParentID,
		RecordingId: e.RecordingID,
	}
	if !v.admin {
		out.Ip, out.UserAgent = "", ""
		if e.Session != v.session {
			out.Session = ""
		}
		out.RecordingId = ""
		if v.viewer {
			// 操作失败的详情可能带着配置里的内容（compose 报错等），参数里也有配置，只读角色只看结果。
			out.Error, out.Params = "", ""
		}
	}
	return out
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

func (s *settingsService) ListAuditEvents(ctx context.Context, req *connect.Request[nagiv1.ListAuditEventsRequest]) (*connect.Response[nagiv1.ListAuditEventsResponse], error) {
	v, err := s.auditViewer(ctx)
	if err != nil {
		return nil, err
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
	rows, err := s.Audit.List(ctx, f)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	counts, _ := s.Audit.ChildCounts(ctx, ids)
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
	row, err := s.Audit.Get(ctx, req.Msg.GetId())
	if err != nil || !v.canSee(row) {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("记录不存在"))
	}
	names := s.auditNames(ctx)
	out := &nagiv1.GetAuditEventResponse{Event: v.entry(row, names)}
	if row.ParentID != 0 {
		if p, err := s.Audit.Get(ctx, row.ParentID); err == nil && v.canSee(p) {
			out.Parent = v.entry(p, names)
		}
	}
	f := audit.Filter{ParentID: row.ID, Limit: 200}
	v.restrict(&f)
	children, err := s.Audit.List(ctx, f)
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

// ExportAuditEvents 按筛选条件导出 JSON Lines，只有管理员能用（角色检查）。导出本身记一条 read 记录。
func (s *settingsService) ExportAuditEvents(ctx context.Context, req *connect.Request[nagiv1.ExportAuditEventsRequest], stream *connect.ServerStream[nagiv1.ExportAuditEventsChunk]) error {
	v, err := s.auditViewer(ctx)
	if err != nil {
		return err
	}
	if !v.admin {
		return connect.NewError(connect.CodePermissionDenied, i18n.New("只有管理员能导出审计记录"))
	}
	f := v.filter(req.Msg.GetFilter())
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	err = s.Audit.Each(ctx, f, func(e audit.Event) error {
		if err := enc.Encode(exportLine(e)); err != nil {
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

// exportLine 是导出的一行：全部字段，时间用 RFC 3339 毫秒，带上哈希链。
func exportLine(e audit.Event) map[string]any {
	var m map[string]any
	b, _ := json.Marshal(e)
	_ = json.Unmarshal(b, &m)
	m["time"] = e.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	m["prev_hash"], m["hash"] = e.PrevHash, e.Hash
	return m
}

// ---- 原来的活动记录接口：改为查询审计库，返回格式不变 ----

// activityEntry 把审计记录转成原来的活动记录格式。
func activityEntry(e audit.Event, names auditNames) *nagiv1.ActivityEntry {
	kind := "operation"
	if e.Category == audit.Alert {
		kind = "alert"
	}
	detail := e.Error
	if kind == "alert" {
		detail = alertTitle(e.Params)
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
	rows, err := s.Audit.List(ctx, f)
	if err != nil {
		return nil, err
	}
	viewer := auth.CurrentUser(ctx).Role == auth.RoleViewer
	names := s.auditNames(ctx)
	out := &nagiv1.ListActivityResponse{}
	for _, r := range rows {
		e := activityEntry(r, names)
		// 操作失败的详情可能带着配置里的内容（compose 报错等），只读角色只看结果。
		if viewer && e.Kind == "operation" {
			e.Detail = ""
		}
		out.Entries = append(out.Entries, e)
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
	rows, err := s.Audit.List(ctx, f)
	if err != nil {
		return nil, err
	}
	names := s.auditNames(ctx)
	out := &nagiv1.ListActivityResponse{}
	for _, r := range rows {
		out.Entries = append(out.Entries, activityEntry(r, names))
	}
	return connect.NewResponse(out), nil
}

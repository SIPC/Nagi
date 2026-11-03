package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
)

// 活动记录：把会改变状态的接口调用记下来。只取固定的几个字段（项目、容器、节点、名称），
// 请求里的密码、令牌、环境变量等都不会写进记录。

// activityKeep 是活动记录保留的条数。
const activityKeep = 5000

// activitySkipped 是不记录的接口：查询以外的、不改变状态或太频繁的调用。
var activitySkipped = map[string]bool{
	nagiv1connect.AuthServiceMeProcedure:             true,
	nagiv1connect.AuthServiceLogoutProcedure:         true,
	nagiv1connect.AuthServiceSetLanguageProcedure:    true,
	nagiv1connect.SetupServiceGetSetupStateProcedure: true,
	nagiv1connect.EnrollServiceEnrollProcedure:       true,
}

func activityRecorded(procedure string) bool {
	if activitySkipped[procedure] {
		return false
	}
	i := strings.LastIndexByte(procedure, '/')
	method := procedure[i+1:]
	for _, p := range readOnlyPrefixes {
		if strings.HasPrefix(method, p) {
			return false
		}
	}
	return true
}

// activityField 读请求里的一个字符串字段，没有时返回空。
func activityField(m proto.Message, names ...string) string {
	if m == nil {
		return ""
	}
	r := m.ProtoReflect()
	for _, name := range names {
		f := r.Descriptor().Fields().ByName(protoreflect.Name(name))
		if f == nil || f.IsList() || f.IsMap() {
			continue
		}
		switch f.Kind() {
		case protoreflect.StringKind:
			if v := r.Get(f).String(); v != "" {
				return v
			}
		case protoreflect.Int64Kind:
			if v := r.Get(f).Int(); v != 0 {
				return strconv.FormatInt(v, 10)
			}
		}
	}
	return ""
}

func (a *API) recordOperation(ctx context.Context, procedure string, req proto.Message, failure string) {
	method := procedure[strings.LastIndexByte(procedure, '/')+1:]
	actor := auth.Username(ctx)
	if actor == "" {
		actor = activityField(req, "username")
	}
	target := activityField(req, "container", "name", "username", "node_id", "id")
	if id := activityField(req, "node_id"); id != "" && target == id {
		target = a.nodeName(context.WithoutCancel(ctx), id)
	}
	// 部署的目标是一个列表。
	if d, ok := req.(*nagiv1.DeployRequest); ok {
		names := make([]string, 0, len(d.GetTargets()))
		for _, c := range d.GetTargets() {
			names = append(names, c.GetContainer())
		}
		target = strings.Join(names, ", ")
	}
	// 文件操作记下动的是哪个文件，失败的也记，这样能看出有人试过什么路径。
	detail := failure
	if path := activityField(req, "path"); path != "" {
		detail = path
		if failure != "" {
			detail = path + ": " + failure
		}
	}
	err := a.Q.InsertActivity(context.WithoutCancel(ctx), store.InsertActivityParams{
		CreatedAt: time.Now().Unix(),
		Kind:      "operation",
		Actor:     clipActivity(actor, 64),
		Action:    method,
		ProjectID: activityField(req, "project_id"),
		Target:    clipActivity(target, 200),
		Detail:    clipActivity(detail, 500),
		Failed:    boolInt(failure != ""),
		Via:       clipActivity(auth.CurrentUser(ctx).Via, 80),
	})
	if err != nil {
		a.Log.Warn("写入活动记录失败", "err", err)
		return
	}
	_ = a.Q.PruneActivity(context.WithoutCancel(ctx), activityKeep)
}

// activityFailure 取错误本身的说明，去掉 RPC 错误码的前缀。
func activityFailure(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}

// clipActivity 限制写进记录的长度：登录失败时的用户名、错误信息都可能很长。
func clipActivity(v string, n int) string {
	r := []rune(v)
	if len(r) > n {
		return string(r[:n])
	}
	return v
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// activityLog 是记录操作的拦截器，放在登录检查之后。
type activityLog struct{ a *API }

func (l activityLog) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		res, err := next(ctx, req)
		if procedure := req.Spec().Procedure; activityRecorded(procedure) {
			msg, _ := req.Any().(proto.Message)
			failure := ""
			if err != nil {
				failure = activityFailure(err)
			}
			l.a.recordOperation(ctx, procedure, msg, failure)
		}
		return res, err
	}
}

func (l activityLog) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (l activityLog) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if !activityRecorded(conn.Spec().Procedure) {
			return next(ctx, conn)
		}
		w := &activityConn{StreamingHandlerConn: conn}
		err := next(ctx, w)
		failure := w.failure
		if err != nil {
			failure = activityFailure(err)
		}
		l.a.recordOperation(ctx, conn.Spec().Procedure, w.req, failure)
		return err
	}
}

// activityConn 记下流式接口的请求和结果：部署等操作失败时是在流里返回结果，不是返回错误。
type activityConn struct {
	connect.StreamingHandlerConn
	req     proto.Message
	failure string
}

func (c *activityConn) Receive(m any) error {
	err := c.StreamingHandlerConn.Receive(m)
	if err == nil && c.req == nil {
		c.req, _ = m.(proto.Message)
	}
	return err
}

func (c *activityConn) Send(m any) error {
	if ev, ok := m.(*nagiv1.DeployEvent); ok {
		if r := ev.GetResult(); r != nil && !r.GetSuccess() {
			c.failure = r.GetError()
		}
	}
	return c.StreamingHandlerConn.Send(m)
}

// activityEntry 把一条记录转成接口返回的格式。
func activityEntry(r store.Activity) *nagiv1.ActivityEntry {
	return &nagiv1.ActivityEntry{
		Id: r.ID, Time: timestamppb.New(time.Unix(r.CreatedAt, 0)), Kind: r.Kind, Actor: r.Actor, Action: r.Action,
		ProjectId: r.ProjectID, Target: r.Target, Detail: r.Detail, Failed: r.Failed != 0, Via: r.Via,
	}
}

// containerEventsLimit 是容器事件一次返回的条数。
const containerEventsLimit = 20

func (s *projectService) ListContainerEvents(ctx context.Context, req *connect.Request[nagiv1.ContainerRef]) (*connect.Response[nagiv1.ListActivityResponse], error) {
	rows, err := s.Q.ListContainerActivity(ctx, store.ListContainerActivityParams{
		ProjectID: req.Msg.GetProjectId(), Target: req.Msg.GetContainer(), Limit: containerEventsLimit,
	})
	if err != nil {
		return nil, err
	}
	viewer := auth.CurrentUser(ctx).Role == auth.RoleViewer
	out := &nagiv1.ListActivityResponse{}
	for _, r := range rows {
		e := activityEntry(r)
		// 操作失败的详情可能带着配置里的内容（compose 报错等），只读角色只看结果。
		if viewer && e.Kind == "operation" {
			e.Detail = ""
		}
		out.Entries = append(out.Entries, e)
	}
	return connect.NewResponse(out), nil
}

func (s *settingsService) ListActivity(ctx context.Context, req *connect.Request[nagiv1.ListActivityRequest]) (*connect.Response[nagiv1.ListActivityResponse], error) {
	limit := int64(req.Msg.GetLimit())
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Q.ListActivity(ctx, store.ListActivityParams{Kind: req.Msg.GetKind(), Before: req.Msg.GetBefore(), Limit: limit})
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ListActivityResponse{}
	for _, r := range rows {
		out.Entries = append(out.Entries, activityEntry(r))
	}
	return connect.NewResponse(out), nil
}

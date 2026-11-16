package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
)

// 审计记录（见 docs/audit.md）：会改变状态的接口调用、敏感查询都记进 audit.db。参数按 audit.Params 脱敏。

// auditSkipped 是不记录的接口：不改变状态或太频繁的调用。
var auditSkipped = map[string]bool{
	nagiv1connect.AuthServiceMeProcedure:             true,
	nagiv1connect.AuthServiceSetLanguageProcedure:    true,
	nagiv1connect.SetupServiceGetSetupStateProcedure: true,
	nagiv1connect.EnrollServiceEnrollProcedure:       true,
	// 退出在处理函数里单独记（这时会话还在，查得到是谁）；登录在 auth 里记（成功时才知道是哪个成员）。
	nagiv1connect.AuthServiceLogoutProcedure: true,
	nagiv1connect.AuthServiceLoginProcedure:  true,
	// 通行密钥登录同样在 auth 里记；取挑战不改变状态。
	nagiv1connect.AuthServiceBeginPasskeyLoginProcedure:            true,
	nagiv1connect.AuthServicePasskeyLoginProcedure:                 true,
	nagiv1connect.SettingsServiceBeginPasskeyRegistrationProcedure: true,
}

// unauthenticatedNoise：不用登录就能调的接口里，不值得记、又能被拿来刷掉记录的失败：初始化以后的 Setup 一律失败；
// 登录被限速拒绝（猜密码的人每次都会碰到）。登录失败在 auth 里按 IP 每小时汇总（见 auth.Service.recordLoginFailure）。
func unauthenticatedNoise(procedure string, err error) bool {
	if err == nil {
		return false
	}
	switch procedure {
	case nagiv1connect.SetupServiceSetupProcedure:
		return true
	case nagiv1connect.AuthServiceLoginProcedure:
		return true
	}
	return false
}

func methodOf(procedure string) string { return procedure[strings.LastIndexByte(procedure, '/')+1:] }

// activityRecorded 判断接口调用要不要记：修改类的都记，查询只记登记在 store.SensitiveReads 里的。
func activityRecorded(procedure string) bool {
	if auditSkipped[procedure] {
		return false
	}
	method := methodOf(procedure)
	if store.SensitiveReads[method] {
		return true
	}
	for _, p := range readOnlyPrefixes {
		if strings.HasPrefix(method, p) {
			return false
		}
	}
	return true
}

// categoryOf 是接口方法对应的类别。
func categoryOf(method string) string {
	switch {
	case method == "Login" || method == "Logout" || method == "Setup":
		return audit.Login
	case store.SessionActions[method]:
		return audit.Session
	case store.SensitiveReads[method]:
		return audit.Read
	case strings.HasPrefix(method, "mcp:") || method == "MCPToolDenied":
		return audit.MCP
	}
	return audit.Operation
}

// activityField 读请求里的一个字符串字段，没有时返回空。
func activityField(m proto.Message, names ...string) string {
	if m == nil {
		return ""
	}
	r := m.ProtoReflect()
	for _, name := range names {
		f := r.Descriptor().Fields().ByName(protoreflect.Name(name))
		if f == nil || f.IsList() || f.IsMap() || audit.Redacted(f) {
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

// target 从请求里找出操作的对象。
func (a *API) target(ctx context.Context, method string, req proto.Message) (typ, id, name string) {
	if d, ok := req.(*nagiv1.DeployRequest); ok {
		// 部署的目标是一个列表。
		names := make([]string, 0, len(d.GetTargets()))
		for _, c := range d.GetTargets() {
			names = append(names, c.GetContainer())
		}
		v := strings.Join(names, ", ")
		return "container", v, v
	}
	if v := activityField(req, "node_id"); v != "" {
		return "node", v, a.nodeName(ctx, v)
	}
	if v := activityField(req, "container"); v != "" {
		return "container", v, v
	}
	if v := activityField(req, "username"); v != "" {
		return "member", v, v
	}
	if v := activityField(req, "user_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			if u, err := a.Q.GetUserByID(ctx, id); err == nil {
				return "member", v, u.Username
			}
		}
		return "member", v, v
	}
	if v := activityField(req, "name"); v != "" {
		return "", v, v
	}
	if v := activityField(req, "id"); v != "" {
		return "", v, v
	}
	return "", "", ""
}

// auditActor 按 ctx 里的用户和请求来源填写记录的操作者和来源。
func auditActor(ctx context.Context, e *audit.Event) {
	u := auth.CurrentUser(ctx)
	m := auth.RequestMeta(ctx)
	if u.ID != 0 {
		e.ActorType, e.ActorID, e.ActorName = audit.ActorUser, strconv.FormatInt(u.ID, 10), u.Name
	}
	e.Via = audit.ViaWeb
	if u.TokenID != 0 {
		e.Via, e.TokenID = audit.ViaMCP, u.TokenID
	}
	e.Session = audit.SessionTag(u.SessionID)
	e.IP, e.UserAgent, e.Client = m.IP, m.UserAgent, m.Client
	if e.ParentID == 0 {
		e.ParentID = audit.Parent(ctx)
	}
}

// Record 写一条审计记录：操作者、来源和上级记录从 ctx 里取。
func (a *API) Record(ctx context.Context, e audit.Event) int64 {
	ctx = context.WithoutCancel(ctx)
	auditActor(ctx, &e)
	if e.Category == "" {
		e.Category = categoryOf(e.Action)
	}
	return a.Audit.Record(ctx, e)
}

// recordOperation 记一次接口调用（不走 Connect 的入口也用它）。failure 为空表示成功。
func (a *API) recordOperation(ctx context.Context, procedure string, req proto.Message, failure string) {
	result := audit.ResultOK
	if failure != "" {
		result = audit.ResultFailed
	}
	a.recordCall(ctx, procedure, req, result, failure, 0)
}

// recordingKey 把录制 ID 带进 ctx：打开终端的记录带上它，终端还开着时也能回放已经录下的部分。
type recordingKey struct{}

func withRecording(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, recordingKey{}, id)
}

func (a *API) recordCall(ctx context.Context, procedure string, req proto.Message, result, failure string, d time.Duration) {
	ctx = context.WithoutCancel(ctx)
	method := methodOf(procedure)
	e := audit.Event{
		Category:   categoryOf(method),
		Action:     method,
		ProjectID:  activityField(req, "project_id"),
		Params:     audit.Params(req),
		Result:     result,
		Error:      failure,
		DurationMS: d.Milliseconds(),
	}
	if id, _ := ctx.Value(recordingKey{}).(string); id != "" && result == audit.ResultOK {
		e.RecordingID = id
	}
	e.TargetType, e.TargetID, e.TargetName = a.target(ctx, method, req)
	// 文件操作的对象是文件：路径放进对象名，失败的也记，这样能看出有人试过什么路径。
	if path := activityField(req, "path"); path != "" && e.TargetType != "node" {
		e.TargetType, e.TargetID = "file", path
		if c := activityField(req, "container"); c != "" {
			e.TargetName = c + ":" + path
		} else {
			e.TargetName = path
		}
	}
	auditActor(ctx, &e)
	if e.ActorName == "" {
		e.ActorName = activityField(req, "username") // 登录、初始化时还没有会话
		if e.ActorName != "" {
			e.ActorType = audit.ActorUser
		}
	}
	a.Audit.Record(ctx, e)
	if result == audit.ResultOK {
		a.notifyChanges(ctx, method, req)
	}
}

// errorResult 把错误分成被拒绝（没登录、没权限）和失败。
func errorResult(err error) string {
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied, connect.CodeUnauthenticated:
		return audit.ResultDenied
	}
	return audit.ResultFailed
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

// readDedup 让轮询的敏感查询（GetProject 每 5 秒一次，带着环境变量的值）每个会话、每个项目每小时只记一条。
type readDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

const readDedupWindow = time.Hour

// pollingReads 是界面会轮询的敏感查询。
var pollingReads = map[string]bool{"GetProject": true, "ListProjects": true}

func (d *readDedup) first(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = map[string]time.Time{}
	}
	if t, ok := d.seen[key]; ok && now.Sub(t) < readDedupWindow {
		return false
	}
	if len(d.seen) > 10000 {
		for k, t := range d.seen {
			if now.Sub(t) >= readDedupWindow {
				delete(d.seen, k)
			}
		}
	}
	d.seen[key] = now
	return true
}

// activityLog 是记录操作的拦截器，放在登录检查之后。
type activityLog struct{ a *API }

func (l activityLog) skip(ctx context.Context, procedure string, req proto.Message, err error) bool {
	method := methodOf(procedure)
	if !pollingReads[method] || err != nil {
		return false
	}
	u := auth.CurrentUser(ctx)
	key := method + "\x00" + u.SessionID + "\x00" + strconv.FormatInt(u.TokenID, 10) + "\x00" + activityField(req, "project_id", "id")
	return !l.a.reads.first(key, time.Now())
}

func (l activityLog) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		res, err := next(ctx, req)
		procedure := req.Spec().Procedure
		if !activityRecorded(procedure) || unauthenticatedNoise(procedure, err) {
			return res, err
		}
		msg, _ := req.Any().(proto.Message)
		if l.skip(ctx, procedure, msg, err) {
			return res, err
		}
		result, failure := audit.ResultOK, ""
		if err != nil {
			result, failure = errorResult(err), activityFailure(err)
		}
		l.a.recordCall(ctx, procedure, msg, result, failure, time.Since(start))
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
		start := time.Now()
		w := &activityConn{StreamingHandlerConn: conn}
		err := next(ctx, w)
		result, failure := audit.ResultOK, w.failure
		if failure != "" {
			result = audit.ResultFailed
		}
		if err != nil {
			result, failure = errorResult(err), activityFailure(err)
		}
		l.a.recordCall(ctx, conn.Spec().Procedure, w.req, result, failure, time.Since(start))
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

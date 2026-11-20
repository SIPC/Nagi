package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
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
	nagiv1connect.AuthServiceMeProcedure:          true,
	nagiv1connect.AuthServiceSetLanguageProcedure: true,
	// 项目页排序只影响自己看到的顺序。
	nagiv1connect.AuthServiceGetProjectOrderProcedure: true,
	nagiv1connect.AuthServiceSetProjectOrderProcedure: true,
	nagiv1connect.AuthServiceGetNodeOrderProcedure:    true,
	nagiv1connect.AuthServiceSetNodeOrderProcedure:    true,
	nagiv1connect.SetupServiceGetSetupStateProcedure:  true,
	nagiv1connect.EnrollServiceEnrollProcedure:        true,
	// 安卓 App 的配对、换取令牌、退出在 auth 里记成安全事件（app_paired、app_unlock_required 等）；Info 是查询。
	nagiv1connect.AppServicePairProcedure:      true,
	nagiv1connect.AppServiceChallengeProcedure: true,
	nagiv1connect.AppServiceTokenProcedure:     true,
	nagiv1connect.AppServiceInfoProcedure:      true,
	nagiv1connect.AppServiceLogoutProcedure:    true,
	// 手机确认在 auth 里记（elevated_by_phone、app_approved、app_approval_denied、app_elevated、Login）；查询和轮询不记。
	nagiv1connect.AuthServiceBeginPhoneApprovalProcedure:    true,
	nagiv1connect.AuthServiceGetPhoneApprovalProcedure:      true,
	nagiv1connect.AuthServiceBeginLoginApprovalProcedure:    true,
	nagiv1connect.AuthServiceCompleteLoginApprovalProcedure: true,
	nagiv1connect.AppServiceListApprovalsProcedure:          true,
	nagiv1connect.AppServiceApproveProcedure:                true,
	nagiv1connect.AppServiceElevateProcedure:                true,
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
	case nagiv1connect.AuthServiceLoginProcedure, nagiv1connect.AuthServiceBeginLoginApprovalProcedure,
		nagiv1connect.AuthServiceCompleteLoginApprovalProcedure:
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
	if r, ok := req.(*nagiv1.ControlServiceRequest); ok {
		// 对象是容器，名称里带上服务，例如 stack/web。
		return "container", r.GetContainer(), r.GetContainer() + "/" + r.GetService()
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
	// 安卓 App：来源是 app，同一台手机的操作用设备 ID 的哈希串在一起，客户端写手机名称（见 docs/android.md#审计）。
	if u.DeviceID != "" {
		e.Via, e.Session, e.Client = audit.ViaApp, audit.SessionTag("app:"+u.DeviceID), u.DeviceName
	}
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
	a.recordCallWith(ctx, procedure, req, result, failure, d, nil)
}

// recordCallWith 同 recordCall，extra 合并进记录的参数（例如批量操作里失败的几项）。
func (a *API) recordCallWith(ctx context.Context, procedure string, req proto.Message, result, failure string, d time.Duration, extra map[string]any) {
	ctx = context.WithoutCancel(ctx)
	method := methodOf(procedure)
	e := audit.Event{
		Category:   categoryOf(method),
		Action:     method,
		ProjectID:  activityField(req, "project_id"),
		Params:     withExtraParams(req, extra),
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

// rawError 让 activityLog 拿到清理之前的报错：scopeCheck 在里面一层，返回给成员的报错已经把他看不到的节点
// 换成了「其他节点」，审计里要记原来的。
type rawError struct {
	err error
	// stream 是流式操作（部署、迁移等）在结果里返回的失败（见 streamOp）：发给浏览器的是按语言拼好的文字，
	// 审计里要记带节点标记的。
	stream error
}

type rawErrorKey struct{}

func noteRawError(ctx context.Context, err error) {
	if h, ok := ctx.Value(rawErrorKey{}).(*rawError); ok {
		h.err = err
	}
}

// noteStreamError 记下流式操作在结果里返回的失败。
func noteStreamError(ctx context.Context, err error) {
	if h, ok := ctx.Value(rawErrorKey{}).(*rawError); ok {
		h.stream = err
	}
}

func (h *rawError) or(err error) error {
	if h.err != nil {
		return h.err
	}
	return err
}

// auditFailure 是记进审计的失败详情：报错里提到的节点写成带 ID 的标记（i18n.Marked），显示时按查看的人
// 换成名称或「其他节点」（auditViewer.refs），不用在文字里找名称。
func auditFailure(err error) string {
	return i18n.Marked(func(hide func(i18n.Ref) (string, bool)) string {
		out, _ := redactError(err, hide)
		return activityFailure(out)
	})
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
		raw := &rawError{}
		res, err := next(context.WithValue(ctx, rawErrorKey{}, raw), req)
		procedure := req.Spec().Procedure
		// MCP 工具在进程内调用时，记下这次失败带引用的文字（见 failurenotes.go）；不记审计的查询也要。
		noteFailure(ctx, err, raw.or(err))
		// 修改成功后通知订阅流（见 overview.go）：部署、操作的结果、配置的修改。
		if err == nil && activityRecorded(procedure) {
			if l.a.Nodes != nil {
				l.a.Nodes.Changes.Bump()
			}
		}
		if !activityRecorded(procedure) || unauthenticatedNoise(procedure, err) {
			return res, err
		}
		msg, _ := req.Any().(proto.Message)
		if l.skip(ctx, procedure, msg, err) || err == nil && conflictProbe(res.Any()) {
			return res, err
		}
		result, failure := audit.ResultOK, ""
		var extra map[string]any
		if err != nil {
			result, failure = errorResult(err), auditFailure(raw.or(err))
		} else if extra = batchSummary(msg, res.Any()); extra != nil {
			// 批量操作（删除、清理镜像）：参数里记下成功、跳过、失败的数量和实际删除的前几项，再列出失败的几项。
			// 每一项都失败时记为失败；部分成功仍记为成功。
			if failed, all := batchFailures(msg, res.Any()); len(failed) > 0 {
				extra["failed"] = failed
				if all {
					result, failure = audit.ResultFailed, failed[0]
				}
			}
		}
		l.a.recordCallWith(ctx, procedure, msg, result, failure, time.Since(start), extra)
		return res, err
	}
}

func (l activityLog) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (l activityLog) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		raw := &rawError{}
		if !activityRecorded(conn.Spec().Procedure) {
			err := next(context.WithValue(ctx, rawErrorKey{}, raw), conn)
			noteFailure(ctx, err, raw.or(err))
			return err
		}
		start := time.Now()
		w := &activityConn{StreamingHandlerConn: conn, ctx: ctx, raw: raw}
		err := next(context.WithValue(ctx, rawErrorKey{}, raw), w)
		noteFailure(ctx, err, raw.or(err))
		if err == nil && w.probe {
			return nil
		}
		result, failure := audit.ResultOK, w.failure
		if failure != "" {
			result = audit.ResultFailed
			// 有原来的报错时记它（节点写成带 ID 的标记）：成员看到的文字是按他的范围换过的，直接记的话，
			// 管理员的操作失败会把范围外节点的名称留在记录里，成员自己的失败又记不下真实的节点。
			if raw.stream != nil {
				failure = auditFailure(raw.stream)
			}
		}
		if err != nil {
			result, failure = errorResult(err), auditFailure(raw.or(err))
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
	// probe：文件操作只返回了同名的名字，什么都没改（见 conflictProbe）。
	probe bool
	// ctx、raw 用来在结果发出去之前记下失败带引用的文字（MCP 工具要用，见 failurenotes.go）。
	ctx context.Context
	raw *rawError
}

// conflictProbe 判断回复是不是文件操作的同名探测：没有指定同名的处理方式时，节点发现同名就只返回名字，
// 不做任何改动；界面问过以后带着处理方式再调用一次，那一次才记审计。
func conflictProbe(m any) bool {
	switch v := m.(type) {
	case *nagiv1.FileOpResult:
		return len(v.GetConflicts()) > 0
	case *nagiv1.FileOpEvent:
		return len(v.GetResult().GetConflicts()) > 0
	}
	return false
}

func (c *activityConn) Receive(m any) error {
	err := c.StreamingHandlerConn.Receive(m)
	if err == nil && c.req == nil {
		c.req, _ = m.(proto.Message)
	}
	return err
}

func (c *activityConn) Send(m any) error {
	if conflictProbe(m) {
		c.probe = true
	}
	if ev, ok := m.(*nagiv1.DeployEvent); ok {
		if r := ev.GetResult(); r != nil && !r.GetSuccess() {
			c.failure = r.GetError()
			if c.raw != nil && c.raw.stream != nil {
				noteFailureText(c.ctx, c.failure, c.raw.stream)
			}
		}
	}
	return c.StreamingHandlerConn.Send(m)
}

// maxBatchFailures 是记录里最多列出的失败项。
const maxBatchFailures = 20

// batchFailures 取批量操作回复里失败的几项（简短的「对象：原因」），all 表示没有一项成功。
func batchFailures(req proto.Message, res any) (failed []string, all bool) {
	item := func(id, tag, reason string) string {
		obj := strings.TrimPrefix(id, "sha256:")
		obj = obj[:min(12, len(obj))]
		if tag != "" {
			obj = tag
		}
		if obj == "" {
			return clipActivity(reason, 120)
		}
		return clipActivity(obj+": "+reason, 120)
	}
	switch m := res.(type) {
	case *nagiv1.DeleteImagesResponse:
		for _, r := range m.GetResults() {
			if r.GetError() != "" {
				failed = append(failed, item(r.GetId(), r.GetTag(), r.GetError()))
			}
		}
		all = len(failed) > 0 && len(failed) == len(m.GetResults())
	case *nagiv1.PruneImagesResponse:
		cacheFailed := false
		for _, r := range m.GetFailed() {
			failed = append(failed, item(r.GetId(), r.GetTag(), r.GetError()))
			cacheFailed = cacheFailed || r.GetId() == ""
		}
		wantCache := false
		if p, ok := req.(*nagiv1.PruneNodeImagesRequest); ok {
			wantCache = p.GetBuildCache()
		}
		all = len(failed) > 0 && m.GetDeleted() == 0 && (!wantCache || cacheFailed)
	}
	if len(failed) > maxBatchFailures {
		failed = append(failed[:maxBatchFailures], fmt.Sprintf("+%d", len(failed)-maxBatchFailures))
	}
	return failed, all
}

// maxBatchListed 是批量操作的记录里最多列出的实际删除项。
const maxBatchListed = 20

// shortImage 是镜像在记录里的简称：12 位短 ID，只删标签时是标签。
func shortImage(id, tag string) string {
	if tag != "" {
		return tag
	}
	id = strings.TrimPrefix(id, "sha256:")
	return id[:min(12, len(id))]
}

// listHead 取前 maxBatchListed 项。
func listHead(items []string) []string {
	if len(items) > maxBatchListed {
		return items[:maxBatchListed]
	}
	return items
}

// batchSummary 是批量删除、清理镜像的结果摘要：请求了几项，删除、跳过、失败各几项，实际删除的前几项（审查 L4）。
// 请求本身的列表很长时会被截断（见 withExtraParams），摘要另外记，追查时看得到实际删了哪些。不是批量操作时返回 nil。
func batchSummary(req proto.Message, res any) map[string]any {
	switch m := res.(type) {
	case *nagiv1.DeleteImagesResponse:
		var deleted []string
		failed := 0
		for _, r := range m.GetResults() {
			if r.GetError() != "" {
				failed++
				continue
			}
			deleted = append(deleted, shortImage(r.GetId(), r.GetTag()))
		}
		out := map[string]any{"requested": len(m.GetResults()), "deleted": len(deleted), "failed_count": failed}
		if p, ok := req.(*nagiv1.DeleteNodeImagesRequest); ok {
			out["requested"] = len(p.GetImages())
		}
		if len(deleted) > 0 {
			out["deleted_items"] = listHead(deleted)
		}
		return out
	case *nagiv1.PruneImagesResponse:
		deleted := make([]string, 0, len(m.GetDeletedIds()))
		for _, id := range m.GetDeletedIds() {
			deleted = append(deleted, shortImage(id, ""))
		}
		out := map[string]any{"deleted": m.GetDeleted(), "skipped": m.GetSkipped(), "failed_count": len(m.GetFailed())}
		if p, ok := req.(*nagiv1.PruneNodeImagesRequest); ok {
			out["requested"] = len(p.GetIds())
		}
		if len(deleted) > 0 {
			out["deleted_items"] = listHead(deleted)
		}
		if m.GetBuildCacheFreed() > 0 {
			out["build_cache_freed"] = m.GetBuildCacheFreed()
		}
		return out
	}
	return nil
}

// shrinkable 是参数超长时可以缩短的列表：请求里的镜像列表和摘要里的列表。缩短后加上 "<键>_truncated"，总数另有记录。
var shrinkable = []string{"images", "ids", "deleted_items", "failed"}

// shrinkLongest 把 obj 里最长的可缩短列表砍掉一半（至少留 5 项），没有可以缩短的时返回 false。
func shrinkLongest(obj map[string]any) bool {
	key, longest := "", 5
	for _, k := range shrinkable {
		if l, ok := obj[k].([]any); ok && len(l) > longest {
			key, longest = k, len(l)
		} else if l, ok := obj[k].([]string); ok && len(l) > longest {
			key, longest = k, len(l)
		}
	}
	if key == "" {
		return false
	}
	switch l := obj[key].(type) {
	case []any:
		obj[key] = l[:max(5, len(l)/2)]
	case []string:
		obj[key] = l[:max(5, len(l)/2)]
	}
	obj[key+"_truncated"] = true
	return true
}

// withExtraParams 把 extra 合并进记录的参数（JSON 对象）。超过 audit.MaxParams 时先缩短最长的列表，
// 让 extra 里的数量和摘要留在记录里，实在放不下才整体截断。
func withExtraParams(req proto.Message, extra map[string]any) string {
	if len(extra) == 0 {
		return audit.Params(req)
	}
	params := audit.ParamsRaw(req)
	obj := map[string]any{}
	if params != "" && json.Unmarshal([]byte(params), &obj) != nil {
		return audit.TruncateParams(params)
	}
	for k, v := range extra {
		obj[k] = v
	}
	b, err := json.Marshal(obj)
	for err == nil && len(b) > audit.MaxParams && shrinkLongest(obj) {
		b, err = json.Marshal(obj)
	}
	if err != nil {
		return audit.TruncateParams(params)
	}
	return audit.TruncateParams(string(b))
}

package agent

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/SIPC/nagi/internal/agent/auditlog"
	"github.com/SIPC/nagi/internal/pkg/auditsink"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 节点本地审计（见 docs/audit.md#节点本地审计）：面板下发的修改类指令由拦截器统一记录，节点终端、node_exec
// 和被拒绝的越权请求在各自的处理函数里记（带上成员和来源地址）。

// openAudit 打开本地审计和外送。打不开时只记日志，节点照常工作。
func (a *agent) openAudit(ctx context.Context) {
	l, err := auditlog.Open(filepath.Join(a.cfg.DataDir, "audit"))
	if err != nil {
		a.log.Warn("无法打开节点本地审计", "err", err)
		return
	}
	a.audit = l
	a.auditShip = &auditlog.Shipper{
		Log: l, NodeID: a.nodeIDForAudit, ConfigPath: filepath.Join(a.cfg.DataDir, "identity", "auditsinks.json"),
		Logger: a.log.With("component", "audit"),
		Node:   func() string { return a.hostname },
		Trusted: func(fp string) bool {
			_, ok := a.id.panel(fp)
			return ok
		},
	}
	a.auditShip.Load()
	go a.auditShip.Run(ctx)
	go func() {
		<-ctx.Done()
		l.Close()
	}()
}

// auditSkip 是不记的方法：查询、上报、心跳，以及在处理函数里另外记的（终端和命令带着成员和来源地址）。
var auditSkip = map[string]bool{
	"StreamLogs": true, "Exec": false, "NodeShell": true, "NodeExec": true, "MeshPing": true, "MeshSpeedTest": true,
	"QueryMetrics": true, "CheckContainer": true, "CheckPorts": true, "CheckImages": true, "InspectComposeDir": true,
	"ReadNodeFile": true, "SetAuditSink": false,
}

var auditReadPrefixes = []string{"Get", "List", "Query", "Read", "Check", "Stream"}

// auditMethod 判断要不要记这个方法：修改类的都记。
func auditMethod(fullMethod string) (string, bool) {
	method := fullMethod[strings.LastIndexByte(fullMethod, '/')+1:]
	if skip, ok := auditSkip[method]; ok {
		return method, !skip
	}
	for _, p := range auditReadPrefixes {
		if strings.HasPrefix(method, p) {
			return method, false
		}
	}
	return method, true
}

// auditTarget 从请求里取出对象：项目/容器、路径、节点。
func auditTarget(req any) string {
	m, ok := req.(proto.Message)
	if !ok {
		return ""
	}
	var parts []string
	r := m.ProtoReflect()
	for _, name := range []protoreflect.Name{"project_id", "container", "path", "name", "version"} {
		f := r.Descriptor().Fields().ByName(name)
		if f == nil || f.Kind() != protoreflect.StringKind || f.Cardinality() == protoreflect.Repeated {
			continue
		}
		if v := r.Get(f).String(); v != "" {
			parts = append(parts, v)
		}
	}
	// 嵌套的容器引用（例如 PutContainerRequest.spec、ContainerRef）。
	for _, name := range []protoreflect.Name{"ref", "spec", "start"} {
		f := r.Descriptor().Fields().ByName(name)
		if f == nil || f.Kind() != protoreflect.MessageKind || !r.Has(f) {
			continue
		}
		if t := auditTarget(r.Get(f).Message().Interface()); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "/")
}

func auditResult(err error) (string, string) {
	if err == nil {
		return "ok", ""
	}
	if s, ok := status.FromError(err); ok {
		if s.Code() == codes.PermissionDenied || s.Code() == codes.Unauthenticated {
			return "denied", s.Message()
		}
		return "failed", s.Message()
	}
	return "failed", err.Error()
}

// auditInterceptors 记下面板 panel 下发的修改类指令。
func (a *agent) auditInterceptors(panel string) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		method, ok := auditMethod(info.FullMethod)
		resp, err := handler(ctx, req)
		if ok {
			result, msg := auditResult(err)
			a.audit.Record(auditlog.Event{Panel: panel, Action: method, Target: auditTarget(req), Result: result, Error: msg})
		}
		return resp, err
	}
	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		method, ok := auditMethod(info.FullMethod)
		if !ok {
			return handler(srv, ss)
		}
		w := &auditStream{ServerStream: ss}
		err := handler(srv, w)
		result, msg := auditResult(err)
		a.audit.Record(auditlog.Event{Panel: panel, Action: method, Target: auditTarget(w.first), Result: result, Error: msg})
		return err
	}
	return unary, stream
}

// auditStream 记下客户端发来的第一条消息（服务端流的请求就是它），用来取对象。
type auditStream struct {
	grpc.ServerStream
	first any
}

func (s *auditStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if err == nil && s.first == nil {
		s.first = m
	}
	return err
}

// SetAuditSink 保存面板下发的外送目标（见 docs/audit.md#节点本地审计）。
func (s *agentService) SetAuditSink(_ context.Context, req *nagiv1.SetAuditSinkRequest) (*nagiv1.SetAuditSinkResponse, error) {
	if s.node.auditShip == nil {
		return nil, status.Error(codes.Unavailable, "节点本地审计没有打开")
	}
	var sinks []auditsink.Config
	for _, k := range req.GetSinks() {
		sinks = append(sinks, auditsink.Config{
			ID: k.GetId(), Name: k.GetName(), Type: k.GetType(), Address: k.GetAddress(), CA: k.GetCa(), URL: k.GetUrl(),
			Headers: k.GetHeaders(), Endpoint: k.GetEndpoint(), Region: k.GetRegion(), Bucket: k.GetBucket(), Prefix: k.GetPrefix(),
			AccessKey: k.GetAccessKey(), SecretKey: k.GetSecretKey(),
		})
	}
	if err := s.node.auditShip.SetSinks(s.panel, sinks); err != nil {
		return nil, status.Errorf(codes.Internal, "无法保存外送配置：%v", err)
	}
	return &nagiv1.SetAuditSinkResponse{}, nil
}

// nodeIDForAudit 是外送时写的节点 ID：信任列表里第一个面板分配的（各面板分配的通常相同）。
func (a *agent) nodeIDForAudit() string {
	for _, p := range a.id.Panels() {
		if p.NodeID != "" {
			return p.NodeID
		}
	}
	return ""
}

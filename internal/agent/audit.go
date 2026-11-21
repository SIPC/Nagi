package agent

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/auditlog"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 节点本地审计（见 docs/audit.md#节点本地审计）：面板下发的修改类指令由拦截器统一记录，节点终端、node_exec
// 和被拒绝的越权请求在各自的处理函数里记（带上成员和来源地址）。

// openAudit 打开本地审计（见 docs/audit.md#节点本地审计）。打不开时只记日志，节点照常工作。外送由面板代为进行（ReadAuditLog）。
//
// 审计库在本机日志目录的 audit/node-audit.db（带哈希链），和节点业务库分开；没有本机日志库时退回数据目录。
// 旧版本的 JSON Lines（<数据目录>/audit/）先按原序号原样导入，升级确认（第一次连上面板）以后核对删除。
func (a *agent) openAudit(ctx context.Context) func() {
	legacy := filepath.Join(a.cfg.DataDir, "audit")
	path := filepath.Join(a.cfg.DataDir, "logs", "audit", "node-audit.db")
	if a.cfg.Logs != nil {
		path = auditlog.Path(a.cfg.Logs.Directory())
	}
	l, err := auditlog.Open(path, legacy)
	if err != nil {
		a.log.Warn("无法打开节点本地审计", "err", err)
		return func() {}
	}
	// 在接受任何请求之前导入：之后的新记录直接接着旧序号写进链里。没导入成的，新记录先照旧追加到旧文件，下次启动再导入。
	if err := l.ImportLegacy(ctx); err != nil {
		a.log.Error("节点旧审计文件导入未完成，旧文件保留并继续写入，下次启动重试", "err", err)
		a.cfg.Logs.SetMigration(logstore.Migration{ID: auditlog.CleanupID, Phase: "failed", Error: err.Error()})
	}
	through := l.Seq()
	a.audit = l
	l.Capture = func(e auditlog.Event) {
		a.cfg.Logs.Report(logstore.Event{Time: e.Time, Category: logstore.Audit, Kind: "node." + e.Action, Object: e.Target, Data: e})
	}
	if err := l.UseBudget(ctx, a.cfg.Logs); err != nil {
		a.log.Error("无法把节点审计接入统一本机容量", "err", err)
	}
	// 升级前节点自己外送，保存过面板的外送凭据：删掉。
	if err := l.MigrateShipping(a.auditSinksPath()); err != nil {
		a.log.Warn("无法删除旧的外送配置", "err", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); l.PruneLoop(ctx) }()
	go func() {
		defer wg.Done()
		if err := l.MigrateHistory(ctx, a.cfg.Logs, through); err != nil && ctx.Err() == nil {
			a.log.Error("节点审计的统一日志副本未导入完，保留已提交位置，下次启动继续", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-a.panelReached:
		}
		if err := l.CleanupLegacy(ctx, a.cfg.Logs); err != nil && ctx.Err() == nil {
			a.log.Error("节点旧审计文件未删除：核对没有通过，已保留，下次启动重试", "err", err)
		} else if err == nil && ctx.Err() == nil {
			a.log.Info("节点旧审计文件已核对并删除")
		}
	}()
	go func() { defer close(done); wg.Wait(); l.Close() }()
	return func() { cancel(); <-done }
}

// auditSkip 是不记的方法：查询、上报、心跳、轮询（升级期间面板每秒问一次的 UpgradeProgress），
// 以及在处理函数里另外记的（终端和命令带着成员和来源地址）。
var auditSkip = map[string]bool{
	"StreamLogs": true, "Exec": false, "NodeShell": true, "NodeExec": true, "MeshPing": true, "MeshSpeedTest": true,
	"QueryMetrics": true, "CheckContainer": true, "CheckPorts": true, "CheckImages": true, "InspectComposeDir": true,
	"ReadNodeFile": true, "SetAuditSink": false, "ReadAuditLog": true, "UpgradeProgress": true,
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
	for _, name := range []protoreflect.Name{"project_id", "container", "path", "name", "version", "image"} {
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
		started := time.Now()
		resp, err := handler(ctx, req)
		// Diagnostics are read-only and intentionally absent from the legacy
		// operation chain, but their measured result belongs to this node.
		if method == "MeshPing" || method == "MeshSpeedTest" {
			in, _ := req.(*nagiv1.MeshPingRequest)
			result, message := auditResult(err)
			peer, mode := in.GetPublicKey(), in.GetSpeedVia()
			data := map[string]any{"panel": panel, "method": method, "peer": hex.EncodeToString(peer[:min(len(peer), 32)]), "peer_bytes": len(peer), "mode": mode[:min(len(mode), 64)],
				"result": result, "duration_ms": time.Since(started).Milliseconds()}
			if err != nil {
				data["error"] = message
			} else {
				switch out := resp.(type) {
				case *nagiv1.MeshPingResponse:
					data["rtt_ms"] = out.GetRttMs()
				case *nagiv1.MeshSpeedResponse:
					data["upload_bytes_per_second"], data["download_bytes_per_second"] = out.GetUpload(), out.GetDownload()
					data["quic"], data["wireguard_tested"] = out.GetQuic(), out.GetWireguardTested()
					data["quic_off_here"], data["quic_off_peer"] = out.GetQuicOffHere(), out.GetQuicOffPeer()
					if out.GetWireguardTested() {
						data["wireguard_upload_bytes_per_second"], data["wireguard_download_bytes_per_second"] = out.GetWireguardUpload(), out.GetWireguardDownload()
					}
				}
			}
			a.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "mesh.diagnostic", Data: data})
		}
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

func (a *agent) auditSinksPath() string {
	return filepath.Join(a.cfg.DataDir, "identity", "auditsinks.json")
}

// SetAuditSink 是旧版本面板下发外送目标（带凭据）的接口。节点不再保存凭据（见 docs/audit.md#节点本地审计）：
// 收到什么都只删掉以前保存的，返回成功，旧面板照常工作。
func (s *agentService) SetAuditSink(context.Context, *nagiv1.SetAuditSinkRequest) (*nagiv1.SetAuditSinkResponse, error) {
	if err := os.Remove(s.node.auditSinksPath()); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "无法删除旧的外送配置：%v", err)
	}
	return &nagiv1.SetAuditSinkResponse{}, nil
}

// ReadAuditLog 返回本地审计，面板代为外送（见 docs/audit.md#节点本地审计）。信任列表里的面板都能读。
func (s *agentService) ReadAuditLog(_ context.Context, req *nagiv1.ReadAuditLogRequest) (*nagiv1.ReadAuditLogResponse, error) {
	l := s.node.audit
	if l == nil {
		return nil, status.Error(codes.Unavailable, "节点本地审计没有打开")
	}
	if req.GetAfter() < 0 {
		return &nagiv1.ReadAuditLogResponse{Start: l.Start()}, nil
	}
	lines, first, last := l.Read(req.GetAfter(), int(req.GetLimit()))
	return &nagiv1.ReadAuditLogResponse{Lines: lines, First: first, Last: last}, nil
}

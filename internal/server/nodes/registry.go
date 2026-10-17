// Package nodes 管理连到面板的节点：接受控制连接、记录节点上报的状态，并提供反向调用节点的客户端。
package nodes

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/tunnel"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// ErrOffline 表示节点当前没有连上面板。
var ErrOffline = errors.New("节点不在线")

// Registry 记录在线的节点。
type Registry struct {
	q   *store.Queries
	log *slog.Logger

	mu    sync.RWMutex
	conns map[string]*Conn
}

// Conn 是一个在线节点的控制连接。
type Conn struct {
	NodeID string
	// Agent 用于反向调用节点。
	Agent nagiv1.AgentServiceClient

	mu     sync.RWMutex
	report *nagiv1.ReportRequest
}

// LastReport 返回节点最近一次上报的内容，可能为 nil。
func (c *Conn) LastReport() *nagiv1.ReportRequest {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.report
}

// NewRegistry 创建节点注册表。
func NewRegistry(q *store.Queries, log *slog.Logger) *Registry {
	return &Registry{q: q, log: log, conns: map[string]*Conn{}}
}

// Get 返回在线节点的连接。
func (r *Registry) Get(nodeID string) (*Conn, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.conns[nodeID]
	if !ok {
		return nil, ErrOffline
	}
	return c, nil
}

// Online 判断节点是否在线。
func (r *Registry) Online(nodeID string) bool {
	_, err := r.Get(nodeID)
	return err == nil
}

// Handle 处理一条已完成 TLS 握手的节点控制连接，直到连接断开。
// 节点必须出示本面板签发的客户端证书，证书的 CommonName 就是节点 ID。
func (r *Registry) Handle(ctx context.Context, conn *tls.Conn) {
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		r.log.Warn("拒绝没有有效客户端证书的节点连接", "remote", conn.RemoteAddr())
		return
	}
	nodeID := state.PeerCertificates[0].Subject.CommonName
	node, err := r.q.GetNode(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && node.Revoked != 0) {
		r.log.Warn("拒绝未知或已移除的节点", "node", nodeID, "remote", conn.RemoteAddr())
		return
	}
	if err != nil {
		r.log.Error("读取节点失败", "node", nodeID, "err", err)
		return
	}

	sess, err := tunnel.Server(conn)
	if err != nil {
		return
	}
	defer sess.Close()

	c := &Conn{NodeID: nodeID}
	srv := grpc.NewServer()
	nagiv1.RegisterPanelServiceServer(srv, &panelService{r: r, conn: c})
	go tunnel.Serve(sess, srv)
	defer srv.Stop()

	cc, err := tunnel.Dial(sess)
	if err != nil {
		return
	}
	defer cc.Close()
	c.Agent = nagiv1.NewAgentServiceClient(cc)

	r.mu.Lock()
	if old, ok := r.conns[nodeID]; ok && old != c {
		r.log.Info("节点重新连接，替换旧连接", "node", nodeID)
	}
	r.conns[nodeID] = c
	r.mu.Unlock()
	r.log.Info("节点已连接", "node", nodeID, "remote", conn.RemoteAddr())

	select {
	case <-sess.CloseChan():
	case <-ctx.Done():
	}

	r.mu.Lock()
	if r.conns[nodeID] == c {
		delete(r.conns, nodeID)
	}
	r.mu.Unlock()
	r.touch(context.Background(), nodeID, c.LastReport())
	r.log.Info("节点已断开", "node", nodeID)
}

// touch 更新节点的最后在线时间和节点信息。
func (r *Registry) touch(ctx context.Context, nodeID string, report *nagiv1.ReportRequest) {
	var info []byte
	if report != nil && report.Node != nil {
		info, _ = proto.Marshal(report.Node)
	}
	if err := r.q.UpdateNodeSeen(ctx, store.UpdateNodeSeenParams{
		LastSeen: time.Now().Unix(), Info: info, ID: nodeID,
	}); err != nil {
		r.log.Warn("更新节点状态失败", "node", nodeID, "err", err)
	}
}

// panelService 实现 PanelService，处理某个节点的上报。
type panelService struct {
	nagiv1.UnimplementedPanelServiceServer
	r    *Registry
	conn *Conn
}

func (p *panelService) Report(ctx context.Context, req *nagiv1.ReportRequest) (*nagiv1.ReportResponse, error) {
	p.conn.mu.Lock()
	p.conn.report = req
	p.conn.mu.Unlock()

	p.r.touch(ctx, p.conn.NodeID, req)
	// 保存项目配置备份。以节点为准，面板只是备份。
	now := time.Now().Unix()
	for _, s := range req.GetProjects() {
		proj := s.GetProject()
		if proj == nil || proj.GetId() == "" {
			continue
		}
		data, err := proto.Marshal(proj)
		if err != nil {
			continue
		}
		if err := p.r.q.UpsertProjectBackup(ctx, store.UpsertProjectBackupParams{
			NodeID: p.conn.NodeID, ProjectID: proj.GetId(), Revision: proj.GetRevision(), Data: data, UpdatedAt: now,
		}); err != nil {
			p.r.log.Warn("保存项目配置备份失败", "node", p.conn.NodeID, "project", proj.GetId(), "err", err)
		}
	}
	return &nagiv1.ReportResponse{}, nil
}

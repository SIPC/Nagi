// Package nodes 管理连到面板的节点：接受控制连接、记录节点上报的状态，并提供反向调用节点的客户端。
package nodes

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/pkg/tunnel"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ErrOffline 表示节点当前没有连上面板。
var ErrOffline = errors.New("节点不在线")

// Registry 记录在线的节点。
type Registry struct {
	q      *store.Queries
	log    *slog.Logger
	sealer *sealer.Sealer
	ca     *tlsutil.CA

	mu       sync.RWMutex
	conns    map[string]*Conn
	mesh     *mesh.Mesh
	meshName string
	witness  *witness
}

// Conn 是一个在线节点的控制连接。
type Conn struct {
	NodeID string
	// Agent 用于反向调用节点。
	Agent nagiv1.AgentServiceClient

	mu     sync.RWMutex
	report *nagiv1.ReportRequest
	close  func()
	name   string
	remote netip.Addr
	mesh   *nagiv1.MeshInfo
}

// LastReport 返回节点最近一次上报的内容，可能为 nil。
func (c *Conn) LastReport() *nagiv1.ReportRequest {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.report
}

// NewRegistry 创建节点注册表。
// 项目配置备份里有密钥，用面板主密钥加密后再存。
func NewRegistry(q *store.Queries, log *slog.Logger, s *sealer.Sealer, ca *tlsutil.CA) *Registry {
	return &Registry{q: q, log: log, sealer: s, ca: ca, conns: map[string]*Conn{}}
}

// NodeCertValidity 是节点客户端证书的有效期。节点在到期前 30 天内连上面板时自动续期。
const NodeCertValidity = 365 * 24 * time.Hour

// BackupContext 是加密项目配置备份时绑定的上下文，防止把一条备份挪到别的项目下使用。
func BackupContext(nodeID, projectID string) []byte {
	return []byte(nodeID + "/" + projectID)
}

// BackupKeyContext 是加密项目快照密码时绑定的上下文。
func BackupKeyContext(nodeID, projectID string) []byte {
	return []byte(nodeID + "/" + projectID + "/backup.key")
}

// BackupKey 返回面板保存的项目快照密码。
func (r *Registry) BackupKey(ctx context.Context, nodeID, projectID string) (string, error) {
	sealed, err := r.q.GetProjectBackupKey(ctx, store.GetProjectBackupKeyParams{NodeID: nodeID, ProjectID: projectID})
	if err != nil || len(sealed) == 0 {
		return "", errors.New("面板上没有这个项目的快照密码（节点还没有上报过）")
	}
	plain, err := r.sealer.Open(sealed, BackupKeyContext(nodeID, projectID))
	if err != nil {
		return "", err
	}
	return string(plain), nil
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

// Disconnect 断开节点的当前连接（例如节点被移除时）。
func (r *Registry) Disconnect(nodeID string) {
	r.mu.RLock()
	c, ok := r.conns[nodeID]
	r.mu.RUnlock()
	if ok && c.close != nil {
		c.close()
	}
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

	c := &Conn{NodeID: nodeID, close: func() { sess.Close() }, name: node.Name}
	if ap, err := netip.ParseAddrPort(conn.RemoteAddr().String()); err == nil {
		c.remote = ap.Addr().Unmap()
	}
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
	go r.pushAlertConfig(ctx, c, nil)

	select {
	case <-sess.CloseChan():
	case <-ctx.Done():
	}

	r.mu.Lock()
	if r.conns[nodeID] == c {
		delete(r.conns, nodeID)
	}
	r.mu.Unlock()
	r.updateMesh()
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
	if req.GetMesh() != nil {
		p.conn.mesh = req.GetMesh()
	}
	p.conn.mu.Unlock()
	p.r.updateMesh()

	p.r.touch(ctx, p.conn.NodeID, req)
	// 保存项目配置备份。以节点为准，面板只是备份：节点上已经没有的项目，备份和 webhook 一并删掉。
	now := time.Now().Unix()
	reported := map[string]bool{}
	for _, s := range req.GetProjects() {
		reported[s.GetProject().GetId()] = true
	}
	if backups, err := p.r.q.ListProjectBackups(ctx, p.conn.NodeID); err == nil {
		for _, b := range backups {
			if !reported[b.ProjectID] {
				_ = p.r.q.DeleteProjectBackup(ctx, store.DeleteProjectBackupParams{NodeID: p.conn.NodeID, ProjectID: b.ProjectID})
			}
		}
	}
	if hooks, err := p.r.q.ListWebhookProjects(ctx, p.conn.NodeID); err == nil {
		for _, id := range hooks {
			if !reported[id] {
				_ = p.r.q.DeleteProjectWebhook(ctx, store.DeleteProjectWebhookParams{NodeID: p.conn.NodeID, ProjectID: id})
			}
		}
	}
	for _, s := range req.GetProjects() {
		proj := s.GetProject()
		if proj == nil || proj.GetId() == "" {
			continue
		}
		plain, err := proto.Marshal(proj)
		if err != nil {
			continue
		}
		data, err := p.r.sealer.Seal(plain, BackupContext(p.conn.NodeID, proj.GetId()))
		if err != nil {
			continue
		}
		if t := proj.GetWebhookToken(); t != "" {
			if err := p.r.q.UpsertWebhook(ctx, store.UpsertWebhookParams{
				TokenHash: hashToken(t), NodeID: p.conn.NodeID, ProjectID: proj.GetId(),
			}); err != nil {
				p.r.log.Warn("保存 webhook 失败", "node", p.conn.NodeID, "project", proj.GetId(), "err", err)
			}
		}
		var key []byte
		if k := req.GetBackupKeys()[proj.GetId()]; k != "" {
			key, _ = p.r.sealer.Seal([]byte(k), BackupKeyContext(p.conn.NodeID, proj.GetId()))
		}
		if err := p.r.q.UpsertProjectBackup(ctx, store.UpsertProjectBackupParams{
			NodeID: p.conn.NodeID, ProjectID: proj.GetId(), Revision: proj.GetRevision(), Data: data, UpdatedAt: now,
			BackupKey: key,
		}); err != nil {
			p.r.log.Warn("保存项目配置备份失败", "node", p.conn.NodeID, "project", proj.GetId(), "err", err)
		}
	}
	return &nagiv1.ReportResponse{Mesh: p.r.meshConfig()}, nil
}

// RenewCertificate 给节点续期证书。证书签名请求里的公钥必须就是这个节点的身份公钥。
func (p *panelService) RenewCertificate(ctx context.Context, req *nagiv1.RenewCertificateRequest) (*nagiv1.RenewCertificateResponse, error) {
	csr, err := x509.ParseCertificateRequest(req.GetCsr())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "证书签名请求无效")
	}
	if NodeID(csr.RawSubjectPublicKeyInfo) != p.conn.NodeID {
		return nil, status.Error(codes.PermissionDenied, "证书签名请求的公钥不属于这个节点")
	}
	cert, err := p.r.ca.SignCSR(req.GetCsr(), p.conn.NodeID, NodeCertValidity)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	p.r.log.Info("节点证书已续期", "node", p.conn.NodeID, "not_after", cert.NotAfter)
	return &nagiv1.RenewCertificateResponse{Certificate: cert.Raw}, nil
}

// NodeID 由公钥（SubjectPublicKeyInfo）计算节点 ID：SHA-256 的前 8 字节，十六进制。
// 同一个节点加入不同的面板，ID 都一样。
func NodeID(spki []byte) string {
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:8])
}

// hashToken 与 auth.HashToken 相同；这里单独写一份，避免 nodes 依赖 auth。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

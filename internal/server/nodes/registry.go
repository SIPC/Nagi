// Package nodes 管理连到面板的节点：接受控制连接、记录节点上报的状态，并提供反向调用节点的客户端。
package nodes

import (
	"bytes"
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

// ErrPending 表示节点是用接入码加入的，还没有批准。
var ErrPending = errors.New("节点是用接入码加入的，还没有批准。在节点页确认是自己的节点后批准，才能操作它")

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

	relayMu sync.Mutex
	relayed map[string][]time.Time // 每个节点最近代发告警的时间
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
	// meshKey 是节点加入时提交的 WireGuard 公钥，上报的 mesh 信息必须和它一致。
	meshKey []byte
	// pending：用接入码加入、还没有批准的节点。
	pending bool

	domainSent bool
}

// Pending 返回节点是否在等管理员批准。
func (c *Conn) Pending() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pending
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
// 还没有批准的节点返回 ErrPending：不能对它做任何操作。
func (r *Registry) Get(nodeID string) (*Conn, error) {
	c, err := r.Peek(nodeID)
	if err != nil {
		return nil, err
	}
	if c.Pending() {
		return nil, ErrPending
	}
	return c, nil
}

// Peek 返回节点的连接，不管是否已经批准。只用来显示节点的信息。
func (r *Registry) Peek(nodeID string) (*Conn, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.conns[nodeID]
	if !ok {
		return nil, ErrOffline
	}
	return c, nil
}

// Approve 批准用接入码加入的节点：之后照常进 mesh、下发告警配置、可以操作。
func (r *Registry) Approve(ctx context.Context, nodeID string) error {
	n, err := r.q.ApproveNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("没有这个节点")
	}
	if c, err := r.Peek(nodeID); err == nil {
		c.mu.Lock()
		c.pending = false
		// 马上用最近一次上报的 mesh 信息加入 mesh，不用等下一次上报。
		if m := c.report.GetMesh(); m != nil && bytes.Equal(m.GetPublicKey(), c.meshKey) {
			c.mesh = m
		}
		c.mu.Unlock()
		r.updateMesh()
		go r.pushAlertConfig(context.Background(), c, nil)
		go r.pushTrusted(context.Background(), c)
	}
	r.log.Info("节点已批准", "node", nodeID)
	return nil
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
	_, err := r.Peek(nodeID)
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

	c := &Conn{NodeID: nodeID, close: func() { sess.Close() }, name: node.Name, meshKey: node.MeshKey, pending: node.Pending != 0}
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
	if !c.pending {
		go r.pushAlertConfig(ctx, c, nil)
		go r.pushTrusted(ctx, c)
	}

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
	pending := p.conn.pending
	// mesh 信息只采用加入时登记的公钥：节点不能冒用别的节点的公钥（那样能抢走它经面板的中转、让它断网）。
	// 还没批准的节点不进 mesh。
	if m := req.GetMesh(); m != nil && !pending {
		if bytes.Equal(m.GetPublicKey(), p.conn.meshKey) {
			p.conn.mesh = m
		} else if p.conn.mesh == nil {
			p.r.log.Warn("节点上报的 WireGuard 公钥和加入时的不一致，不采用", "node", p.conn.NodeID)
		}
	}
	pushDomain := req.GetNode().GetEmbedded() && !p.conn.domainSent
	p.conn.domainSent = p.conn.domainSent || pushDomain
	p.conn.mu.Unlock()
	if pushDomain {
		// 面板自带的节点连上后，把面板域名告诉它（可能在节点离线时改过）。
		go p.r.pushDomain(p.conn)
	}
	p.r.updateMesh()

	p.r.touch(ctx, p.conn.NodeID, req)
	if pending {
		// 批准之前不登记它的项目备份和 webhook（webhook 令牌可能是从别的节点那里偷来的）。
		return &nagiv1.ReportResponse{}, nil
	}
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
	// 每个容器一个 webhook。节点上已经没有的项目或容器，令牌一并删掉。
	// 读取失败的项目（例如磁盘满了）上报的只有 ID，它的 webhook 和配置备份都保留原样。
	unreadable := map[string]bool{}
	for _, s := range req.GetProjects() {
		if s.GetError() != "" {
			unreadable[s.GetProject().GetId()] = true
		}
	}
	tokens := map[string]bool{}
	for _, s := range req.GetProjects() {
		for _, m := range s.GetProject().GetContainers() {
			if t := m.GetWebhookToken(); t != "" {
				tokens[hashToken(t)] = true
			}
		}
	}
	if hooks, err := p.r.q.ListNodeWebhooks(ctx, p.conn.NodeID); err == nil {
		for _, h := range hooks {
			if !tokens[h.TokenHash] && !unreadable[h.ProjectID] {
				_ = p.r.q.DeleteWebhook(ctx, h.TokenHash)
			}
		}
	}
	for _, s := range req.GetProjects() {
		proj := s.GetProject()
		if proj == nil || proj.GetId() == "" || s.GetError() != "" {
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
		for _, m := range proj.GetContainers() {
			t := m.GetWebhookToken()
			if t == "" {
				continue
			}
			// 令牌跟着项目走（顶替、迁移后不变），所以按令牌登记：项目换了节点，地址照常能用。
			// 令牌已经登记在别的节点上、而那个节点还在上报这个项目时不改：知道令牌的节点（例如备份节点）
			// 不能把部署请求抢到自己这里。项目真的搬走以后，原节点不再上报它，下一次上报就会改过来。
			if h, err := p.r.q.GetWebhook(ctx, hashToken(t)); err == nil && h.NodeID != p.conn.NodeID && p.r.reportsProject(h.NodeID, h.ProjectID) {
				continue
			}
			if err := p.r.q.UpsertWebhook(ctx, store.UpsertWebhookParams{
				TokenHash: hashToken(t), NodeID: p.conn.NodeID, ProjectID: proj.GetId(), Container: m.GetName(),
			}); err != nil {
				p.r.log.Warn("保存 webhook 失败", "node", p.conn.NodeID, "project", proj.GetId(), "container", m.GetName(), "err", err)
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

// reportsProject 判断节点是否在线并且最近一次上报里有这个项目。
func (r *Registry) reportsProject(nodeID, projectID string) bool {
	c, err := r.Peek(nodeID)
	if err != nil {
		return false
	}
	for _, s := range c.LastReport().GetProjects() {
		if s.GetProject().GetId() == projectID {
			return true
		}
	}
	return false
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

// settingDomain 和 api.SettingDomain 相同（面板绑定的域名）。
const settingDomain = "panel_domain"

func (r *Registry) pushDomain(c *Conn) {
	d, _ := r.q.GetSetting(context.Background(), settingDomain)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Agent.SetPanelDomain(ctx, &nagiv1.SetPanelDomainRequest{Domain: d}); err != nil {
		r.log.Warn("把面板域名告诉自带节点失败", "err", err)
	}
}

// PushPanelDomain 把新的面板域名告诉面板自带的节点（它的 Caddy 负责反代面板）。
func (r *Registry) PushPanelDomain(string) {
	r.mu.RLock()
	conns := make([]*Conn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.RUnlock()
	for _, c := range conns {
		if c.LastReport().GetNode().GetEmbedded() {
			go r.pushDomain(c)
		}
	}
}

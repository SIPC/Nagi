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

	"github.com/SIPC/nagi/internal/pkg/i18n"
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
var ErrOffline = i18n.New("节点不在线")

// ErrPending 表示节点是用接入码加入的，还没有批准。
var ErrPending = i18n.New("节点是用接入码加入的，还没有批准。在节点页确认是自己的节点后批准，才能操作它")

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

	// OnReport 在节点上报后调用（不阻塞上报）：面板据此给缓存过时的容器补推配置。
	OnReport func(c *Conn, req *nagiv1.ReportRequest)
	// Cluster 返回主备面板的信息，随上报的回复告诉节点（见 docs/standby.md）。
	Cluster func(ctx context.Context) *nagiv1.PanelCluster
	// Self 是本面板的公钥指纹。
	Self string
	// ReadOnly：本面板是备用面板，不给节点下发任何东西（告警配置、面板域名），只查询。
	ReadOnly bool
	// Behind 检查节点上报里的任期：返回 true 表示本面板已经落后（有了新的主面板），不再处理这次上报。
	Behind func(ctx context.Context, req *nagiv1.ReportRequest) bool
}

// Conns 返回所有在线节点的连接。
func (r *Registry) Conns() []*Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Conn, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, c)
	}
	return out
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
	// accepted：节点最近一次上报认本面板是主面板（见 docs/standby.md）。之前下发的修改会被节点拒绝。
	accepted bool
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
func NewRegistry(q *store.Queries, log *slog.Logger, s *sealer.Sealer, ca *tlsutil.CA) *Registry {
	return &Registry{q: q, log: log, sealer: s, ca: ca, conns: map[string]*Conn{}}
}

// NodeCertValidity 是节点客户端证书的有效期。节点在到期前 30 天内连上面板时自动续期。
const NodeCertValidity = 365 * 24 * time.Hour

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
		return i18n.New("没有这个节点")
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
		if r.OnReport != nil && c.LastReport() != nil {
			go r.OnReport(c, c.LastReport())
		}
	}
	r.log.Info("节点已批准", "node", nodeID)
	return nil
}

// Rename 修改节点在面板上的名称。节点在线时同时改连接上记的名字：mesh 成员列表里用它，
// 其他节点下次上报（最多 15 秒）就看到新名字；再重新下发告警配置，节点之后发的告警用新名字。
// 节点离线时，等它连上来上报时再下发（见 Report）。
func (r *Registry) Rename(ctx context.Context, nodeID, name string) error {
	n, err := r.q.RenameNode(ctx, store.RenameNodeParams{Name: name, ID: nodeID})
	if err != nil {
		return err
	}
	if n == 0 {
		return i18n.New("没有这个节点")
	}
	if c, err := r.Peek(nodeID); err == nil {
		c.mu.Lock()
		c.name = name
		push := !c.pending && c.accepted
		c.mu.Unlock()
		if push {
			go r.pushAlertConfig(context.Background(), c, nil)
		}
	}
	r.log.Info("节点已改名", "node", nodeID, "name", name)
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
	// 节点 ID 由节点的身份公钥算出：证书里写的 ID 必须和证书的公钥对得上，不能拿一张别的证书冒充别的节点。
	cert := state.PeerCertificates[0]
	nodeID := cert.Subject.CommonName
	if nodeID != NodeID(cert.RawSubjectPublicKeyInfo) {
		r.log.Warn("拒绝节点 ID 和公钥不一致的证书", "node", nodeID, "remote", conn.RemoteAddr())
		return
	}
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
	if ctx.Err() == nil {
		r.touch(context.Background(), nodeID, c.LastReport())
	}
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
	// 节点认的主面板不是本面板时（例如刚提升，节点还没收到新的任期），下发的东西都会被拒绝：
	// 等节点认了本面板再下发告警配置、面板域名、补推配置。节点收到新的主面板后会马上再上报一次。
	accepts := req.GetPrimary() == "" || req.GetPrimary() == p.r.Self
	pushAlerts := accepts && !p.conn.accepted && !pending
	p.conn.accepted = accepts
	pushDomain := accepts && p.conn.ownEmbedded(req) && !p.conn.domainSent
	p.conn.domainSent = p.conn.domainSent || pushDomain
	p.conn.mu.Unlock()
	if pushAlerts {
		go p.r.pushAlertConfig(context.Background(), p.conn, nil)
	}
	if pushDomain {
		// 面板自带的节点连上后，把面板域名告诉它（可能在节点离线时改过）。
		go p.r.pushDomain(p.conn)
	}
	p.r.updateMesh()

	p.r.touch(ctx, p.conn.NodeID, req)
	// 节点认的主面板不是本面板时，Behind 去核实（本面板可能已经落后）；这次不补推配置，
	// 但回复照常带上主备信息：节点可能只是还没收到本面板的新任期。
	if !pending && p.r.Behind != nil && p.r.Behind(ctx, req) {
		accepts = false
	}
	if !pending && accepts && p.r.OnReport != nil {
		go p.r.OnReport(p.conn, req)
	}
	if pending {
		return &nagiv1.ReportResponse{}, nil
	}
	resp := &nagiv1.ReportResponse{Mesh: p.r.meshConfig()}
	if p.r.Cluster != nil {
		resp.Cluster = p.r.Cluster(ctx)
	}
	return resp, nil
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

// SettingProxySecret 是自带节点的 Caddy 反代面板时带的 X-Nagi-Proxy 请求头的值（每个面板自己的，不随主备同步）。
const SettingProxySecret = "proxy_secret"

// settingDomain 和 api.SettingDomain 相同（面板绑定的域名）。
const settingDomain = "panel_domain"

func (r *Registry) pushDomain(c *Conn) {
	if r.ReadOnly {
		return
	}
	d, _ := r.q.GetSetting(context.Background(), settingDomain)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	secret, _ := r.q.GetSetting(context.Background(), SettingProxySecret)
	if _, err := c.Agent.SetPanelDomain(ctx, &nagiv1.SetPanelDomainRequest{Domain: d, ProxySecret: secret}); err != nil {
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
		if c.ownEmbedded(c.LastReport()) {
			go r.pushDomain(c)
		}
	}
}

// ownEmbedded 判断这是不是本面板自带的节点：自带的节点从本机（127.0.0.1）连面板。
// 主备面板时别的面板自带的节点也连着本面板，面板域名不能推给它（那台机器的 Caddy 反代的是那个面板）。
func (c *Conn) ownEmbedded(req *nagiv1.ReportRequest) bool {
	return req.GetNode().GetEmbedded() && c.remote.IsLoopback()
}

// PushAddress 把面板的新对外地址告诉在线的节点（节点只接受面板改自己的地址），之后节点用新地址重连。
// 本机自带的节点从 127.0.0.1 连，不改。离线的节点要在节点上用 nagi panel add 更新地址。
func (r *Registry) PushAddress(address, fingerprint string) {
	if r.ReadOnly {
		return
	}
	for _, c := range r.Conns() {
		if c.remote.IsLoopback() || c.Pending() {
			continue
		}
		go func(c *Conn) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := c.Agent.AddPanel(ctx, &nagiv1.AddPanelRequest{Address: address, Fingerprint: fingerprint}); err != nil {
				r.log.Warn("把面板的新地址告诉节点失败", "node", c.NodeID, "err", err)
			}
		}(c)
	}
}

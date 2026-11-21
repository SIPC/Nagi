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
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/pkg/tunnel"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ErrOffline 表示节点当前没有连上面板。Get、Peek 返回的是带节点名称的 OfflineError，errors.Is 能识别为 ErrOffline。
var ErrOffline = i18n.New("节点离线")

// OfflineError 是某个节点离线：消息里说明是哪个节点。名称在用到消息时才查（Get 常被用来判断在不在线）。
type OfflineError struct {
	ID   string
	name func() string
}

// NewOfflineError 返回节点离线的错误：id 是节点 ID，name 是显示的名称。
func NewOfflineError(id, name string) OfflineError {
	return OfflineError{ID: id, name: func() string { return name }}
}

func (e OfflineError) Name() string {
	if e.name != nil {
		return e.name()
	}
	return e.ID
}
func (e OfflineError) Error() string        { return "节点 " + e.Name() + " 离线" }
func (e OfflineError) Is(target error) bool { return target == ErrOffline }
func (e OfflineError) Localize(l i18n.Lang) string {
	return i18n.Sprintf(l, "节点 %s 离线", e.Name())
}

// Redact 按查看的人换掉节点的名称（见 i18n.Redact）。
func (e OfflineError) Redact(hide func(i18n.Ref) (string, bool)) (error, bool) {
	name, ok := hide(i18n.NodeRef(e.ID, e.Name()))
	if !ok {
		return e, false
	}
	return OfflineError{ID: e.ID, name: func() string { return name }}, true
}

// ErrPending 表示节点是用接入码加入的，还没有批准。
var ErrPending = i18n.New("节点通过接入码加入，尚未批准。请在节点页确认为自有机器后批准")

// Registry 记录在线的节点。
type Registry struct {
	q      *store.Queries
	log    *slog.Logger
	sealer *sealer.Sealer
	ca     *tlsutil.CA
	// Audit 是审计日志（见 docs/audit.md），节点和面板发出的告警记在这里。
	Audit *audit.Log
	// AlertLogs owns lifecycle checkpoints and human handling, never settings.
	AlertLogs *logstore.Store
	// AlertPrimary connects only to the current authenticated primary when standby.
	AlertPrimary func(context.Context) (*grpc.ClientConn, error)
	// LogCopies is local history, not part of standby configuration snapshots.
	LogCopies *logstore.Copies
	// AuthorizeLogPanel checks the current certificate-bound panel membership on
	// every mesh reconciliation RPC, independently of node upload permissions.
	AuthorizeLogPanel func(context.Context, mesh.Key) error
	// OnIsolate 在节点被隔离或解除隔离后调用：面板据此重算各节点的代理计划，撤销它的项目放行。
	OnIsolate func(ctx context.Context)
	iso       isolation

	mu    sync.RWMutex
	conns map[string]*Conn
	// sessions 是每个节点所有还没断开的控制连接（包括已被替换、正在断开的），移除节点时全部断开。
	sessions     map[string]map[*Conn]struct{}
	mesh         *mesh.Mesh
	meshName     string
	panelPeers   map[string]mesh.Peer
	witness      *witness
	meshUpdateMu sync.Mutex

	relayMu  sync.Mutex
	relayed  map[string][]time.Time // 每个节点最近代发告警的时间
	reported map[string][]time.Time // 节点上报告警的时间，用于限制频率
	// violations 是节点上报的越权记录的时间，限制每个节点能写多少条（见 ReportViolations）。
	violations map[string][]time.Time

	// states 是按节点 ID 记的状态（见 nodeState），跨控制连接保留。
	stateMu sync.Mutex
	states  map[string]*nodeState

	// Changes 通知订阅流有变化（见 changes.go）。
	Changes Changes
	alerts  activeAlerts
	// policyMu 串行修改告警的全局设置（修订号检查和保存在一起）。
	policyMu sync.Mutex
	// panelRules 是面板判断的规则的状态（见 panel_rules.go）。
	panelRules panelRules

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
	// directKey、directAddr：直连下载签名链接用的密钥和节点的公网地址（见 direct.go），这次连接上告诉过节点以后才有。
	directKey  []byte
	directAddr string
	directSent bool
	// directPush：同一时间只告诉节点一次密钥（连上时的和换密钥的可能同时），两边最后的密钥一样。
	directPush sync.Mutex

	// reporting 是这条连接上正在处理的上报。上报的额度、写库去重、OnReport 的排队按节点记（见 nodeState）。
	reporting atomic.Bool
}

// nodeState 是按节点 ID 记的状态。节点断开重连以后接着用：记在连接上的话，重连一次就有一份新的额度，
// 写库的去重也从头算，同一个节点还能在新旧连接上各跑一个 OnReport。
// 只有登记过的节点才有（Handle 先查库），每项很小（节点信息只记哈希）；节点被移除时删掉（见 Forget）。
type nodeState struct {
	mu sync.Mutex
	// 上报的令牌桶（见 allowReport）：dropped、droppedLog 记被丢弃的次数和上次记日志的时间。
	reportTokens float64
	reportAt     time.Time
	dropped      int
	droppedLog   time.Time
	// seen、seenInfo、seenAt：上次写进数据库的节点信息（SHA-256）和时间（见 touch）。
	seen     bool
	seenInfo [sha256.Size]byte
	seenAt   time.Time
	// 同一个节点同时只跑一个 OnReport，跑的时候来的上报只留最新的一份和它所在的连接（见 runOnReport）。
	syncing  bool
	syncConn *Conn
	syncNext *nagiv1.ReportRequest
}

// state 返回节点的状态，没有时新建。
func (r *Registry) state(nodeID string) *nodeState {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.states == nil {
		r.states = map[string]*nodeState{}
	}
	st := r.states[nodeID]
	if st == nil {
		st = &nodeState{}
		r.states[nodeID] = st
	}
	return st
}

// Pending 返回节点是否在等管理员批准。
func (c *Conn) Pending() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pending
}

// trusted 判断能不能给这个节点下发告警配置、代发它的本地审计：已经批准，并且认本面板是主面板。
// 待批准的节点可能是拿泄露的接入码加入的，告警配置里有渠道地址（见 docs/control-plane.md）。
func (c *Conn) trusted() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !c.pending && c.accepted
}

// Trusted 判断节点是否已经批准、并且认本面板是主面板。
func (c *Conn) Trusted() bool { return c.trusted() }

// LastReport 返回节点最近一次上报的内容，可能为 nil。
func (c *Conn) LastReport() *nagiv1.ReportRequest {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.report
}

// PatchBackup 把节点刚回答的一个备份节点的同步状态（「立即同步」「停止同步」时 SyncBackup 返回的）先合进节点最近一次的上报，
// 界面马上就能看到「同步中」「已停止」，不用等节点下一次上报（最多 5 秒，再加上界面刷新的间隔）。下一次上报照常整个替换它。
// 上报被替换成新的副本（不改原来的）：别处拿到的旧上报不会在读的时候被改。
func (r *Registry) PatchBackup(nodeID, projectID, container string, st *nagiv1.BackupStatus) {
	if st == nil || st.GetTargetNodeId() == "" {
		return
	}
	c, err := r.Peek(nodeID)
	if err != nil {
		return
	}
	c.mu.Lock()
	patched := false
	if c.report != nil {
		for i, cs := range c.report.GetContainers() {
			if cs.GetProjectId() != projectID || cs.GetName() != container {
				continue
			}
			for j, b := range cs.GetBackups() {
				if b.GetTargetNodeId() != st.GetTargetNodeId() {
					continue
				}
				report := proto.Clone(c.report).(*nagiv1.ReportRequest)
				report.Containers[i].Backups[j] = proto.Clone(st).(*nagiv1.BackupStatus)
				c.report, patched = report, true
				break
			}
			break
		}
	}
	c.mu.Unlock()
	if patched {
		r.Changes.Bump()
	}
}

// NewRegistry 创建节点注册表。
func NewRegistry(q *store.Queries, log *slog.Logger, s *sealer.Sealer, ca *tlsutil.CA) *Registry {
	return &Registry{q: q, log: log, sealer: s, ca: ca, conns: map[string]*Conn{}, sessions: map[string]map[*Conn]struct{}{}}
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
	c, ok := r.conns[nodeID]
	r.mu.RUnlock()
	if !ok {
		return nil, r.offline(nodeID)
	}
	return c, nil
}

// offline 返回带节点名称的离线错误。
func (r *Registry) offline(nodeID string) error {
	return OfflineError{ID: nodeID, name: func() string {
		if r.q != nil {
			if n, err := r.q.GetNode(context.Background(), nodeID); err == nil && n.Name != "" {
				return n.Name
			}
		}
		return nodeID
	}}
}

// Approve 批准用接入码加入的节点：之后照常进 mesh、下发告警配置、可以操作。
func (r *Registry) Approve(ctx context.Context, nodeID string) error {
	n, err := r.q.ApproveNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if n == 0 {
		return i18n.New("节点不存在")
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
		if last := c.LastReport(); last != nil {
			r.runOnReport(c, last)
		}
	}
	r.Changes.Bump()
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
		return i18n.New("节点不存在")
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

// Disconnect 断开节点所有的控制连接（例如节点被移除时）。之后这个节点的调用一律拒绝，直到它重新连上。
func (r *Registry) Disconnect(nodeID string) {
	r.mu.Lock()
	all := make([]*Conn, 0, len(r.sessions[nodeID])+1)
	for c := range r.sessions[nodeID] {
		all = append(all, c)
	}
	if c, ok := r.conns[nodeID]; ok {
		if _, listed := r.sessions[nodeID][c]; !listed {
			all = append(all, c)
		}
		delete(r.conns, nodeID)
	}
	r.mu.Unlock()
	for _, c := range all {
		if c.close != nil {
			c.close()
		}
	}
	r.Changes.Bump()
}

// current 判断 c 是不是节点当前的控制连接。被替换或已经断开的连接上的调用一律拒绝。
func (r *Registry) current(c *Conn) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conns[c.NodeID] == c
}

var errStale = status.Error(codes.Unavailable, "这条控制连接已被替换或节点已移除")

// onlyCurrent 返回只放行节点当前控制连接上的调用的拦截器。
func (r *Registry) onlyCurrent(c *Conn) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if !r.current(c) {
				return nil, errStale
			}
			return handler(ctx, req)
		}, func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if !r.current(c) {
				return errStale
			}
			return handler(srv, ss)
		}
}

// 面板接受节点调用的限制。节点只开一个 gRPC 客户端连接（见 tunnel.Serve 的通道上限），
// 同时进行的调用通常不超过 5 个（上报、mesh 中转、告警、越权上报、续期），超出的在节点那边排队。
// 消息大小沿用 gRPC 的默认上限：上报里每个容器一项，大小随容器数增长，改小了容器多的节点会每次上报都失败。
const (
	maxRecvMsgSize       = 4 << 20
	maxConcurrentStreams = 16
	// 节点每 15 秒上报一次，正常的连接不会空闲这么久。
	maxConnectionIdle = 5 * time.Minute
)

// 待批准节点同时在线的控制连接上限（见 addSession）。多次接入码每用一次就多一个待批准的节点身份，
// 每个身份各有一条控制连接和它的通道、流配额；不限制时持有接入码的一方能按身份数叠加占用面板的资源。
// 已批准的节点不受影响。
const (
	maxPendingSessions  = 16
	maxPendingPerSource = 4
	pendingSourceV6Bits = 64
)

// pendingSource 是按来源计数时的来源：IPv4 按地址，IPv6 按 /64（一台机器通常能用整个 /64）。
func pendingSource(a netip.Addr) netip.Prefix {
	if a.Is6() && !a.Is4In6() {
		p, _ := a.Prefix(pendingSourceV6Bits)
		return p
	}
	p, _ := a.Unmap().Prefix(32)
	return p
}

// pendingFull 判断 c（待批准）能不能再登记：别的待批准节点的连接已经到了全局或同一来源的上限就不能。
// 同一个节点的旧连接不算，它会被这条连接替换。调用方持有 r.mu。
func (r *Registry) pendingFull(c *Conn) bool {
	src := pendingSource(c.remote)
	total, same := 0, 0
	for id, set := range r.sessions {
		if id == c.NodeID {
			continue
		}
		for x := range set {
			x.mu.RLock()
			pending, remote := x.pending, x.remote
			x.mu.RUnlock()
			if !pending {
				continue
			}
			total++
			if pendingSource(remote) == src {
				same++
			}
		}
	}
	return total >= maxPendingSessions || same >= maxPendingPerSource
}

// addSession 登记节点的一条控制连接，返回被它替换的旧连接。待批准节点的连接超过上限时不登记，ok 为 false。调用方持有 r.mu。
func (r *Registry) addSession(c *Conn) (old *Conn, ok bool) {
	if c.Pending() && r.pendingFull(c) {
		return nil, false
	}
	if r.conns == nil {
		r.conns = map[string]*Conn{}
	}
	if r.sessions == nil {
		r.sessions = map[string]map[*Conn]struct{}{}
	}
	if r.sessions[c.NodeID] == nil {
		r.sessions[c.NodeID] = map[*Conn]struct{}{}
	}
	r.sessions[c.NodeID][c] = struct{}{}
	old = r.conns[c.NodeID]
	r.conns[c.NodeID] = c
	return old, true
}

// removeSession 去掉节点的一条控制连接，返回它是不是当时的当前连接。
func (r *Registry) removeSession(c *Conn) (wasCurrent bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions[c.NodeID], c)
	if len(r.sessions[c.NodeID]) == 0 {
		delete(r.sessions, c.NodeID)
	}
	if r.conns[c.NodeID] == c {
		delete(r.conns, c.NodeID)
		return true
	}
	return false
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
	cc, err := tunnel.Dial(sess) // 不马上建立连接，第一次调用时才打开通道
	if err != nil {
		return
	}
	defer cc.Close()
	c.Agent = nagiv1.NewAgentServiceClient(cc)

	// 先登记成当前连接再开始处理节点的调用：节点连上后马上就会上报，不是当前连接的调用会被拒绝。
	// 同一个节点的旧连接立即断开：被攻破的节点不能留着旧连接，在移除以后继续调用面板。
	// 待批准节点的连接超过上限时拒绝这条新连接（不动已有的连接）。
	r.mu.Lock()
	old, ok := r.addSession(c)
	r.mu.Unlock()
	if !ok {
		r.log.Warn("待批准的节点连接过多，拒绝新连接", "node", nodeID, "remote", conn.RemoteAddr())
		return
	}
	defer func() {
		r.removeSession(c)
		r.Changes.Bump()
		r.updateMesh()
	}()
	if old != nil {
		r.log.Info("节点重新连接，断开旧连接", "node", nodeID)
		if old.close != nil {
			old.close()
		}
	}
	// 登记以后再查一次：上面查过以后、登记之前移除的节点，Disconnect 断不到这条连接。
	if !r.stillEnrolled(ctx, nodeID) {
		r.log.Warn("拒绝未知或已移除的节点", "node", nodeID, "remote", conn.RemoteAddr())
		return
	}

	unary, streamGuard := r.onlyCurrent(c)
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unary), grpc.ChainStreamInterceptor(streamGuard),
		grpc.MaxRecvMsgSize(maxRecvMsgSize),
		grpc.MaxConcurrentStreams(maxConcurrentStreams),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: maxConnectionIdle}),
	)
	nagiv1.RegisterPanelServiceServer(srv, &panelService{r: r, conn: c})
	go tunnel.Serve(sess, srv)
	defer srv.Stop()

	r.Changes.Bump()
	r.log.Info("节点已连接", "node", nodeID, "remote", conn.RemoteAddr())

	select {
	case <-sess.CloseChan():
	case <-ctx.Done():
	}

	// 被替换的旧连接不写：它的节点信息是旧的，最后在线时间由新连接记。
	if ctx.Err() == nil && r.current(c) {
		r.touch(context.Background(), c, c.LastReport(), true)
	}
	r.log.Info("节点已断开", "node", nodeID)
}

// enrollRetry 是登记以后复查节点时，数据库出错后等多久再查一次。
var enrollRetry = 200 * time.Millisecond

// stillEnrolled 复查节点还在、没有被移除。只有节点不存在或已吊销时返回 false；数据库临时出错时重试一次，
// 不然新连接登记时已经断开了旧连接，一次读库失败就会让节点两条连接都没了。重试也失败时拒绝。
func (r *Registry) stillEnrolled(ctx context.Context, nodeID string) bool {
	for attempt := 0; ; attempt++ {
		node, err := r.q.GetNode(ctx, nodeID)
		switch {
		case err == nil:
			return node.Revoked == 0
		case errors.Is(err, sql.ErrNoRows):
			return false
		case attempt > 0 || ctx.Err() != nil:
			r.log.Error("读取节点失败", "node", nodeID, "err", err)
			return false
		}
		r.log.Warn("读取节点失败，稍后重试", "node", nodeID, "err", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(enrollRetry):
		}
	}
}

// touchInterval：节点信息没变时，最后在线时间最多这么久写一次库。在线节点的状态看连接，最后在线时间只在离线以后用到。
// closeTouchInterval：连接断开时也记一次最后在线时间，但离上次写库不到这么久就不写，反复重连的节点不能每次都写库。
const (
	touchInterval      = time.Minute
	closeTouchInterval = 5 * time.Second
)

// touch 更新节点的最后在线时间和节点信息。节点信息没变、离上次写库不到 touchInterval 时不写；
// closing（连接断开）时间隔改成 closeTouchInterval，这条连接上没有上报过就不写（没有新的内容，写了会清掉库里的节点信息）。
// 去重按节点记，不随连接重置。
func (r *Registry) touch(ctx context.Context, c *Conn, report *nagiv1.ReportRequest, closing bool) {
	if closing && report == nil {
		return
	}
	var info []byte
	if report != nil && report.Node != nil {
		info, _ = proto.MarshalOptions{Deterministic: true}.Marshal(report.Node)
	}
	sum := sha256.Sum256(info)
	gap := touchInterval
	if closing {
		gap = closeTouchInterval
	}
	now := time.Now()
	st := r.state(c.NodeID)
	st.mu.Lock()
	if st.seen && sum == st.seenInfo && now.Sub(st.seenAt) < gap {
		st.mu.Unlock()
		return
	}
	st.seen, st.seenInfo, st.seenAt = true, sum, now
	st.mu.Unlock()
	if err := r.q.UpdateNodeSeen(ctx, store.UpdateNodeSeenParams{
		LastSeen: now.Unix(), Info: info, ID: c.NodeID,
	}); err != nil {
		r.log.Warn("更新节点状态失败", "node", c.NodeID, "err", err)
		st.mu.Lock()
		st.seen = false // 下次再写
		st.mu.Unlock()
	}
}

// 节点上报的限制。节点每 15 秒上报一次；容器状态变了、备份同步有进度（每 5 秒）时马上再报，
// 相邻两次至少隔 300 毫秒。令牌桶能攒 reportBurst 次，每 reportRefill 补一次：正常节点远远用不完，
// 不停上报的节点每秒最多处理一次，超出的丢弃（回复空的结果，不断开连接）。额度按节点记，重连不会补满。
const (
	reportBurst  = 30
	reportRefill = time.Second
	// maxNodeInfoSize 是上报里节点信息的大小上限，超过的不采用（沿用上一次的）。正常的节点信息只有几百字节。
	maxNodeInfoSize = 64 << 10
	// maxHostnameLen 是主机名的长度上限（DNS 名称最长 253 个字符，Linux 主机名最长 64 个）。
	maxHostnameLen = 253
	// droppedLogInterval：丢弃上报时最多这么久记一次日志。
	droppedLogInterval = time.Minute
)

// allowReport 从令牌桶里取一次上报的额度。没有额度时 ok 为 false；该记日志时 logDropped 是这段时间丢弃的次数。
func (s *nodeState) allowReport(now time.Time) (ok bool, logDropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reportAt.IsZero() {
		s.reportTokens = reportBurst
	} else {
		s.reportTokens = min(reportBurst, s.reportTokens+float64(now.Sub(s.reportAt))/float64(reportRefill))
	}
	s.reportAt = now
	if s.reportTokens >= 1 {
		s.reportTokens--
		return true, 0
	}
	s.dropped++
	if now.Sub(s.droppedLog) >= droppedLogInterval {
		n := s.dropped
		s.dropped, s.droppedLog = 0, now
		return false, n
	}
	return false, 0
}

// runOnReport 调用 OnReport（不阻塞）。同一个节点同时只跑一个，按节点 ID 算，不管上报来自哪条连接：
// 跑的时候来的上报只留最新的一份，跑完接着用它再跑一次。留下的那份所在的连接已被替换或断开时不再跑。
func (r *Registry) runOnReport(c *Conn, req *nagiv1.ReportRequest) {
	if r.OnReport == nil {
		return
	}
	st := r.state(c.NodeID)
	st.mu.Lock()
	if st.syncing {
		st.syncConn, st.syncNext = c, req
		st.mu.Unlock()
		return
	}
	st.syncing = true
	st.mu.Unlock()
	go func() {
		for req != nil {
			r.OnReport(c, req)
			st.mu.Lock()
			c, req = st.syncConn, st.syncNext
			st.syncConn, st.syncNext = nil, nil
			if req != nil && !r.current(c) {
				req = nil
			}
			if req == nil {
				st.syncing = false
			}
			st.mu.Unlock()
		}
	}()
}

// panelService 实现 PanelService，处理某个节点的上报。
type panelService struct {
	nagiv1.UnimplementedPanelServiceServer
	r    *Registry
	conn *Conn
}

func (p *panelService) Report(ctx context.Context, req *nagiv1.ReportRequest) (*nagiv1.ReportResponse, error) {
	// 节点一条连接上的上报是一次接一次的，不会同时有两个。
	if !p.conn.reporting.CompareAndSwap(false, true) {
		return nil, status.Error(codes.ResourceExhausted, "上一次上报还在处理")
	}
	defer p.conn.reporting.Store(false)
	if ok, n := p.r.state(p.conn.NodeID).allowReport(time.Now()); !ok {
		if n > 0 {
			p.r.log.Warn("节点上报过于频繁，丢弃", "node", p.conn.NodeID, "dropped", n)
		}
		return &nagiv1.ReportResponse{}, nil
	}
	// 节点信息过大或主机名过长：不采用，沿用上一次的。节点信息每次变了都要写库。
	if n := req.GetNode(); n != nil && (len(n.GetHostname()) > maxHostnameLen || proto.Size(n) > maxNodeInfoSize) {
		p.r.log.Warn("节点上报的节点信息过大，不采用", "node", p.conn.NodeID, "size", proto.Size(n))
		req.Node = p.conn.LastReport().GetNode()
	}
	// 上报的直连地址要转给所有节点，先整理好再存：每次上报都要给所有节点重算成员列表。
	if m := req.GetMesh(); m != nil {
		eps, ok := cleanEndpoints(m.GetEndpoints())
		if !ok {
			p.r.log.Warn("节点上报的直连地址过多，不采用", "node", p.conn.NodeID, "count", len(m.GetEndpoints()))
		}
		m.Endpoints = eps
	}
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
	if p.conn.takeDirectPush(req, pending, accepts) {
		go p.r.pushDirect(p.conn)
	}
	if pushDomain {
		// 面板自带的节点连上后，把面板域名告诉它（可能在节点离线时改过）。
		go p.r.pushDomain(p.conn)
	}
	p.r.updateMesh()

	p.r.touch(ctx, p.conn, req, false)
	p.r.Changes.Bump()
	if t := req.GetNodeTime(); t != nil && !pending {
		go p.r.checkClock(context.WithoutCancel(ctx), p.conn, t.AsTime(), time.Now())
	}
	// 节点认的主面板不是本面板时，Behind 去核实（本面板可能已经落后）；这次不补推配置，
	// 但回复照常带上主备信息：节点可能只是还没收到本面板的新任期。
	if !pending && p.r.Behind != nil && p.r.Behind(ctx, req) {
		accepts = false
	}
	// 被隔离的节点的上报只记录、不采用（见 docs/mesh.md#隔离）。
	if !pending && accepts && p.r.OnReport != nil && !p.r.Isolated(ctx, p.conn.NodeID) {
		p.r.runOnReport(p.conn, req)
	}
	if pending {
		return &nagiv1.ReportResponse{}, nil
	}
	resp := &nagiv1.ReportResponse{Mesh: p.r.meshConfig(p.conn)}
	if !p.r.ReadOnly && accepts && !p.r.Isolated(ctx, p.conn.NodeID) && req.GetAlerts() != nil && p.r.AlertLogs != nil {
		if err := p.r.syncAlerts(ctx, p.conn.NodeID, req.Alerts, false); err != nil {
			p.r.log.Warn("保存告警快照失败", "node", p.conn.NodeID, "err", err)
		} else if !p.r.ReadOnly && accepts {
			resp.AlertHandling, _ = p.r.AlertStates(ctx, p.conn.NodeID, false)
		}
	}
	if p.r.Cluster != nil {
		resp.Cluster = p.r.Cluster(ctx)
	}
	return resp, nil
}

// RenewCertificate 给节点续期证书。证书签名请求里的公钥必须就是这个节点的身份公钥。
func (p *panelService) RenewCertificate(ctx context.Context, req *nagiv1.RenewCertificateRequest) (*nagiv1.RenewCertificateResponse, error) {
	csr, err := x509.ParseCertificateRequest(req.GetCsr())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "节点的证书请求无法解析")
	}
	if NodeID(csr.RawSubjectPublicKeyInfo) != p.conn.NodeID {
		return nil, status.Error(codes.PermissionDenied, "证书请求中的公钥不属于此节点")
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

// Source 返回面板看到的这条连接的来源地址；从本机连来（或者没有地址）时 ok 为 false。
// 只看连接本身，不看节点上报的内容：节点自称是面板自带的，也改不了显示的来源。
func (c *Conn) Source() (addr netip.Addr, ok bool) {
	ip := c.Remote()
	return ip, ip.IsValid() && !ip.IsLoopback()
}

// OwnEmbedded 判断这是不是本面板自带的节点（和本面板在同一台机器上）。
func (c *Conn) OwnEmbedded() bool {
	r := c.LastReport()
	return r != nil && c.ownEmbedded(r)
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

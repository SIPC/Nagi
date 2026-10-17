package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// meshNode 是节点的 mesh：成员列表来自每个信任的面板（面板自己和它管理的在线节点），合并后使用。
// 只和共同面板下的成员互联；跨面板合并成一个大 mesh 等有功能需要时再做。
type meshNode struct {
	m   *mesh.Mesh
	log interface {
		Warn(string, ...any)
	}

	mu        sync.Mutex
	members   map[string][]*nagiv1.MeshPeer // 面板指纹 → 这个面板告诉我们的成员
	panelKeys map[string]mesh.Key           // 面板指纹 → 面板的 WireGuard 公钥
	panelAddr map[string]string             // 面板指纹 → 面板地址（host:port），面板的 UDP 用同一个端口
	names     map[mesh.Key]string
	file      string // 成员列表写在这里，节点重启后面板不在也能联系其他成员（顶替投票要用）
}

type savedPanel struct {
	Key     []byte            `json:"key"`
	Address string            `json:"address"`
	Peers   []json.RawMessage `json:"peers"`
}

// save 把各面板告诉我们的成员列表写盘。调用方持有 n.mu。
func (n *meshNode) save() {
	out := map[string]savedPanel{}
	for fp, peers := range n.members {
		k := n.panelKeys[fp]
		sp := savedPanel{Key: k[:], Address: n.panelAddr[fp]}
		for _, p := range peers {
			if b, err := protojson.Marshal(p); err == nil {
				sp.Peers = append(sp.Peers, b)
			}
		}
		out[fp] = sp
	}
	b, _ := json.Marshal(out)
	tmp := n.file + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, n.file)
	}
}

// load 读取上次保存的成员列表并应用。
func (n *meshNode) load() {
	b, err := os.ReadFile(n.file)
	if err != nil {
		return
	}
	saved := map[string]savedPanel{}
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	n.mu.Lock()
	for fp, sp := range saved {
		k, err := mesh.ParseKey(sp.Key)
		if err != nil {
			continue
		}
		var peers []*nagiv1.MeshPeer
		for _, raw := range sp.Peers {
			p := &nagiv1.MeshPeer{}
			if protojson.Unmarshal(raw, p) == nil {
				peers = append(peers, p)
			}
		}
		n.members[fp], n.panelKeys[fp], n.panelAddr[fp] = peers, k, sp.Address
	}
	n.mu.Unlock()
	n.apply()
}

// meshPortFile 记住第一次随机选的 UDP 端口，以后重启不变。
func meshPortFile(dataDir string) string { return filepath.Join(dataDir, "identity", "mesh-port") }

// startMesh 启动 mesh。port 为 0 时用上次记住的端口，第一次随机选；记住的端口被占用时重新随机选。
func (a *agent) startMesh(ctx context.Context, port uint16) error {
	file := meshPortFile(a.cfg.DataDir)
	if port == 0 {
		if b, err := os.ReadFile(file); err == nil {
			n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			port = uint16(n)
		}
	}
	var priv mesh.Key
	copy(priv[:], a.id.wgKey)
	log := a.log.With("component", "mesh")
	m, err := mesh.New(mesh.Config{PrivateKey: priv, Port: port, Logger: log})
	if err != nil && port != 0 && a.cfg.MeshPort == 0 {
		log.Warn("记住的 mesh 端口不可用，重新随机选择", "port", port, "err", err)
		m, err = mesh.New(mesh.Config{PrivateKey: priv, Logger: log})
	}
	if err != nil {
		return err
	}
	_ = os.WriteFile(file, []byte(strconv.Itoa(int(m.Port()))), 0o600)
	a.mesh = &meshNode{
		m: m, log: log, members: map[string][]*nagiv1.MeshPeer{}, panelKeys: map[string]mesh.Key{},
		panelAddr: map[string]string{}, names: map[mesh.Key]string{},
	}
	a.mesh.file = filepath.Join(a.cfg.DataDir, "identity", "mesh-peers.json")
	a.mesh.load()
	log.Info("mesh 已启动", "address", m.Addr(), "udp_port", m.Port())
	go func() {
		<-ctx.Done()
		m.Close()
	}()
	go func() {
		if err := mesh.Serve(ctx, m, &meshService{Service: mesh.Service{M: m}, a: a}); err != nil && ctx.Err() == nil {
			log.Warn("mesh 服务停止", "err", err)
		}
	}()
	return nil
}

// info 是上报给面板的 mesh 信息。
func (n *meshNode) info() *nagiv1.MeshInfo {
	if n == nil {
		return nil
	}
	k := n.m.PublicKey()
	out := &nagiv1.MeshInfo{PublicKey: k[:], Port: uint32(n.m.Port())}
	for _, e := range n.m.Endpoints() {
		out.Endpoints = append(out.Endpoints, e.String())
	}
	return out
}

// update 记录某个面板告诉我们的成员列表，返回面板的公钥。
func (n *meshNode) update(fingerprint, address string, cfg *nagiv1.MeshConfig) (mesh.Key, bool) {
	if n == nil || cfg == nil {
		return mesh.Key{}, false
	}
	pk, err := mesh.ParseKey(cfg.GetPanelKey())
	if err != nil {
		return mesh.Key{}, false
	}
	n.mu.Lock()
	n.members[fingerprint] = cfg.GetPeers()
	n.panelKeys[fingerprint] = pk
	n.panelAddr[fingerprint] = address
	n.save()
	n.mu.Unlock()
	n.apply()
	return pk, true
}

// retain 删除已经不信任的面板告诉我们的成员。
func (n *meshNode) retain(fingerprints []string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	changed := false
	for fp := range n.members {
		if !slices.Contains(fingerprints, fp) {
			delete(n.members, fp)
			delete(n.panelKeys, fp)
			delete(n.panelAddr, fp)
			changed = true
		}
	}
	if changed {
		n.save()
	}
	n.mu.Unlock()
	if changed {
		n.apply()
	}
}

// apply 合并所有面板的成员列表并应用到 mesh。
func (n *meshNode) apply() {
	self := n.m.PublicKey()
	n.mu.Lock()
	merged := map[mesh.Key]*mesh.Peer{}
	var order []mesh.Key
	for fp, peers := range n.members {
		pk := n.panelKeys[fp]
		for _, mp := range peers {
			k, err := mesh.ParseKey(mp.GetPublicKey())
			if err != nil || k == self {
				continue
			}
			p := merged[k]
			if p == nil {
				p = &mesh.Peer{Key: k}
				merged[k] = p
				order = append(order, k)
			}
			for _, e := range mp.GetEndpoints() {
				if ap, err := netip.ParseAddrPort(e); err == nil && !slices.Contains(p.Endpoints, ap) {
					p.Endpoints = append(p.Endpoints, ap)
				}
			}
			if mp.GetIsPanel() && k == pk {
				// 面板的 UDP 和控制连接用同一个地址和端口。
				if ap, ok := resolve(n.panelAddr[fp]); ok && !slices.Contains(p.Endpoints, ap) {
					p.Endpoints = append(p.Endpoints, ap)
				}
			} else if !slices.Contains(p.Relays, pk) {
				p.Relays = append(p.Relays, pk)
			}
			if mp.GetName() != "" {
				n.names[k] = mp.GetName()
			}
		}
	}
	n.mu.Unlock()
	peers := make([]mesh.Peer, 0, len(order))
	for _, k := range order {
		peers = append(peers, *merged[k])
	}
	if err := n.m.SetPeers(peers); err != nil {
		n.log.Warn("更新 mesh 成员失败", "err", err)
	}
}

func resolve(hostport string) (netip.AddrPort, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return netip.AddrPort{}, false
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return netip.AddrPort{}, false
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ips[0].Unmap(), uint16(p)), true
}

func (n *meshNode) name(k mesh.Key) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s, ok := n.names[k]; ok {
		return s
	}
	return k.Short()
}

// relayLoop 维持经某个面板控制连接的 TLS 中转通道，直到连接断开。
func (n *meshNode) relayLoop(ctx context.Context, panel nagiv1.PanelServiceClient, panelKey mesh.Key) {
	if n == nil {
		return
	}
	stream, err := panel.MeshRelay(ctx)
	if err != nil {
		return
	}
	var mu sync.Mutex
	detach := n.m.AttachLink(panelKey, func(src, dst mesh.Key, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return stream.Send(&nagiv1.MeshPacket{Src: src[:], Dst: dst[:], Payload: payload})
	})
	defer detach()
	for {
		pkt, err := stream.Recv()
		if err != nil {
			return
		}
		src, err1 := mesh.ParseKey(pkt.GetSrc())
		dst, err2 := mesh.ParseKey(pkt.GetDst())
		if err1 == nil && err2 == nil {
			n.m.HandleLink(src, dst, pkt.GetPayload())
		}
	}
}

// ---- AgentService ----

func (s *agentService) GetMeshStatus(context.Context, *nagiv1.MeshStatusRequest) (*nagiv1.MeshStatus, error) {
	n := s.node.mesh
	if n == nil {
		return nil, status.Error(codes.Unavailable, "mesh 没有启动")
	}
	return mesh.StatusProto(n.m, n.name), nil
}

func (s *agentService) MeshPing(ctx context.Context, req *nagiv1.MeshPingRequest) (*nagiv1.MeshPingResponse, error) {
	n := s.node.mesh
	if n == nil {
		return nil, status.Error(codes.Unavailable, "mesh 没有启动")
	}
	k, err := mesh.ParseKey(req.GetPublicKey())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rtt, err := mesh.Ping(ctx, n.m, k)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, status.Error(codes.DeadlineExceeded, "经 mesh 连接超时")
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &nagiv1.MeshPingResponse{RttMs: float64(rtt.Microseconds()) / 1000}, nil
}

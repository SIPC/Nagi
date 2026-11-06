package mesh

import (
	"context"
	"net"
	"sort"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ServicePort 是每个成员在 mesh 地址上提供 MeshService 的 TCP 端口。mesh 上的其他端口：
// 7001 是节点之间的项目内网（agent/intranet.go），7002 是测速（SpeedPort）。
const ServicePort = 7000

// Serve 在 mesh 上提供 MeshService 和测速（见 speed.go），直到 ctx 取消。impl 为 nil 时只提供 Ping。
// mesh 上的流量已经由 WireGuard 加密和认证，gRPC 本身不再加 TLS；调用方身份由来源 mesh 地址确定。
func Serve(ctx context.Context, m *Mesh, impl nagiv1.MeshServiceServer) error {
	ln, err := m.Listen(ServicePort)
	if err != nil {
		return err
	}
	if impl == nil {
		impl = &Service{M: m}
	}
	go func() {
		if err := serveSpeed(ctx, m); err != nil && ctx.Err() == nil {
			m.log.Warn("mesh 测速服务停止", "err", err)
		}
	}()
	srv := grpc.NewServer()
	nagiv1.RegisterMeshServiceServer(srv, impl)
	go func() {
		<-ctx.Done()
		srv.Stop()
	}()
	return srv.Serve(ln)
}

// Caller 返回 MeshService 调用方的公钥。
func Caller(ctx context.Context, m *Mesh) (Key, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return Key{}, false
	}
	return m.KeyOf(p.Addr)
}

// Service 实现 MeshService 的公共部分（Ping）。节点嵌入它再加上自己的方法。
type Service struct {
	nagiv1.UnimplementedMeshServiceServer
	M *Mesh
}

func (s *Service) Ping(ctx context.Context, _ *nagiv1.PingRequest) (*nagiv1.PingResponse, error) {
	k, _ := Caller(ctx, s.M)
	return &nagiv1.PingResponse{Caller: k[:]}, nil
}

// Client 经 mesh 连接某个成员的 MeshService。
func Client(m *Mesh, k Key) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///"+Address(k).String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return m.Dial(ctx, k, ServicePort)
		}))
}

// Ping 经 mesh 调用某个成员的 Ping，返回往返时间（包括建立连接）。
func Ping(ctx context.Context, m *Mesh, k Key) (time.Duration, error) {
	cc, err := Client(m, k)
	if err != nil {
		return 0, err
	}
	defer cc.Close()
	start := time.Now()
	_, err = nagiv1.NewMeshServiceClient(cc).Ping(ctx, &nagiv1.PingRequest{})
	return time.Since(start), err
}

// StatusProto 把 mesh 状态转成接口格式。name 返回成员的显示名称。
func StatusProto(m *Mesh, name func(Key) string) *nagiv1.MeshStatus {
	self := m.PublicKey()
	out := &nagiv1.MeshStatus{PublicKey: self[:], Address: m.Addr().String(), Port: uint32(m.Port()), QuicOff: !m.QUIC()}
	for _, e := range m.Endpoints() {
		out.Endpoints = append(out.Endpoints, e.String())
	}
	for _, s := range m.Status() {
		k := s.Key
		ps := &nagiv1.MeshPeerStatus{
			PublicKey: k[:], Name: name(k), Address: Address(k).String(), Path: s.Path,
			RttMs: float64(s.RTT.Microseconds()) / 1000, RxBytes: s.RxBytes, TxBytes: s.TxBytes,
			Fec: s.FEC >= 0, FecParity: int32(max(s.FEC, 0)), LossPpm: s.LossPPM, FecRecovered: s.Recovered, Quic: s.QUIC,
			TxLossKnown: s.TxLoss >= 0, TxLossPpm: uint32(max(s.TxLoss, 0)), RxLossKnown: s.RxLoss >= 0, RxLossPpm: uint32(max(s.RxLoss, 0)),
		}
		if s.Direct.IsValid() {
			ps.Endpoint = s.Direct.String()
		}
		if !s.Via.IsZero() {
			ps.Via = name(s.Via)
		}
		if !s.LastHandshake.IsZero() {
			ps.LastHandshake = timestamppb.New(s.LastHandshake)
		}
		out.Peers = append(out.Peers, ps)
	}
	sort.Slice(out.Peers, func(i, j int) bool { return out.Peers[i].Name < out.Peers[j].Name })
	return out
}

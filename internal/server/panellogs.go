package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/standby"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// This separate control protocol exchanges only mesh identities and encrypted
// WireGuard packets. Neither the snapshot nor the legacy audit protocol changes.
const panelMeshALPN = "nagi-panel-mesh/1"

func (s *Server) alertPrimary(ctx context.Context) (*grpc.ClientConn, error) {
	if s.standby == nil || s.mesh == nil {
		return nil, errors.New("告警处理需要连接当前主面板")
	}
	fp := s.standby.Info.PrimaryFingerprint
	if current, err := s.q.GetSetting(ctx, standby.SettingPrimary); err == nil && current != "" {
		fp = current
	}
	members, err := s.logPanelMembers(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := members[fp]; !ok {
		return nil, errors.New("主备日志成员已撤权或无法核验")
	}
	s.panelMesh.mu.Lock()
	p, ok := s.panelMesh.peers[fp]
	s.panelMesh.mu.Unlock()
	if !ok {
		return nil, errors.New("告警处理需要连接当前主面板")
	}
	return mesh.Client(s.mesh, p.Key)
}

type panelMeshState struct {
	mu    sync.Mutex
	peers map[string]mesh.Peer
}

// logPanelMembers is deliberately fail-closed. Historical panel_members is not
// authorization: removed members remain there for configuration provenance.
func (s *Server) logPanelMembers(ctx context.Context) (map[string]string, error) {
	rows, err := s.q.ListStandbyPanels(ctx)
	if err != nil {
		return nil, err
	}
	removed, err := s.q.GetSetting(ctx, standby.SettingRemoved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var denied []string
	if removed != "" {
		if err := json.Unmarshal([]byte(removed), &denied); err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for _, p := range rows {
		out[p.ServerFp] = p.Address
	}
	primary, err := s.q.GetSetting(ctx, standby.SettingPrimary)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	address, err := s.q.GetSetting(ctx, standby.SettingPrimaryAddress)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if s.standby != nil && (primary == "" || primary == s.standby.Info.PrimaryFingerprint) {
		primary, address = s.standby.Info.PrimaryFingerprint, s.standby.PrimaryAddress()
	}
	if primary != "" {
		out[primary] = address
	}
	delete(out, s.fingerprint)
	for _, fp := range denied {
		delete(out, fp)
	}
	return out, nil
}

func (s *Server) authorizeLogPanel(ctx context.Context, key mesh.Key) error {
	members, err := s.logPanelMembers(ctx)
	if err != nil {
		return err
	}
	s.panelMesh.mu.Lock()
	defer s.panelMesh.mu.Unlock()
	for fp, p := range s.panelMesh.peers {
		if _, ok := members[fp]; ok && p.Key == key {
			return nil
		}
	}
	return errors.New("面板日志成员未登记或已撤权")
}

func (s *Server) savePanelPeersLocked() error {
	b, err := json.Marshal(s.panelMesh.peers)
	if err != nil {
		return err
	}
	path := filepath.Join(s.cfg.DataDir, "pki", "panel-mesh.json")
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return err
	}
	defer os.Remove(path + ".tmp")
	return os.Rename(path+".tmp", path)
}

// The key is learned only inside a pinned, mutually authenticated TLS session.
// Existing node keys and other panel identities cannot be claimed by a panel.
func (s *Server) bindLogPanel(ctx context.Context, fp string, p mesh.Peer) error {
	members, err := s.logPanelMembers(ctx)
	if err != nil {
		return err
	}
	if _, ok := members[fp]; !ok || p.Key == (mesh.Key{}) || p.Key == s.mesh.PublicKey() {
		return errors.New("无效的主备 mesh 身份")
	}
	nodes, err := s.q.ListNodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if bytes.Equal(n.MeshKey, p.Key[:]) {
			return errors.New("主备 mesh 公钥与节点冲突")
		}
	}
	s.panelMesh.mu.Lock()
	if s.panelMesh.peers == nil {
		s.panelMesh.peers = map[string]mesh.Peer{}
	}
	for other, old := range s.panelMesh.peers {
		if other != fp && old.Key == p.Key {
			s.panelMesh.mu.Unlock()
			return errors.New("主备 mesh 公钥被另一成员占用")
		}
	}
	old, existed := s.panelMesh.peers[fp]
	s.panelMesh.peers[fp] = p
	if err := s.savePanelPeersLocked(); err != nil {
		if existed {
			s.panelMesh.peers[fp] = old
		} else {
			delete(s.panelMesh.peers, fp)
		}
		s.panelMesh.mu.Unlock()
		return err
	}
	s.panelMesh.mu.Unlock()
	s.nodes.SetPanelPeer(fp, p)
	return nil
}

func panelMeshWrite(c net.Conn, msg proto.Message) error {
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	if len(b) > 64<<10 {
		return errors.New("主备 mesh 帧过大")
	}
	frame := make([]byte, 4, len(b)+4)
	binary.BigEndian.PutUint32(frame, uint32(len(b)))
	frame = append(frame, b...)
	if err := c.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	n, err := c.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return err
}

func panelMeshRead(c net.Conn, msg proto.Message) error {
	var size [4]byte
	if err := c.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	if _, err := io.ReadFull(c, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n > 64<<10 {
		return errors.New("主备 mesh 帧过大")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		return err
	}
	return proto.Unmarshal(b, msg)
}

func (s *Server) servePanelMesh(ctx context.Context, c *tls.Conn, expected string) error {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	state := c.ConnectionState()
	if s.mesh == nil || len(state.PeerCertificates) == 0 || expected == "" && len(state.VerifiedChains) == 0 {
		return errors.New("主备 mesh 需要已认证证书")
	}
	fp := tlsutil.Fingerprint(state.PeerCertificates[0])
	if expected != "" && fp != expected {
		return errors.New("主备 mesh 证书指纹不符")
	}
	members, err := s.logPanelMembers(ctx)
	if err != nil {
		return err
	}
	if _, ok := members[fp]; !ok {
		return errors.New("主备 mesh 成员未登记或已移除")
	}
	self := s.mesh.PublicKey()
	hello := &nagiv1.MeshInfo{PublicKey: self[:], Port: uint32(s.mesh.Port())}
	for _, ep := range s.mesh.Endpoints() {
		hello.Endpoints = append(hello.Endpoints, ep.String())
	}
	if err := panelMeshWrite(c, hello); err != nil {
		return err
	}
	var remote nagiv1.MeshInfo
	if err := panelMeshRead(c, &remote); err != nil {
		return err
	}
	k, err := mesh.ParseKey(remote.PublicKey)
	if err != nil || remote.Port == 0 || remote.Port > 65535 || len(remote.Endpoints) > 32 {
		return errors.New("无效的主备 mesh 引导")
	}
	p := mesh.Peer{Key: k}
	if addr, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		p.Endpoints = append(p.Endpoints, netip.AddrPortFrom(addr.Addr().Unmap(), uint16(remote.Port)))
	}
	for _, raw := range remote.Endpoints {
		if ap, err := netip.ParseAddrPort(raw); err == nil && mesh.UsableEndpoint(ap) && len(p.Endpoints) < mesh.MaxEndpoints {
			p.Endpoints = append(p.Endpoints, ap)
		}
	}
	if err := s.bindLogPanel(ctx, fp, p); err != nil {
		return err
	}
	var writes sync.Mutex
	send := func(src, dst mesh.Key, payload []byte) error {
		// This link is only a direct encrypted path between these two panels.
		// It does not grant transit or trust the source field of a received frame.
		if src != self || dst != k {
			return errors.New("主备 mesh 通道不授权第三方中转")
		}
		if err := s.authorizeLogPanel(ctx, k); err != nil {
			c.Close()
			return err
		}
		writes.Lock()
		defer writes.Unlock()
		return panelMeshWrite(c, &nagiv1.MeshPacket{Src: self[:], Dst: k[:], Payload: payload})
	}
	detach := s.mesh.AttachLink(k, send)
	defer detach()
	heartCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-heartCtx.Done():
				return
			case <-t.C:
				if err := send(self, k, nil); err != nil {
					c.Close()
					return
				}
			}
		}
	}()
	for {
		var packet nagiv1.MeshPacket
		if err := panelMeshRead(c, &packet); err != nil {
			return err
		}
		if err := s.authorizeLogPanel(ctx, k); err != nil {
			return err
		}
		dst, err := mesh.ParseKey(packet.Dst)
		if err != nil || dst != self || !bytes.Equal(packet.Src, k[:]) {
			return errors.New("主备 mesh 包来源或目标不符")
		}
		if len(packet.Payload) > 0 {
			s.mesh.HandleLink(k, self, packet.Payload)
		}
	}
}

func (s *Server) panelMeshConnect(ctx context.Context, fp, address string) error {
	cert := s.groupClientCert()
	if cert == nil {
		return errors.New("无法签发主备 mesh 身份证书")
	}
	cfg := tlsutil.PinnedConfig(fp)
	cfg.NextProtos, cfg.Certificates = []string{panelMeshALPN}, []tls.Certificate{*cert}
	d := tls.Dialer{Config: cfg, NetDialer: &net.Dialer{Timeout: 10 * time.Second}}
	c, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	tc := c.(*tls.Conn)
	// PinnedConfig authenticates a self-signed server, so VerifiedChains may
	// be empty on this side. Verify the pinned fingerprint again, while the
	// receiver still requires a CA-verified, pinned client certificate.
	if tc.ConnectionState().NegotiatedProtocol != panelMeshALPN {
		c.Close()
		return errors.New("对方面板尚不支持日志 mesh 引导")
	}
	return s.servePanelMesh(ctx, tc, fp)
}

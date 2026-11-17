package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/pkg/directdl"
	"github.com/SIPC/nagi/internal/pkg/filehttp"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 直连下载（见 docs/agent.md#直连下载）：浏览器拿着面板签的链接，经 Caddy 的 443（节点公网 IP 的 Let's Encrypt
// 证书）直接从节点下载，不经面板和 mesh。面板每次连上时告诉节点验证签名用的密钥和节点的公网地址；
// 链接写明下载哪个文件、15 分钟内有效，权限由面板在签发前检查。浏览器先探测一下，连不上就照旧经面板下载。

type directDL struct {
	a  *agent
	mu sync.Mutex
	// keys 是各个面板给的密钥（面板指纹 → 密钥），只在内存里：节点重启后面板重新连上时再给。
	keys    map[string][]byte
	address string
	// portErr：443 被别的程序占用，开不了；certErr：最近一次申请证书失败的原因。
	portErr, certErr string
}

func newDirectDL(a *agent) *directDL {
	return &directDL{a: a, keys: map[string][]byte{}}
}

func directOffFile(dataDir string) string {
	return filepath.Join(dataDir, "identity", "direct-download-off")
}

// off：节点的网络设置里关了直连下载。
func (d *directDL) off() bool {
	if d == nil {
		return false
	}
	_, err := os.Stat(directOffFile(d.a.cfg.DataDir))
	return err == nil
}

func (s *agentService) SetDirectDownload(_ context.Context, req *nagiv1.SetDirectDownloadRequest) (*nagiv1.SetDirectDownloadResponse, error) {
	if len(req.GetKey()) < 32 {
		return nil, status.Error(codes.InvalidArgument, "密钥太短")
	}
	addr := ""
	if a, err := netip.ParseAddr(req.GetAddress()); err == nil && directdl.PublicAddress(a) {
		addr = a.Unmap().String()
	}
	d := s.node.direct
	d.mu.Lock()
	d.keys[s.panel] = append([]byte(nil), req.GetKey()...)
	if addr != d.address {
		d.address, d.certErr = addr, ""
	}
	d.mu.Unlock()
	s.node.changed() // 重新生成 Caddy 配置
	return &nagiv1.SetDirectDownloadResponse{}, nil
}

// wanted 返回要开直连下载的 IP：没关、有公网地址、有面板给的密钥，而且 443 能用。
// 443 已经由 Caddy 监听着（有绑了域名的映射，或者已经开了直连）时直接用；否则先试着占一下，被别的程序占着就不开。
func (d *directDL) wanted(p *proxy.Proxy) string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	addr, haveKey := d.address, len(d.keys) > 0
	d.mu.Unlock()
	if addr == "" || !haveKey || d.off() || p == nil {
		d.setPortErr("")
		return ""
	}
	listening := false
	for _, k := range p.DomainPorts() {
		if k == (spec.PortKey{Proto: spec.ProtoTCP, Port: 443}) {
			listening = true
		}
	}
	if !listening {
		if err := portsFree(); err != nil {
			d.setPortErr(i18n.New("端口 443 被其他程序占用").Error())
			return ""
		}
	}
	d.setPortErr("")
	return addr
}

func (d *directDL) setPortErr(e string) {
	d.mu.Lock()
	d.portErr = e
	d.mu.Unlock()
}

// portsFree 试着占一下 TCP 和 UDP（HTTP/3）的 443。
func portsFree() error {
	l, err := net.Listen("tcp", ":443")
	if err != nil {
		return err
	}
	l.Close()
	pc, err := net.ListenPacket("udp", ":443")
	if err != nil {
		return err
	}
	pc.Close()
	return nil
}

// certEvent 处理这个 IP 的证书申请结果。返回 false 表示不是直连下载的 IP（由调用方按域名处理）。
// 申请不到证书不告警：节点在 NAT 后面、防火墙没放行 80/443 时本来就拿不到，下载照常经面板，节点页写明原因。
func (d *directDL) certEvent(domain string, err error) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if domain == "" || domain != d.address {
		return false
	}
	d.certErr = ""
	if err != nil {
		d.certErr = err.Error()
	}
	return true
}

// status 返回上报给面板的状态。
func (d *directDL) status(p *proxy.Proxy) *nagiv1.DirectDownload {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := &nagiv1.DirectDownload{Supported: true, Off: d.off(), Address: d.address}
	if st.Off {
		return st
	}
	if p == nil {
		st.Error = i18n.New("节点没有启动 Caddy（--no-proxy）").Error()
		return st
	}
	switch {
	case d.portErr != "":
		st.Error = d.portErr
	case d.address == "":
	case p.Direct() != d.address:
	case p.HasCert(d.address):
		st.Ready = true
	case d.certErr != "":
		st.Error = d.certErr
	}
	return st
}

// keysFor 返回能验证 method 这种下载的密钥：信任列表里的面板给的，并且这个面板能调用 method（和经 mesh 下载的规则一样）。
// 同时返回本节点在这些面板上的 ID。
func (d *directDL) keysFor(method string) ([][]byte, map[string]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keys [][]byte
	ids := map[string]bool{}
	for fp, k := range d.keys {
		p, ok := d.a.id.panel(fp)
		if !ok || d.a.checkPrimary(fp, method) != nil {
			continue
		}
		keys = append(keys, k)
		ids[p.NodeID] = true
	}
	return keys, ids
}

// ServeHTTP 处理 Caddy 交过来的直连下载请求（只有节点公网 IP 上的请求）。
func (d *directDL) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lang := i18n.Parse(r.Header.Get("Accept-Language"))
	fail := func(code int, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": i18n.Localize(lang, err)})
	}
	h := w.Header()
	// 浏览器先从面板的页面探测（跨域的 HEAD），能连上才跳到这个地址下载。不带凭据，链接本身就是凭据。
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Length, Content-Range, Accept-Ranges, ETag")
	h.Set("Referrer-Policy", "no-referrer")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, HEAD")
		h.Set("Access-Control-Allow-Headers", "Range, If-Range")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	token, ok := strings.CutPrefix(r.URL.Path, directdl.PathPrefix)
	if !ok || (r.Method != http.MethodGet && r.Method != http.MethodHead) || d.off() {
		http.NotFound(w, r)
		return
	}
	// 先不知道是哪种下载：按种类分别取能用的密钥核对。
	var c directdl.Claims
	var err error = directdl.ErrInvalid
	for _, kind := range []string{directdl.KindVolume, directdl.KindNode} {
		keys, ids := d.keysFor(directMethod(kind))
		got, verr := directdl.Verify(keys, token, time.Now())
		if verr == nil && got.Kind == kind && ids[got.Node] {
			c, err = got, nil
			break
		}
		if errors.Is(verr, directdl.ErrExpired) {
			err = verr
		}
	}
	if err != nil {
		fail(http.StatusForbidden, err)
		return
	}
	if c.Kind == directdl.KindNode {
		if err := d.a.checkNodeAccess(); err != nil {
			fail(http.StatusForbidden, err)
			return
		}
	}
	// 探测：链接有效就行，不读文件（目录要打包，读一下也不便宜）。
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	filehttp.Download{
		Open: func(ctx context.Context, offset, length int64) (filehttp.ChunkStream, error) {
			if c.Kind == directdl.KindNode {
				return filehttp.Pipe(ctx, func(send func(*nagiv1.VolumeFileChunk) error) error {
					return d.a.files.Download(ctx, c.Path, offset, length, send)
				}), nil
			}
			ref := &nagiv1.VolumeFileRef{ProjectId: c.Project, Container: c.Container, Volume: c.Volume, Path: c.Path,
				SnapshotId: c.Snapshot, Offset: offset, Length: length}
			return filehttp.Pipe(ctx, func(send func(*nagiv1.VolumeFileChunk) error) error {
				return d.a.projects.ReadVolumeFile(ctx, ref, send)
			}), nil
		},
		Fail: func(code int, err error) { fail(code, errors.New(status.Convert(err).Message())) },
	}.Serve(w, r)
}

// directMethod 是和这种下载对应的、经 mesh 下载的方法：谁能用直连下载，按它的规则判断（checkPrimary）。
func directMethod(kind string) string {
	if kind == directdl.KindNode {
		return nagiv1.MeshService_NodeFileDownload_FullMethodName
	}
	return nagiv1.MeshService_VolumeDownload_FullMethodName
}

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/auditlog"
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
// 链接写明下载哪个文件、5 分钟内有效，权限由面板在签发前检查。浏览器先探测一下，连不上就照旧经面板下载。
// 每次下载记进节点的本地审计（见 audit），随节点的审计外送。

type directDL struct {
	a  *agent
	mu sync.Mutex
	// keys 是各个面板给的密钥（面板指纹 → 密钥），只在内存里：节点重启后面板重新连上时再给。
	keys    map[string][]byte
	address string
	// portErr：443 被别的程序占用，开不了；certErr：最近一次申请证书失败的原因。
	portErr, certErr string

	// 本地审计的限额（见 audit）：这一段时间从 auditStart 开始，记了 audited 条，超出的 dropped 条只计数。
	auditStart       time.Time
	audited, dropped int

	// 加密 HTTP 下载（见 directhttp.go）：httpPortErr 是开不了的原因（端口被占用）；
	// prepared 是签发命令时记下的链接（链接编号 → 文件的版本、目录打包用过没有）；hashSlots 限制同时算 SHA-256 的数量。
	httpPortErr string
	prepared    map[string]*preparedLink
	hashSlots   chan struct{}
	// 按需监听（startHTTP）：httpUntil 之前、或者有正在进行的下载（httpActive）时开着端口；
	// reload 立即重新生成 Caddy 配置（agent.applyProxy，没有 Caddy 时为空）。
	httpUntil   time.Time
	httpActive  int
	reload      func(ctx context.Context)
	badVersions map[string]time.Time // 下载中途发现变了的文件版本（见 invalidate）
}

// 直连下载记进本地审计的限额：每 10 分钟最多 directAuditLimit 条。拿着有效链接的人反复请求，不能把本地审计刷满、
// 挤掉别的记录；超出的只计数，写进下一条记录。同一个链接从同一个地址的分段请求 10 分钟内只记第一次
// （filehttp 按 Key 去重，和经面板下载一样），整个文件的下载每次都记。
const (
	directAuditWindow = 10 * time.Minute
	directAuditLimit  = 500
)

func newDirectDL(a *agent) *directDL {
	return &directDL{a: a, keys: map[string][]byte{}, prepared: map[string]*preparedLink{}, hashSlots: make(chan struct{}, 2),
		badVersions: map[string]time.Time{}}
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
	st := &nagiv1.DirectDownload{Supported: true, Off: d.off(), Address: d.address, HttpSupported: true}
	d.httpStatus(p, st)
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

// panelKey 是一个面板给的密钥，连同面板的指纹和本节点在这个面板上的 ID。
type panelKey struct {
	fp, node string
	key      []byte
}

// keysFor 返回能验证 method 这种下载的密钥：信任列表里的面板给的，并且这个面板能调用 method（和经 mesh 下载的规则一样）。
func (d *directDL) keysFor(method string) []panelKey {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keys []panelKey
	for fp, k := range d.keys {
		p, ok := d.a.id.panel(fp)
		if !ok || d.a.checkPrimary(fp, method) != nil {
			continue
		}
		keys = append(keys, panelKey{fp: fp, node: p.NodeID, key: k})
	}
	return keys
}

// verify 核对链接：签名对得上某个能用的面板的密钥、种类对、写的是本节点在这个面板上的 ID、没过期。
// 返回链接的内容和签发的面板；签名对得上、只是过期时也返回它们（记进本地审计），错误是 ErrExpired。
func (d *directDL) verify(token string, now time.Time) (directdl.Claims, string, error) {
	c, fp, _, err := d.verifyWith(directdl.Verify, token, now)
	return c, fp, err
}

// verifyWith 和 verify 一样，用 check 核对（directdl.Verify 或 VerifyHTTP），另外返回对上的面板密钥。
func (d *directDL) verifyWith(check func([][]byte, string, time.Time) (directdl.Claims, error), token string, now time.Time) (directdl.Claims, string, []byte, error) {
	err := directdl.ErrInvalid
	var expired directdl.Claims
	var expiredBy string
	for _, kind := range []string{directdl.KindVolume, directdl.KindNode} {
		for _, k := range d.keysFor(directMethod(kind)) {
			got, verr := check([][]byte{k.key}, token, now)
			if got.Kind != kind || got.Node != k.node {
				continue
			}
			if verr == nil {
				return got, k.fp, k.key, nil
			}
			if errors.Is(verr, directdl.ErrExpired) {
				err, expired, expiredBy = verr, got, k.fp
			}
		}
	}
	return expired, expiredBy, nil, err
}

// audit 把一次直连下载记进节点的本地审计（见 docs/audit.md#节点本地审计），有限额（见 directAuditLimit）。
// actor 是签发链接的成员（面板写在链接里），ip 是下载的来源地址；detail 写链接编号、范围。
func (d *directDL) audit(panel string, c directdl.Claims, token string, r *http.Request, err error, result string) {
	if d.a.audit == nil {
		return
	}
	now := time.Now()
	d.mu.Lock()
	if now.Sub(d.auditStart) >= directAuditWindow {
		d.auditStart, d.audited = now, 0
	}
	if d.audited >= directAuditLimit {
		d.dropped++
		d.mu.Unlock()
		return
	}
	d.audited++
	dropped := d.dropped
	d.dropped = 0
	d.mu.Unlock()

	detail := []string{"kind=" + c.Kind, "link=" + linkID(c, token)}
	if c.Transport == directdl.TransportHTTP {
		detail = append(detail, "transport=http")
	}
	if rg := r.Header.Get("Range"); rg != "" {
		detail = append(detail, "range="+rg)
	}
	if c.Snapshot != "" {
		detail = append(detail, "snapshot="+c.Snapshot)
	}
	if dropped > 0 {
		detail = append(detail, "dropped="+strconv.Itoa(dropped))
	}
	target := c.Path
	if c.Kind == directdl.KindVolume {
		var parts []string
		for _, p := range []string{c.Project, c.Container, c.Volume} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		target = strings.Join(parts, "/") + ":" + c.Path
	}
	ev := auditlog.Event{Panel: panel, Actor: c.User, IP: remoteIP(r), Action: "direct_download", Target: target,
		Detail: strings.Join(detail, " "), Result: result}
	if err != nil {
		ev.Error = err.Error()
	}
	d.a.audit.Record(ev)
}

// linkID 是链接的编号：面板写在链接里的（和面板活动记录里的 direct_link 一样）；旧版本面板签的没有，用链接的哈希开头。
func linkID(c directdl.Claims, token string) string {
	if c.ID != "" {
		return c.ID
	}
	h := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(h[:8])
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ServeHTTP 处理 Caddy 交过来的直连下载请求（只有节点公网 IP 上的请求）。
func (d *directDL) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 加密 HTTP 下载走自己的路径（见 directhttp.go），不允许跨域。
	if token, ok := strings.CutPrefix(r.URL.Path, directdl.HTTPPathPrefix); ok {
		d.serveHTTPDL(w, r, token)
		return
	}
	// 明文 HTTP 上不发未加密的内容（HTTPS 的规则只在 443 上，这里再挡一下）。
	if r.TLS == nil {
		http.NotFound(w, r)
		return
	}
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
	// 先不知道是哪种下载：按种类分别取能用的密钥核对。签名对不上的不记（谁都能乱发请求）；
	// 过期的、节点访问关着被拒绝的记下（拿到过链接的人才发得出来），探测（HEAD）不记。
	c, panel, err := d.verify(token, time.Now())
	if err != nil {
		if errors.Is(err, directdl.ErrExpired) && r.Method == http.MethodGet {
			d.audit(panel, c, token, r, err, "denied")
		}
		fail(http.StatusForbidden, err)
		return
	}
	if c.Kind == directdl.KindNode {
		if err := d.a.checkNodeAccess(); err != nil {
			if r.Method == http.MethodGet {
				d.audit(panel, c, token, r, err, "denied")
			}
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
		// 同一个链接从同一个地址的分段请求 10 分钟内只记第一次（多线程下载工具会发很多次）。
		Key: "direct|" + linkID(c, token) + "|" + remoteIP(r),
		Record: func(err error) {
			result := "ok"
			if err != nil {
				result = "failed"
			}
			d.audit(panel, c, token, r, err, result)
		},
		Open: func(ctx context.Context, offset, length int64) (filehttp.ChunkStream, error) {
			return d.open(ctx, c, offset, length), nil
		},
		Fail: func(code int, err error) { fail(code, errors.New(status.Convert(err).Message())) },
	}.Serve(w, r)
}

// open 读链接里写的文件（offset、length 见 filehttp.Download.Open）。
func (d *directDL) open(ctx context.Context, c directdl.Claims, offset, length int64) filehttp.ChunkStream {
	if c.Kind == directdl.KindNode {
		return filehttp.Pipe(ctx, func(send func(*nagiv1.VolumeFileChunk) error) error {
			return d.a.files.Download(ctx, c.Path, offset, length, send)
		})
	}
	ref := &nagiv1.VolumeFileRef{ProjectId: c.Project, Container: c.Container, Volume: c.Volume, Path: c.Path,
		SnapshotId: c.Snapshot, Offset: offset, Length: length}
	return filehttp.Pipe(ctx, func(send func(*nagiv1.VolumeFileChunk) error) error {
		return d.a.projects.ReadVolumeFile(ctx, ref, send)
	})
}

// directMethod 是和这种下载对应的、经 mesh 下载的方法：谁能用直连下载，按它的规则判断（checkPrimary）。
func directMethod(kind string) string {
	if kind == directdl.KindNode {
		return nagiv1.MeshService_NodeFileDownload_FullMethodName
	}
	return nagiv1.MeshService_VolumeDownload_FullMethodName
}

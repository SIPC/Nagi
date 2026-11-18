package agent

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/pkg/byterange"
	"github.com/SIPC/nagi/internal/pkg/directdl"
	"github.com/SIPC/nagi/internal/pkg/filehttp"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 加密 HTTP 下载（见 docs/agent.md#加密-http-下载）：直连下载开着、HTTPS 不能用（申请不到 IP 证书、443 被占用等）时的
// 备用方式，没有单独的开关，只能设置端口（默认 80）。HTTPS 能用时不开这个端口。
// 面板签一个 HTTP 种类的链接，先经控制连接交给节点（PrepareDirectHttp）：节点核对后记下文件的版本（ETag），
// 文件不大时算出 SHA-256；面板据此生成一条 curl + openssl 的命令。之后 curl 经明文 HTTP 来下载，
// 节点用 AES-256-CTR 加密内容，密钥由节点和面板各自从直连下载的密钥派生（directdl.FileKey、ArchiveKey），
// 不经网络传。只认签发命令时登记过的链接；文件之后变了就拒绝（同一段密钥流不加密两份不同的内容），
// 目录打包的链接只能用一次。链接无效时一律 404，不说原因。

func directHTTPFile(dataDir string) string {
	return filepath.Join(dataDir, "identity", "direct-http")
}

// defaultHTTPPort 是加密 HTTP 下载默认的端口。
const defaultHTTPPort = 80

// httpPort 返回加密 HTTP 下载的端口：存在 identity/direct-http 里，没有这个文件、内容为空或者不对时用 80。
func (d *directDL) httpPort() int {
	if d == nil {
		return defaultHTTPPort
	}
	b, err := os.ReadFile(directHTTPFile(d.a.cfg.DataDir))
	if err != nil {
		return defaultHTTPPort
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || validHTTPPort(port) != nil {
		return defaultHTTPPort
	}
	return port
}

// validHTTPPort：1~65535，不能是 443（留给 HTTPS 的直连下载和域名访问）。
func validHTTPPort(port int) error {
	if port < 1 || port > 65535 {
		return status.Error(codes.InvalidArgument, "端口应在 1 到 65535 之间")
	}
	if port == 443 {
		return status.Error(codes.InvalidArgument, "443 端口不能用于 HTTP 下载")
	}
	return nil
}

// setHTTPPort 修改加密 HTTP 下载的端口，0 和 80 表示默认（删掉文件）。
func (d *directDL) setHTTPPort(port int) error {
	file := directHTTPFile(d.a.cfg.DataDir)
	if port == 0 || port == defaultHTTPPort {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return status.Error(codes.Internal, err.Error())
		}
		return nil
	}
	if err := validHTTPPort(port); err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(strconv.Itoa(port)), 0o600); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}

// 按需监听（见 docs/agent.md#加密-http-下载）：平时不占端口，面板登记链接（PrepareDirectHttp）时才开，
// 最后一个链接过期 httpIdle 以后、没有正在进行的下载时关掉。
const httpIdle = 10 * time.Minute

// httpNeeded：现在有没过期（或刚过期不到 httpIdle）的链接，或者有正在进行的下载。
func (d *directDL) httpNeeded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.httpActive > 0 || time.Now().Before(d.httpUntil)
}

// wantedHTTP 返回要开加密 HTTP 下载的 IP 和端口：直连下载开着、有公网地址、有面板给的密钥，HTTPS 不能用，
// 现在需要（httpNeeded），而且端口能用。https 是这次要为 HTTPS 开的 IP（wanted 的结果）：已经有它的证书时
// HTTPS 能用，不开 HTTP。在 80 上、Caddy 已经监听着 80（有绑了域名的映射，或者已经开着）时直接用；
// 别的端口已经由这里开着时也直接用；否则先试着占一下，被别的程序占着就不开，记下原因（上报，签发命令时报错）。
// 被映射占着的由 proxy.Apply 判断。
func (d *directDL) wantedHTTP(p *proxy.Proxy, https string) (string, int) {
	if d == nil {
		return "", 0
	}
	port := d.httpPort()
	d.mu.Lock()
	addr, haveKey := d.address, len(d.keys) > 0
	d.mu.Unlock()
	if d.off() || addr == "" || !haveKey || p == nil || (https != "" && p.HasCert(https)) || !d.httpNeeded() {
		return "", 0
	}
	listening := false
	if _, loaded, _ := p.DirectHTTP(); loaded == port {
		listening = true
	}
	if port == 80 {
		for _, k := range p.DomainPorts() {
			if k == (spec.PortKey{Proto: spec.ProtoTCP, Port: 80}) {
				listening = true
			}
		}
	}
	if !listening {
		l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			d.setHTTPPortErr(i18n.Errorf("端口 %d 被其他程序占用", port).Error())
			return "", 0
		}
		l.Close()
	}
	return addr, port
}

func (d *directDL) setHTTPPortErr(e string) {
	d.mu.Lock()
	d.httpPortErr = e
	d.mu.Unlock()
}

// startHTTP 在签发命令前确保加密 HTTP 下载在听着：延长需要的时间，立即重新生成 Caddy 配置（reload），
// 再看是不是开好了。开不了（端口被其他程序或映射占用）时返回原因，面板照原样告诉用户。
// 没有 reload（测试里没有 Caddy）时不检查。
func (d *directDL) startHTTP(ctx context.Context, until time.Time) error {
	d.mu.Lock()
	if until.After(d.httpUntil) {
		d.httpUntil = until
	}
	reload := d.reload
	d.mu.Unlock()
	if reload == nil {
		return nil
	}
	reload(ctx)
	p := d.a.proxy
	if p == nil {
		return status.Error(codes.FailedPrecondition, "节点没有启动 Caddy（--no-proxy）")
	}
	addr, port, perr := p.DirectHTTP()
	d.mu.Lock()
	defer d.mu.Unlock()
	if addr != "" && addr == d.address && port == d.httpPort() {
		d.httpPortErr = ""
		return nil
	}
	if perr != "" {
		d.httpPortErr = perr
	}
	if d.httpPortErr != "" {
		return status.Error(codes.FailedPrecondition, d.httpPortErr)
	}
	return status.Error(codes.FailedPrecondition, "加密 HTTP 下载暂时不能使用，请稍后重试")
}

// httpStatus 把加密 HTTP 下载的状态填进上报的 st（调用时持有 d.mu）。关了直连下载时什么都不填。
// http_ready 表示可以用加密 HTTP（HTTPS 不能用、有地址和密钥），不表示端口正在监听：端口按需打开（startHTTP）。
// http_error 是最近一次打开失败的原因，下次打开成功时清掉。
func (d *directDL) httpStatus(p *proxy.Proxy, st *nagiv1.DirectDownload) {
	if d.off() {
		return
	}
	st.HttpPort = uint32(d.httpPort())
	st.HttpError = d.httpPortErr
	if p == nil || d.address == "" || len(d.keys) == 0 {
		return
	}
	httpsReady := p.Direct() == d.address && p.HasCert(d.address)
	st.HttpReady = !httpsReady
}

// preparedLink 是签发命令时登记的链接。
type preparedLink struct {
	etag    string // 文件的版本；目录打包为空
	archive bool
	used    bool // 目录打包：已经下载过（只能用一次）
	expires time.Time
	// ip 是第一次下载的来源地址：之后只认这个地址（中间人拿到链接也不能再下载一份密文）。
	ip string
	// served 是这个链接已经发出的字节，limit 是上限（文件大小的 2 倍加一点余量，目录打包不限，本来只能用一次）。
	served, limit int64
	// changed：下载中途发现文件变了，这个链接作废，之后一律 409。
	changed bool
}

// linkAllowance 是每个链接在文件大小 2 倍之外允许多发的字节（分段重试）。
const linkAllowance = 1 << 20

// maxPrepared：同时登记的链接最多这么多（每个 5 分钟后过期）。
const maxPrepared = 10000

func (d *directDL) remember(id string, l *preparedLink) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if len(d.prepared) >= maxPrepared/2 {
		for k, v := range d.prepared {
			if now.After(v.expires) {
				delete(d.prepared, k)
			}
		}
	}
	if len(d.prepared) >= maxPrepared {
		return status.Error(codes.ResourceExhausted, "下载命令太多，请稍后重试")
	}
	d.prepared[id] = l
	return nil
}

// 用链接下载前的检查结果。
var (
	errLinkUnknown = errors.New("没有登记、已过期或者已经用过")
	errLinkIP      = i18n.New("下载链接只能从第一次下载的地址使用")
	errLinkBudget  = i18n.New("下载链接已经发出太多内容")
)

// claim 在一次下载开始前核对登记的链接：第一次下载时记下来源地址，之后只认这个地址；发出的字节到了上限不再给；
// 目录打包只能用一次。返回链接的副本。
func (d *directDL) claim(id, ip string) (preparedLink, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.prepared[id]
	if !ok || id == "" || time.Now().After(l.expires) {
		return preparedLink{}, errLinkUnknown
	}
	switch {
	case l.ip != "" && l.ip != ip:
		return preparedLink{}, errLinkIP
	case l.archive && l.used:
		return preparedLink{}, errLinkUnknown
	case l.limit > 0 && l.served >= l.limit:
		return preparedLink{}, errLinkBudget
	}
	l.ip, l.used = ip, true
	return *l, nil
}

// spend 记下这个链接又发出了 n 字节，超过上限时返回 false（中断这次下载）。
func (d *directDL) spend(id string, n int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.prepared[id]
	if !ok {
		return true // 过期后被清理掉了：已经开始的下载照常传完
	}
	l.served += n
	return l.limit == 0 || l.served <= l.limit
}

// invalidate 作废链接：下载中途文件变了。同一个文件的这个版本 badVersionTTL 内也不再登记（改完把修改时间改回去的情况）。
func (d *directDL) invalidate(id, version string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l, ok := d.prepared[id]; ok {
		l.changed = true
	}
	if len(d.badVersions) > 1000 {
		clear(d.badVersions)
	}
	d.badVersions[version] = time.Now().Add(badVersionTTL)
}

// badVersionTTL：下载中途发现变了的文件版本这么久内不再登记。
const badVersionTTL = 10 * time.Minute

func (d *directDL) badVersion(version string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	until, ok := d.badVersions[version]
	return ok && time.Now().Before(until)
}

// fileVersion 标识一个文件的一个版本（链接里写的文件加上 ETag）。
func fileVersion(c directdl.Claims, etag string) string {
	return strings.Join([]string{c.Kind, c.Project, c.Container, c.Volume, c.Snapshot, c.Path, etag}, "\x00")
}

var errFileChanged = i18n.New("文件已变化，请重新获取下载命令")

func (s *agentService) PrepareDirectHttp(ctx context.Context, req *nagiv1.PrepareDirectHttpRequest) (*nagiv1.PrepareDirectHttpResponse, error) {
	d := s.node.direct
	if d.off() {
		return nil, status.Error(codes.FailedPrecondition, "节点关闭了直连下载")
	}
	c, panel, _, err := d.verifyWith(directdl.VerifyHTTP, req.GetToken(), time.Now())
	if err != nil || panel != s.panel || c.ID == "" {
		return nil, status.Error(codes.InvalidArgument, directdl.ErrInvalid.Error())
	}
	if c.Kind == directdl.KindNode {
		if err := s.node.checkNodeAccess(); err != nil {
			return nil, err
		}
	}
	// 读文件开头拿到名字、大小和版本；普通文件不太大时接着读完算 SHA-256，最多 HTTPHashTimeout。
	hctx, cancel := context.WithTimeout(ctx, directdl.HTTPHashTimeout)
	defer cancel()
	stream := d.open(hctx, c, 0, 0)
	head, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	resp := &nagiv1.PrepareDirectHttpResponse{Name: head.GetName(), Size: head.GetSize(), Archive: head.GetArchive()}
	if !resp.Archive && d.badVersion(fileVersion(c, filehttp.ETag(head))) {
		return nil, status.Error(codes.FailedPrecondition, "文件正在被修改，请稍后重新获取下载命令")
	}
	switch {
	case head.GetArchive():
		resp.Size, resp.HashSkipped = 0, directdl.HashSkippedArchive
	case head.GetSize() > directdl.HTTPHashLimit:
		resp.Etag, resp.HashSkipped = filehttp.ETag(head), directdl.HashSkippedTooLarge
	default:
		resp.Etag = filehttp.ETag(head)
		sum, err := d.hash(hctx, stream)
		switch {
		case err == nil:
			resp.Sha256 = sum
		case errors.Is(hctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
			resp.HashSkipped = directdl.HashSkippedTimeout
		default:
			return nil, err
		}
	}
	expires := time.Unix(c.Expires, 0).Add(time.Minute)
	link := &preparedLink{etag: resp.Etag, archive: resp.Archive, expires: expires}
	if !resp.Archive {
		link.limit = 2*resp.Size + linkAllowance
	}
	if err := d.remember(c.ID, link); err != nil {
		return nil, err
	}
	// 按需打开端口：开不了时这个链接也用不了，告诉面板原因。
	if err := d.startHTTP(ctx, expires.Add(httpIdle)); err != nil {
		return nil, err
	}
	return resp, nil
}

// hash 读完 stream 算 SHA-256。同时最多算 2 个（cap(hashSlots)），等不到名额时按超时处理。
func (d *directDL) hash(ctx context.Context, stream filehttp.ChunkStream) (string, error) {
	select {
	case d.hashSlots <- struct{}{}:
		defer func() { <-d.hashSlots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	h := sha256.New()
	for {
		c, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", err
		}
		h.Write(c.GetData())
	}
}

// recheckEvery：下载中每发出这么多字节再核对一次文件的版本，发完时也核对一次。
var recheckEvery int64 = 8 << 20

// currentETag 重新看一下文件现在的版本（只读文件头和 1 个字节）。
func (d *directDL) currentETag(ctx context.Context, c directdl.Claims) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	head, err := d.open(ctx, c, 0, 1).Recv()
	if err != nil {
		return "", err
	}
	if head.GetArchive() {
		return "", errFileChanged
	}
	return filehttp.ETag(head), nil
}

// serveHTTPDL 处理加密 HTTP 下载的请求。链接无效（签名不对、过期、没登记、目录打包用过了、别的来源地址、
// 发出的内容到了上限、没开启）时一律 404，不说原因；过期的、来源地址不对的、用过的、到了上限的记进本地审计。
func (d *directDL) serveHTTPDL(w http.ResponseWriter, r *http.Request, token string) {
	// HTTP 的链接只走明文 HTTP（443 上的直连规则也会把这个路径交过来）。
	if r.Method != http.MethodGet || d.off() || r.TLS != nil {
		http.NotFound(w, r)
		return
	}
	c, panel, key, err := d.verifyWith(directdl.VerifyHTTP, token, time.Now())
	if err != nil {
		if errors.Is(err, directdl.ErrExpired) {
			d.audit(panel, c, token, r, err, "denied")
		}
		http.NotFound(w, r)
		return
	}
	prep, err := d.claim(c.ID, remoteIP(r))
	if err != nil {
		if !errors.Is(err, errLinkUnknown) {
			d.audit(panel, c, token, r, err, "denied")
		}
		http.NotFound(w, r)
		return
	}
	lang := i18n.Parse(r.Header.Get("Accept-Language"))
	fail := func(code int, err error) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(code)
		_, _ = fmt.Fprintln(w, i18n.Localize(lang, err))
	}
	if prep.changed {
		fail(http.StatusConflict, errFileChanged)
		return
	}
	if c.Kind == directdl.KindNode {
		if err := d.a.checkNodeAccess(); err != nil {
			d.audit(panel, c, token, r, err, "denied")
			fail(http.StatusForbidden, errors.New(status.Convert(err).Message()))
			return
		}
	}
	d.mu.Lock()
	d.httpActive++
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.httpActive--
		d.mu.Unlock()
	}()
	ck := directdl.ArchiveKey(key, c)
	if !prep.archive {
		ck = directdl.FileKey(key, c, prep.etag)
	}
	enc := &encryptStream{key: ck, etag: prep.etag, archive: prep.archive,
		spend: func(n int64) bool { return d.spend(c.ID, n) }}
	// 快照里的文件不会变，不用再核对。
	if !prep.archive && c.Snapshot == "" {
		enc.recheck = func(ctx context.Context) error {
			cur, err := d.currentETag(ctx, c)
			if err == nil && cur == prep.etag {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err() // 客户端断开了，不是文件变了
			}
			d.invalidate(c.ID, fileVersion(c, prep.etag))
			d.audit(panel, c, token, r, errFileChanged, "failed")
			return errFileChanged
		}
	}
	filehttp.Download{
		Key: "direct-http|" + linkID(c, token) + "|" + remoteIP(r),
		Record: func(err error) {
			result := "ok"
			if err != nil {
				result = "failed"
			}
			d.audit(panel, c, token, r, err, result)
		},
		Open: func(ctx context.Context, offset, length int64) (filehttp.ChunkStream, error) {
			s := *enc
			s.ctx, s.inner, s.offset, s.length = ctx, d.open(ctx, c, offset, length), offset, length
			return &s, nil
		},
		Fail: func(code int, err error) {
			if errors.Is(err, errFileChanged) {
				fail(http.StatusConflict, err)
				return
			}
			fail(code, errors.New(status.Convert(err).Message()))
		},
	}.Serve(w, r)
}

// encryptStream 把文件流加密成 AES-256-CTR 的密文：从这次实际发的第一个字节的位置开始取密钥流，
// 所以续传、分段请求拼起来和整个文件的密文一样，openssl 能一次解密。第一条（文件名、大小）照原样交出，
// 先核对文件还是签发命令时的那个版本；之后每发出 recheckEvery 字节、发完时再核对一次（recheck），
// 变了就返回错误（filehttp 中断连接）。每一块都计入链接发出的字节（spend），到了上限也中断。
type encryptStream struct {
	ctx            context.Context
	inner          filehttp.ChunkStream
	key            directdl.ContentKey
	offset, length int64
	etag           string
	archive        bool
	s              cipher.Stream
	recheck        func(ctx context.Context) error
	spend          func(n int64) bool
	sinceCheck     int64
}

func (e *encryptStream) Recv() (*nagiv1.VolumeFileChunk, error) {
	c, err := e.inner.Recv()
	if errors.Is(err, io.EOF) && e.s != nil && e.recheck != nil {
		if err := e.recheck(e.ctx); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	if e.s == nil {
		if c.GetArchive() != e.archive || (!e.archive && filehttp.ETag(c) != e.etag) {
			return nil, errFileChanged
		}
		start := int64(0)
		if c.GetRanged() && (e.offset != 0 || e.length != 0) {
			start, _, _ = byterange.Resolve(e.offset, e.length, c.GetSize())
		}
		e.s = e.key.Stream(start)
		return &nagiv1.VolumeFileChunk{Name: c.GetName(), Size: c.GetSize(), ModifiedUnixNano: c.GetModifiedUnixNano(), Archive: c.GetArchive(), Ranged: c.GetRanged()}, nil
	}
	n := int64(len(c.GetData()))
	if e.spend != nil && !e.spend(n) {
		return nil, errLinkBudget
	}
	if e.recheck != nil {
		e.sinceCheck += n
		if e.sinceCheck >= recheckEvery {
			e.sinceCheck = 0
			if err := e.recheck(e.ctx); err != nil {
				return nil, err
			}
		}
	}
	out := make([]byte, n)
	e.s.XORKeyStream(out, c.GetData())
	return &nagiv1.VolumeFileChunk{Data: out}, nil
}

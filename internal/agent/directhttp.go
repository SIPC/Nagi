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

// 加密 HTTP 下载（见 docs/agent.md#加密-http-下载）：节点申请不到 IP 证书时的备用方式，默认关闭，在节点设置里开启。
// 面板签一个 HTTP 种类的链接，先经控制连接交给节点（PrepareDirectHttp）：节点核对后记下文件的版本（ETag），
// 文件不大时算出 SHA-256；面板据此生成一条 curl + openssl 的命令。之后 curl 经明文 HTTP 来下载，
// 节点用 AES-256-CTR 加密内容，密钥由节点和面板各自从直连下载的密钥派生（directdl.FileKey、ArchiveKey），
// 不经网络传。只认签发命令时登记过的链接；文件之后变了就拒绝（同一段密钥流不加密两份不同的内容），
// 目录打包的链接只能用一次。链接无效时一律 404，不说原因。

func directHTTPFile(dataDir string) string {
	return filepath.Join(dataDir, "identity", "direct-http")
}

// httpPort 返回加密 HTTP 下载的端口，没有开启时为 0。开关存成文件，内容是端口号。
func (d *directDL) httpPort() int {
	if d == nil {
		return 0
	}
	b, err := os.ReadFile(directHTTPFile(d.a.cfg.DataDir))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || validHTTPPort(port) != nil {
		return 0
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

// setHTTPPort 修改加密 HTTP 下载的端口，0 表示关闭。
func (d *directDL) setHTTPPort(port int) error {
	file := directHTTPFile(d.a.cfg.DataDir)
	if port == 0 {
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

// wantedHTTP 返回要开加密 HTTP 下载的 IP 和端口：开启了、有公网地址、有面板给的密钥，而且端口能用。
// 在 80 上、Caddy 已经监听着 80（有绑了域名的映射，或者已经开着）时直接用；别的端口已经由这里开着时也直接用；
// 否则先试着占一下，被别的程序占着就不开。被映射占着的由 proxy.Apply 判断。
func (d *directDL) wantedHTTP(p *proxy.Proxy) (string, int) {
	if d == nil {
		return "", 0
	}
	port := d.httpPort()
	d.mu.Lock()
	addr, haveKey := d.address, len(d.keys) > 0
	d.mu.Unlock()
	if port == 0 || addr == "" || !haveKey || p == nil {
		d.setHTTPPortErr("")
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
	d.setHTTPPortErr("")
	return addr, port
}

func (d *directDL) setHTTPPortErr(e string) {
	d.mu.Lock()
	d.httpPortErr = e
	d.mu.Unlock()
}

// httpStatus 把加密 HTTP 下载的状态填进上报的 st（调用时持有 d.mu）。
func (d *directDL) httpStatus(p *proxy.Proxy, st *nagiv1.DirectDownload) {
	st.HttpPort = uint32(d.httpPort())
	if st.HttpPort == 0 || p == nil {
		return
	}
	addr, port, perr := p.DirectHTTP()
	switch {
	case d.httpPortErr != "":
		st.HttpError = d.httpPortErr
	case perr != "":
		st.HttpError = perr
	case d.address != "" && addr == d.address && port == int(st.HttpPort):
		st.HttpReady = true
	}
}

// preparedLink 是签发命令时登记的链接。
type preparedLink struct {
	etag    string // 文件的版本；目录打包为空
	archive bool
	used    bool // 目录打包：已经下载过（只能用一次）
	expires time.Time
}

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

// preparedFor 返回登记过的链接；目录打包的同时标记为用过（第二次返回 false）。
func (d *directDL) preparedFor(id string, consume bool) (preparedLink, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.prepared[id]
	if !ok || id == "" || time.Now().After(l.expires) {
		return preparedLink{}, false
	}
	if l.archive && consume {
		if l.used {
			return preparedLink{}, false
		}
		l.used = true
	}
	return *l, true
}

var errFileChanged = i18n.New("文件已变化，请重新获取下载命令")

func (s *agentService) PrepareDirectHttp(ctx context.Context, req *nagiv1.PrepareDirectHttpRequest) (*nagiv1.PrepareDirectHttpResponse, error) {
	d := s.node.direct
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
	if err := d.remember(c.ID, &preparedLink{etag: resp.Etag, archive: resp.Archive, expires: expires}); err != nil {
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

// serveHTTPDL 处理加密 HTTP 下载的请求。链接无效（签名不对、过期、没登记、目录打包用过了、没开启）时一律 404，
// 不说原因；过期的和用过的记进本地审计。
func (d *directDL) serveHTTPDL(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodGet || d.httpPort() == 0 {
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
	prep, ok := d.preparedFor(c.ID, false)
	if !ok {
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
	if c.Kind == directdl.KindNode {
		if err := d.a.checkNodeAccess(); err != nil {
			d.audit(panel, c, token, r, err, "denied")
			fail(http.StatusForbidden, errors.New(status.Convert(err).Message()))
			return
		}
	}
	if prep.archive {
		if _, ok := d.preparedFor(c.ID, true); !ok {
			d.audit(panel, c, token, r, i18n.New("下载链接已使用"), "denied")
			http.NotFound(w, r)
			return
		}
	}
	ck := directdl.ArchiveKey(key, c)
	if !prep.archive {
		ck = directdl.FileKey(key, c, prep.etag)
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
			return &encryptStream{inner: d.open(ctx, c, offset, length), key: ck, offset: offset, length: length, etag: prep.etag, archive: prep.archive}, nil
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
// 先核对文件还是签发命令时的那个版本。
type encryptStream struct {
	inner          filehttp.ChunkStream
	key            directdl.ContentKey
	offset, length int64
	etag           string
	archive        bool
	s              cipher.Stream
}

func (e *encryptStream) Recv() (*nagiv1.VolumeFileChunk, error) {
	c, err := e.inner.Recv()
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
	out := make([]byte, len(c.GetData()))
	e.s.XORKeyStream(out, c.GetData())
	return &nagiv1.VolumeFileChunk{Data: out}, nil
}

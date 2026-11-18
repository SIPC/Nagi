package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/directdl"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/proto"
)

// 经面板下载的签名链接（见 docs/panel.md#会话）：下载工具（IDM 等）截下浏览器的下载后，用自己的客户端带着浏览器的 cookie
// 重新请求，TLS 指纹和浏览器的不同，按 cookie 检查会被当成会话被盗、退出登录。所以界面先在 /api/volume-file、/api/node-file
// 上带 check=1&sign=1 请求（照常带 cookie、做同样的检查），面板签一个 5 分钟有效的链接 /api/download/<token>，浏览器再去下载它。
//
// 链接只凭签名和有效期，不看 cookie，不做会话的设备和指纹检查，不会让人退出登录。每次使用都按链接里的会话重查：用户和会话还在、
// 没退出、没过期，角色和访问范围仍然允许（LinkUser 和下面的 linkAllowed）。有效期内可以从第一次使用的地址多次使用
// （续传、多线程下载），别的地址 403（linkBinds）；已经开始的传输过期后照常传完。
//
// token 是 AES-256-GCM 加密的链接内容：防篡改，也不让路径、会话 ID 出现在下载工具或代理的日志里。密钥每个进程随机生成，
// 面板重启后之前的链接失效。

// downloadLinkTTL 是签名链接的有效期。
const downloadLinkTTL = 5 * time.Minute

// downloadLinkPath 是签名链接的前缀。
const downloadLinkPath = "/api/download/"

const (
	linkKindVolume = "volume"
	linkKindNode   = "node"
)

// downloadClaims 是签名链接的内容。
type downloadClaims struct {
	Kind      string `json:"k"`
	Project   string `json:"p,omitempty"`
	Container string `json:"c,omitempty"`
	Volume    string `json:"v,omitempty"`
	Snapshot  string `json:"s,omitempty"`
	Node      string `json:"n,omitempty"`
	Path      string `json:"f"`
	User      int64  `json:"u"`
	Session   string `json:"sid"`
	// IP 是签发时浏览器的地址：第一次使用的地址不是它时多记一条活动记录（不算错误）。
	IP string `json:"ip,omitempty"`
	// ID 是链接的编号，签发和使用的活动记录都带上它（download_link）。
	ID      string `json:"id"`
	Expires int64  `json:"e"`
}

// linkAAD 绑定用途：别处用同一个密钥加密的内容不能当下载链接。
var linkAAD = []byte("nagi-download-link-v1")

var linkAEAD = sync.OnceValue(func() cipher.AEAD {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return g
})

func sealDownload(c downloadClaims) (string, error) {
	plain, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	g := linkAEAD()
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(g.Seal(nonce, nonce, plain, linkAAD)), nil
}

var errBadLink = errors.New("invalid download link")

// openDownload 解开 token，核对完整性、有效期和种类。
func openDownload(token string, now time.Time) (downloadClaims, error) {
	var c downloadClaims
	if len(token) > 4096 {
		return c, errBadLink
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	g := linkAEAD()
	if err != nil || len(raw) < g.NonceSize()+g.Overhead() {
		return c, errBadLink
	}
	plain, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], linkAAD)
	if err != nil {
		return c, errBadLink
	}
	if json.Unmarshal(plain, &c) != nil || c.Session == "" || c.User == 0 || c.ID == "" {
		return c, errBadLink
	}
	if c.Kind != linkKindVolume && c.Kind != linkKindNode {
		return c, errBadLink
	}
	if now.Unix() >= c.Expires {
		return c, errBadLink
	}
	return c, nil
}

// issueDownloadLink 签发链接并回答浏览器（{"url": "/api/download/…"}），成功时记一次下载（record，参数里带链接编号）。
// 调用前已经按 cookie 检查过会话、权限，并确认文件能下载。
func issueDownloadLink(w http.ResponseWriter, r *http.Request, ctx context.Context, u auth.User, c downloadClaims, record func(extra map[string]any)) {
	if u.SessionID == "" {
		jsonFail(w, r, http.StatusForbidden, i18n.New("当前账号没有权限执行这个操作"))
		return
	}
	c.User, c.Session = u.ID, u.SessionID
	c.IP = auth.RequestMeta(ctx).IP
	c.ID = directdl.NewID()
	c.Expires = time.Now().Add(downloadLinkTTL).Unix()
	token, err := sealDownload(c)
	if err != nil {
		jsonFail(w, r, http.StatusInternalServerError, err)
		return
	}
	record(map[string]any{"download_link": c.ID})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"url": downloadLinkPath + token, "expires": c.Expires})
}

// fileSource 是从节点读文件的方式（经 mesh），用完调 close。
type fileSource struct {
	open  func(ctx context.Context, offset, length int64) (chunkStream, error)
	close func()
}

// sourceError 是打开 fileSource 失败时给浏览器的状态码。
type sourceError struct {
	code int
	err  error
}

func (e *sourceError) Error() string { return e.err.Error() }
func (e *sourceError) Unwrap() error { return e.err }

func sourceCode(err error) int {
	var se *sourceError
	if errors.As(err, &se) {
		return se.code
	}
	return http.StatusBadGateway
}

// meshClient 连上节点的 MeshService。
func (a *API) meshClient(nodeID string) (nagiv1.MeshServiceClient, func(), error) {
	m := a.Nodes.Mesh()
	if m == nil {
		return nil, nil, &sourceError{http.StatusServiceUnavailable, i18n.New("面板的节点网络没有启动，无法传输文件")}
	}
	key, err := a.Nodes.NodeMeshKey(nodeID)
	if err != nil {
		return nil, nil, &sourceError{http.StatusServiceUnavailable, err}
	}
	cc, err := mesh.Client(m, key)
	if err != nil {
		return nil, nil, &sourceError{http.StatusServiceUnavailable, err}
	}
	return nagiv1.NewMeshServiceClient(cc), func() { cc.Close() }, nil
}

// volumeSourceFor、nodeSourceFor 打开卷文件、节点文件，测试里换掉。
var volumeSourceFor = func(a *API, ctx context.Context, ref *nagiv1.VolumeFileRef) (fileSource, error) {
	nodeID, err := a.Projects.Locate(ctx, ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return fileSource{}, &sourceError{http.StatusNotFound, err}
	}
	client, done, err := a.meshClient(nodeID)
	if err != nil {
		return fileSource{}, err
	}
	return fileSource{close: done, open: func(ctx context.Context, offset, length int64) (chunkStream, error) {
		part := proto.CloneOf(ref)
		part.Offset, part.Length = offset, length
		return client.VolumeDownload(ctx, part)
	}}, nil
}

var nodeSourceFor = func(a *API, _ context.Context, nodeID, path string) (fileSource, error) {
	client, done, err := a.meshClient(nodeID)
	if err != nil {
		return fileSource{}, err
	}
	return fileSource{close: done, open: func(ctx context.Context, offset, length int64) (chunkStream, error) {
		return client.NodeFileDownload(ctx, &nagiv1.NodeFileRef{NodeId: nodeID, Path: path, Offset: offset, Length: length})
	}}, nil
}

// linkAllowed 是使用链接时重查的权限，和签发时（VolumeFileHandler、NodeFileHandler 的下载）一样。
// 节点文件的高权限确认只在签发时检查：链接 5 分钟内有效，确认 15 分钟有效。
func (a *API) linkAllowed(ctx context.Context, u auth.User, c downloadClaims) bool {
	switch c.Kind {
	case linkKindVolume:
		return auth.CanOperate(u.Role) && a.projectAllowed(ctx, u, c.Project) == nil
	case linkKindNode:
		return u.Role == auth.RoleAdmin
	}
	return false
}

// linkBinds 记下每个链接第一次使用时的来源地址（见 docs/panel.md#会话）：之后只接受这个地址，下载工具和浏览器
// 在同一台电脑上，地址相同，续传和多线程下载照常；拿到链接的人从别的地址来会被拒绝。按确切的地址比较：
// 同一台电脑先后经 IPv4 和 IPv6 来、换了网络或 NAT 出口时，要在界面上重新下载。
var linkBinds = struct {
	sync.Mutex
	m map[string]*linkBind
}{m: map[string]*linkBind{}}

type linkBind struct {
	ip string
	at time.Time
	// otherIP：第一次使用的地址不是签发的地址，已经记过活动记录。
	otherIP bool
	// denied 是被拒绝过的地址，每个只记一次安全事件，最多记 maxLinkDenied 个。
	denied map[string]bool
}

const maxLinkDenied = 20

// bindLink 返回链接 id 绑定的地址（第一次使用时绑定到 ip）。ip 不是绑定的地址时，report 表示要为它记一条安全事件。
func bindLink(id, ip string) (bound string, report bool) {
	linkBinds.Lock()
	defer linkBinds.Unlock()
	now := time.Now()
	if len(linkBinds.m) > 10000 {
		// 链接签发后 5 分钟过期，绑定 10 分钟以后的记录已经用不上了。
		for k, b := range linkBinds.m {
			if now.Sub(b.at) > 2*downloadLinkTTL {
				delete(linkBinds.m, k)
			}
		}
	}
	b := linkBinds.m[id]
	if b == nil {
		linkBinds.m[id] = &linkBind{ip: ip, at: now}
		return ip, false
	}
	if b.ip == ip || b.denied[ip] || len(b.denied) >= maxLinkDenied {
		return b.ip, false
	}
	if b.denied == nil {
		b.denied = map[string]bool{}
	}
	b.denied[ip] = true
	return b.ip, true
}

// firstOtherIP 报告链接是不是第一次从签发以外的地址（也就是它绑定的地址）下载完成，每个链接只报一次。
func firstOtherIP(id string) bool {
	linkBinds.Lock()
	defer linkBinds.Unlock()
	b := linkBinds.m[id]
	if b == nil || b.otherIP {
		return false
	}
	b.otherIP = true
	return true
}

// DownloadLinkHandler：GET /api/download/{token}，经面板下载签名链接指向的文件。只凭链接，不看 cookie（见文件开头）。
// 链接无效、过期一律 404，会话或权限不再允许时 403，都不说明原因。
func (a *API) DownloadLinkHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "private, no-store")
		c, err := openDownload(r.PathValue("token"), time.Now())
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if c.Kind == linkKindNode && a.Panel != nil && a.Panel.Standby != nil {
			http.Error(w, i18n.Localize(requestLang(r), a.standbyError()), http.StatusServiceUnavailable)
			return
		}
		ip := auth.RequestIP(r.RemoteAddr, r.Header)
		u, err := a.Auth.LinkUser(r.Context(), c.Session, c.User, ip)
		if err != nil || !a.linkAllowed(r.Context(), u, c) {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		ctx := auth.WithRequest(auth.WithUser(r.Context(), u), r)
		// 第一次使用时绑定来源地址，之后别的地址 403，记一条安全事件（每个链接每个地址一次）。
		if bound, report := bindLink(c.ID, ip); bound != ip {
			if report {
				a.Auth.LinkAddressDenied(ctx, u, c.ID, bound, c.IP)
			}
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		lang := requestLang(r)
		fail := func(code int, err error) {
			h.Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": errorText(lang, err)})
		}

		var src fileSource
		var record func(failure string)
		switch c.Kind {
		case linkKindVolume:
			ref := &nagiv1.VolumeFileRef{ProjectId: c.Project, Container: c.Container, Volume: c.Volume, Path: c.Path, SnapshotId: c.Snapshot}
			src, err = volumeSourceFor(a, ctx, ref)
			record = func(failure string) {
				params := map[string]any{"download_link": c.ID, "other_ip": true, "issued_ip": c.IP}
				result := audit.ResultOK
				if failure != "" {
					result = audit.ResultFailed
				}
				a.recordCallWith(ctx, "/DownloadVolumeFile", ref, result, failure, 0, params)
			}
		case linkKindNode:
			src, err = nodeSourceFor(a, ctx, c.Node, c.Path)
			record = func(failure string) {
				params := map[string]any{"detail": c.Path, "download_link": c.ID, "other_ip": true, "issued_ip": c.IP}
				e := audit.Event{Action: "DownloadNodeFile", TargetType: "node", TargetID: c.Node, TargetName: a.nodeName(ctx, c.Node)}
				if failure != "" {
					e.Result, e.Error = audit.ResultFailed, failure
				}
				b, _ := json.Marshal(params)
				e.Params = string(b)
				a.Record(context.WithoutCancel(ctx), e)
			}
		}
		if err != nil {
			fail(sourceCode(err), err)
			return
		}
		defer src.close()
		// 签发时已经记过这次下载；第一次使用的地址不是签发的地址时（下载工具走代理、浏览器换了 IPv4 和 IPv6）多记一条，
		// 带上两个地址，每个链接只记一次。
		fileDownload{
			open: src.open,
			record: func(failure string) {
				if failure == "" && ip != c.IP && firstOtherIP(c.ID) {
					record("")
				}
			},
			fail: fail,
			key:  "link|" + c.ID + "|" + ip,
		}.serve(w, r)
	})
}

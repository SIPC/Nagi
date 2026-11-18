package nodes

import (
	"context"
	"crypto/rand"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/directdl"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 直连下载（见 docs/agent.md#直连下载）：节点连上、认本面板是主面板以后，面板为这条连接随机生成一个密钥，
// 连同面板看到的节点公网地址告诉节点。签发下载链接时用这个密钥，节点用它核对。密钥只在内存里：
// 面板重启或者节点重连后换一个，之前签的链接随之失效（本来也只有 5 分钟）。成员的权限被撤销时也换（RotateDirect）。

// takeDirectPush 判断这次上报以后要不要告诉节点直连下载的设置：每条连接一次，节点已批准、认本面板是主面板，
// 不是本面板自带的节点（和面板在同一台机器上，直连没有意义）。
func (c *Conn) takeDirectPush(req *nagiv1.ReportRequest, pending, accepts bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.directSent || pending || !accepts || c.ownEmbedded(req) || req.GetNode().GetDirectDownload() == nil {
		return false
	}
	c.directSent = true
	return true
}

func (r *Registry) pushDirect(c *Conn) {
	if r.ReadOnly {
		return
	}
	c.directPush.Lock()
	defer c.directPush.Unlock()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return
	}
	addr := ""
	if ip := c.Remote(); directdl.PublicAddress(ip) {
		addr = ip.Unmap().String()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Agent.SetDirectDownload(ctx, &nagiv1.SetDirectDownloadRequest{Key: key, Address: addr}); err != nil {
		if status.Code(err) != codes.Unimplemented {
			r.log.Warn("告诉节点直连下载的设置失败", "node", c.NodeID, "err", err)
		}
		return
	}
	c.mu.Lock()
	c.directKey, c.directAddr = key, addr
	c.mu.Unlock()
}

// RotateDirect 给每个告诉过密钥的在线节点换一个新密钥，之前签的直连下载链接立即失效（已经开始的下载照常传完）。
// 成员被删除、角色降低、访问范围改了、密码被重置、退出登录、撤销登录设备或访问令牌时调用：
// 链接不绑定用户，不换密钥的话撤销以后链接在有效期内照样能用。换好之前不签新链接（浏览器照旧经面板下载）。
// 等所有节点回答后返回；告诉节点失败时只记日志（节点重连时会拿到新密钥，链接最多 5 分钟后也会过期）。
func (r *Registry) RotateDirect() {
	if r.ReadOnly {
		return
	}
	var wg sync.WaitGroup
	for _, c := range r.Conns() {
		c.mu.Lock()
		sent := c.directSent
		c.directKey = nil
		c.mu.Unlock()
		if !sent {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.pushDirect(c)
		}()
	}
	wg.Wait()
}

// DirectLink 签一个直连下载链接（claims 里的 Node、Expires 由这里填，没有 ID 时也填上）。节点不能直连时（没有公网地址、关了、
// 还没拿到证书、旧版本、还没告诉过它密钥）返回空。
func (c *Conn) DirectLink(claims directdl.Claims) string {
	c.mu.RLock()
	key, addr := c.directKey, c.directAddr
	st := c.report.GetNode().GetDirectDownload()
	c.mu.RUnlock()
	if len(key) == 0 || addr == "" || !st.GetReady() || st.GetOff() || st.GetAddress() != addr {
		return ""
	}
	claims.Node = c.NodeID
	claims.Expires = time.Now().Add(directdl.TTL).Unix()
	if claims.ID == "" {
		claims.ID = directdl.NewID()
	}
	return directdl.URL(addr, directdl.Sign(key, claims))
}

// DirectHTTPCommand 是一条加密 HTTP 下载的命令（见 docs/agent.md#加密-http-下载），连同界面要显示的信息。
// Command 里有内容密钥，只交给签发它的成员，不记进活动记录和日志。
type DirectHTTPCommand struct {
	// Command 是 POSIX sh 的命令（Linux、macOS），CommandWindows 是 PowerShell 的（见 directdl.Command）。
	Command        string
	CommandWindows string
	// Segmented：命令分段同时下载（大文件，见 directdl.Command.Segmented）。
	Segmented bool
	// ID 是链接编号（活动记录的 direct_link）。
	ID      string
	Name    string
	Size    int64
	Archive bool
	// SHA256 为空时命令不核对内容，原因在 HashSkipped（directdl.HashSkipped*）。
	SHA256      string
	HashSkipped string
	Expires     time.Time
}

// DirectHTTP 签一个加密 HTTP 下载的链接，交给节点核对、登记（节点同时算出文件的 SHA-256），
// 再用同一个密钥派生内容密钥，生成命令。节点不能用（关了直连下载、HTTPS 能用所以没开、端口不能用、旧版本、
// 还没告诉过它密钥）时返回 nil, nil。
func (c *Conn) DirectHTTP(ctx context.Context, claims directdl.Claims) (*DirectHTTPCommand, error) {
	c.mu.RLock()
	key, addr := c.directKey, c.directAddr
	st := c.report.GetNode().GetDirectDownload()
	c.mu.RUnlock()
	if len(key) == 0 || addr == "" || st.GetOff() || !st.GetHttpReady() || st.GetHttpPort() == 0 || st.GetAddress() != addr {
		return nil, nil
	}
	expires := time.Now().Add(directdl.TTL)
	claims.Node = c.NodeID
	claims.Expires = expires.Unix()
	if claims.ID == "" {
		claims.ID = directdl.NewID()
	}
	token := directdl.SignHTTP(key, claims)
	// 节点算 SHA-256 最多 directdl.HTTPHashTimeout，再留些余量。
	ctx, cancel := context.WithTimeout(ctx, directdl.HTTPHashTimeout+15*time.Second)
	defer cancel()
	res, err := c.Agent.PrepareDirectHttp(ctx, &nagiv1.PrepareDirectHttpRequest{Token: token})
	if err != nil {
		return nil, err
	}
	ck := directdl.ArchiveKey(key, claims)
	if !res.GetArchive() {
		ck = directdl.FileKey(key, claims, res.GetEtag())
	}
	cmd := directdl.Command{URL: directdl.HTTPURL(addr, int(st.GetHttpPort()), token), Name: res.GetName(), Key: ck,
		SHA256: res.GetSha256(), Resume: !res.GetArchive(), Size: res.GetSize()}
	return &DirectHTTPCommand{Command: cmd.String(), CommandWindows: cmd.Windows(), Segmented: cmd.Segmented(), ID: claims.ID, Name: directdl.SafeName(res.GetName()), Size: res.GetSize(),
		Archive: res.GetArchive(), SHA256: res.GetSha256(), HashSkipped: res.GetHashSkipped(), Expires: expires}, nil
}

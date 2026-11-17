package nodes

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/SIPC/nagi/internal/pkg/directdl"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 直连下载（见 docs/agent.md#直连下载）：节点连上、认本面板是主面板以后，面板为这条连接随机生成一个密钥，
// 连同面板看到的节点公网地址告诉节点。签发下载链接时用这个密钥，节点用它核对。密钥只在内存里：
// 面板重启或者节点重连后换一个，之前签的链接随之失效（本来也只有 15 分钟）。

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

// DirectLink 签一个直连下载链接（claims 里的 Node、Expires 由这里填）。节点不能直连时（没有公网地址、关了、
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
	return directdl.URL(addr, directdl.Sign(key, claims))
}

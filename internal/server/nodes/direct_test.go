package nodes

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/directdl"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
)

// directAgent 记下面板告诉节点的直连下载密钥。
type directAgent struct {
	nagiv1.AgentServiceClient
	mu   sync.Mutex
	keys [][]byte
}

func (a *directAgent) SetDirectDownload(_ context.Context, in *nagiv1.SetDirectDownloadRequest, _ ...grpc.CallOption) (*nagiv1.SetDirectDownloadResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys = append(a.keys, append([]byte(nil), in.GetKey()...))
	return &nagiv1.SetDirectDownloadResponse{}, nil
}

func (a *directAgent) received() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]byte(nil), a.keys...)
}

// L3：撤销成员的权限时给告诉过密钥的节点换密钥，之前签的链接节点不再认；没告诉过密钥的节点（旧版本、
// 本面板自带的）不发；备用面板不发。
func TestRotateDirect(t *testing.T) {
	r := NewRegistry(nil, slog.New(slog.DiscardHandler), nil, nil)
	ready := &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{DirectDownload: &nagiv1.DirectDownload{Supported: true, Address: "203.0.113.5", Ready: true}}}
	old := make([]byte, 32)
	old[0] = 1
	sent, fresh := &directAgent{}, &directAgent{}
	r.mu.Lock()
	r.conns["sent"] = &Conn{NodeID: "sent", Agent: sent, directSent: true, directKey: old, directAddr: "203.0.113.5", report: ready,
		remote: netip.MustParseAddr("203.0.113.5")}
	r.conns["fresh"] = &Conn{NodeID: "fresh", Agent: fresh, report: ready}
	r.mu.Unlock()
	c, _ := r.Peek("sent")
	prefix := "https://203.0.113.5" + directdl.PathPrefix
	token := strings.TrimPrefix(c.DirectLink(directdl.Claims{Kind: directdl.KindVolume, Project: "p", Container: "web", Path: "a.txt"}), prefix)

	r.RotateDirect()
	keys := sent.received()
	if len(keys) != 1 || len(keys[0]) != 32 || bytes.Equal(keys[0], old) {
		t.Fatalf("应给节点换一个新密钥：%x", keys)
	}
	if len(fresh.received()) != 0 {
		t.Fatal("没告诉过密钥的节点不该发")
	}
	if _, err := directdl.Verify(keys, token, time.Now()); err == nil {
		t.Fatal("换密钥以后，之前签的链接节点不该再认")
	}
	// 新签的链接用新密钥，带着编号。
	newToken := strings.TrimPrefix(c.DirectLink(directdl.Claims{Kind: directdl.KindVolume, Project: "p", Container: "web", Path: "a.txt"}), prefix)
	if got, err := directdl.Verify(keys, newToken, time.Now()); err != nil || got.ID == "" {
		t.Fatalf("新链接应该能用：%+v %v", got, err)
	}

	r.ReadOnly = true
	r.RotateDirect()
	if len(sent.received()) != 1 {
		t.Fatal("备用面板不该给节点发密钥")
	}
}

// 只有告诉过节点密钥、节点报告这个地址已经拿到证书、没有关掉时才签链接；签出的链接节点能用同一个密钥核对。
func TestDirectLink(t *testing.T) {
	key := make([]byte, 32)
	report := func(dd *nagiv1.DirectDownload) *nagiv1.ReportRequest {
		return &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{DirectDownload: dd}}
	}
	ready := &nagiv1.DirectDownload{Supported: true, Address: "203.0.113.5", Ready: true}
	cases := []struct {
		name string
		key  []byte
		addr string
		dd   *nagiv1.DirectDownload
		ok   bool
	}{
		{"可以直连", key, "203.0.113.5", ready, true},
		{"还没告诉节点密钥", nil, "203.0.113.5", ready, false},
		{"没有公网地址", key, "", ready, false},
		{"还没拿到证书", key, "203.0.113.5", &nagiv1.DirectDownload{Supported: true, Address: "203.0.113.5"}, false},
		{"节点关了直连", key, "203.0.113.5", &nagiv1.DirectDownload{Supported: true, Address: "203.0.113.5", Ready: true, Off: true}, false},
		{"证书是旧地址的", key, "203.0.113.9", ready, false},
		{"旧版本节点", key, "203.0.113.5", nil, false},
	}
	for _, c := range cases {
		conn := &Conn{NodeID: "n1", directKey: c.key, directAddr: c.addr, report: report(c.dd)}
		url := conn.DirectLink(directdl.Claims{Kind: directdl.KindVolume, Project: "p", Container: "web", Path: "a.txt"})
		if (url != "") != c.ok {
			t.Errorf("%s：%q", c.name, url)
			continue
		}
		if !c.ok {
			continue
		}
		token, ok := strings.CutPrefix(url, "https://203.0.113.5"+directdl.PathPrefix)
		if !ok {
			t.Fatalf("链接：%s", url)
		}
		got, err := directdl.Verify([][]byte{key}, token, time.Now())
		if err != nil || got.Node != "n1" || got.Path != "a.txt" || got.Expires < time.Now().Add(directdl.TTL-time.Minute).Unix() {
			t.Errorf("节点核对不过：%+v %v", got, err)
		}
	}
}

// prepareAgent 像节点一样核对加密 HTTP 下载的链接，返回文件的版本和 SHA-256。
type prepareAgent struct {
	nagiv1.AgentServiceClient
	key    []byte
	tokens []string
}

func (a *prepareAgent) PrepareDirectHttp(_ context.Context, in *nagiv1.PrepareDirectHttpRequest, _ ...grpc.CallOption) (*nagiv1.PrepareDirectHttpResponse, error) {
	if _, err := directdl.VerifyHTTP([][]byte{a.key}, in.GetToken(), time.Now()); err != nil {
		return nil, err
	}
	a.tokens = append(a.tokens, in.GetToken())
	return &nagiv1.PrepareDirectHttpResponse{Name: "a b.txt", Size: 3, Etag: `"3-1"`, Sha256: strings.Repeat("ab", 32)}, nil
}

// 加密 HTTP 下载的命令：节点开启了、规则生效、地址一致、告诉过密钥时才生成；链接是 HTTP 种类的，带端口；
// 内容密钥和节点用同一个面板密钥、同一个文件版本派生的一样；命令里没有面板的密钥本身。换密钥后之前的链接节点不认。
func TestDirectHTTPCommand(t *testing.T) {
	key := make([]byte, 32)
	key[3] = 4
	report := func(dd *nagiv1.DirectDownload) *nagiv1.ReportRequest {
		return &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{DirectDownload: dd}}
	}
	ready := &nagiv1.DirectDownload{Supported: true, Address: "203.0.113.5", HttpSupported: true, HttpPort: 8080, HttpReady: true}
	cases := []struct {
		name string
		key  []byte
		addr string
		dd   *nagiv1.DirectDownload
		ok   bool
	}{
		{"可以", key, "203.0.113.5", ready, true},
		// 「直连下载」是唯一的开关：关了就都经面板。
		{"关了直连下载", key, "203.0.113.5", &nagiv1.DirectDownload{Supported: true, Off: true, Address: "203.0.113.5", HttpPort: 8080, HttpReady: true}, false},
		{"还没告诉节点密钥", nil, "203.0.113.5", ready, false},
		{"没有公网地址", key, "", ready, false},
		{"没开启", key, "203.0.113.5", &nagiv1.DirectDownload{Supported: true, Address: "203.0.113.5", HttpSupported: true}, false},
		{"端口不能用", key, "203.0.113.5", &nagiv1.DirectDownload{Supported: true, Address: "203.0.113.5", HttpPort: 8080, HttpError: "占用"}, false},
		{"旧地址", key, "203.0.113.9", ready, false},
		{"旧版本节点", key, "203.0.113.5", nil, false},
	}
	for _, c := range cases {
		ag := &prepareAgent{key: key}
		conn := &Conn{NodeID: "n1", Agent: ag, directKey: c.key, directAddr: c.addr, report: report(c.dd)}
		claims := directdl.Claims{Kind: directdl.KindVolume, Project: "p", Container: "web", Volume: "data", Path: "a b.txt", ID: "0000000000000001"}
		cmd, err := conn.DirectHTTP(context.Background(), claims)
		if err != nil {
			t.Fatalf("%s：%v", c.name, err)
		}
		if (cmd != nil) != c.ok {
			t.Errorf("%s：%v", c.name, cmd)
			continue
		}
		if !c.ok {
			continue
		}
		if len(ag.tokens) != 1 || !strings.Contains(cmd.Command, "'http://203.0.113.5:8080"+directdl.HTTPPathPrefix+ag.tokens[0]+"'") {
			t.Fatalf("%s：命令里的链接不对：%s", c.name, cmd.Command)
		}
		claims.Node = "n1"
		want := directdl.FileKey(key, claims, `"3-1"`)
		if !strings.Contains(cmd.Command, hex.EncodeToString(want.Key[:])) || !strings.Contains(cmd.Command, hex.EncodeToString(want.IV[:])) {
			t.Errorf("%s：内容密钥和节点派生的不一样：%s", c.name, cmd.Command)
		}
		if strings.Contains(cmd.Command, hex.EncodeToString(key)) || strings.Contains(ag.tokens[0], hex.EncodeToString(want.Key[:])) {
			t.Errorf("%s：面板密钥不该出现在命令里，内容密钥不该出现在链接里", c.name)
		}
		if cmd.ID != "0000000000000001" || cmd.SHA256 == "" || !strings.Contains(cmd.Command, "SHA-256 OK") || cmd.Name != "a b.txt" {
			t.Errorf("%s：%+v", c.name, cmd)
		}
	}

	// 换密钥以后，之前签的 HTTP 链接节点也不认。
	r := NewRegistry(nil, slog.New(slog.DiscardHandler), nil, nil)
	sent := &directAgent{}
	r.mu.Lock()
	r.conns["n1"] = &Conn{NodeID: "n1", Agent: sent, directSent: true, directKey: key, directAddr: "203.0.113.5", report: report(ready),
		remote: netip.MustParseAddr("203.0.113.5")}
	r.mu.Unlock()
	token := directdl.SignHTTP(key, directdl.Claims{Node: "n1", Kind: directdl.KindNode, Path: "/x", Expires: time.Now().Add(time.Minute).Unix()})
	r.RotateDirect()
	if _, err := directdl.VerifyHTTP(sent.received(), token, time.Now()); err == nil {
		t.Fatal("换密钥以后，之前签的 HTTP 链接节点不该再认")
	}
}

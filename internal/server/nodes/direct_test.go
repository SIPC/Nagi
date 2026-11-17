package nodes

import (
	"bytes"
	"context"
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

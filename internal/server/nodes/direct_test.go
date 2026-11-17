package nodes

import (
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/directdl"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

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

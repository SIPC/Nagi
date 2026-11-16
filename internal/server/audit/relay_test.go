package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeNode 是一个本地审计在内存里的节点。
type fakeNode struct {
	mu      sync.Mutex
	lines   [][]byte
	start   int64
	offline bool
}

func (n *fakeNode) add(extra string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	seq := len(n.lines) + 1
	n.lines = append(n.lines, fmt.Appendf(nil, `{"seq":%d,"action":"node_exec"%s}`, seq, extra))
}

func (n *fakeNode) read(_ context.Context, after int64, limit int) ([][]byte, int64, int64, int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.offline {
		return nil, 0, 0, 0, errors.New("unavailable")
	}
	if after < 0 {
		return nil, 0, 0, n.start, nil
	}
	var out [][]byte
	for i := after; i < int64(len(n.lines)) && len(out) < limit; i++ {
		out = append(out, n.lines[i])
	}
	if len(out) == 0 {
		return nil, 0, 0, 0, nil
	}
	return out, after + 1, after + int64(len(out)), 0, nil
}

// H2：节点不再持有外送凭据，面板读走节点的本地审计代为外送。每行的来源由面板填，节点冒充不了面板和别的节点；
// 节点连不上时不算目标失败，恢复后从游标处补发；第一次从节点给的起点开始。
func TestRelayNodeAudit(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			var m map[string]any
			_ = json.Unmarshal(sc.Bytes(), &m)
			got = append(got, m)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	l := openTestLog(t)
	n := &fakeNode{start: 2}
	for range 2 {
		n.add("")
	}
	// 被攻破的节点想冒充面板和别的节点。
	n.add(`,"source":"panel","node_id":"other","node":"other"`)
	nodes := []NodeSource{{ID: "n1", Name: "web-1", Read: n.read}}
	s := &Shipper{Log: l, Nodes: func() []NodeSource { return nodes }}
	s.Configure(ctx, nil)
	sk := SinkConfig{ID: "s1", Type: "http", URL: srv.URL}
	if err := s.relayNodes(ctx, sk); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(got) != 1 || got[0]["seq"] != float64(3) || got[0]["source"] != "node" || got[0]["node_id"] != "n1" || got[0]["node"] != "web-1" {
		t.Fatalf("应该从起点之后开始发，来源由面板填：%v", got)
	}
	mu.Unlock()

	// 节点连不上：不算目标失败，记录留在节点上。
	n.mu.Lock()
	n.offline = true
	n.mu.Unlock()
	for range 700 {
		n.add("")
	}
	if err := s.relayNodes(ctx, sk); err != nil {
		t.Fatalf("节点连不上不应该算目标失败：%v", err)
	}
	n.mu.Lock()
	n.offline = false
	n.mu.Unlock()
	if err := s.relayNodes(ctx, sk); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(got) != 701 || got[700]["seq"] != float64(703) {
		t.Fatalf("恢复后应该补发全部 700 条，实际 %d 条", len(got))
	}
	mu.Unlock()
	// 游标存在审计库里，面板重启后接着发。
	if v := l.Meta(ctx, nodeCursorKey("s1", "n1")); v != "703" {
		t.Fatalf("游标 %q", v)
	}
	s2 := &Shipper{Log: l, Nodes: func() []NodeSource { return nodes }}
	s2.Configure(ctx, nil)
	n.add("")
	_ = s2.relayNodes(ctx, sk)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 702 {
		t.Fatalf("重启后接着发：%d 条", len(got))
	}
}

// H2：S3 目标上，节点的记录由面板写到 node/<节点 ID>/ 下。
func TestRelayNodeAuditS3Key(t *testing.T) {
	var mu sync.Mutex
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		puts = append(puts, r.URL.Path)
		mu.Unlock()
	}))
	defer srv.Close()
	ctx := context.Background()
	n := &fakeNode{}
	n.add("")
	s := &Shipper{Log: openTestLog(t), Nodes: func() []NodeSource { return []NodeSource{{ID: "n1", Name: "a", Read: n.read}} }}
	s.Configure(ctx, nil)
	sk := SinkConfig{ID: "s", Type: "s3", Endpoint: srv.URL, Bucket: "b", Prefix: "nagi/", AccessKey: "a", SecretKey: "s"}
	if err := s.relayNodes(ctx, sk); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 1 || !strings.HasPrefix(puts[0], "/b/nagi/node/n1/") {
		t.Fatalf("puts = %v", puts)
	}
}

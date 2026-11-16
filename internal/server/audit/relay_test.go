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

// R2：被攻破的节点返回伪造的 first、last：之前面板用节点给的 first 拼 S3 的键（能覆盖这一小时早些时候的对象），
// 游标不检查是否前进（每轮重复上传）；一个节点出错时排在后面的节点这一轮都代发不了，目标还被记成失败。
func TestRelayNodeForgedBatches(t *testing.T) {
	var mu sync.Mutex
	puts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		puts[r.URL.Path]++
		// evil 的第一批被接收端拒收。
		if strings.Contains(r.URL.Path, "/node/evil/") && puts[r.URL.Path] == 1 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	l := openTestLog(t)
	good := &fakeNode{}
	good.add("")
	// evil 每次都说 first=1、last=1，并且接收端拒收它的内容。
	evil := func(_ context.Context, after int64, _ int) ([][]byte, int64, int64, int64, error) {
		if after < 0 {
			return nil, 0, 0, 0, nil
		}
		return [][]byte{[]byte(`{"seq":1}`)}, 1, 1, 0, nil
	}
	nodes := []NodeSource{{ID: "evil", Name: "evil", Read: evil}, {ID: "good", Name: "good", Read: good.read}}
	s := &Shipper{Log: l, Nodes: func() []NodeSource { return nodes }}
	s.Configure(ctx, nil)
	sk := SinkConfig{ID: "s", Type: "s3", Endpoint: srv.URL, Bucket: "b", Prefix: "nagi/", AccessKey: "a", SecretKey: "s"}
	if err := s.relayNodes(ctx, sk); err != nil {
		t.Fatalf("单个节点出错不应算目标失败：%v", err)
	}
	// 第二轮重试成功，游标到 1；第三轮 evil 再给 first=1，要丢弃。
	for range 2 {
		if err := s.relayNodes(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	var evilPuts, goodPuts int
	for p, n := range puts {
		if strings.Contains(p, "/node/evil/") {
			evilPuts += n
		}
		if strings.Contains(p, "/node/good/") {
			goodPuts += n
		}
	}
	if goodPuts != 1 {
		t.Fatalf("evil 出错时 good 照样要代发：%v", puts)
	}
	if evilPuts != 2 {
		t.Fatalf("序号没有前进的批次应丢弃，不应重复上传：%v", puts)
	}
	if !validBatch([][]byte{[]byte("x")}, 5, 5, 4) || validBatch([][]byte{[]byte("x")}, 4, 5, 4) || validBatch([][]byte{[]byte("x")}, 6, 5, 4) {
		t.Fatal("validBatch")
	}
	if validBatch([][]byte{make([]byte, nodeLineBytes+1)}, 5, 5, 4) {
		t.Fatal("超长的行应丢弃")
	}
}

// R2：S3 的键用面板自己的游标，节点给一个用过的 first 也写不到已有的键上。
func TestRelayNodeKeyFromCursor(t *testing.T) {
	var mu sync.Mutex
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		puts = append(puts, r.URL.Path)
		mu.Unlock()
	}))
	defer srv.Close()
	ctx := context.Background()
	l := openTestLog(t)
	seq := int64(0)
	// 节点每次都把 first 报成 1（想让面板覆盖第一个对象），last 照常前进。
	read := func(_ context.Context, after int64, _ int) ([][]byte, int64, int64, int64, error) {
		if after < 0 {
			return nil, 0, 0, 0, nil
		}
		if after >= seq {
			return nil, 0, 0, 0, nil
		}
		return [][]byte{[]byte(`{"seq":0}`)}, 1, seq, 0, nil
	}
	s := &Shipper{Log: l, Nodes: func() []NodeSource { return []NodeSource{{ID: "n1", Name: "a", Read: read}} }}
	s.Configure(ctx, nil)
	sk := SinkConfig{ID: "s", Type: "s3", Endpoint: srv.URL, Bucket: "b", Prefix: "nagi/", AccessKey: "a", SecretKey: "s"}
	for range 3 {
		seq += 2
		_ = s.relayNodes(ctx, sk)
	}
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, p := range puts {
		if seen[p] {
			t.Fatalf("同一个键写了两次：%v", puts)
		}
		seen[p] = true
	}
	if len(puts) == 0 {
		t.Fatalf("puts = %v", puts)
	}
}

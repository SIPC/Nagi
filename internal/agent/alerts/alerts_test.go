package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

type receiver struct {
	mu   sync.Mutex
	got  []map[string]any
	fail bool
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	body, _ := io.ReadAll(req.Body)
	m := map[string]any{}
	_ = json.Unmarshal(body, &m)
	r.got = append(r.got, m)
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func newEngine(t *testing.T, relay Relay) *Engine {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), "node1", t.TempDir(), t.TempDir(), nil, nil,
		func(id string) string { return "name-" + id }, relay)
}

func TestFireResolveAndDedupe(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	e := newEngine(t, nil)
	ch := &nagiv1.AlertChannel{Id: "a", Type: "webhook", Url: srv.URL}
	// 两个面板配置了同一个 webhook，只应该发一次。
	_ = e.SetConfig("p1", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{ch}})
	_ = e.SetConfig("p2", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "b", Type: "webhook", Url: srv.URL}}})

	ctx := context.Background()
	e.Fire(ctx, "snapshot:x", "定时快照失败", "boom", "x")
	e.Fire(ctx, "snapshot:x", "定时快照失败", "boom", "x") // 4 小时内不重复提醒
	if n := rcv.count(); n != 1 {
		t.Fatalf("firing 发送了 %d 次，应该是 1 次", n)
	}
	if rcv.got[0]["project"] != "name-x" || rcv.got[0]["status"] != "firing" {
		t.Fatalf("内容不对：%v", rcv.got[0])
	}
	e.Resolve(ctx, "snapshot:x")
	e.Resolve(ctx, "snapshot:x")
	if n := rcv.count(); n != 2 || rcv.got[1]["status"] != "resolved" {
		t.Fatalf("恢复通知不对：%v", rcv.got)
	}

	// 配置保存在本地，重启后还在。
	e2 := newEngine(t, nil)
	e2.dir = e.dir
	e2.loadConfigs()
	if len(e2.channels()) != 1 {
		t.Fatalf("重新读取配置后渠道数 %d", len(e2.channels()))
	}
}

func TestRelayAndQueue(t *testing.T) {
	rcv := &receiver{fail: true}
	srv := httptest.NewServer(rcv)
	defer srv.Close()

	var relayed []string
	relayOK := false
	e := newEngine(t, func(_ context.Context, panel string, a *nagiv1.Alert, ids []string) error {
		if !relayOK {
			return errors.New("面板不可达")
		}
		relayed = append(relayed, panel+"/"+ids[0]+"/"+a.GetStatus())
		return nil
	})
	_ = e.SetConfig("p1", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "a", Type: "webhook", Url: srv.URL}}})
	ctx := context.Background()

	e.Fire(ctx, "k", "t", "", "")
	if len(e.queue) != 1 {
		t.Fatalf("直接发送和代发都失败时应该进队列，队列长度 %d", len(e.queue))
	}
	// 面板恢复后，补发走代发。
	relayOK = true
	e.flushQueue(ctx)
	if len(e.queue) != 0 || len(relayed) != 1 || relayed[0] != "p1/a/firing" {
		t.Fatalf("补发不对：queue=%d relayed=%v", len(e.queue), relayed)
	}
	// 渠道恢复后直接发送。
	rcv.mu.Lock()
	rcv.fail = false
	rcv.mu.Unlock()
	e.Resolve(ctx, "k")
	if rcv.count() != 1 || len(relayed) != 1 {
		t.Fatalf("直接发送不对：direct=%d relayed=%v", rcv.count(), relayed)
	}
}

// 告警里的节点名称用面板下发的（节点可能在面板上改过名），没有时用主机名。
func TestNodeName(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	ch := []*nagiv1.AlertChannel{{Id: "a", Type: "webhook", Url: srv.URL}}
	ctx := context.Background()

	e := newEngine(t, nil)
	_ = e.SetConfig("p2", &nagiv1.AlertConfig{Channels: ch})
	e.Fire(ctx, "k1", "t", "", "")
	if got := rcv.got[0]["node"]; got != "node1" {
		t.Fatalf("面板没下发名称时应该用主机名，得到 %v", got)
	}

	_ = e.SetConfig("p2", &nagiv1.AlertConfig{Channels: ch, NodeName: "东京-1"})
	e.Resolve(ctx, "k1")
	e.Notify(ctx, "t", "", "")
	for _, m := range rcv.got[1:] {
		if m["node"] != "东京-1" {
			t.Fatalf("应该用面板下发的名称：%v", m)
		}
	}

	// 两个面板的名称不同时（移交过程中），按面板指纹取第一个；重启后从本地配置读回。
	_ = e.SetConfig("p1", &nagiv1.AlertConfig{NodeName: "大阪-1"})
	e2 := newEngine(t, nil)
	e2.dir = e.dir
	e2.loadConfigs()
	if got := e2.nodeName(); got != "大阪-1" {
		t.Fatalf("重新读取配置后名称是 %q", got)
	}
}

func TestStrictestRules(t *testing.T) {
	e := newEngine(t, nil)
	_ = e.SetConfig("p1", &nagiv1.AlertConfig{Rules: &nagiv1.AlertRules{DiskPercent: 80}})
	_ = e.SetConfig("p2", &nagiv1.AlertConfig{Rules: &nagiv1.AlertRules{DiskPercent: 95, CertDays: 14}})
	r := e.rules()
	if r.DiskPercent != 80 || r.CertDays != 14 || r.MemPercent != 90 {
		t.Fatalf("规则合并不对：%+v", r)
	}
}

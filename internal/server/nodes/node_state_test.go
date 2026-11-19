package nodes

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/store"
)

// 上报的额度和写库的去重按节点记：节点每次连上就发一批内容不同的上报、然后断开重连，
// 写库的次数不随重连的次数增加。
func TestReconnectKeepsReportLimits(t *testing.T) {
	var fdb *faultDB
	p := newSessionPanelDB(t, func(d store.DBTX) store.DBTX { fdb = &faultDB{DBTX: d}; return fdb }, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	const reconnects = 10
	var prev *nodeSession
	for i := range reconnects {
		s := p.connect(t)
		for j := range reportBurst + 1 {
			// 主面板写别的面板：面板不下发告警配置（测试里没有 agent）。
			req := &nagiv1.ReportRequest{Primary: "other", Node: &nagiv1.NodeInfo{Hostname: fmt.Sprintf("h-%d-%d", i, j)}}
			if _, err := s.panel.Report(ctx, req); err != nil {
				t.Fatalf("第 %d 条连接的第 %d 次上报：%v", i+1, j+1, err)
			}
		}
		if prev != nil {
			waitClosed(t, "旧连接", prev.closed)
		}
		prev = s
	}
	prev.sess.Close()
	waitClosed(t, "最后一条连接", prev.closed)
	// 攒着的 30 次，加上测试期间每秒补的一次、断开时可能写的一次。
	limit := reportBurst + int(time.Since(start)/reportRefill) + 2
	if w, _ := fdb.counts(); w > limit {
		t.Fatalf("重连 %d 次写了 %d 次库，最多应是 %d 次", reconnects, w, limit)
	}

	// 断开以后再连上、不上报就断开：不写库，库里的节点信息还在。
	before, _ := fdb.counts()
	s := p.connect(t)
	deadline := time.Now().Add(5 * time.Second)
	for !p.r.Online(p.nodeID) {
		if time.Now().After(deadline) {
			t.Fatal("节点没有连上")
		}
		time.Sleep(time.Millisecond)
	}
	s.sess.Close()
	waitClosed(t, "没有上报的连接", s.closed)
	if after, _ := fdb.counts(); after != before {
		t.Fatalf("没有上报的连接断开时不应写库：%d → %d", before, after)
	}
	n, err := p.q.GetNode(context.Background(), p.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Info) == 0 {
		t.Fatal("库里的节点信息被清掉了")
	}
}

// 断开时写库也去重：内容没变、刚写过就不写；过了间隔才再记一次最后在线时间。新连接沿用旧连接的去重。
func TestTouchOnClose(t *testing.T) {
	p, fdb := reportPanel(t)
	ctx := context.Background()
	report := &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{Hostname: "h"}}
	p.r.touch(ctx, p.conn, report, false)
	p.r.touch(ctx, p.conn, report, true)
	other := &Conn{NodeID: "n", name: "n"}
	p.r.touch(ctx, other, report, false)
	p.r.touch(ctx, other, report, true)
	if w, _ := fdb.counts(); w != 1 {
		t.Fatalf("内容没变、刚写过，应只写一次库：%d", w)
	}
	st := p.r.state("n")
	st.mu.Lock()
	st.seenAt = time.Now().Add(-closeTouchInterval)
	st.mu.Unlock()
	p.r.touch(ctx, other, report, false)
	if w, _ := fdb.counts(); w != 1 {
		t.Fatalf("没断开时一分钟内不重复写：%d", w)
	}
	p.r.touch(ctx, other, report, true)
	if w, _ := fdb.counts(); w != 2 {
		t.Fatalf("断开时过了间隔应记一次最后在线时间：%d", w)
	}
}

// 同一个节点同时只跑一个 OnReport，新旧连接合起来算：旧连接上的还没跑完时，新连接的上报排着，跑完再跑。
func TestOnReportOnePerNode(t *testing.T) {
	r := NewRegistry(nil, slog.Default(), nil, nil)
	gate := make(chan struct{})
	var running, maxRunning atomic.Int32
	var mu sync.Mutex
	var got []string
	r.OnReport = func(c *Conn, req *nagiv1.ReportRequest) {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		<-gate
		mu.Lock()
		got = append(got, c.name+":"+req.GetNode().GetHostname())
		mu.Unlock()
		running.Add(-1)
	}
	req := func(h string) *nagiv1.ReportRequest {
		return &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{Hostname: h}}
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("等不到%s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	add := func(name string) *Conn {
		c := &Conn{NodeID: "n", name: name}
		r.mu.Lock()
		r.addSession(c)
		r.mu.Unlock()
		return c
	}

	// 反复重连，每条连接上都来一次上报：第一条上的正在跑，其余的都排着，只留最新的一份。
	first := add("c0")
	r.runOnReport(first, req("a"))
	waitFor("第一次开始", func() bool { return running.Load() == 1 })
	for i := 1; i <= 5; i++ {
		r.runOnReport(add(fmt.Sprintf("c%d", i)), req("b"))
	}
	time.Sleep(20 * time.Millisecond)
	if n := running.Load(); n != 1 {
		t.Fatalf("重连以后同一个节点同时跑着 %d 个 OnReport", n)
	}
	gate <- struct{}{} // c0:a
	gate <- struct{}{} // c5:b
	waitFor("跑完", func() bool {
		st := r.state("n")
		st.mu.Lock()
		defer st.mu.Unlock()
		return !st.syncing
	})
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "c0:a,c5:b" || maxRunning.Load() != 1 {
		t.Fatalf("应依次跑 c0:a、c5:b，同时最多一个：%v max=%d", got, maxRunning.Load())
	}
}

// 来源地址只看连接：节点自称是面板自带的，显示的仍是它连来的地址；本面板自带的节点要真的从本机连来。
func TestSourceIgnoresReport(t *testing.T) {
	embedded := &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{Embedded: true}}
	remote := &Conn{NodeID: "n", remote: netip.MustParseAddr("203.0.113.9"), report: embedded}
	if ip, ok := remote.Source(); !ok || ip.String() != "203.0.113.9" {
		t.Fatalf("自称自带的节点应显示它连来的地址：%v %v", ip, ok)
	}
	if remote.OwnEmbedded() {
		t.Fatal("不是从本机连来的节点不算本面板自带的")
	}
	local := &Conn{NodeID: "n", remote: netip.MustParseAddr("127.0.0.1"), report: embedded}
	if _, ok := local.Source(); ok {
		t.Fatal("从本机连来的节点没有对外的来源地址")
	}
	if !local.OwnEmbedded() {
		t.Fatal("从本机连来的自带节点应算本面板自带的")
	}
	if _, ok := (&Conn{NodeID: "n"}).Source(); ok {
		t.Fatal("没有地址时不应返回来源")
	}
}

// 面板发现的越权写审计也合并：同一个节点的同一种越权 10 分钟内只马上记第一条，之后的条数在窗口结束时写进汇总。
func TestPanelViolationAuditCoalesced(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log, err := audit.Open(ctx, filepath.Join(dir, "audit.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	r := NewRegistry(store.New(d), slog.New(slog.DiscardHandler), nil, nil)
	r.Audit = log
	c := &Conn{NodeID: "x", name: "attacker"}
	count := func() (n int, last string) {
		rows, _ := log.List(ctx, audit.Filter{Limit: 5000})
		for _, e := range rows {
			if e.Category == audit.Security && e.Action == "violation" {
				if n == 0 || strings.Contains(e.Params, "suppressed") {
					last = e.Params
				}
				n++
			}
		}
		return n, last
	}
	for i := range 500 {
		r.PanelViolation(ctx, c, "report_foreign_container", fmt.Sprintf("p/c%d", i))
	}
	if n, _ := count(); n != 1 {
		t.Fatalf("同一种越权应只记一条，记了 %d 条", n)
	}
	// 别的种类、别的节点各记各的。
	r.PanelViolation(ctx, c, "relay_alert_limit", "")
	r.PanelViolation(ctx, &Conn{NodeID: "y", name: "other"}, "report_foreign_container", "p/c")
	if n, _ := count(); n != 3 {
		t.Fatalf("别的种类和别的节点应各记一条，共 %d 条", n)
	}
	// 窗口结束时补一条汇总，写明没记的条数；之后再越权，重新马上记。
	r.flushViolationAudit("x\x00report_foreign_container", "x", "report_foreign_container")
	n, last := count()
	if n != 4 || !strings.Contains(last, `"suppressed":"499"`) {
		t.Fatalf("窗口结束时应补一条汇总并写明没记的条数：%d 条，%s", n, last)
	}
	r.PanelViolation(ctx, c, "report_foreign_container", "p/again")
	if n, _ := count(); n != 5 {
		t.Fatalf("新的窗口里第一条应马上记：%d 条", n)
	}
}

// 窗口结束时补一条汇总：节点之后不再越权，没记的条数也进审计；窗口里只有一条的不补。窗口结束后表里的项删掉，
// 再越权时重新马上记。
func TestPanelViolationAuditSummary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log, err := audit.Open(ctx, filepath.Join(dir, "audit.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	r := NewRegistry(store.New(d), slog.New(slog.DiscardHandler), nil, nil)
	r.Audit = log
	const window = 500 * time.Millisecond
	r.iso.auditWindow = window
	list := func() []audit.Event {
		rows, _ := log.List(ctx, audit.Filter{Limit: 100})
		var out []audit.Event
		for _, e := range rows {
			if e.Category == audit.Security && e.Action == "violation" {
				out = append(out, e)
			}
		}
		return out
	}
	start := time.Now()
	c := &Conn{NodeID: "x", name: "attacker"}
	for i := range 20 {
		r.PanelViolation(ctx, c, "report_foreign_container", fmt.Sprintf("p/c%d", i))
	}
	r.PanelViolation(ctx, &Conn{NodeID: "y", name: "other"}, "report_foreign_container", "p/c")
	if time.Since(start) >= window {
		t.Skip("机器太慢，窗口在测试里提前结束了")
	}
	if n := len(list()); n != 2 {
		t.Fatalf("窗口里应只马上记 2 条，记了 %d 条", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(list()) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(window / 2) // y 的窗口也已结束：只有一条，不补汇总
	got := list()
	if len(got) != 3 {
		t.Fatalf("窗口结束时应补 1 条汇总，共 %d 条", len(got))
	}
	var summary *audit.Event
	for i := range got {
		if strings.Contains(got[i].Params, "suppressed") {
			summary = &got[i]
		}
	}
	if summary == nil || summary.ActorID != "x" || summary.ActorName != "attacker" ||
		!strings.Contains(summary.Params, `"suppressed":"19"`) || !strings.Contains(summary.Params, `"detail":"p/c19"`) {
		t.Fatalf("汇总应写明没记的条数和最后一条的详情：%+v", summary)
	}
	r.iso.mu.Lock()
	left := len(r.iso.audits)
	r.iso.mu.Unlock()
	if left != 0 {
		t.Fatalf("窗口结束后表里还有 %d 项", left)
	}
	r.PanelViolation(ctx, c, "report_foreign_container", "p/again")
	if n := len(list()); n != 4 {
		t.Fatalf("新的窗口里第一条应马上记：%d 条", n)
	}
}

// 节点被移除时（Disconnect 之后 Forget）删掉按节点记的状态，断开的连接不会再把它建回来。
func TestForgetDropsNodeState(t *testing.T) {
	p := newSessionPanel(t)
	s := p.connect(t)
	if err := s.report(t); err != nil {
		t.Fatal(err)
	}
	p.r.stateMu.Lock()
	_, ok := p.r.states[p.nodeID]
	p.r.stateMu.Unlock()
	if !ok {
		t.Fatal("上报以后应有节点的状态")
	}
	if err := p.q.RevokeNode(context.Background(), p.nodeID); err != nil {
		t.Fatal(err)
	}
	p.r.Disconnect(p.nodeID)
	p.r.Forget(p.nodeID)
	waitClosed(t, "连接的 Handle ", s.closed)
	p.r.stateMu.Lock()
	n := len(p.r.states)
	p.r.stateMu.Unlock()
	if n != 0 {
		t.Fatalf("移除以后还留着 %d 个节点的状态", n)
	}
}

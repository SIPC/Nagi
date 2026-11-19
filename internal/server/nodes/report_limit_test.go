package nodes

import (
	"context"
	"database/sql"
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
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// faultDB 包一层数据库：数写了几次节点状态，布防以后让指定的第几次 GetNode 出错。
type faultDB struct {
	store.DBTX
	mu      sync.Mutex
	writes  int
	armed   bool
	getNode int
	fail    map[int]bool
	failed  int
}

func (d *faultDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if strings.Contains(q, "-- name: UpdateNodeSeen ") {
		d.mu.Lock()
		d.writes++
		d.mu.Unlock()
	}
	return d.DBTX.ExecContext(ctx, q, args...)
}

func (d *faultDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	if strings.Contains(q, "-- name: GetNode :one") {
		d.mu.Lock()
		fail := false
		if d.armed {
			d.getNode++
			fail = d.fail[d.getNode]
			if fail {
				d.failed++
			}
		}
		d.mu.Unlock()
		if fail {
			cctx, cancel := context.WithCancel(ctx)
			cancel()
			return d.DBTX.QueryRowContext(cctx, q, args...)
		}
	}
	return d.DBTX.QueryRowContext(ctx, q, args...)
}

func (d *faultDB) arm(fail ...int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed, d.getNode, d.failed, d.fail = true, 0, 0, map[int]bool{}
	for _, n := range fail {
		d.fail[n] = true
	}
}

func (d *faultDB) counts() (writes, failed int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writes, d.failed
}

// refill 把连接的上报额度补满（测试里代替等待）。
func (c *Conn) refill() {
	c.mu.Lock()
	c.reportAt = time.Time{}
	c.mu.Unlock()
}

// v0.35.0#13：令牌桶攒 30 次、每秒补一次；正常节点的上报节奏用不完，超出的丢弃。
func TestAllowReport(t *testing.T) {
	c := &Conn{}
	now := time.Unix(1000, 0)
	for i := range reportBurst {
		if ok, _ := c.allowReport(now); !ok {
			t.Fatalf("第 %d 次就被丢弃了", i+1)
		}
	}
	if ok, n := c.allowReport(now); ok || n != 1 {
		t.Fatalf("额度用完后应丢弃并记日志：ok=%v n=%d", ok, n)
	}
	if ok, n := c.allowReport(now); ok || n != 0 {
		t.Fatalf("一分钟内只记一次日志：ok=%v n=%d", ok, n)
	}
	now = now.Add(reportRefill)
	if ok, _ := c.allowReport(now); !ok {
		t.Fatal("过一秒应补一次")
	}
	if ok, _ := c.allowReport(now); ok {
		t.Fatal("只补了一次")
	}

	// 正常节点：每 15 秒定时上报，备份同步时每 5 秒一次，每分钟一次部署连着报 10 次（间隔 300 毫秒），一小时不丢。
	c = &Conn{}
	start := time.Unix(2000, 0)
	var times []time.Duration
	for s := time.Duration(0); s < time.Hour; s += 5 * time.Second {
		times = append(times, s)
		if s%time.Minute == 0 {
			for i := 1; i <= 10; i++ {
				times = append(times, s+time.Duration(i)*300*time.Millisecond)
			}
		}
	}
	for _, d := range times {
		if ok, _ := c.allowReport(start.Add(d)); !ok {
			t.Fatalf("正常节奏的上报在 %v 被丢弃", d)
		}
	}
}

// reportPanel 是只有注册表和数据库的面板，节点 n 已登记为当前连接。
func reportPanel(t *testing.T) (*panelService, *faultDB) {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	fdb := &faultDB{DBTX: d}
	q := store.New(fdb)
	if err := q.CreateNode(ctx, store.CreateNodeParams{ID: "n", Name: "n", MeshKey: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(q, slog.Default(), nil, nil)
	c := &Conn{NodeID: "n", name: "n", accepted: true} // accepted：不下发告警配置（测试里没有 agent）
	r.mu.Lock()
	r.addSession(c)
	r.mu.Unlock()
	return &panelService{r: r, conn: c}, fdb
}

func storedHostname(t *testing.T, p *panelService) string {
	t.Helper()
	n, err := p.r.q.GetNode(context.Background(), "n")
	if err != nil {
		t.Fatal(err)
	}
	var info nagiv1.NodeInfo
	if err := proto.Unmarshal(n.Info, &info); err != nil {
		t.Fatal(err)
	}
	return info.GetHostname()
}

// v0.35.0#13：不停上报的节点被限频；内容没变不写库；节点信息过大不采用；同一条连接不能同时上报。
func TestReportLimits(t *testing.T) {
	ctx := context.Background()
	p, fdb := reportPanel(t)
	report := func(host string) (*nagiv1.ReportResponse, error) {
		return p.Report(ctx, &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{Hostname: host}})
	}

	// 内容不变：只写一次库。超出额度的上报不报错（不断开节点），也不采用。
	for i := range 100 {
		if _, err := report("h"); err != nil {
			t.Fatalf("第 %d 次上报：%v", i+1, err)
		}
	}
	if w, _ := fdb.counts(); w != 1 {
		t.Fatalf("内容没变应只写一次库，写了 %d 次", w)
	}
	if _, err := report("dropped"); err != nil {
		t.Fatal(err)
	}
	if h := p.conn.LastReport().GetNode().GetHostname(); h != "h" {
		t.Fatalf("超出额度的上报不应采用：%q", h)
	}

	// 内容变了：写库。
	p.conn.refill()
	if _, err := report("h2"); err != nil {
		t.Fatal(err)
	}
	if w, _ := fdb.counts(); w != 2 {
		t.Fatalf("内容变了应写库：%d", w)
	}

	// 节点信息过大、主机名过长：不采用，沿用上一次的，不写库。
	for _, n := range []*nagiv1.NodeInfo{
		{Hostname: strings.Repeat("a", 4<<20)},
		{Hostname: strings.Repeat("a", maxHostnameLen+1)},
		{Hostname: "h3", DockerVersion: strings.Repeat("v", maxNodeInfoSize)},
	} {
		if _, err := p.Report(ctx, &nagiv1.ReportRequest{Node: n}); err != nil {
			t.Fatal(err)
		}
		if h := p.conn.LastReport().GetNode().GetHostname(); h != "h2" {
			t.Fatalf("过大的节点信息不应采用：%d 字节", len(h))
		}
	}
	if w, _ := fdb.counts(); w != 2 {
		t.Fatalf("不采用的节点信息不应写库：%d", w)
	}
	if h := storedHostname(t, p); h != "h2" {
		t.Fatalf("库里的节点信息应是 h2：%d 字节", len(h))
	}
	// 正常长度的主机名照常采用。
	if _, err := report(strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if h := storedHostname(t, p); h != strings.Repeat("b", 64) {
		t.Fatalf("正常的主机名应采用：%q", h)
	}

	// 同一条连接上同时来第二次上报：拒绝。
	p.conn.reporting.Store(true)
	if _, err := report("h"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("同时上报应拒绝：%v", err)
	}
	p.conn.reporting.Store(false)
	if _, err := report("h"); err != nil {
		t.Fatal(err)
	}
}

// v0.35.0#13：同一个节点同时只跑一个 OnReport；跑的时候来的上报只留最新的一份，跑完接着跑；
// 连接被替换或断开以后，留下的那份不再跑。
func TestOnReportOneAtATime(t *testing.T) {
	r := NewRegistry(nil, slog.Default(), nil, nil)
	c := &Conn{NodeID: "n"}
	r.mu.Lock()
	r.addSession(c)
	r.mu.Unlock()
	gate := make(chan struct{})
	var running, maxRunning atomic.Int32
	var mu sync.Mutex
	var got []string
	r.OnReport = func(_ *Conn, req *nagiv1.ReportRequest) {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		<-gate
		mu.Lock()
		got = append(got, req.GetNode().GetHostname())
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
	idle := func() bool {
		c.syncMu.Lock()
		defer c.syncMu.Unlock()
		return !c.syncing
	}

	r.runOnReport(c, req("a"))
	waitFor("第一次开始", func() bool { return running.Load() == 1 })
	for _, h := range []string{"b", "c", "d"} {
		r.runOnReport(c, req(h))
	}
	gate <- struct{}{} // a
	gate <- struct{}{} // d
	waitFor("跑完", idle)
	mu.Lock()
	if strings.Join(got, ",") != "a,d" || maxRunning.Load() != 1 {
		t.Fatalf("应依次跑 a、d，同时最多一个：%v max=%d", got, maxRunning.Load())
	}
	mu.Unlock()

	// 断开以后，排着的那份不再跑（跑了会卡在 gate 上，等不到空闲）。
	r.runOnReport(c, req("e"))
	waitFor("e 开始", func() bool { return running.Load() == 1 })
	r.runOnReport(c, req("f"))
	r.Disconnect("n")
	gate <- struct{}{} // e
	waitFor("断开以后停下", idle)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "a,d,e" {
		t.Fatalf("断开以后不应再跑排着的上报：%v", got)
	}
}

// v0.35.0-fix#3：待批准节点同时在线的连接全局最多 16 条、同一来源最多 4 条；已批准的节点、同一节点重连不受影响。
func TestPendingSessionLimit(t *testing.T) {
	r := NewRegistry(nil, slog.Default(), nil, nil)
	add := func(id, ip string, pending bool) bool {
		c := &Conn{NodeID: id, pending: pending, remote: netip.MustParseAddr(ip)}
		r.mu.Lock()
		defer r.mu.Unlock()
		_, ok := r.addSession(c)
		return ok
	}
	for i := range maxPendingPerSource {
		if !add(fmt.Sprintf("p%d", i), "203.0.113.1", true) {
			t.Fatalf("同一来源第 %d 个待批准节点被拒绝", i+1)
		}
	}
	if add("p9", "203.0.113.1", true) {
		t.Fatal("同一来源超过上限应拒绝")
	}
	if add("p9", "::ffff:203.0.113.1", true) {
		t.Fatal("IPv4 映射地址应和 IPv4 地址算同一来源")
	}
	if !add("p0", "203.0.113.1", true) {
		t.Fatal("同一节点重连不应被拒绝")
	}
	if !add("approved", "203.0.113.1", false) {
		t.Fatal("已批准的节点不应受限制")
	}
	// IPv6 按 /64 算同一来源。
	for i := range maxPendingPerSource {
		if !add(fmt.Sprintf("v%d", i), fmt.Sprintf("2001:db8::%x", i+1), true) {
			t.Fatalf("IPv6 第 %d 个被拒绝", i+1)
		}
	}
	if add("v9", "2001:db8::ffff", true) {
		t.Fatal("同一个 /64 超过上限应拒绝")
	}

	// 全局上限：不同来源合计最多 16 条。
	r = NewRegistry(nil, slog.Default(), nil, nil)
	for i := range maxPendingSessions {
		if !add(fmt.Sprintf("g%d", i), fmt.Sprintf("198.51.100.%d", i+1), true) {
			t.Fatalf("第 %d 个待批准节点被拒绝", i+1)
		}
	}
	if add("g99", "192.0.2.1", true) {
		t.Fatal("超过全局上限应拒绝")
	}
	if !add("approved", "192.0.2.1", false) {
		t.Fatal("已批准的节点不应受全局上限影响")
	}
}

// v0.35.0-fix#3：待批准节点的连接到了上限时，Handle 拒绝新连接；有空位以后照常连上。
func TestPendingSessionLimitHandle(t *testing.T) {
	p := newSessionPanelDB(t, nil, true)
	// 别的待批准节点占满同一来源的名额（测试连接没有来源地址，都算同一来源）。
	var others []*Conn
	p.r.mu.Lock()
	for i := range maxPendingPerSource {
		c := &Conn{NodeID: fmt.Sprintf("other%d", i), pending: true}
		others = append(others, c)
		p.r.addSession(c)
	}
	p.r.mu.Unlock()
	rejected := p.connect(t)
	waitClosed(t, "超过上限的新连接", rejected.closed)
	if p.r.Online(p.nodeID) {
		t.Fatal("超过上限的待批准节点不应上线")
	}
	p.r.removeSession(others[0])
	ok := p.connect(t)
	if err := ok.report(t); err != nil {
		t.Fatalf("有空位以后应能连上：%v", err)
	}
	if !p.r.Online(p.nodeID) {
		t.Fatal("节点应在线")
	}
}

// v0.35.0-fix#5：新连接登记以后复查节点时数据库临时出错，重试一次，新连接照常；连着两次出错才拒绝。
func TestReconnectSurvivesTransientDBError(t *testing.T) {
	old := enrollRetry
	enrollRetry = time.Millisecond
	t.Cleanup(func() { enrollRetry = old })
	var fdb *faultDB
	p := newSessionPanelDB(t, func(d store.DBTX) store.DBTX { fdb = &faultDB{DBTX: d}; return fdb }, false)
	waitOnline := func(c *Conn) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			cur, err := p.r.Peek(p.nodeID)
			if err == nil && cur != c {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("节点没有登记")
			}
			time.Sleep(time.Millisecond)
		}
	}
	first := p.connect(t)
	waitOnline(nil)
	first0, _ := p.r.Peek(p.nodeID)

	// 第二条连接：登记前的 GetNode 正常，登记后的第一次出错、重试成功。
	// 上报里的主面板写别的面板：面板不下发告警配置，后台不会另外读库，GetNode 的次数只有 Handle 的。
	fdb.arm(2)
	second := p.connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := second.panel.Report(ctx, &nagiv1.ReportRequest{Primary: "other"}); err != nil {
		t.Fatalf("数据库临时出错时新连接应照常：%v", err)
	}
	waitClosed(t, "旧连接", first.sess.CloseChan())
	if _, failed := fdb.counts(); failed != 1 {
		t.Fatalf("应出错一次后重试：%d", failed)
	}
	if !p.r.Online(p.nodeID) {
		t.Fatal("节点应在线")
	}
	second0, _ := p.r.Peek(p.nodeID)
	if second0 == first0 {
		t.Fatal("当前连接应是第二条")
	}

	// 连着两次出错：拒绝。
	fdb.arm(2, 3)
	third := p.connect(t)
	waitClosed(t, "两次读库失败的新连接", third.closed)
	if _, failed := fdb.counts(); failed != 2 {
		t.Fatalf("应出错两次：%d", failed)
	}
}

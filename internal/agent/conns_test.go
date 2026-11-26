package agent

import (
	"context"
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// diagMsg 按 linux/inet_diag.h 拼一条 SOCK_DIAG_BY_FAMILY 回复：inet_diag_msg，TCP 时再带一个 INET_DIAG_INFO 属性。
// info 是 tcp_info 的长度（0 表示不带），sent、received、rtt 写在内核的偏移上。
func diagMsg(family, state uint8, local, remote netip.AddrPort, inode uint32, info int, sent, received uint64, rtt uint32) []byte {
	ne := binary.NativeEndian
	body := make([]byte, inetDiagMsgLen)
	body[0], body[1] = family, state
	binary.BigEndian.PutUint16(body[4:], local.Port())
	binary.BigEndian.PutUint16(body[6:], remote.Port())
	if family == afInet {
		l, r := local.Addr().As4(), remote.Addr().As4()
		copy(body[8:], l[:])
		copy(body[24:], r[:])
	} else {
		l, r := local.Addr().As16(), remote.Addr().As16()
		copy(body[8:], l[:])
		copy(body[24:], r[:])
	}
	ne.PutUint32(body[68:], inode)
	if info > 0 {
		attr := make([]byte, 4+info)
		ne.PutUint16(attr[0:], uint16(len(attr)))
		ne.PutUint16(attr[2:], inetDiagInfo)
		t := attr[4:]
		ne.PutUint32(t[tcpInfoRTT:], rtt)
		if info >= tcpInfoReceived+8 {
			ne.PutUint64(t[tcpInfoAcked:], sent/2) // 已确认的比发出的少：区分开，看读的是哪一项
			ne.PutUint64(t[tcpInfoReceived:], received)
		}
		if info >= tcpInfoSent+8 {
			ne.PutUint64(t[tcpInfoSent:], sent)
		}
		for len(attr)%4 != 0 {
			attr = append(attr, 0)
		}
		body = append(body, attr...)
	}
	return nlmsg(sockDiagByFam, body)
}

func nlmsg(typ uint16, body []byte) []byte {
	b := make([]byte, nlmsgHdrLen, nlmsgHdrLen+len(body)+3)
	binary.NativeEndian.PutUint32(b[0:], uint32(nlmsgHdrLen+len(body)))
	binary.NativeEndian.PutUint16(b[4:], typ)
	b = append(b, body...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

var ap = netip.MustParseAddrPort

// 解析回复：IPv4 和 IPv6 的地址、端口（网络序）、inode，IPv4 映射的 IPv6 地址按 IPv4 显示；
// tcp_info 新内核读 bytes_sent，4.1～4.18 只有 bytes_acked 时用它，更老的没有字节数。
func TestParseDiag(t *testing.T) {
	var b []byte
	b = append(b, diagMsg(afInet, 1, ap("10.0.0.2:41234"), ap("192.168.0.13:3260"), 77, 232, 9000, 5000, 1180)...)
	b = append(b, diagMsg(afInet6, 2, ap("[2001:db8::1]:50000"), ap("[2001:db8::99]:443"), 78, 0, 0, 0, 0)...)
	b = append(b, diagMsg(afInet6, 1, ap("[::ffff:172.17.0.2]:40000"), ap("[::ffff:1.2.3.4]:80"), 79, 136, 8000, 300, 50)...)
	b = append(b, diagMsg(afInet, 1, ap("10.0.0.2:1"), ap("10.0.0.3:2"), 80, 104, 0, 0, 7)...)
	b = append(b, nlmsg(nlmsgDone, []byte{0, 0, 0, 0})...)
	got, done, err := parseDiag(b, ipprotoTCP)
	if err != nil || !done || len(got) != 4 {
		t.Fatalf("got %d sockets, done=%v err=%v", len(got), done, err)
	}
	want := []struct {
		local, remote  string
		inode          uint32
		state          uint8
		sent, received uint64
		rtt            uint32
	}{
		{"10.0.0.2:41234", "192.168.0.13:3260", 77, 1, 9000, 5000, 1180},
		{"[2001:db8::1]:50000", "[2001:db8::99]:443", 78, 2, 0, 0, 0},
		{"172.17.0.2:40000", "1.2.3.4:80", 79, 1, 4000, 300, 50},
		{"10.0.0.2:1", "10.0.0.3:2", 80, 1, 0, 0, 7},
	}
	for i, w := range want {
		g := got[i]
		if g.local.String() != w.local || g.remote.String() != w.remote || g.inode != w.inode || g.state != w.state || g.sent != w.sent || g.received != w.received || g.rtt != w.rtt {
			t.Errorf("#%d = %+v, want %+v", i, g, w)
		}
	}
}

// 回复分成几次收到：没有结束标记时 done 为 false；长度不对时报错，不越界。内核报错时返回错误。
func TestParseDiagPartsAndErrors(t *testing.T) {
	one := diagMsg(afInet, 1, ap("10.0.0.2:1"), ap("10.0.0.3:2"), 1, 0, 0, 0, 0)
	if got, done, err := parseDiag(one, ipprotoTCP); err != nil || done || len(got) != 1 {
		t.Fatalf("部分回复：%d %v %v", len(got), done, err)
	}
	if _, _, err := parseDiag(one[:len(one)-8], ipprotoTCP); err == nil {
		t.Fatal("截断的回复应报错")
	}
	errMsg := make([]byte, 4)
	binary.NativeEndian.PutUint32(errMsg, uint32(0xffffffff-1)) // -2
	if _, done, err := parseDiag(nlmsg(nlmsgError, errMsg), ipprotoTCP); err == nil || !done {
		t.Fatalf("内核报错：done=%v err=%v", done, err)
	}
}

// 监听的 TCP 和没连对端的 UDP 不列出，但它们的端口决定入站：连到本机 80 的是入站，本机发起的是出站。
// 同一个本地端口，TCP 在监听不代表 UDP 也是入站。
func TestConnsOfDirection(t *testing.T) {
	sockets := []diagSocket{
		{protocol: ipprotoTCP, state: tcpListen, local: ap("0.0.0.0:80")},
		{protocol: ipprotoTCP, state: 1, local: ap("10.0.0.2:80"), remote: ap("203.0.113.9:51000"), inode: 1},
		{protocol: ipprotoTCP, state: 1, local: ap("10.0.0.2:41234"), remote: ap("192.168.0.13:3260"), inode: 2},
		{protocol: ipprotoUDP, state: 7, local: ap("0.0.0.0:53")},
		{protocol: ipprotoUDP, state: 1, local: ap("10.0.0.2:53"), remote: ap("10.0.0.9:40000"), inode: 3},
		{protocol: ipprotoUDP, state: 1, local: ap("10.0.0.2:80"), remote: ap("8.8.8.8:53"), inode: 4},
		{protocol: ipprotoTCP, state: 6, local: ap("10.0.0.2:41000"), remote: ap("1.1.1.1:443"), inode: 0},
	}
	got := connsOf("ns1", sockets)
	type row struct {
		proto, state, remote string
		inbound              bool
	}
	want := []row{
		{"tcp", "ESTABLISHED", "203.0.113.9", true},
		{"tcp", "ESTABLISHED", "192.168.0.13", false},
		{"udp", "", "10.0.0.9", true},
		{"udp", "", "8.8.8.8", false},
		{"tcp", "TIME_WAIT", "1.1.1.1", false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d conns, want %d", len(got), len(want))
	}
	for i, w := range want {
		c := got[i].c
		if (row{c.GetProtocol(), c.GetState(), c.GetRemoteIp(), c.GetInbound()}) != w {
			t.Errorf("#%d = %v, want %v", i, row{c.GetProtocol(), c.GetState(), c.GetRemoteIp(), c.GetInbound()}, w)
		}
	}
	// 不同网络命名空间里一样的地址是不同的连接（容器各有 172.17.0.2）。
	if other := connsOf("ns2", sockets); other[1].key == got[1].key {
		t.Error("不同命名空间的连接 key 应不同")
	}
}

// 速率是两轮之间的字节差除以间隔；第一次看到、计数变小（换了一条连接）时没有速率。
func TestApplyRates(t *testing.T) {
	mk := func(key string, sent, recv uint64) sampledConn {
		return sampledConn{key: key, c: &nagiv1.NodeConnection{BytesSent: sent, BytesReceived: recv}}
	}
	first := []sampledConn{mk("a", 1000, 100), mk("b", 5000, 0)}
	prev := applyRates(first, nil, 0)
	if first[0].c.GetSendRate() != 0 {
		t.Fatal("第一轮不应有速率")
	}
	second := []sampledConn{mk("a", 5000, 600), mk("b", 10, 0), mk("c", 9, 9)}
	applyRates(second, prev, 2*time.Second)
	if a := second[0].c; a.GetSendRate() != 2000 || a.GetReceiveRate() != 250 {
		t.Errorf("a = %v/%v, want 2000/250", a.GetSendRate(), a.GetReceiveRate())
	}
	if b := second[1].c; b.GetSendRate() != 0 {
		t.Errorf("计数变小不应算速率：%v", b.GetSendRate())
	}
	if c := second[2].c; c.GetSendRate() != 0 {
		t.Errorf("新连接不应有速率：%v", c.GetSendRate())
	}
}

// 按速率排序（速率一样按累计字节），超过上限的只计数。
func TestBuildSnapshotOrderAndLimit(t *testing.T) {
	var conns []sampledConn
	for i := range connsMax + 5 {
		conns = append(conns, sampledConn{key: string(rune(i)), c: &nagiv1.NodeConnection{BytesSent: uint64(i)}})
	}
	conns[3].c.SendRate = 10
	conns[7].c.ReceiveRate = 20
	s := buildSnapshot(connSample{conns: conns}, connsInterval)
	if len(s.GetConnections()) != connsMax || s.GetOmitted() != 5 || s.GetIntervalMs() != 2000 {
		t.Fatalf("len=%d omitted=%d interval=%d", len(s.GetConnections()), s.GetOmitted(), s.GetIntervalMs())
	}
	if s.Connections[0].GetReceiveRate() != 20 || s.Connections[1].GetSendRate() != 10 || s.Connections[2].GetBytesSent() != uint64(connsMax+4) {
		t.Errorf("顺序不对：%v %v %v", s.Connections[0], s.Connections[1], s.Connections[2])
	}
}

// 一轮太慢时放慢到 5 秒，快下来以后恢复；介于两者之间保持不变。
func TestNextConnsInterval(t *testing.T) {
	if got := nextConnsInterval(connsInterval, 400*time.Millisecond); got != connsSlowInterval {
		t.Errorf("慢：%v", got)
	}
	if got := nextConnsInterval(connsSlowInterval, 200*time.Millisecond); got != connsSlowInterval {
		t.Errorf("中间：%v", got)
	}
	if got := nextConnsInterval(connsSlowInterval, 50*time.Millisecond); got != connsInterval {
		t.Errorf("快：%v", got)
	}
}

// cgroup v2、v1、systemd 单元和不在容器里的进程。
func TestCgroupOwner(t *testing.T) {
	id := strings.Repeat("ab", 32)
	cases := []struct{ content, id, unit string }{
		{"0::/system.slice/docker-" + id + ".scope\n", id, ""},
		{"12:pids:/docker/" + id + "\n11:cpu,cpuacct:/docker/" + id + "\n", id, ""},
		{"0::/system.slice/nagi-agent.service\n", "", "nagi-agent.service"},
		{"0::/user.slice/user-0.slice/session-3.scope\n", "", ""},
		// 容器里又建了子 cgroup（docker-in-docker）：取最里面的容器 ID。
		{"0::/system.slice/docker-" + id + ".scope/docker/" + strings.Repeat("cd", 32) + "\n", strings.Repeat("cd", 32), ""},
		{"", "", ""},
	}
	for _, c := range cases {
		gotID, gotUnit := cgroupOwner(c.content)
		if gotID != c.id || gotUnit != c.unit {
			t.Errorf("%q = %q %q, want %q %q", c.content, gotID, gotUnit, c.id, c.unit)
		}
	}
}

// 命令行的 NUL 换成空格，截到 512 字节时不切开多字节字符。
func TestCleanCmdline(t *testing.T) {
	if got := cleanCmdline([]byte("redis-server\x00*:6379\x00")); got != "redis-server *:6379" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("a", cmdlineMax-1) + "中文"
	got := cleanCmdline([]byte(long))
	if len(got) > cmdlineMax || !strings.HasSuffix(got, "a") || !strings.HasPrefix(long, got) {
		t.Errorf("截断：len=%d suffix=%q", len(got), got[len(got)-3:])
	}
}

// 有人看才采样，几个人共用一份；没要命令行的人拿到的没有命令行；最后一个人走了停止采样；超过人数上限拒绝。
func TestConnWatchSharing(t *testing.T) {
	samples := make(chan bool, 1)
	w := newConnWatch(func(cmdline bool) (connSample, error) {
		select {
		case samples <- cmdline:
		default:
		}
		return connSample{conns: []sampledConn{{key: "a", c: &nagiv1.NodeConnection{Cmdline: map[bool]string{true: "secret --token=x"}[cmdline]}}}}, nil
	})
	plain, donePlain, err := w.subscribe(false)
	if err != nil {
		t.Fatal(err)
	}
	if <-samples {
		t.Fatal("没人要命令行时不应读命令行")
	}
	<-plain.ch
	withCmd, doneCmd, _ := w.subscribe(true)
	// 下一轮起要读命令行，同一轮里两个人都拿到结果（ch 里总是最新的一份）。
	deadline := time.After(10 * time.Second)
	for got := false; !got; {
		select {
		case s := <-withCmd.ch:
			got = s.GetConnections()[0].GetCmdline() == "secret --token=x"
		case <-deadline:
			t.Fatal("等不到带命令行的采样")
		}
	}
	if c := (<-plain.ch).GetConnections()[0].GetCmdline(); c != "" {
		t.Errorf("没要命令行的人拿到了命令行：%q", c)
	}
	var subs []func()
	for range connsMaxWatchers - 2 {
		_, done, err := w.subscribe(false)
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, done)
	}
	if _, _, err := w.subscribe(false); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("超过上限应拒绝：%v", err)
	}
	donePlain()
	doneCmd()
	for _, d := range subs {
		d()
	}
	w.mu.Lock()
	stopped := w.stop == nil
	w.mu.Unlock()
	if !stopped {
		t.Fatal("没人看了应停止采样")
	}
}

// 不支持的系统（不是 Linux）报 Unimplemented，面板提示升级或不支持；面板断开时结束。
func TestStreamConnectionsEnds(t *testing.T) {
	if err := streamConnections(context.Background(), nil, false, nil); status.Code(err) != codes.Unimplemented {
		t.Fatalf("没有采样器：%v", err)
	}
	w := newConnWatch(func(bool) (connSample, error) { return connSample{}, nil })
	ctx, cancel := context.WithCancel(context.Background())
	sent := 0
	err := streamConnections(ctx, w, false, func(*nagiv1.ConnectionSnapshot) error { sent++; cancel(); return nil })
	if sent != 1 || err == nil {
		t.Fatalf("sent=%d err=%v", sent, err)
	}
	if w.stop != nil {
		t.Fatal("面板断开后应停止采样")
	}
}

// 网络连接是查询：备用面板也能看；面板断开时节点停止采样，不像部署那样做完。
func TestStreamConnectionsReadOnlyFollow(t *testing.T) {
	m := nagiv1.AgentService_StreamConnections_FullMethodName
	if !readOnlyRPCs[m] || !isFollowStream(m) {
		t.Fatalf("readOnly=%v follow=%v", readOnlyRPCs[m], isFollowStream(m))
	}
}

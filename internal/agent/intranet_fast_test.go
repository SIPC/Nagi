package agent

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/SIPC/nagi/internal/agent/project"
)

// 容器地址的缓存：找到的才缓存；清空以后重新查；清空之前开始的查询，结果不记进缓存；
// 缓存的地址连不上时重新查一次，换了地址就连新的。
func TestEndpointCache(t *testing.T) {
	ctx := context.Background()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)

	calls := 0
	ip := netip.MustParseAddr("127.0.0.1")
	found := true
	var during func()
	n := &intranet{}
	n.endpoint = func(context.Context, string, string) (project.Endpoint, bool) {
		calls++
		if during != nil {
			during()
		}
		return project.Endpoint{IP: ip, Ports: []uint32{uint32(port)}}, found
	}

	// 找不到的不缓存。
	found = false
	if _, ok, _ := n.lookupEndpoint(ctx, "p", "web"); ok {
		t.Fatal("应该找不到")
	}
	if _, ok, _ := n.lookupEndpoint(ctx, "p", "web"); ok || calls != 2 {
		t.Fatalf("找不到的不该缓存：查了 %d 次", calls)
	}
	// 找到的缓存。
	found = true
	if _, ok, cached := n.lookupEndpoint(ctx, "p", "web"); !ok || cached {
		t.Fatal("第一次应该是现查的")
	}
	if e, ok, cached := n.lookupEndpoint(ctx, "p", "web"); !ok || !cached || e.IP != ip || calls != 3 {
		t.Fatalf("第二次应该用缓存：查了 %d 次", calls)
	}
	// 清空以后重新查。
	n.forgetEndpoints()
	if _, _, cached := n.lookupEndpoint(ctx, "p", "web"); cached || calls != 4 {
		t.Fatalf("清空以后应该重新查：查了 %d 次", calls)
	}
	// 查询途中清空了缓存：这次的结果不记进去。
	n.forgetEndpoints()
	during = n.forgetEndpoints
	n.lookupEndpoint(ctx, "p", "web")
	during = nil
	if _, _, cached := n.lookupEndpoint(ctx, "p", "web"); cached {
		t.Fatal("清空之前开始的查询，结果不该记进缓存")
	}

	// 缓存的地址连不上（容器换了地址，事件还没到）：重新查到新地址再连。
	// 容器"现在"在 ::1 上听着；缓存里还是旧地址 127.0.0.1（同一个端口没人听）。
	ln6, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("没有 IPv6 回环：", err)
	}
	defer ln6.Close()
	go func() {
		for {
			c, err := ln6.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port6 := uint16(ln6.Addr().(*net.TCPAddr).Port)
	n.forgetEndpoints()
	ip = netip.MustParseAddr("127.0.0.1")
	n.lookupEndpoint(ctx, "p", "web") // 缓存里记下旧地址
	ip = netip.MustParseAddr("::1")
	before := calls
	c, e, ok, err := n.dialEndpoint(ctx, "p", "web", port6)
	if !ok || err != nil {
		t.Fatalf("应该重新查到新地址并连上：%v %v", ok, err)
	}
	c.Close()
	if e.IP != ip || calls != before+1 {
		t.Fatalf("应该重新查一次、用新地址：%v，查了 %d 次", e.IP, calls-before)
	}
	// 之后直接用缓存里的新地址。
	if _, _, cached := n.lookupEndpoint(ctx, "p", "web"); !cached {
		t.Fatal("重新查到的地址应该记进缓存")
	}
	// 容器已经不在了：缓存的地址连不上，重新查找不到。
	ln6.Close()
	found = false
	if _, _, ok, _ := n.dialEndpoint(ctx, "p", "web", port6); ok {
		t.Fatal("容器不在了应该返回找不到")
	}
}

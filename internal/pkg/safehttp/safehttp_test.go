package safehttp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	cases := []struct {
		ip string
		// public：默认就能连；private：允许内网地址时才能连；其余任何时候都不能连。
		want string
	}{
		{"1.1.1.1", "public"},
		{"8.8.8.8", "public"},
		{"2606:4700:4700::1111", "public"},
		{"198.18.0.1", "private"}, // 基准测试网段，代理的 fake-ip 常用：勾了允许内网才能用
		{"10.0.0.1", "private"},
		{"172.16.5.4", "private"},
		{"172.31.255.255", "private"},
		{"192.168.1.10", "private"},
		{"100.64.0.1", "private"},
		{"100.127.255.254", "private"},
		{"fc00::1", "private"},
		{"fd12:3456::1", "private"},
		{"::ffff:192.168.1.1", "private"},
		{"64:ff9b::a00:1", "private"}, // NAT64 里的 10.0.0.1
		{"127.0.0.1", "never"},
		{"127.1.2.3", "never"},
		{"::1", "never"},
		{"::ffff:127.0.0.1", "never"},
		{"0.0.0.0", "never"},
		{"0.1.2.3", "never"},
		{"::", "never"},
		{"169.254.169.254", "never"},
		{"169.254.0.23", "never"},
		{"fe80::1", "never"},
		{"fe80::1%eth0", "never"},
		{"224.0.0.1", "never"},
		{"ff02::1", "never"},
		{"255.255.255.255", "never"},
		{"100.100.100.200", "never"},
		{"fd00:ec2::254", "never"},
		{"168.63.129.16", "never"},
		{"64:ff9b::7f00:1", "never"}, // NAT64 里的 127.0.0.1
	}
	for _, c := range cases {
		ip := netip.MustParseAddr(c.ip)
		pub, priv := Check(ip, false), Check(ip, true)
		switch c.want {
		case "public":
			if pub != nil || priv != nil {
				t.Errorf("%s 应该允许：%v / %v", c.ip, pub, priv)
			}
		case "private":
			if b, ok := Blocked(pub); !ok || !b.Private {
				t.Errorf("%s 默认应该作为内网地址拦下：%v", c.ip, pub)
			}
			if priv != nil {
				t.Errorf("%s 允许内网地址时应该允许：%v", c.ip, priv)
			}
		default:
			for _, err := range []error{pub, priv} {
				if b, ok := Blocked(err); !ok || b.Private {
					t.Errorf("%s 应该一直拦下（不是内网地址的那种）：%v", c.ip, err)
				}
			}
		}
	}
}

// 域名在连接时才检查：localhost 解析到 127.0.0.1 / ::1，允许内网地址也连不上。
func TestClientBlocksLoopbackByName(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	for _, target := range []string{"http://localhost:" + port + "/", srv.URL + "/"} {
		for _, allowPrivate := range []bool{false, true} {
			_, err := Client(5*time.Second, allowPrivate).Get(target)
			if b, ok := Blocked(err); !ok || b.Private || !b.IP.IsLoopback() {
				t.Errorf("%s（允许内网 %v）：想要拦下本机地址，得到 %v", target, allowPrivate, err)
			}
		}
	}
	if hit {
		t.Fatal("请求到达了本机的服务")
	}
}

// 检查用的是连接时的地址：同一个域名，检查函数拒绝时连不上，允许时照常连接。
func TestClientChecksAtDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	var seen []netip.Addr
	allow := client(5*time.Second, func(ip netip.Addr) error { seen = append(seen, ip); return nil })
	res, err := allow.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(seen) == 0 || !seen[0].IsLoopback() {
		t.Fatalf("连接前应该检查解析后的地址，检查到 %v", seen)
	}
	deny := client(5*time.Second, func(ip netip.Addr) error { return &BlockedError{IP: ip, Private: true} })
	if _, err := deny.Get(srv.URL); err == nil {
		t.Fatal("检查拒绝时应该连不上")
	} else if b, ok := Blocked(err); !ok || !b.Private {
		t.Fatalf("错误里应该能取出 BlockedError：%v", err)
	}
}

// 不跟随跳转，也不用环境变量里的代理。
func TestClientNoRedirectNoProxy(t *testing.T) {
	followed := false
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
	mux.HandleFunc("/b", func(http.ResponseWriter, *http.Request) { followed = true })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("HTTP_PROXY", "http://192.0.2.1:9")
	c := client(5*time.Second, func(netip.Addr) error { return nil })
	if c.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("不应该用代理")
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/a", nil)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || followed {
		t.Fatalf("不应该跟随跳转：状态 %d，跟随了 %v", res.StatusCode, followed)
	}
}

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func randKey() mesh.Key {
	var k mesh.Key
	_, _ = rand.Read(k[:])
	return k
}

// 成员从面板给的列表里消失（例如节点正在重启、和面板断开了一下）时先留着，过了 meshGrace 才删。
func TestMeshKeepsRecentPeers(t *testing.T) {
	m, err := mesh.New(mesh.Config{PrivateKey: randKey(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	n := &meshNode{m: m, log: slog.New(slog.NewTextHandler(io.Discard, nil)), panelPeers: map[string][]*nagiv1.MeshPeer{},
		panelKeys: map[string]mesh.Key{}, panelAddr: map[string]string{}, names: map[mesh.Key]string{},
		file: filepath.Join(t.TempDir(), "mesh-peers.json")}
	panel := mesh.PublicKey(randKey())
	x, y := mesh.PublicKey(randKey()), mesh.PublicKey(randKey())
	cfg := func(keys ...mesh.Key) *nagiv1.MeshConfig {
		c := &nagiv1.MeshConfig{PanelKey: panel[:], Peers: []*nagiv1.MeshPeer{{PublicKey: panel[:], IsPanel: true}}}
		for _, k := range keys {
			c.Peers = append(c.Peers, &nagiv1.MeshPeer{PublicKey: k[:], Endpoints: []string{"127.0.0.1:1"}})
		}
		return c
	}
	has := func(k mesh.Key) bool {
		for _, s := range m.Status() {
			if s.Key == k {
				return true
			}
		}
		return false
	}

	n.update("fp", "127.0.0.1:2", cfg(x, y))
	if !has(x) || !has(y) {
		t.Fatal("成员没有加上")
	}
	n.update("fp", "127.0.0.1:2", cfg(x))
	if !has(y) {
		t.Fatal("刚从列表里消失的成员应该先留着")
	}
	// 过了 meshGrace 以后删掉。
	n.mu.Lock()
	r := n.recent[y]
	r.at = time.Now().Add(-meshGrace)
	n.recent[y] = r
	n.mu.Unlock()
	n.apply()
	if has(y) {
		t.Fatal("过了 meshGrace 应该删掉")
	}
}

func TestMeshRates(t *testing.T) {
	a, b := mesh.Key{1}, mesh.Key{2}
	st := func(k mesh.Key, rx, tx uint64) mesh.Status {
		s := mesh.Status{RxBytes: rx, TxBytes: tx}
		s.Key = k
		return s
	}
	name := func(k mesh.Key, dir string) string { return "m:" + hex.EncodeToString(k[:]) + ":" + dir }
	prev := map[mesh.Key]meshCounter{}

	if got := meshRates(prev, []mesh.Status{st(a, 100, 200)}, 10); len(got) != 0 {
		t.Fatalf("第一次见到的成员还没有速率：%v", got)
	}
	got := meshRates(prev, []mesh.Status{st(a, 200, 500), st(b, 1, 1)}, 10)
	if got[name(a, "rx")] != 10 || got[name(a, "tx")] != 30 {
		t.Fatalf("速率不对：%v", got)
	}
	if _, ok := got[name(b, "rx")]; ok {
		t.Fatalf("新成员这一轮不该有速率：%v", got)
	}
	if got := meshRates(prev, []mesh.Status{st(a, 5, 5)}, 10); len(got) != 0 {
		t.Fatalf("计数器变小的这一轮不算：%v", got)
	}
	if _, ok := prev[b]; ok {
		t.Fatal("离开的成员要清掉")
	}
	got = meshRates(prev, []mesh.Status{st(a, 15, 25)}, 10)
	if got[name(a, "rx")] != 1 || got[name(a, "tx")] != 2 {
		t.Fatalf("计数器重置后从新值算起：%v", got)
	}

	// 直连时记延迟，第一次见到也记；中转时不记。
	d := st(b, 0, 0)
	d.Path, d.RTT = "direct", 12500*time.Microsecond
	r := st(a, 30, 30)
	r.Path, r.RTT = "udp-relay", 0
	got = meshRates(prev, []mesh.Status{d, r}, 10)
	if got[name(b, "rtt")] != 12.5 {
		t.Fatalf("直连的延迟：%v", got)
	}
	if _, ok := got[name(a, "rtt")]; ok {
		t.Fatalf("中转不该记延迟：%v", got)
	}
}

// 移除的面板的 mesh 公钥为了网络连续性还留着，但不能再调用只给面板的接口；备用面板不能修改。
func TestAuthorizePanel(t *testing.T) {
	id, err := loadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := tlsutil.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}))
	primary := trustedPanel{Address: "a:1", Fingerprint: "fp-a", NodeID: "n1", CertPEM: "cert", CAPEM: caPEM}
	if err := id.addPanel(primary); err != nil {
		t.Fatal(err)
	}
	ka, kb, kold := mesh.PublicKey(randKey()), mesh.PublicKey(randKey()), mesh.PublicKey(randKey())
	a := &agent{id: id, log: slog.Default(), reload: make(chan struct{}, 1),
		mesh: &meshNode{panelKeys: map[string]mesh.Key{"fp-a": ka, "fp-b": kb, "fp-old": kold}}}
	a.applyCluster(primary, &nagiv1.PanelCluster{Term: 1, Primary: "fp-a", Panels: []*nagiv1.ClusterPanel{
		{Address: "a:1", Fingerprint: "fp-a"}, {Address: "b:1", Fingerprint: "fp-b"}}})

	code := func(k mesh.Key, method string) codes.Code { return status.Code(a.authorizePanel(k, method)) }
	for _, m := range []string{nagiv1.MeshService_VolumeDownload_FullMethodName, nagiv1.MeshService_VolumeUpload_FullMethodName, nagiv1.MeshService_Upload_FullMethodName} {
		if c := code(ka, m); c != codes.OK {
			t.Fatalf("%s: 主面板应该通过，得到 %v", m, c)
		}
		if c := code(kb, m); c != codes.FailedPrecondition {
			t.Fatalf("%s: 备用面板不能调用，得到 %v", m, c)
		}
		if c := code(kold, m); c != codes.PermissionDenied {
			t.Fatalf("%s: 移除的面板不能调用，得到 %v", m, c)
		}
		if c := code(mesh.PublicKey(randKey()), m); c != codes.PermissionDenied {
			t.Fatalf("%s: 不是面板的公钥不能调用，得到 %v", m, c)
		}
	}
}

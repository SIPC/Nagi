package agent

import (
	"crypto/rand"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
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

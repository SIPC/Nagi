package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 旧版本面板（不知道直连下载）修改 QUIC、中转时，请求里没有 direct_download_off，不能把管理员关掉的直连下载重新打开；
// 新面板带着 set_direct_download，照常按请求开关。
func TestSetMeshOptionsKeepsDirectDownload(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := mesh.New(mesh.Config{PrivateKey: randKey(), Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "identity"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: Config{DataDir: dataDir}, log: quiet, mesh: &meshNode{m: m, log: quiet}}
	a.direct = newDirectDL(a)
	s := &agentService{node: a, panel: "fp"}
	set := func(req *nagiv1.MeshOptions) *nagiv1.MeshOptions {
		t.Helper()
		out, err := s.SetMeshOptions(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// 新面板关掉直连下载。
	if out := set(&nagiv1.MeshOptions{DirectDownloadOff: true, SetDirectDownload: true}); !out.GetDirectDownloadOff() || !a.direct.off() {
		t.Fatalf("新面板应能关掉直连下载：%v", out)
	}
	// 旧面板改中转：请求里没有直连下载的开关。
	if out := set(&nagiv1.MeshOptions{RelayOff: true}); !out.GetDirectDownloadOff() || !out.GetRelayOff() || !a.direct.off() {
		t.Fatalf("旧面板的请求不应打开直连下载：%v", out)
	}
	// 新面板打开。
	if out := set(&nagiv1.MeshOptions{SetDirectDownload: true}); out.GetDirectDownloadOff() || a.direct.off() {
		t.Fatalf("新面板应能打开直连下载：%v", out)
	}
}

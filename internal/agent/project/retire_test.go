package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 面板核对以后、退役请求到达之前，容器又部署到了本节点（缓存的修订号比面板核对的新）：不能停掉这一份，目录也不改名。
func TestRetireRejectsNewerSpec(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m, err := NewManager(d, nil, DefaultSubnetPool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const pid = "0123456789abcdef"
	if err := m.store(ctx, &nagiv1.ContainerSpec{ProjectId: pid, Name: "web", Dir: dir, Revision: 8, Config: &nagiv1.ContainerConfig{}}); err != nil {
		t.Fatal(err)
	}
	// 面板核对的是修订号 5 的那一份，节点上已经是 8。
	if _, err := m.Retire(ctx, pid, "web", 5, func(string) {}); !errors.Is(err, ErrRetireStale) {
		t.Fatalf("缓存比面板核对的新时应该拒绝：%v", err)
	}
	if !m.Has(ctx, pid, "web") {
		t.Fatal("拒绝以后容器的缓存不能删")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("拒绝以后目录不能改名：%v", err)
	}
	// 本节点上没有这个容器时照旧报不存在。
	if _, err := m.Retire(ctx, pid, "db", 5, func(string) {}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("没有这个容器：%v", err)
	}
}

package nodes

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
)

// 旧版本的全局规则搬到各个节点（节点的部分）和项目（容器的部分）上，搬完以后告警配置里不再有规则；
// 已经设置过规则的节点不覆盖。
func TestMigrateAlertRules(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	key, err := sealer.Load(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	q := store.New(d)
	r := NewRegistry(q, slog.Default(), key, nil)
	for _, id := range []string{"n1", "n2"} {
		if err := q.CreateNode(ctx, store.CreateNodeParams{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SetNodeAlertRules(ctx, "n2", &nagiv1.AlertRules{DiskPercent: 70}); err != nil {
		t.Fatal(err)
	}
	cfg := &nagiv1.AlertConfig{
		Channels: []*nagiv1.AlertChannel{{Id: "a", Type: "webhook", Url: "https://example.com/hook"}},
		Rules:    &nagiv1.AlertRules{OfflineSeconds: 120, ErrorPercent: 50},
	}
	if err := r.SetAlertConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	var projectRules *nagiv1.AlertRules
	if err := r.MigrateAlertRules(ctx, func(x *nagiv1.AlertRules) error { projectRules = x; return nil }); err != nil {
		t.Fatal(err)
	}
	n1, _ := q.GetNode(ctx, "n1")
	n2, _ := q.GetNode(ctx, "n2")
	if got := NodeAlertRules(n1); got.GetOfflineSeconds() != 120 || got.GetErrorPercent() != 0 {
		t.Errorf("n1 = %v，应该拿到节点的那部分", got)
	}
	if got := NodeAlertRules(n2); got.GetDiskPercent() != 70 || got.GetOfflineSeconds() != 0 {
		t.Errorf("n2 = %v，已经设置过的不覆盖", got)
	}
	if projectRules.GetErrorPercent() != 50 || projectRules.GetOfflineSeconds() != 0 {
		t.Errorf("项目的规则 %v", projectRules)
	}
	after, _ := r.AlertConfig(ctx)
	if after.GetRules() != nil || len(after.GetChannels()) != 1 {
		t.Errorf("搬完以后的配置 %v", after)
	}
}

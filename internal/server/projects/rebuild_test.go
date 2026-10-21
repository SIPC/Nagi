package projects

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
)

// 节点说"配置是别的面板写的"时，只有面板记录里本来就在这个节点上的容器才算数；
// 本组面板（包括已经移除的备用面板）写的不算别的面板写的。
func TestStaleProjectsOnlyOwnContainers(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q := store.New(d)
	for _, stmt := range []string{
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('aaaaaaaaaaaaaaaa', 'a', 'a', 1, 0, x'')`,
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('bbbbbbbbbbbbbbbb', 'b', 'b', 1, 0, x'')`,
		`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
		 VALUES ('aaaaaaaaaaaaaaaa', 'web', 1, 'n1', '/srv/a', 1, 0, 'h1', x'', '')`,
		`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
		 VALUES ('bbbbbbbbbbbbbbbb', 'web', 1, 'n2', '/srv/b', 1, 0, 'h2', x'', '')`,
	} {
		if _, err := d.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := standby.AddMember(ctx, q, "fp-old-standby"); err != nil {
		t.Fatal(err)
	}
	s := &Service{Q: q, Log: slog.Default(), Fingerprint: func() string { return "fp-self" }}
	report := []*nagiv1.ContainerStatus{
		{ProjectId: "aaaaaaaaaaaaaaaa", Name: "web", Panel: "fp-other"},
		{ProjectId: "bbbbbbbbbbbbbbbb", Name: "web", Panel: "fp-other"}, // 不在 n1 上：n1 说了不算
	}
	got := s.staleProjects(ctx, "n1", report)
	if len(got) != 1 || got[0] != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("stale = %v, want only project a", got)
	}
	report[0].Panel = "fp-old-standby"
	if got := s.staleProjects(ctx, "n1", report[:1]); len(got) != 0 {
		t.Fatalf("本组面板写的不应算别的面板写的: %v", got)
	}
}

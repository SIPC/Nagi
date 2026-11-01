package projects

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
)

func entryTestService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, stmt := range []string{
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('aaaaaaaaaaaaaaaa', 'a', 'a', 1, 0, x'')`,
		`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
		 VALUES ('aaaaaaaaaaaaaaaa', 'web', 1, 'n1', '/srv/a', 1, 0, 'h1', x'', '')`,
		`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
		 VALUES ('aaaaaaaaaaaaaaaa', 'db', 2, 'n2', '/srv/b', 1, 0, 'h2', x'', '')`,
	} {
		if _, err := d.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	return &Service{Q: store.New(d), Log: slog.Default()}
}

// 映射占用它所有入口节点上的端口。
func TestCheckConflictsCoversEntryNodes(t *testing.T) {
	ctx := context.Background()
	s := entryTestService(t)
	const project = "aaaaaaaaaaaaaaaa"
	// web（在 n1 上）的映射：8080 开在 n1 和入口节点 n3 上。
	existing := &nagiv1.Exposure{Name: "web-80", Container: "web", Service: "web", Target: 80, Protocol: "tcp", Published: 8080, EntryNodes: []string{"n3"}}
	if err := s.upsertRoute(ctx, project, existing); err != nil {
		t.Fatal(err)
	}
	route := func(entries []string, noLocal bool) *nagiv1.Exposure {
		return &nagiv1.Exposure{Name: "db-1", Container: "db", Service: "db", Target: 1, Protocol: "tcp", Published: 8080, EntryNodes: entries, NoLocalEntry: noLocal}
	}
	cases := []struct {
		name     string
		node     string
		r        *nagiv1.Exposure
		conflict bool
	}{
		{"容器所在节点和入口都没重叠", "n2", route(nil, false), false},
		{"我的入口撞上它在容器所在节点的端口", "n2", route([]string{"n1"}, false), true},
		{"我的入口撞上它的入口", "n2", route([]string{"n3"}, false), true},
		{"容器放到它的入口节点上", "n3", route(nil, false), true},
		{"容器放到它的入口节点上，但我取消了容器所在节点", "n3", route([]string{"n4"}, true), false},
		{"其他入口节点不撞", "n2", route([]string{"n4"}, false), false},
	}
	for _, c := range cases {
		err := s.checkConflicts(ctx, c.node, project, "db-1", c.r)
		if (err != nil) != c.conflict {
			t.Errorf("%s: err = %v, conflict want %v", c.name, err, c.conflict)
		}
	}
	// 取消了容器所在节点的映射不占那里的端口：n1 可以被别的映射用。
	existing.EntryNodes, existing.NoLocalEntry = []string{"n3"}, true
	if err := s.upsertRoute(ctx, project, existing); err != nil {
		t.Fatal(err)
	}
	if err := s.checkConflicts(ctx, "n1", project, "db-1", route(nil, false)); err != nil {
		t.Errorf("取消了容器所在节点后不应冲突: %v", err)
	}
}

// 入口节点存进数据库能读回来；入口都没了的映射标为无效。
func TestRouteProtoEntries(t *testing.T) {
	ctx := context.Background()
	s := entryTestService(t)
	const project = "aaaaaaaaaaaaaaaa"
	in := &nagiv1.Exposure{Name: "web-80", Container: "web", Service: "web", Target: 80, Protocol: "tcp", Published: 8080, EntryNodes: []string{"n3"}, NoLocalEntry: true}
	if err := s.upsertRoute(ctx, project, in); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Q.ListRoutes(ctx, project)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v err %v", rows, err)
	}
	got := RouteProto(rows[0])
	if !got.GetNoLocalEntry() || strings.Join(got.GetEntryNodes(), ",") != "n3" || got.GetError() != "" {
		t.Fatalf("读回来不对: %+v", got)
	}
	got.EntryNodes = nil
	if err := s.upsertRoute(ctx, project, got); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.Q.ListRoutes(ctx, project)
	if RouteProto(rows[0]).GetError() == "" {
		t.Fatal("没有入口的映射应该标为无效")
	}
}

// 入口节点算作项目相关的节点：移交时要一起带上。
func TestRelatedNodesIncludesEntryNodes(t *testing.T) {
	ctx := context.Background()
	s := entryTestService(t)
	const project = "aaaaaaaaaaaaaaaa"
	if err := s.upsertRoute(ctx, project, &nagiv1.Exposure{Name: "web-80", Container: "web", Service: "web", Target: 80, Protocol: "tcp", Published: 8080, EntryNodes: []string{"n3"}}); err != nil {
		t.Fatal(err)
	}
	related, err := s.RelatedNodes(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"n1", "n2", "n3"} {
		if !related[n] {
			t.Errorf("缺 %s: %v", n, related)
		}
	}
}

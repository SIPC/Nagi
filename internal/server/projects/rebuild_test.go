package projects

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	nodeproject "github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

func TestRebuiltEntriesNeedConfirmation(t *testing.T) {
	s := haTestService(t)
	var err error
	s.Sealer, err = sealer.Load(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sp := &nagiv1.ContainerSpec{ProjectId: "aaaaaaaaaaaaaaaa", Name: "app", Config: &nagiv1.ContainerConfig{Compose: "services:\n  app:\n    image: alpine\n"}, Routes: []*nagiv1.Exposure{{Name: "app-80", Container: "app", Service: "app", Target: 80, Published: 18080, Protocol: "tcp", EntryNodes: []string{"n2"}}}}
	if err := s.rebuildContainer(ctx, "n1", sp, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Q.ListRoutes(ctx, sp.ProjectId)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	r := RouteProto(rows[0])
	if len(r.EntryNodes) != 1 || r.EntryNodes[0] != "n2" || r.Error == "" {
		t.Errorf("restored route must retain choices but require confirmation: %v", r)
	}
	// 面板生成下发配置，写入节点实际使用的 SQLite 缓存，再由节点读取、上报给新面板。
	p, err := s.Q.GetProject(ctx, sp.ProjectId)
	if err != nil {
		t.Fatal(err)
	}
	// haTestService 为已有项目放的是空 settings 占位，生成配置前使用真实加密格式。
	p.Settings, err = s.seal(&nagiv1.ProjectSettings{}, settingsCtx(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: sp.ProjectId, Name: sp.Name})
	if err != nil {
		t.Fatal(err)
	}
	down, err := s.specFor(ctx, p, c, 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := proto.Marshal(down)
	if err != nil {
		t.Fatal(err)
	}
	nodeDB, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer nodeDB.Close()
	if err := nodestore.New(nodeDB).UpsertContainer(ctx, nodestore.UpsertContainerParams{ProjectID: down.ProjectId, Name: down.Name, Spec: data}); err != nil {
		t.Fatal(err)
	}
	manager, err := nodeproject.NewManager(nodeDB, nil, nodeproject.DefaultSubnetPool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache := manager.List(ctx)
	if len(cache) != 1 || len(cache[0].Spec.Routes) != 1 || cache[0].Spec.Routes[0].GetError() != r.Error {
		t.Fatalf("pending state lost in node cache: %v", cache)
	}
	restored := haTestService(t)
	restored.Sealer = s.Sealer
	if err := restored.rebuildContainer(ctx, "n1", cache[0].Spec, nil); err != nil {
		t.Fatal(err)
	}
	back, err := restored.Q.ListRoutes(ctx, sp.ProjectId)
	if err != nil || len(back) != 1 || back[0].PendingConfirmation != 1 {
		t.Fatalf("pending state lost on second recovery: %v %v", back, err)
	}
	all, _ := s.Q.ListAllContainers(ctx)
	plan := &nagiv1.LinkPlan{}
	if err := s.planEntries(ctx, "n2", all, map[string][]byte{"n1": {1}, "n2": {2}}, nil, plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 0 {
		t.Fatal("unconfirmed cross-node entry activated")
	}
	// 待确认的映射不能阻止其他项目正常分配端口。
	if err := s.checkConflicts(ctx, "n2", "other", "", r); err != nil {
		t.Fatal(err)
	}
	// 确认后可以下发；没有跨节点入口的缓存照常恢复。
	r.Error = ""
	if err := s.upsertRoute(ctx, sp.ProjectId, r); err != nil {
		t.Fatal(err)
	}
	plan = &nagiv1.LinkPlan{}
	if err := s.planEntries(ctx, "n2", all, map[string][]byte{"n1": {1}, "n2": {2}}, nil, plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 1 {
		t.Fatal("confirmed entry did not activate")
	}
	sp.Name, sp.Routes[0].Name, sp.Routes[0].EntryNodes = "local", "local-80", nil
	sp.Routes[0].Published = 18081
	if err := s.rebuildContainer(ctx, "n1", sp, nil); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Q.ListRoutes(ctx, sp.ProjectId)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if RouteProto(row).GetError() != "" {
			t.Fatalf("local or confirmed route disabled: %v", row)
		}
	}
}

// 节点缓存里的项目连接只在两边都是刚拼回的项目、代理所属的项目又在这个节点上时才连回去；
// 节点不能凭缓存把面板上本来就有的项目连起来。
func TestRestoreLinksOnlyRebuiltProjects(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now().Unix()
	for _, stmt := range []string{
		// a、b 刚拼回，c 是面板上本来就有的项目；a 在 n1 上，b 在 n2 上。
		fmt.Sprintf(`INSERT INTO projects (id, name, network_name, revision, created_at, settings, rebuilt_at) VALUES ('aaaaaaaaaaaaaaaa', 'a', 'a', 1, 0, x'', %d)`, now),
		fmt.Sprintf(`INSERT INTO projects (id, name, network_name, revision, created_at, settings, rebuilt_at) VALUES ('bbbbbbbbbbbbbbbb', 'b', 'b', 1, 0, x'', %d)`, now),
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('cccccccccccccccc', 'c', 'c', 1, 0, x'')`,
		`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
		 VALUES ('aaaaaaaaaaaaaaaa', 'web', 1, 'n1', '/srv/a', 1, 0, 'h1', x'', '')`,
		`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
		 VALUES ('bbbbbbbbbbbbbbbb', 'web', 1, 'n2', '/srv/b', 1, 0, 'h2', x'', '')`,
		`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
		 VALUES ('cccccccccccccccc', 'web', 1, 'n1', '/srv/c', 1, 0, 'h3', x'', '')`,
	} {
		if _, err := d.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{Q: store.New(d), Log: slog.Default()}
	px := func(a, b string) *nagiv1.LinkProxy { return &nagiv1.LinkProxy{ProjectId: a, TargetProject: b} }
	for _, tc := range []struct {
		name  string
		node  string
		proxy *nagiv1.LinkProxy
	}{
		{"existing project", "n1", px("aaaaaaaaaaaaaaaa", "cccccccccccccccc")},
		{"existing project as source", "n1", px("cccccccccccccccc", "aaaaaaaaaaaaaaaa")},
		{"project not on node", "n1", px("bbbbbbbbbbbbbbbb", "aaaaaaaaaaaaaaaa")},
	} {
		if s.restoreLinks(ctx, tc.node, []*nagiv1.LinkProxy{tc.proxy}) {
			t.Fatalf("%s: link restored", tc.name)
		}
	}
	if !s.restoreLinks(ctx, "n1", []*nagiv1.LinkProxy{px("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")}) {
		t.Fatal("link between two rebuilt projects should be restored")
	}
	links, err := s.linkedMap(ctx)
	if err != nil || len(links) != 2 || !containsString(links["aaaaaaaaaaaaaaaa"], "bbbbbbbbbbbbbbbb") {
		t.Fatalf("links = %v, %v", links, err)
	}
}

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
	got := s.staleProjects(ctx, "n1", report, nil)
	if len(got) != 1 || got[0] != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("stale = %v, want only project a", got)
	}
	// 写配置的面板节点还信任着（两个面板同时管着节点）：不以节点为准。
	if got := s.staleProjects(ctx, "n1", report[:1], []string{"fp-self", "fp-other"}); len(got) != 0 {
		t.Fatalf("另一个面板还在管这个节点时不应以节点为准: %v", got)
	}
	report[0].Panel = "fp-old-standby"
	if got := s.staleProjects(ctx, "n1", report[:1], nil); len(got) != 0 {
		t.Fatalf("本组面板写的不应算别的面板写的: %v", got)
	}
}

// 节点报告的过期项目只列出来，不自动重建；没报告过的项目不能以节点为准。
func TestStaleProjectsNeedConfirmation(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ExecContext(ctx, `INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('aaaaaaaaaaaaaaaa', 'shop', 'shop', 1, 0, x'')`); err != nil {
		t.Fatal(err)
	}
	s := &Service{Q: store.New(d), Log: slog.Default()}
	s.stale = map[string]map[string]bool{"n1": {"aaaaaaaaaaaaaaaa": true, "cccccccccccccccc": true}}
	got := s.StaleProjects(ctx, "n1")
	if len(got) != 1 || got[0].GetProjectId() != "aaaaaaaaaaaaaaaa" || got[0].GetProjectName() != "shop" {
		t.Fatalf("stale = %v，应该只有项目 shop（已删除的项目不列）", got)
	}
	if err := s.AdoptStaleProject(ctx, "n2", "aaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("别的节点没有报告过这个项目，不能以它为准")
	}
}

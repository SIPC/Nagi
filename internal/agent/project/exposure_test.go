package project

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/agent/store/projectstore"
	"github.com/SIPC/nagi/internal/pkg/db"
)

func TestReadRoutes(t *testing.T) {
	p := paths{root: t.TempDir()}
	os.MkdirAll(p.routesDir(), 0o755)
	files := map[string]string{
		"web-80.yml":   "member: web\nport: 80\nprotocol: http\ndomains: [example.com]\n",
		"db-5432.yml":  "member: umami\nservice: db\nport: 5432\nprotocol: tcp\npublished: 15432\n",
		"dup.yml":      "member: web\nport: 1\nprotocol: tcp\npublished: 15432\n",
		"bad.yml":      "service: [",
		"proto.yml":    "member: web\nport: 1\nprotocol: ftp\npublished: 9\n",
		"nomember.yml": "service: x\nport: 2\nprotocol: tcp\npublished: 19\n",
		"gone-80.yml":  "member: gone\nport: 80\nprotocol: tcp\npublished: 18\n",
		"note.txt":     "ignored",
	}
	for name, content := range files {
		os.WriteFile(filepath.Join(p.routesDir(), name), []byte(content), 0o644)
	}
	routes := readRoutes(p, map[string]bool{"web": true, "umami": true})
	got := map[string]string{}
	for _, r := range routes {
		got[r.GetName()] = r.GetError()
		if r.GetName() == "web-80" && r.GetService() != "web" {
			t.Errorf("single member route should default service to the member name: %q", r.GetService())
		}
	}
	if len(got) != 7 {
		t.Fatalf("routes: %v", got)
	}
	for _, ok := range []string{"web-80", "db-5432"} {
		if got[ok] != "" {
			t.Errorf("%s should be valid: %s", ok, got[ok])
		}
	}
	for _, bad := range []string{"dup", "bad", "proto", "nomember", "gone-80"} {
		if got[bad] == "" {
			t.Errorf("%s should be invalid", bad)
		}
	}
}

func TestImportPortsOnlyNew(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	nodeDB, err := db.OpenMigrated(ctx, filepath.Join(root, "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer nodeDB.Close()
	m := &Manager{nodeDB: nodeDB, nodeQ: nodestore.New(nodeDB), locks: map[string]*sync.Mutex{}}
	dir := filepath.Join(root, "proj")
	p := paths{root: dir}
	os.MkdirAll(p.configDir(), 0o755)
	if err := writeFormat(p); err != nil {
		t.Fatal(err)
	}
	if err := m.nodeQ.InsertProject(ctx, nodestore.InsertProjectParams{ID: "p1", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	pdb, err := openProject(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer pdb.Close()
	if err := pdb.q.InsertMember(ctx, projectstore.InsertMemberParams{Name: "web", WebhookToken: "t"}); err != nil {
		t.Fatal(err)
	}

	load := func(compose string) {
		t.Helper()
		proj, err := loadCompose(ctx, compose, nil, p.member("web").dataDir(), "nagi-p1-web")
		if err != nil {
			t.Fatal(err)
		}
		mrow, err := pdb.member(ctx, "web")
		if err != nil {
			t.Fatal(err)
		}
		if err := m.importPorts(ctx, pdb, p, "p1", mrow, proj, func(string) {}); err != nil {
			t.Fatal(err)
		}
	}
	names := func() map[string]bool {
		out := map[string]bool{}
		for _, r := range readRoutes(p, nil) {
			out[r.GetName()] = true
		}
		return out
	}

	load("services:\n  web:\n    image: x\n    ports: [\"38080:80\", \"35353:53/udp\"]\n")
	if n := names(); !n["web-80"] || !n["web-53"] || len(n) != 2 {
		t.Fatalf("first import: %v", n)
	}
	// 用户删掉一条映射，再次部署不会加回来；compose 新增的端口会导入。
	os.Remove(filepath.Join(p.routesDir(), "web-53.yml"))
	load("services:\n  web:\n    image: x\n    ports: [\"38080:80\", \"35353:53/udp\", \"38443:443\"]\n")
	if n := names(); !n["web-80"] || n["web-53"] || !n["web-443"] || len(n) != 2 {
		t.Fatalf("second import: %v", n)
	}
}

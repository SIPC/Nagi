package projects

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

// 加了目录又取消：草稿和最新版本一样，不应该留下。
func TestSaveDraftSameAsLatestClearsDraft(t *testing.T) {
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
	s := &Service{Q: store.New(d), Sealer: key, Log: slog.Default()}

	const project, name = "aaaaaaaaaaaaaaaa", "web"
	latest := &nagiv1.ContainerConfig{Form: &nagiv1.ImageForm{Image: "alpine", Volumes: []string{"/data"}}}
	settings, err := s.seal(&nagiv1.ProjectSettings{}, settingsCtx(project))
	if err != nil {
		t.Fatal(err)
	}
	config, err := s.seal(latest, versionCtx(project, name, 1)+"/config")
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.seal(&nagiv1.EnvList{}, versionCtx(project, name, 1)+"/shared-env")
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := key.Seal([]byte("{}"), []byte(secretsCtx(project, name)))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES (?, 'a', 'a', 1, 0, ?)`, []any{project, settings}},
		{`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
		 VALUES (?, ?, 1, 'n1', '/srv/a', 1, 0, 'h1', ?, '')`, []any{project, name, secrets}},
		{`INSERT INTO versions (project_id, container, version, config, shared_env, created_at) VALUES (?, ?, 1, ?, ?, 0)`, []any{project, name, config, env}},
	} {
		if _, err := d.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	draft := func() ([]byte, int64) {
		c, err := s.container(ctx, project, name)
		if err != nil {
			t.Fatal(err)
		}
		return c.Draft, c.Revision
	}

	added := &nagiv1.ContainerConfig{Form: &nagiv1.ImageForm{Image: "alpine", Volumes: []string{"/data", "/cache"}}}
	if _, err := s.SaveDraft(ctx, project, name, 1, added); err != nil {
		t.Fatal(err)
	}
	if got, _ := draft(); got == nil {
		t.Fatal("有改动时应该留下草稿")
	}
	_, revision := draft()
	if _, err := s.SaveDraft(ctx, project, name, revision, latest); err != nil {
		t.Fatal(err)
	}
	got, next := draft()
	if got != nil {
		t.Fatal("改回和最新版本一样时不应该留下草稿")
	}
	if next != revision+1 {
		t.Fatalf("修订号应该加一：%d -> %d", revision, next)
	}
}

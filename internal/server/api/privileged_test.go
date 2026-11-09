package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/projects"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 管理员部署的特权容器，操作员不能进入终端、执行命令；按部署的版本判断，从未部署时按最新版本。
func TestCheckPrivilegedEntry(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	key, err := sealer.Load(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	q := store.New(d)
	a := &API{Q: q, Projects: &projects.Service{Q: q, Sealer: key, Log: slog.Default()}}

	const project = "aaaaaaaaaaaaaaaa"
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 加密的上下文和 projects 包里的 versionCtx 一致。
	seal := func(m proto.Message, context string) []byte {
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		out, err := key.Seal(b, []byte(context))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	addVersion := func(name string, v int64, compose string) {
		ctx := fmt.Sprintf("version/%s/%s/%d", project, name, v)
		exec(`INSERT INTO versions (project_id, container, version, config, shared_env, created_at) VALUES (?, ?, ?, ?, ?, 0)`,
			project, name, v, seal(&nagiv1.ContainerConfig{Compose: compose}, ctx+"/config"), seal(&nagiv1.EnvList{}, ctx+"/shared-env"))
	}
	addContainer := func(name string, deployed int64) {
		exec(`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub, deployed_version)
		 VALUES (?, ?, 1, 'n1', '/srv/a', 1, 0, ?, x'', '', ?)`, project, name, "h-"+name, deployed)
	}
	const plain = "services:\n  web:\n    image: nginx\n"
	const privileged = plain + "    privileged: true\n"
	exec(`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES (?, 'a', 'a', 1, 0, x'')`, project)
	// web：部署的是特权版本。
	addContainer("web", 1)
	addVersion("web", 1, privileged)
	// back：回滚到了没有特权的版本 1，最新的版本 2 有特权。
	addContainer("back", 1)
	addVersion("back", 1, plain)
	addVersion("back", 2, privileged)
	// fresh：还没部署，最新版本有特权。
	addContainer("fresh", 0)
	addVersion("fresh", 1, privileged)

	cases := []struct {
		role, container string
		denied          bool
	}{
		{auth.RoleAdmin, "web", false},
		{auth.RoleOperator, "web", true},
		{auth.RoleOperator, "back", false},
		{auth.RoleOperator, "fresh", true},
		{auth.RoleAdmin, "fresh", false},
	}
	for _, c := range cases {
		err := a.checkPrivilegedEntry(ctx, c.role, project, c.container)
		if got := errors.Is(err, errPrivilegedEntry); got != c.denied || err != nil && !got {
			t.Errorf("%s 进入 %s：%v", c.role, c.container, err)
		}
	}
}

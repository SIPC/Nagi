package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
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
		// 往容器里写文件按同样的规则。
		err = a.checkPrivilegedWrite(ctx, c.role, project, c.container)
		if got := errors.Is(err, errPrivilegedWrite); got != c.denied || err != nil && !got {
			t.Errorf("%s 写入 %s：%v", c.role, c.container, err)
		}
		// 卷的所有写接口（删除、新建目录、上传、恢复、改名、移动、复制、压缩、解压）都经 checkVolumeWrite。
		err = a.checkVolumeWrite(auth.WithUser(ctx, auth.User{ID: 1, Role: c.role}), project, c.container)
		if got := errors.Is(err, errPrivilegedWrite); got != c.denied || err != nil && !got {
			t.Errorf("%s 经 checkVolumeWrite 写入 %s：%v", c.role, c.container, err)
		}
	}

	// 访问令牌：没有「执行命令」权限时不能写，有时和网页的会话一样。
	for _, c := range []struct {
		user   auth.User
		target string
		want   error
	}{
		{auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 7, AllowWrite: true}, "back", errTokenNoExec},
		{auth.User{ID: 1, Role: auth.RoleOperator, TokenID: 7, AllowWrite: true}, "back", errTokenNoExec},
		{auth.User{ID: 1, Role: auth.RoleOperator, TokenID: 7, AllowWrite: true, AllowExec: true}, "back", nil},
		{auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 7, AllowWrite: true, AllowExec: true}, "web", nil},
		{auth.User{ID: 1, Role: auth.RoleOperator, TokenID: 7, AllowWrite: true, AllowExec: true}, "web", errPrivilegedWrite},
		{auth.User{ID: 1, Role: auth.RoleOperator}, "back", nil}, // 网页的会话
	} {
		err := a.checkVolumeWrite(auth.WithUser(ctx, c.user), project, c.target)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%+v 写入 %s：%v，应为 %v", c.user, c.target, err, c.want)
		}
	}

	// 原有的删除、新建目录、恢复也走同样的检查：被拒绝时根本不会去找节点。
	ps := &projectService{a}
	opCtx := auth.WithUser(ctx, auth.User{ID: 2, Role: auth.RoleOperator})
	ref := &nagiv1.VolumeFileRef{ProjectId: project, Container: "web", Volume: ".data", Path: "a"}
	for name, call := range map[string]func() error{
		"DeleteVolumeFile":  func() error { _, err := ps.DeleteVolumeFile(opCtx, connect.NewRequest(ref)); return err },
		"CreateVolumeDir":   func() error { _, err := ps.CreateVolumeDir(opCtx, connect.NewRequest(ref)); return err },
		"RestoreVolumeFile": func() error { _, err := ps.RestoreVolumeFile(opCtx, connect.NewRequest(ref)); return err },
		"RenameVolumeFile": func() error {
			_, err := ps.RenameVolumeFile(opCtx, connect.NewRequest(&nagiv1.VolumeTransferRequest{ProjectId: project, Container: "web", Paths: []string{"a"}, NewName: "b"}))
			return err
		},
	} {
		if err := call(); connect.CodeOf(err) != connect.CodePermissionDenied || !errors.Is(err, errPrivilegedWrite) {
			t.Errorf("操作员 %s 特权容器：%v", name, err)
		}
	}
	tokenCtx := auth.WithUser(ctx, auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 7, AllowWrite: true})
	if _, err := ps.DeleteVolumeFile(tokenCtx, connect.NewRequest(&nagiv1.VolumeFileRef{ProjectId: project, Container: "back", Volume: ".data", Path: "a"})); !errors.Is(err, errTokenNoExec) {
		t.Errorf("没有执行命令权限的令牌删除文件：%v", err)
	}
}

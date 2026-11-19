package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/projects"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 特权配置等于节点的 root：有「特权配置」权限的成员（包括管理员）没做高权限确认时，保存特权配置、部署特权草稿、
// 进入特权容器、改它的文件和引用的共享变量、把容器目录放到项目根目录以外，都要求先验证身份。
// 没有这项权限的成员照旧是权限不足；访问令牌照旧放行。
func TestPrivilegedNeedsElevation(t *testing.T) {
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
	reg := nodes.NewRegistry(q, slog.Default(), key, nil)
	a := &API{Q: q, Projects: &projects.Service{Q: q, Sealer: key, Log: slog.Default(), Nodes: reg}, Nodes: reg}
	ps := &projectService{a}

	const project = "aaaaaaaaaaaaaaaa"
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatal(err)
		}
	}
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
	const plain = "services:\n  web:\n    image: nginx\n"
	const privileged = plain + "    privileged: true\n"
	add := func(name, compose, draft string) {
		secrets, err := key.Seal([]byte("{}"), []byte("container/"+project+"/"+name+"/secrets"))
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub, deployed_version)
		 VALUES (?, ?, 1, 'n1', '/srv/a', 1, 0, ?, ?, '', 1)`, project, name, "h-"+name, secrets)
		vctx := fmt.Sprintf("version/%s/%s/1", project, name)
		exec(`INSERT INTO versions (project_id, container, version, config, shared_env, created_at) VALUES (?, ?, 1, ?, ?, 0)`,
			project, name, seal(&nagiv1.ContainerConfig{Compose: compose}, vctx+"/config"), seal(&nagiv1.EnvList{}, vctx+"/shared-env"))
		if draft != "" {
			exec(`UPDATE containers SET draft = ? WHERE project_id = ? AND name = ?`,
				seal(&nagiv1.ContainerConfig{Compose: draft}, "container/"+project+"/"+name+"/draft"), project, name)
		}
	}
	exec(`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES (?, 'a', 'a', 1, 0, ?)`,
		project, seal(&nagiv1.ProjectSettings{}, "project/"+project+"/settings"))
	// root：运行的就是特权配置，引用共享变量 TOKEN。plain：没有特权。next：运行的没有特权，草稿有。
	add("root", privileged+"    environment:\n      TOKEN: ${TOKEN}\n", "")
	add("plain", plain, "")
	add("next", plain, privileged)

	soon := time.Now().Add(time.Minute).Unix()
	admin := auth.User{ID: 1, Role: auth.RoleAdmin}
	confirmed := auth.User{ID: 1, Role: auth.RoleAdmin, ElevatedUntil: soon, ElevatedBy: auth.ElevatedByApp}
	operator := auth.User{ID: 2, Role: auth.RoleOperator, Perms: auth.PresetOperator, ElevatedUntil: soon}
	token := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 7, AllowWrite: true, AllowExec: true}

	const (
		ok = iota
		elevate
		denied
	)
	check := func(what string, err error, want int) {
		t.Helper()
		got := ok
		switch {
		case isElevationError(err):
			got = elevate
		case err != nil:
			got = denied
		}
		if got != want {
			t.Errorf("%s：得到 %v（%d），应为 %d", what, err, got, want)
		}
		if want == elevate && connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s：要求验证身份时错误码应为 permission_denied，得到 %v", what, connect.CodeOf(err))
		}
	}
	users := []struct {
		name string
		u    auth.User
		// 碰到特权内容时的结果。
		want int
	}{
		{"没确认的管理员", admin, elevate},
		{"在手机上确认过的管理员", confirmed, ok},
		{"确认过的操作员", operator, denied},
		{"管理员的访问令牌", token, ok},
	}
	for _, c := range users {
		uctx := auth.WithUser(ctx, c.u)
		// 进入容器、写文件：按运行的配置。
		check(c.name+" 进入特权容器", a.checkPrivilegedEntry(ctx, c.u, project, "root"), c.want)
		check(c.name+" 进入普通容器", a.checkPrivilegedEntry(ctx, c.u, project, "plain"), ok)
		check(c.name+" 写特权容器的文件", a.checkVolumeWrite(uctx, project, "root"), c.want)
		check(c.name+" 写普通容器的文件", a.checkVolumeWrite(uctx, project, "plain"), ok)
		if c.want != ok {
			// 接口返回的错误要带着「请先验证身份」的标记，界面靠它弹验证框。放行的会去找节点，这里没有搭。
			_, err := ps.DeleteVolumeFile(uctx, connect.NewRequest(&nagiv1.VolumeFileRef{ProjectId: project, Container: "root", Volume: ".data", Path: "a"}))
			check(c.name+" DeleteVolumeFile", err, c.want)
		}
		// 保存配置：新的特权内容要确认；和现有的一样（没有新的特权内容）可以保存。
		check(c.name+" 新建特权容器", ps.checkOperatorConfig(uctx, "", "new", &nagiv1.ContainerConfig{Compose: privileged}), c.want)
		check(c.name+" 把普通容器改成特权", ps.checkOperatorConfig(uctx, project, "plain", &nagiv1.ContainerConfig{Compose: privileged}), c.want)
		check(c.name+" 原样保存特权配置", ps.checkOperatorConfig(uctx, project, "next", &nagiv1.ContainerConfig{Compose: privileged}), ok)
		check(c.name+" 保存普通配置", ps.checkOperatorConfig(uctx, project, "plain", &nagiv1.ContainerConfig{Compose: plain}), ok)
		// 特权容器引用的共享变量。
		check(c.name+" 改特权容器引用的共享变量", ps.checkOperatorSharedEnv(uctx, project, []*nagiv1.EnvVar{{Key: "TOKEN", Value: "x"}}), c.want)
		check(c.name+" 改没人引用的共享变量", ps.checkOperatorSharedEnv(uctx, project, []*nagiv1.EnvVar{{Key: "OTHER", Value: "x"}}), ok)
		// 容器目录放到项目根目录以外。
		check(c.name+" 容器目录在根目录以外", ps.checkOperatorDir(uctx, "n1", "/etc/cron.d"), c.want)
	}

	// 部署：只有会让特权草稿生效的要确认。操作员能不能部署由别处决定，这里不拦。
	target := func(names ...string) []*nagiv1.DeployTarget {
		var out []*nagiv1.DeployTarget
		for _, n := range names {
			out = append(out, &nagiv1.DeployTarget{Container: n})
		}
		return out
	}
	check("没确认的管理员部署特权草稿", ps.checkPrivilegedDeploy(auth.WithUser(ctx, admin), project, target("plain", "next")), elevate)
	check("没确认的管理员重新部署没有草稿的容器", ps.checkPrivilegedDeploy(auth.WithUser(ctx, admin), project, target("root", "plain")), ok)
	check("确认过的管理员部署特权草稿", ps.checkPrivilegedDeploy(auth.WithUser(ctx, confirmed), project, target("next")), ok)
	check("访问令牌部署特权草稿", ps.checkPrivilegedDeploy(auth.WithUser(ctx, token), project, target("next")), ok)

	// 操作员被拒绝的原因照旧是权限，不是验证身份。
	if err := a.checkPrivilegedEntry(ctx, operator, project, "root"); !errors.Is(err, errPrivilegedEntry) {
		t.Errorf("操作员进入特权容器：%v", err)
	}
	// 已经打开的终端定时重查时，确认过期不算失去权限（只在打开时要求确认）。
	if err := a.checkPrivilegedEntry(ctx, admin, project, "root"); errors.Is(err, errPrivilegedEntry) {
		t.Errorf("确认过期不应当成没有权限：%v", err)
	}
}

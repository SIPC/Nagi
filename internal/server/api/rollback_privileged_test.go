package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/projects"
	"google.golang.org/protobuf/proto"
)

const (
	rbPlain      = "services:\n  web:\n    image: nginx\n"
	rbPrivileged = rbPlain + "    privileged: true\n    environment:\n      TOKEN: ${TOKEN}\n"
)

// 回滚和回到某次部署时的状态：要回到的版本有特权内容、又和正在运行的不同时，按保存配置的规则检查。
// 否则先回滚到没有特权的版本、往卷里写文件（这时不要确认），再回滚回特权版本，特权容器就会运行这些文件。
func TestRollbackCheck(t *testing.T) {
	soon := time.Now().Add(time.Minute).Unix()
	admin := auth.User{ID: 1, Role: auth.RoleAdmin}
	confirmed := auth.User{ID: 1, Role: auth.RoleAdmin, ElevatedUntil: soon, ElevatedBy: auth.ElevatedByPassword}
	operator := auth.User{ID: 2, Role: auth.RoleOperator, Perms: auth.PresetOperator, ElevatedUntil: soon}
	token := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 7, AllowWrite: true}
	opToken := auth.User{ID: 2, Role: auth.RoleOperator, Perms: auth.PresetOperator, TokenID: 8, AllowWrite: true}

	cfg := func(compose string) *nagiv1.ContainerConfig { return &nagiv1.ContainerConfig{Compose: compose} }
	shared := []*nagiv1.EnvVar{{Key: "TOKEN", Value: "a"}}
	targets := []struct {
		name string
		t    projects.RollbackTarget
		// 碰到特权内容
		risky bool
	}{
		{"回到没有特权的版本", projects.RollbackTarget{Container: "web", Config: cfg(rbPlain), Running: cfg(rbPrivileged)}, false},
		{"回到正在运行的同一份特权配置", projects.RollbackTarget{Container: "web", Config: cfg(rbPrivileged), Shared: shared, Running: cfg(rbPrivileged), RunningShared: shared}, false},
		{"从没有特权的版本回到特权版本", projects.RollbackTarget{Container: "web", Config: cfg(rbPrivileged), Shared: shared, Running: cfg(rbPlain), RunningShared: shared}, true},
		{"特权版本引用的共享变量不同", projects.RollbackTarget{Container: "web", Config: cfg(rbPrivileged), Shared: shared, Running: cfg(rbPrivileged),
			RunningShared: []*nagiv1.EnvVar{{Key: "TOKEN", Value: "b"}}}, true},
		{"特权版本的变量不同", projects.RollbackTarget{Container: "web", Config: &nagiv1.ContainerConfig{Compose: rbPrivileged, Env: []*nagiv1.EnvVar{{Key: "LD_PRELOAD", Value: "/data/x.so"}}},
			Running: cfg(rbPrivileged)}, true},
		{"容器没有部署过", projects.RollbackTarget{Container: "web", Config: cfg(rbPrivileged)}, true},
	}
	const (
		ok = iota
		elevate
		denied
	)
	users := []struct {
		name string
		u    auth.User
		want int
	}{
		{"没确认的管理员", admin, elevate},
		{"确认过的管理员", confirmed, ok},
		{"操作员", operator, denied},
		{"管理员的访问令牌", token, ok},
		{"操作员的访问令牌", opToken, denied},
	}
	for _, u := range users {
		check := rollbackCheck(auth.WithUser(context.Background(), u.u))
		for _, tc := range targets {
			var err error
			if check != nil {
				err = check(tc.t)
			}
			want := ok
			if tc.risky {
				want = u.want
			}
			got := ok
			switch {
			case isElevationError(err):
				got = elevate
			case connect.CodeOf(err) == connect.CodePermissionDenied:
				got = denied
			case err != nil:
				t.Errorf("%s %s：%v", u.name, tc.name, err)
				continue
			}
			if got != want {
				t.Errorf("%s %s：得到 %v（%d），应为 %d", u.name, tc.name, err, got, want)
			}
		}
	}
}

// 接口上：检查在拿着项目锁、开始部署前做，交给它的是要回到的版本和正在运行的版本；没通过时作为流的错误返回，
// 界面靠它弹出验证框后重试，开始前不输出。
func TestRollbackNeedsPrivileged(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	h.a.Projects = &projects.Service{Q: h.a.Q, Sealer: h.a.Sealer, Log: slog.New(slog.DiscardHandler)}
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.Auth.CreateUser(ctx, "op", "op-password-1", auth.OperatorRoleID); err != nil {
		t.Fatal(err)
	}
	const project = "aaaaaaaaaaaaaaaa"
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := h.db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatal(err)
		}
	}
	seal := func(m proto.Message, context string) []byte {
		t.Helper()
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		out, err := h.a.Sealer.Seal(b, []byte(context))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	exec(`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES (?, 'a', 'a', 1, 0, ?)`,
		project, seal(&nagiv1.ProjectSettings{}, "project/"+project+"/settings"))
	secrets, err := h.a.Sealer.Seal([]byte("{}"), []byte("container/"+project+"/web/secrets"))
	if err != nil {
		t.Fatal(err)
	}
	// 版本 1 有特权，版本 2 没有，正在运行的是版本 2。
	exec(`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub, deployed_version)
	 VALUES (?, 'web', 1, 'n1', '/srv/a', 1, 0, 'h-web', ?, '', 2)`, project, secrets)
	for v, compose := range map[int]string{1: rbPrivileged, 2: rbPlain} {
		vctx := fmt.Sprintf("version/%s/web/%d", project, v)
		exec(`INSERT INTO versions (project_id, container, version, config, shared_env, created_at, deployed_at, has_files) VALUES (?, 'web', ?, ?, ?, 0, 1, 1)`,
			project, v, seal(&nagiv1.ContainerConfig{Compose: compose}, vctx+"/config"), seal(&nagiv1.EnvList{}, vctx+"/shared-env"))
	}
	exec(`INSERT INTO deployments (id, project_id, created_at, source, containers, versions, success) VALUES (1, ?, 0, 'deploy', '["web"]', '{"web":1}', 1)`, project)

	// 交给检查的是要回到的版本和正在运行的版本。
	stop := errors.New("stop")
	var got projects.RollbackTarget
	if _, err := h.a.Projects.Rollback(ctx, project, "web", 1, false, func(t projects.RollbackTarget) error { got = t; return stop }, func(string) {}); !errors.Is(err, stop) {
		t.Fatalf("回滚没有调用检查：%v", err)
	}
	if got.Container != "web" || got.Config.GetCompose() != rbPrivileged || got.Running.GetCompose() != rbPlain {
		t.Errorf("交给检查的版本不对：%+v", got)
	}

	streamErr := func(pc interface {
		Receive() bool
		Err() error
	}) (int, error) {
		n := 0
		for pc.Receive() {
			n++
		}
		return n, pc.Err()
	}
	for _, c := range []struct {
		user, pw string
		elevate  bool
	}{
		{"admin", "admin-password-1", true},
		{"op", "op-password-1", false},
	} {
		pc := projectClientAs(t, h, c.user, c.pw)
		rb, err := pc.Rollback(ctx, connect.NewRequest(&nagiv1.RollbackRequest{ProjectId: project, Container: "web", Version: 1}))
		if err != nil {
			t.Fatal(err)
		}
		n, err := streamErr(rb)
		rs, err2 := pc.RestoreDeployment(ctx, connect.NewRequest(&nagiv1.RestoreDeploymentRequest{ProjectId: project, DeploymentId: 1}))
		if err2 != nil {
			t.Fatal(err2)
		}
		n2, err2 := streamErr(rs)
		for name, r := range map[string]struct {
			n   int
			err error
		}{"回滚": {n, err}, "回到某次部署时的状态": {n2, err2}} {
			if connect.CodeOf(r.err) != connect.CodePermissionDenied {
				t.Errorf("%s %s 到特权版本：%v，应该被拒绝", c.user, name, r.err)
				continue
			}
			var ce *connect.Error
			elevate := errors.As(r.err, &ce) && ce.Meta().Get(elevationHeader) == "required"
			if elevate != c.elevate {
				t.Errorf("%s %s 到特权版本：要求验证身份 = %v，应为 %v（%v）", c.user, name, elevate, c.elevate, r.err)
			}
			if r.n != 0 {
				t.Errorf("%s %s：检查没通过时不该有输出，得到 %d 条", c.user, name, r.n)
			}
		}
	}
}

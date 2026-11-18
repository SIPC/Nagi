package store_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// 升级到 0027：已有的管理员、操作员、只读成员分别对应管理员、收紧后的预置操作员、预置只读；
// 节点范围是全部、项目范围是指定的成员改成跟随项目。
func TestRolesMigration(t *testing.T) {
	ctx := context.Background()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(store.Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "0027") || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := fs.ReadFile(store.Migrations, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		before[e.Name()] = &fstest.MapFile{Data: b}
	}
	d, err := db.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := db.Migrate(ctx, d, before); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, created_at, role, all_projects, all_nodes) VALUES (1, 'a', 'x', 0, 'admin', 1, 1)`,
		`INSERT INTO users (id, username, password_hash, created_at, role, all_projects, all_nodes) VALUES (2, 'o', 'x', 0, 'operator', 0, 1)`,
		`INSERT INTO users (id, username, password_hash, created_at, role, all_projects, all_nodes) VALUES (3, 'v', 'x', 0, 'viewer', 1, 1)`,
		`INSERT INTO users (id, username, password_hash, created_at, role, all_projects, all_nodes) VALUES (4, 'n', 'x', 0, 'operator', 0, 0)`,
	} {
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	sub, _ := fs.Sub(store.Migrations, "migrations")
	if err := db.Migrate(ctx, d, sub); err != nil {
		t.Fatal(err)
	}
	q := store.New(d)
	roles, err := q.ListRoles(ctx)
	if err != nil || len(roles) != 3 {
		t.Fatal(roles, err)
	}
	want := map[string]auth.Perms{auth.RoleOperator: auth.PresetOperator, auth.RoleViewer: auth.PresetViewer}
	for _, r := range roles {
		if r.Builtin == auth.RoleAdmin {
			if r.ID != auth.AdminRoleID || len(r.Permissions) != 0 {
				t.Errorf("管理员角色：%+v", r)
			}
			continue
		}
		got := auth.ParsePerms(r.Permissions)
		if got != want[r.Builtin] {
			t.Errorf("预置角色 %s 的权限 %v，想要 %v", r.Builtin, got.Names(), want[r.Builtin].Names())
		}
		if slices.Contains(r.Permissions, "project.backup") || slices.Contains(r.Permissions, "project.ha") {
			t.Errorf("收紧后的 %s 不应有备份、高可用", r.Builtin)
		}
	}
	for id, w := range map[int64]struct {
		role        string
		roleID      int64
		allNodes    int64
		nodesFollow int64
	}{
		1: {"admin", auth.AdminRoleID, 1, 0},
		2: {"member", auth.OperatorRoleID, 0, 1}, // 节点全部、项目指定 → 跟随项目
		3: {"member", auth.ViewerRoleID, 1, 0},   // 两项都是全部：保持全部
		4: {"member", auth.OperatorRoleID, 0, 0}, // 指定的节点保持
	} {
		u, err := q.GetUserByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if u.Role != w.role || u.RoleID != w.roleID || u.AllNodes != w.allNodes || u.NodesFollow != w.nodesFollow {
			t.Errorf("成员 %d：%+v，想要 %+v", id, u, w)
		}
	}
	// 角色的增删改：正在使用的角色不能删。
	id, err := q.CreateRole(ctx, "看日志", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetRolePermissions(ctx, id, []string{"project.view", "project.logs"}); err != nil {
		t.Fatal(err)
	}
	if taken, _ := q.RoleNameTaken(ctx, "看日志", 0); !taken {
		t.Error("名称已被使用")
	}
	if err := q.SetUserRole(ctx, store.SetUserRoleParams{Role: "member", RoleID: id, ID: 3}); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.DeleteRole(ctx, id); n != 0 {
		t.Error("正在使用的角色不能删除")
	}
	if n, _ := q.DeleteRole(ctx, auth.OperatorRoleID); n != 0 {
		t.Error("预置角色不能删除")
	}
	if err := q.SetUserRole(ctx, store.SetUserRoleParams{Role: "member", RoleID: auth.ViewerRoleID, ID: 3}); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.DeleteRole(ctx, id); n != 1 {
		t.Error("没人用的自定义角色可以删除")
	}
	if p, _ := q.RolePerms(ctx, id); p != "" {
		t.Errorf("删除角色时权限一起删掉：%q", p)
	}
}

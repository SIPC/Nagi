package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/store"
)

func open(t *testing.T) *store.Queries {
	t.Helper()
	d, err := db.OpenMigrated(context.Background(), filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return store.New(d)
}

func TestUsersAndRoles(t *testing.T) {
	ctx := context.Background()
	q := open(t)
	admin, err := q.CreateUser(ctx, store.CreateUserParams{Username: "a", PasswordHash: "h", CreatedAt: 1, Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := q.CreateUser(ctx, store.CreateUserParams{Username: "v", PasswordHash: "h", CreatedAt: 2, Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}

	users, err := q.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Role != "admin" || users[1].Role != "viewer" {
		t.Fatalf("ListUsers = %+v", users)
	}
	if u, err := q.GetUserByID(ctx, viewer); err != nil || u.Role != "viewer" {
		t.Fatalf("GetUserByID = %+v, %v", u, err)
	}
	if u, err := q.GetUserByUsername(ctx, "a"); err != nil || u.ID != admin || u.Role != "admin" {
		t.Fatalf("GetUserByUsername = %+v, %v", u, err)
	}
	if n, err := q.CountAdmins(ctx); err != nil || n != 1 {
		t.Fatalf("CountAdmins = %d, %v", n, err)
	}
	if err := q.SetUserRole(ctx, store.SetUserRoleParams{Role: "admin", ID: viewer}); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.CountAdmins(ctx); n != 2 {
		t.Fatalf("改角色后 CountAdmins = %d", n)
	}
	if err := q.ClearUserTOTP(ctx, viewer); err != nil {
		t.Fatal(err)
	}
	if err := q.DeleteUser(ctx, viewer); err != nil {
		t.Fatal(err)
	}
	if users, _ := q.ListUsers(ctx); len(users) != 1 {
		t.Fatalf("删除后 ListUsers = %+v", users)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	q := open(t)
	u, err := q.CreateUser(ctx, store.CreateUserParams{Username: "a", PasswordHash: "h", CreatedAt: 1, Role: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	const hash = "0123456789abcdef0123456789abcdef"
	if err := q.CreateSession(ctx, store.CreateSessionParams{TokenHash: hash, UserID: u, CreatedAt: 1, ExpiresAt: 2, Ip: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	s, err := q.GetSession(ctx, hash)
	if err != nil || s.Role != "operator" || s.Username != "a" {
		t.Fatalf("GetSession = %+v, %v", s, err)
	}
	list, err := q.ListUserSessions(ctx, u)
	if err != nil || len(list) != 1 || list[0].Ip != "10.0.0.1" {
		t.Fatalf("ListUserSessions = %+v, %v", list, err)
	}
	// 别人的会话删不掉。
	if n, err := q.DeleteUserSession(ctx, store.DeleteUserSessionParams{UserID: u + 1, Substr: hash[:16]}); err != nil || n != 0 {
		t.Fatalf("删除别人的会话 = %d, %v", n, err)
	}
	if n, err := q.DeleteUserSession(ctx, store.DeleteUserSessionParams{UserID: u, Substr: hash[:16]}); err != nil || n != 1 {
		t.Fatalf("DeleteUserSession = %d, %v", n, err)
	}
}

func TestContainerActivity(t *testing.T) {
	ctx := context.Background()
	q := open(t)
	for _, target := range []string{"web", "api, web", "webapp", "cache"} {
		if err := q.InsertActivity(ctx, store.InsertActivityParams{CreatedAt: 1, Kind: "operation", Action: "Deploy", ProjectID: "p", Target: target}); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.InsertActivity(ctx, store.InsertActivityParams{CreatedAt: 1, Kind: "operation", Action: "Deploy", ProjectID: "other", Target: "web"}); err != nil {
		t.Fatal(err)
	}
	rows, err := q.ListContainerActivity(ctx, store.ListContainerActivityParams{ProjectID: "p", Target: "web", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Target != "api, web" || rows[1].Target != "web" {
		t.Fatalf("web 的事件 = %+v", rows)
	}
	if rows, _ := q.ListContainerActivity(ctx, store.ListContainerActivityParams{ProjectID: "p", Target: "web", Limit: 1}); len(rows) != 1 {
		t.Fatalf("Limit 没生效：%+v", rows)
	}
}

func TestActivity(t *testing.T) {
	ctx := context.Background()
	q := open(t)
	add := func(kind string) {
		t.Helper()
		if err := q.InsertActivity(ctx, store.InsertActivityParams{CreatedAt: 1, Kind: kind, Action: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	add("operation")
	add("alert")
	add("operation")
	add("operation")

	all, err := q.ListActivity(ctx, store.ListActivityParams{Kind: "", Before: int64(0), Limit: 10})
	if err != nil || len(all) != 4 || all[0].ID <= all[1].ID {
		t.Fatalf("全部 = %+v, %v", all, err)
	}
	alerts, err := q.ListActivity(ctx, store.ListActivityParams{Kind: "alert", Before: int64(0), Limit: 10})
	if err != nil || len(alerts) != 1 {
		t.Fatalf("告警 = %+v, %v", alerts, err)
	}
	page, err := q.ListActivity(ctx, store.ListActivityParams{Kind: "", Before: all[1].ID, Limit: 10})
	if err != nil || len(page) != 2 {
		t.Fatalf("翻页 = %+v, %v", page, err)
	}
	if err := q.PruneActivity(ctx, int64(2)); err != nil {
		t.Fatal(err)
	}
	kept, _ := q.ListActivity(ctx, store.ListActivityParams{Kind: "", Before: int64(0), Limit: 10})
	if len(kept) != 2 {
		t.Fatalf("清理后 = %+v", kept)
	}
}

package api

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// 打开着的容器终端、日志流：踢出登录、降权、移出项目范围之后都要断开。
func TestWatchSession(t *testing.T) {
	old := sessionRecheck
	sessionRecheck = 5 * time.Millisecond
	t.Cleanup(func() { sessionRecheck = old })

	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	q := store.New(d)
	a := &API{Q: q, Auth: auth.New(q, nil)}
	const token = "tok"
	u := auth.User{ID: 2, Name: "op", Role: auth.RoleOperator, LimitProjects: true}

	reset := func() {
		exec(`DELETE FROM users`)
		exec(`DELETE FROM projects`)
		exec(`INSERT INTO users (id, username, password_hash, created_at, role, all_projects, all_nodes) VALUES (2, 'op', 'x', 0, 'operator', 0, 1)`)
		exec(`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('p1', 'one', 'one', 1, 0, x'')`)
		exec(`INSERT INTO user_projects (user_id, project_id) VALUES (2, 'p1')`)
		exec(`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, 2, 0, ?)`, auth.HashToken(token), time.Now().Add(time.Hour).Unix())
	}
	// watch 在 limit 之内等重查断开，没断开时返回 nil。
	watch := func(allowed func(string) bool, limit time.Duration) <-chan error {
		ctx, cancel := context.WithTimeout(ctx, limit)
		done := make(chan error, 1)
		go func() {
			defer cancel()
			a.watchSession(ctx, token, u, func(cur auth.User) error {
				return a.containerAccess(ctx, cur, "p1", allowed)
			}, func(err error) { done <- err })
			close(done)
		}()
		return done
	}
	logs := func(role string) bool { return auth.Allowed(role, nagiv1connect.ProjectServiceStreamLogsProcedure) }

	cases := []struct {
		name    string
		allowed func(string) bool
		change  string
		want    error
	}{
		{"踢出登录", auth.CanOperate, `DELETE FROM sessions`, errSessionExpired},
		{"降为只读", auth.CanOperate, `UPDATE users SET role = 'viewer'`, errRoleChanged},
		{"移出项目范围", auth.CanOperate, `DELETE FROM user_projects`, errRoleChanged},
		{"日志：移出项目范围", logs, `DELETE FROM user_projects`, errRoleChanged},
	}
	for _, c := range cases {
		reset()
		done := watch(c.allowed, 5*time.Second)
		select {
		case err := <-done:
			t.Fatalf("%s：还没改就断开了：%v", c.name, err)
		case <-time.After(30 * time.Millisecond):
		}
		exec(c.change)
		if err := <-done; !errors.Is(err, c.want) {
			t.Errorf("%s：得到 %v，想要 %v", c.name, err, c.want)
		}
	}

	// 只读成员能看日志：降为只读不影响日志流。
	reset()
	exec(`UPDATE users SET role = 'viewer'`)
	if err := <-watch(logs, 50*time.Millisecond); err != nil {
		t.Fatalf("只读成员可以看日志：%v", err)
	}
}

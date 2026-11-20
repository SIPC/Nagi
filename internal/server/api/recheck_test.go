package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
)

func TestRecheckCurrentWebPolicy(t *testing.T) {
	h := newAuditHarness(t)
	bg := context.Background()
	id, err := h.a.Auth.CreateUser(bg, "admin", "admin-password-1", auth.AdminRoleID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithMeta(bg, auth.Meta{IP: "198.51.100.20"})
	c, err := h.a.Auth.NewSession(ctx, id, "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	u, err := h.a.Auth.SessionUser(ctx, c.Value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.recheckCaller(ctx, u, nil, appByDevice, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT OR REPLACE INTO settings (key,value) VALUES ('admin_networks','192.0.2.1')`,
		`UPDATE sessions SET created_at = 1`,
		`UPDATE sessions SET last_seen = 1`,
	} {
		if _, err := h.db.Exec(sql); err != nil {
			t.Fatal(err)
		}
		if _, err := h.a.recheckCaller(ctx, u, nil, appByDevice, time.Now()); !errors.Is(err, errSessionExpired) {
			t.Errorf("policy not enforced: %s: %v", sql, err)
		}
		if _, err := h.db.Exec(`DELETE FROM settings WHERE key = 'admin_networks'; UPDATE sessions SET created_at = ?, last_seen = ?`, time.Now().Unix(), time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStandbyWebhookDoesNotDisclosePrimary(t *testing.T) {
	h := newAuditHarness(t)
	h.a.Panel.Standby = &standby.Client{Info: &standby.Info{PrimaryAddress: "secret.example:9443"}}
	w := httptest.NewRecorder()
	h.a.standbyBlock(h.a.WebhookHandler()).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hooks/unknown", nil))
	if w.Code != http.StatusNotFound || w.Body.String() != "404 page not found\n" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

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
	u := auth.User{ID: 2, Name: "op", Role: auth.RoleOperator, Perms: auth.PresetOperator, LimitProjects: true, SessionID: "sid"}

	reset := func() {
		exec(`DELETE FROM users`)
		exec(`DELETE FROM projects`)
		exec(`INSERT INTO users (id, username, password_hash, created_at, role, all_projects, all_nodes) VALUES (2, 'op', 'x', 0, 'member', 0, 1)`)
		exec(`UPDATE users SET role_id = 2`)
		exec(`DELETE FROM role_permissions WHERE role_id = 2`)
		for _, p := range auth.PresetOperator.Names() {
			exec(`INSERT INTO role_permissions (role_id, permission) VALUES (2, ?)`, p)
		}
		exec(`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('p1', 'one', 'one', 1, 0, x'')`)
		exec(`INSERT INTO user_projects (user_id, project_id) VALUES (2, 'p1')`)
		exec(`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, id) VALUES (?, 2, ?, ?, 'sid')`, auth.HashToken(token), time.Now().Unix(), time.Now().Add(time.Hour).Unix())
	}
	// watch 在 limit 之内等重查断开，没断开时返回 nil。
	watch := func(need auth.Perm, limit time.Duration) <-chan error {
		ctx, cancel := context.WithTimeout(ctx, limit)
		done := make(chan error, 1)
		go func() {
			defer cancel()
			a.watchSession(ctx, u, nil, appByDevice, func(cur auth.User) error {
				return a.containerAccess(ctx, cur, "p1", need)
			}, func(err error) { done <- err })
			close(done)
		}()
		return done
	}

	cases := []struct {
		name   string
		need   auth.Perm
		change string
		want   error
	}{
		{"踢出登录", auth.PermTerminal, `DELETE FROM sessions`, errSessionExpired},
		{"降为只读", auth.PermTerminal, `UPDATE users SET role_id = 3`, errRoleChanged},
		{"移出项目范围", auth.PermTerminal, `DELETE FROM user_projects`, errRoleChanged},
		{"日志：移出项目范围", auth.PermLogs, `DELETE FROM user_projects`, errRoleChanged},
		// 角色的权限改了（例如去掉日志），打开着的日志流也断开。
		{"日志：角色去掉日志权限", auth.PermLogs, `DELETE FROM role_permissions WHERE role_id = 2 AND permission = 'project.logs'`, errRoleChanged},
		{"终端：角色去掉终端权限", auth.PermTerminal, `DELETE FROM role_permissions WHERE role_id = 2 AND permission = 'project.terminal'`, errRoleChanged},
	}
	for _, c := range cases {
		reset()
		done := watch(c.need, 5*time.Second)
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
	exec(`UPDATE users SET role_id = 3`)
	if err := <-watch(auth.PermLogs, 50*time.Millisecond); err != nil {
		t.Fatalf("只读成员可以看日志：%v", err)
	}
}

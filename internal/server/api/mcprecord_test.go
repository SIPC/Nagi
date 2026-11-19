package api

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
)

// MCP 调用的记录按参数里的项目挂到项目上：只认调用者范围内的项目。参数是调用者自己写的，
// 写一个范围外的项目名称，记录不能挂到那个项目上（出现在别人的活动记录里）。
func TestRecordMCPCallProjectInScope(t *testing.T) {
	ctx := context.Background()
	a, _ := scopeTestAPI(t,
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('p3', 'shop', 'shop-a', 1, 0, x''), ('p4', 'Shop', 'shop-b', 1, 0, x'')`)
	l, err := audit.Open(ctx, filepath.Join(t.TempDir(), "audit.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a.Audit = l
	member := auth.User{ID: 2, Role: auth.RoleOperator, Perms: auth.PresetOperator, LimitProjects: true, LimitNodes: true, TokenID: 7}
	admin := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 8}
	cases := []struct {
		action  string
		u       auth.User
		project string
		want    string
	}{
		{"member-out-of-scope-name", member, "two", ""},
		{"member-out-of-scope-id", member, "p2", ""},
		{"member-in-scope-name", member, "ONE", "p1"},
		{"member-in-scope-id", member, "p1", "p1"},
		{"admin-name", admin, "two", "p2"},
		{"admin-network-name", admin, "shop-b", "p4"},
		{"admin-ambiguous-name", admin, "shop", ""},
		{"admin-unknown", admin, "nowhere", ""},
	}
	for _, c := range cases {
		_, col := audit.WithCollector(ctx)
		a.RecordMCPCall(ctx, c.u, audit.Event{Category: audit.MCP, Action: c.action, TargetType: "tool", TargetID: "t", TargetName: "t"}, c.project, col)
	}
	rows, err := l.List(ctx, audit.Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Action] = r.ProjectID
	}
	for _, c := range cases {
		if p, ok := got[c.action]; !ok || p != c.want {
			t.Errorf("%s：项目 %q，应该是 %q", c.action, p, c.want)
		}
	}
}

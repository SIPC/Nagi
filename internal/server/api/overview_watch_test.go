package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/cookiejar"
	"testing"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/projects"
	"google.golang.org/protobuf/proto"
)

// M1：订阅流打开以后降低成员的角色、缩小访问范围，下一条消息就按新的算，不再按打开时的权限推送。
func TestWatchOverviewRechecksUser(t *testing.T) {
	h := newAuditHarness(t)
	reg := nodes.NewRegistry(h.a.Q, slog.New(slog.DiscardHandler), h.a.Sealer, nil)
	h.a.Nodes = reg
	h.a.Projects = &projects.Service{Q: h.a.Q, Sealer: h.a.Sealer, Log: slog.New(slog.DiscardHandler), Nodes: reg}
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 两个项目：p1 一个容器、p2 两个容器；一个待批准的节点（管理员的那一行带「批准」）。
	exec(`INSERT INTO nodes (id, name, created_at, mesh_key) VALUES ('n1', 'a', 0, x'01')`)
	exec(`INSERT INTO nodes (id, name, created_at, mesh_key, pending) VALUES ('n2', 'b', 0, x'02', 1)`)
	// 加密的上下文和 projects 包里的一致。
	sealRaw := func(b []byte, c string) []byte {
		out, err := h.a.Sealer.Seal(b, []byte(c))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	seal := func(m proto.Message, c string) []byte {
		b, _ := proto.Marshal(m)
		return sealRaw(b, c)
	}
	addContainer := func(project, name string) {
		exec(`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub, deployed_version)
		 VALUES (?, ?, 1, 'n1', '/srv/a', 1, 0, ?, ?, '', 0)`, project, name, "h-"+project+name, sealRaw([]byte("{}"), "container/"+project+"/"+name+"/secrets"))
		vctx := fmt.Sprintf("version/%s/%s/%d", project, name, 1)
		exec(`INSERT INTO versions (project_id, container, version, config, shared_env, created_at) VALUES (?, ?, 1, ?, ?, 0)`, project, name,
			seal(&nagiv1.ContainerConfig{Compose: "services:\n  web:\n    image: nginx\n"}, vctx+"/config"), seal(&nagiv1.EnvList{}, vctx+"/shared-env"))
	}
	for _, p := range []string{"p1", "p2"} {
		exec(`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES (?, ?, ?, 1, 0, ?)`, p, p, p,
			seal(&nagiv1.ProjectSettings{}, "project/"+p+"/settings"))
	}
	addContainer("p1", "web")
	addContainer("p2", "web")
	addContainer("p2", "db")

	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	bob, err := h.a.Auth.CreateUser(ctx, "bob", "bob-password-1", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, sc := h.login("admin", "admin-password-1")

	// bob 登录网页，打开订阅流。
	jar, _ := cookiejar.New(nil)
	hc := *h.srv.Client()
	hc.Jar = jar
	if _, err := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL).Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: "bob", Password: "bob-password-1"})); err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := nagiv1connect.NewProjectServiceClient(&hc, h.srv.URL).WatchOverview(sctx, connect.NewRequest(&nagiv1.WatchOverviewRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	msgs := make(chan *nagiv1.Overview, 16)
	go func() {
		defer close(msgs)
		for stream.Receive() {
			if m := stream.Msg(); !m.GetHeartbeat() {
				msgs <- m
			}
		}
	}()
	approve := func(o *nagiv1.Overview) string {
		for _, it := range o.GetAttention().GetItems() {
			if it.GetKind() == "node_pending" {
				return it.GetAction()
			}
		}
		return "-"
	}
	// next 等下一条和 prev 不同的消息（流量统计就绪等无关的变化跳过）。
	next := func(name string, prev *nagiv1.Overview, same func(a, b *nagiv1.Overview) bool) *nagiv1.Overview {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case m, ok := <-msgs:
				if !ok {
					t.Fatalf("%s：流结束了：%v", name, stream.Err())
				}
				if prev == nil || !same(prev, m) {
					return m
				}
			case <-deadline:
				t.Fatalf("%s：10 秒内没有新的消息", name)
			}
		}
	}
	scope := func(a, b *nagiv1.Overview) bool {
		return a.GetContainersTotal() == b.GetContainersTotal() && approve(a) == approve(b)
	}

	first := next("打开", nil, scope)
	if first.GetContainersTotal() != 3 || approve(first) != "approve" {
		t.Fatalf("管理员、全部项目：容器 %d，批准 %q", first.GetContainersTotal(), approve(first))
	}

	// 降为只读：待批准的节点那一行不再带「批准」。
	if _, err := sc.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: bob, Role: auth.RoleViewer})); err != nil {
		t.Fatal(err)
	}
	m := next("降为只读", first, scope)
	if approve(m) != "" || m.GetContainersTotal() != 3 {
		t.Fatalf("降为只读后：容器 %d，批准 %q", m.GetContainersTotal(), approve(m))
	}

	// 从全部项目改成只有 p1：下一条消息里只有 p1 的容器。
	if _, err := sc.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: bob,
		Scope: &nagiv1.UserScope{AllProjects: false, ProjectIds: []string{"p1"}, AllNodes: true}})); err != nil {
		t.Fatal(err)
	}
	m = next("缩小访问范围", m, scope)
	if m.GetContainersTotal() != 1 {
		t.Fatalf("只剩 p1 时容器应为 1：%d", m.GetContainersTotal())
	}

	// 会话被退出：流结束。
	exec(`DELETE FROM sessions WHERE user_id = ?`, bob)
	reg.Changes.Bump()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-msgs:
			if !ok {
				if connect.CodeOf(stream.Err()) != connect.CodeUnauthenticated {
					t.Errorf("会话被退出后应 unauthenticated：%v", stream.Err())
				}
				return
			}
		case <-deadline:
			t.Fatal("会话被退出后流没有结束")
		}
	}
}

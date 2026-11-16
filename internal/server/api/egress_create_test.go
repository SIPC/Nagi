package api

import (
	"context"
	"log/slog"
	"net/http/cookiejar"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/projects"
)

func projectClientAs(t *testing.T, h *auditHarness, user, pw string) nagiv1connect.ProjectServiceClient {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := *h.srv.Client()
	c.Jar = jar
	ac := nagiv1connect.NewAuthServiceClient(&c, h.srv.URL)
	if _, err := ac.Login(context.Background(), connect.NewRequest(&nagiv1.LoginRequest{Username: user, Password: pw})); err != nil {
		t.Fatal(err)
	}
	return nagiv1connect.NewProjectServiceClient(&c, h.srv.URL)
}

// M1：操作员新建项目时不能直接关掉出站限制或加放行，只能用默认值；管理员可以。
func TestCreateProjectEgressAdminOnly(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	h.a.Projects = &projects.Service{Q: h.a.Q, Sealer: h.a.Sealer, Log: slog.New(slog.DiscardHandler)}
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.Auth.CreateUser(ctx, "op", "op-password-1", auth.RoleOperator); err != nil {
		t.Fatal(err)
	}
	create := func(pc nagiv1connect.ProjectServiceClient, name string, e *nagiv1.Egress) (*nagiv1.Project, error) {
		res, err := pc.CreateProject(ctx, connect.NewRequest(&nagiv1.CreateProjectRequest{Name: name, NetworkName: name,
			Settings: &nagiv1.ProjectSettings{Egress: e}}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	op := projectClientAs(t, h, "op", "op-password-1")
	for i, e := range []*nagiv1.Egress{
		{Mode: "off"},
		{Mode: "log"},
		{Mode: "block", Allow: []string{"169.254.169.254:80"}},
		{Mode: "off", Allow: []string{"169.254.169.254:80"}},
	} {
		if _, err := create(op, "evil"+string(rune('a'+i)), e); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("操作员带 %v 新建项目：err = %v，应该被拒绝", e, err)
		}
	}
	for i, e := range []*nagiv1.Egress{nil, {Mode: "block"}} {
		p, err := create(op, "ok"+string(rune('a'+i)), e)
		if err != nil {
			t.Fatalf("操作员用默认出站限制新建项目：%v", err)
		}
		if got := p.GetSettings().GetEgress(); got.GetMode() != "block" || len(got.GetAllow()) != 0 {
			t.Errorf("新项目的出站限制 = %v，应该是默认的拦截", got)
		}
	}
	admin := projectClientAs(t, h, "admin", "admin-password-1")
	p, err := create(admin, "adm", &nagiv1.Egress{Mode: "block", Allow: []string{"10.0.0.5:5432"}})
	if err != nil {
		t.Fatalf("管理员新建带放行的项目：%v", err)
	}
	if got := p.GetSettings().GetEgress().GetAllow(); len(got) != 1 {
		t.Errorf("管理员的放行没有保存：%v", got)
	}
}

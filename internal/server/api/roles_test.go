package api

import (
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
)

func secretProject() *nagiv1.Project {
	return &nagiv1.Project{
		Settings:       &nagiv1.ProjectSettings{SharedEnv: []*nagiv1.EnvVar{{Key: "TOKEN", Value: "x"}}},
		SharedEnvDraft: []*nagiv1.EnvVar{{Key: "TOKEN", Value: "y"}},
		Containers: []*nagiv1.Container{{
			Name:         "web",
			Config:       &nagiv1.ContainerConfig{Compose: "secret"},
			Draft:        &nagiv1.ContainerConfig{Compose: "secret"},
			DeployKey:    "key",
			WebhookToken: "token",
		}},
	}
}

func TestRedactViewerProject(t *testing.T) {
	res := &nagiv1.ListProjectsResponse{Projects: []*nagiv1.ProjectSummary{{Project: secretProject()}}}
	redactFor(auth.User{ID: 2, Role: auth.RoleViewer}, res)
	p := res.Projects[0].Project
	c := p.Containers[0]
	if c.Name != "web" {
		t.Errorf("容器名不该被去掉：%q", c.Name)
	}
	if c.Config != nil || c.Draft != nil || c.DeployKey != "" || c.WebhookToken != "" {
		t.Errorf("容器的配置和密钥没有去掉：%v", c)
	}
	if len(p.Settings.SharedEnv) != 0 || len(p.SharedEnvDraft) != 0 {
		t.Errorf("共享变量没有去掉")
	}
}

func TestRedactKeepsForOperatorAndAdmin(t *testing.T) {
	for _, role := range []string{auth.RoleOperator, auth.RoleAdmin} {
		p := secretProject()
		redactFor(auth.User{ID: 1, Role: role}, p)
		if p.Containers[0].Config == nil || p.Containers[0].DeployKey == "" {
			t.Errorf("%s 应该能看到配置和密钥", role)
		}
	}
}

func TestRedactMasterKeyPath(t *testing.T) {
	for role, want := range map[string]string{auth.RoleAdmin: "/k", auth.RoleOperator: "", auth.RoleViewer: ""} {
		res := &nagiv1.GetSettingsResponse{Settings: &nagiv1.PanelSettings{MasterKeyPath: "/k"}}
		redactFor(auth.User{ID: 1, Role: role}, res)
		if got := res.Settings.MasterKeyPath; got != want {
			t.Errorf("%s：路径 = %q，应为 %q", role, got, want)
		}
	}
}

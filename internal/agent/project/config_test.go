package project

import (
	"context"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/dotenv"
)

// 写出去的 .env 必须被 compose 原样读回来。
func TestEnvFileRoundTrip(t *testing.T) {
	values := map[string]string{
		"PLAIN":    "hello",
		"SPACES":   "a b  c",
		"DOLLAR":   "pa$$word${NOT_A_VAR}",
		"SINGLE":   "it's",
		"DOUBLE":   `say "hi"`,
		"MIXED":    `it's "$HOME" \ back`,
		"NEWLINE":  "line1\nline2",
		"HASH":     "a#b # c",
		"EMPTY":    "",
		"UNICODE":  "凪 Nagi",
		"EQUALS":   "a=b=c",
		"BACKTICK": "`cmd`",
	}
	var vars []*nagiv1.EnvVar
	for k, v := range values {
		vars = append(vars, &nagiv1.EnvVar{Key: k, Value: v})
	}
	got, err := dotenv.UnmarshalBytesWithLookup(envFile(vars), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, envFile(vars))
	}
	for k, want := range values {
		if got[k] != want {
			t.Errorf("%s: got %q, want %q", k, got[k], want)
		}
	}
}

func TestFormCompose(t *testing.T) {
	text, err := formCompose(&nagiv1.ImageForm{
		Image:   "redis:7",
		Command: "redis-server --save 60 1",
		Ports:   []*nagiv1.PortMapping{{Published: 6380, Target: 6379}},
		Volumes: []string{"/data", "/var/lib/x"},
		Workdir: "/app",
	})
	if err != nil {
		t.Fatal(err)
	}
	proj, err := loadCompose(context.Background(), text, nil, t.TempDir(), "nagi-test")
	if err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, text)
	}
	svc := proj.Services[formService]
	if svc.Image != "redis:7" || svc.WorkingDir != "/app" {
		t.Fatalf("unexpected service: %+v", svc)
	}
	if len(svc.Ports) != 1 || svc.Ports[0].Target != 6379 || svc.Ports[0].Published != "6380" {
		t.Fatalf("ports: %+v", svc.Ports)
	}
	if _, ok := proj.Volumes["data"]; !ok {
		t.Fatalf("volumes: %v\n%s", proj.Volumes, text)
	}
	if _, ok := proj.Volumes["var_lib_x"]; !ok {
		t.Fatalf("volumes: %v\n%s", proj.Volumes, text)
	}
}

func TestValidate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ok := &nagiv1.ProjectConfig{
		Compose: "services:\n  web:\n    image: nginx:${TAG}\n",
		Env:     []*nagiv1.EnvVar{{Key: "TAG", Value: "alpine"}},
		ServiceEnv: map[string]*nagiv1.EnvList{
			"web": {Vars: []*nagiv1.EnvVar{{Key: "A", Value: "1"}}},
		},
	}
	proj, err := validate(ctx, ok, dir, "nagi-test")
	if err != nil {
		t.Fatal(err)
	}
	if proj.Services["web"].Image != "nginx:alpine" {
		t.Fatalf("interpolation: %q", proj.Services["web"].Image)
	}

	cases := map[string]*nagiv1.ProjectConfig{
		"empty":         {Compose: "  "},
		"bad yaml":      {Compose: "services: ["},
		"bad key":       {Compose: ok.Compose, Env: []*nagiv1.EnvVar{{Key: "1BAD"}}},
		"dup key":       {Compose: ok.Compose, Env: []*nagiv1.EnvVar{{Key: "A"}, {Key: "A"}}},
		"unknown svc":   {Compose: ok.Compose, ServiceEnv: map[string]*nagiv1.EnvList{"db": {Vars: []*nagiv1.EnvVar{{Key: "A"}}}}},
		"form no image": {Form: &nagiv1.ImageForm{}},
	}
	for name, cfg := range cases {
		if _, err := validate(ctx, cfg, dir, "nagi-test"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

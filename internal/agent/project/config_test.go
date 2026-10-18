package project

import (
	"context"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/agent/store/projectstore"
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
	}, "cache")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := loadCompose(context.Background(), text, nil, t.TempDir(), "nagi-test")
	if err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, text)
	}
	svc := proj.Services["cache"]
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
	ok := &nagiv1.ContainerConfig{
		Compose: "services:\n  web:\n    image: nginx:${TAG}\n",
		Env:     []*nagiv1.EnvVar{{Key: "TAG", Value: "alpine"}},
		ServiceEnv: map[string]*nagiv1.EnvList{
			"web": {Vars: []*nagiv1.EnvVar{{Key: "A", Value: "1"}}},
		},
	}
	proj, err := validate(ctx, ok, "web", nil, dir, "nagi-test")
	if err != nil {
		t.Fatal(err)
	}
	if proj.Services["web"].Image != "nginx:alpine" {
		t.Fatalf("interpolation: %q", proj.Services["web"].Image)
	}

	cases := map[string]*nagiv1.ContainerConfig{
		"empty":         {Compose: "  "},
		"bad yaml":      {Compose: "services: ["},
		"bad key":       {Compose: ok.Compose, Env: []*nagiv1.EnvVar{{Key: "1BAD"}}},
		"dup key":       {Compose: ok.Compose, Env: []*nagiv1.EnvVar{{Key: "A"}, {Key: "A"}}},
		"unknown svc":   {Compose: ok.Compose, ServiceEnv: map[string]*nagiv1.EnvList{"db": {Vars: []*nagiv1.EnvVar{{Key: "A"}}}}},
		"form no image": {Form: &nagiv1.ImageForm{}},
		"self depend":   {Compose: ok.Compose, DependsOn: []string{"web"}},
		"bad depend":    {Compose: ok.Compose, DependsOn: []string{"Bad_Name"}},
	}
	for name, cfg := range cases {
		if _, err := validate(ctx, cfg, "web", nil, dir, "nagi-test"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// 共享变量可以在 compose 里引用，同名时以容器自己的变量为准。
func TestSharedEnv(t *testing.T) {
	ctx := context.Background()
	shared := []*nagiv1.EnvVar{{Key: "TAG", Value: "1"}, {Key: "DB_PASS", Value: "x"}}
	cfg := &nagiv1.ContainerConfig{Compose: "services:\n  api:\n    image: app:${TAG}\n    environment:\n      P: ${DB_PASS}\n"}
	proj, err := validate(ctx, cfg, "api", shared, t.TempDir(), "nagi-test")
	if err != nil {
		t.Fatal(err)
	}
	if proj.Services["api"].Image != "app:1" {
		t.Fatalf("shared: %q", proj.Services["api"].Image)
	}
	cfg.Env = []*nagiv1.EnvVar{{Key: "TAG", Value: "2"}}
	if proj, _ = validate(ctx, cfg, "api", shared, t.TempDir(), "nagi-test"); proj.Services["api"].Image != "app:2" {
		t.Fatalf("container should win: %q", proj.Services["api"].Image)
	}
	// TAG 被容器自己的变量覆盖，改共享的 TAG 不影响它；改 DB_PASS 影响。
	changedTag := []*nagiv1.EnvVar{{Key: "TAG", Value: "9"}, {Key: "DB_PASS", Value: "x"}}
	changedPass := []*nagiv1.EnvVar{{Key: "TAG", Value: "1"}, {Key: "DB_PASS", Value: "y"}}
	if sharedEnvChanged(cfg, "api", shared, changedTag) {
		t.Fatal("TAG is overridden by the container")
	}
	if !sharedEnvChanged(cfg, "api", shared, changedPass) {
		t.Fatal("DB_PASS change should be noticed")
	}
	other := &nagiv1.ContainerConfig{Compose: "services:\n  web:\n    image: nginx\n"}
	if sharedEnvChanged(other, "web", shared, changedPass) {
		t.Fatal("web does not reference DB_PASS")
	}
}

func TestContainerName(t *testing.T) {
	for _, ok := range []string{"web", "api-v2", "a", "db1"} {
		if checkContainerName(ok) != nil {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Web", "-web", "web-", "a_b", "a.b", "abcdefghijabcdefghijabcdefghijabc"} {
		if checkContainerName(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

// 一起部署时按依赖排序：被依赖的先部署，其余按创建顺序；有循环时报错。
func TestOrderJobs(t *testing.T) {
	mk := func(name string, pos int64, deps ...string) job {
		return job{row: projectstore.Container{Name: name, Position: pos}, cfg: &nagiv1.ContainerConfig{DependsOn: deps}}
	}
	got, err := orderJobs([]job{mk("web", 1, "api"), mk("api", 2, "db"), mk("db", 3), mk("cron", 4)})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, j := range got {
		names = append(names, j.row.Name)
	}
	if strings.Join(names, ",") != "db,api,web,cron" {
		t.Fatalf("order: %v", names)
	}
	if _, err := orderJobs([]job{mk("a", 1, "b"), mk("b", 2, "a")}); err == nil {
		t.Fatal("cycle should fail")
	}
}

// 单个容器（内网开启）只加入项目内网；容器组用自己的默认网络，项目内网由节点接入。
func TestOverrideNetworks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mp := containerPaths{root: dir}
	text, _ := formCompose(&nagiv1.ImageForm{Image: "nginx"}, "web")
	single, err := loadCompose(ctx, text, nil, dir, "nagi-p-web")
	if err != nil {
		t.Fatal(err)
	}
	ov, err := buildOverride(overrideInput{proj: single, mp: mp, projectID: "p", container: "web", version: 1, single: true, intranet: true})
	if err != nil {
		t.Fatal(err)
	}
	nets := ov["networks"].(map[string]any)
	if n, ok := nets["nagi"].(map[string]any); !ok || n["name"] != "nagi-p" || n["external"] != true {
		t.Fatalf("single container networks: %v", nets)
	}
	svc := ov["services"].(map[string]any)["web"].(map[string]any)
	if svc["networks"] == nil || svc["labels"].(map[string]string)[LabelSingle] != "1" {
		t.Fatalf("single container service: %v", svc)
	}

	compose, err := loadCompose(ctx, "services:\n  db:\n    image: postgres\n", nil, dir, "nagi-p-umami")
	if err != nil {
		t.Fatal(err)
	}
	ov, err = buildOverride(overrideInput{proj: compose, mp: mp, projectID: "p", container: "umami", version: 1, intranet: true, subnet: "10.200.0.64/26"})
	if err != nil {
		t.Fatal(err)
	}
	nets = ov["networks"].(map[string]any)
	if _, ok := nets["default"]; !ok || nets["nagi"] != nil {
		t.Fatalf("compose container networks: %v", nets)
	}
	if ov["services"].(map[string]any)["db"].(map[string]any)["networks"] != nil {
		t.Fatal("compose container must not join the project network in compose")
	}
}

func TestUnmanagedVolumes(t *testing.T) {
	text := `services:
  app:
    image: nginx
    network_mode: host
    volumes: [data:/a, ext:/b, nfs:/c]
  db:
    image: postgres
volumes:
  data: {}
  ext:
    external: true
  nfs:
    driver_opts: {type: nfs, o: "addr=10.0.0.1", device: ":/x"}
`
	cp, err := loadCompose(context.Background(), text, nil, t.TempDir(), "nagi-p-m")
	if err != nil {
		t.Fatal(err)
	}
	if got := unmanagedVolumes(cp); len(got) != 2 || got[0] != "ext" || got[1] != "nfs" {
		t.Fatalf("unmanaged volumes: %v", got)
	}
	if got := hostNetworkServices(cp); len(got) != 1 || got[0] != "app" {
		t.Fatalf("host network services: %v", got)
	}
}

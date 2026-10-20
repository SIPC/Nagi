package spec

import (
	"context"
	"strings"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

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
	proj, err := LoadCompose(context.Background(), text, nil, t.TempDir(), "nagi-test")
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
	proj, err := Validate(ctx, ok, "web", nil, dir, "nagi-test")
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
		"local git":     {Compose: ok.Compose, Source: &nagiv1.Source{GitUrl: "/srv/repo@x:y"}},
	}
	for name, cfg := range cases {
		if _, err := Validate(ctx, cfg, "web", nil, dir, "nagi-test"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// 共享变量可以在 compose 里引用，同名时以容器自己的变量为准。
func TestSharedEnv(t *testing.T) {
	ctx := context.Background()
	shared := []*nagiv1.EnvVar{{Key: "TAG", Value: "1"}, {Key: "DB_PASS", Value: "x"}}
	cfg := &nagiv1.ContainerConfig{Compose: "services:\n  api:\n    image: app:${TAG}\n    environment:\n      P: ${DB_PASS}\n"}
	proj, err := Validate(ctx, cfg, "api", shared, t.TempDir(), "nagi-test")
	if err != nil {
		t.Fatal(err)
	}
	if proj.Services["api"].Image != "app:1" {
		t.Fatalf("shared: %q", proj.Services["api"].Image)
	}
	cfg.Env = []*nagiv1.EnvVar{{Key: "TAG", Value: "2"}}
	if proj, _ = Validate(ctx, cfg, "api", shared, t.TempDir(), "nagi-test"); proj.Services["api"].Image != "app:2" {
		t.Fatalf("container should win: %q", proj.Services["api"].Image)
	}
	// TAG 被容器自己的变量覆盖，改共享的 TAG 不影响它；改 DB_PASS 影响。
	changedTag := []*nagiv1.EnvVar{{Key: "TAG", Value: "9"}, {Key: "DB_PASS", Value: "x"}}
	changedPass := []*nagiv1.EnvVar{{Key: "TAG", Value: "1"}, {Key: "DB_PASS", Value: "y"}}
	if SharedEnvChanged(cfg, "api", shared, changedTag) {
		t.Fatal("TAG is overridden by the container")
	}
	if !SharedEnvChanged(cfg, "api", shared, changedPass) {
		t.Fatal("DB_PASS change should be noticed")
	}
	other := &nagiv1.ContainerConfig{Compose: "services:\n  web:\n    image: nginx\n"}
	if SharedEnvChanged(other, "web", shared, changedPass) {
		t.Fatal("web does not reference DB_PASS")
	}
}

func TestContainerName(t *testing.T) {
	for _, ok := range []string{"web", "api-v2", "a", "db1"} {
		if CheckContainerName(ok) != nil {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Web", "-web", "web-", "a_b", "a.b", "abcdefghijabcdefghijabcdefghijabc"} {
		if CheckContainerName(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
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
	cp, err := LoadCompose(context.Background(), text, nil, t.TempDir(), "nagi-p-m")
	if err != nil {
		t.Fatal(err)
	}
	if got := UnmanagedVolumes(cp); len(got) != 2 || got[0] != "ext" || got[1] != "nfs" {
		t.Fatalf("unmanaged volumes: %v", got)
	}
	if got := HostNetworkServices(cp); len(got) != 1 || got[0] != "app" {
		t.Fatalf("host network services: %v", got)
	}
}

// 表单里的命令原样交给容器里的 shell，$ 不能被 compose 当成变量替换掉。
func TestFormCommandKeepsDollar(t *testing.T) {
	cfg := &nagiv1.ContainerConfig{Form: &nagiv1.ImageForm{Image: "alpine", Command: `sh -c 'echo $HOME ${USER}'`, HealthCommand: "test -n $HOME"}}
	cp, err := Load(context.Background(), cfg, "app", nil, t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	svc := cp.Services["app"]
	if got := strings.Join(svc.Command, " "); !strings.Contains(got, "echo $HOME ${USER}") {
		t.Fatalf("command = %q", got)
	}
	if got := strings.Join(svc.HealthCheck.Test, " "); !strings.Contains(got, "test -n $HOME") {
		t.Fatalf("healthcheck = %q", got)
	}
}

func TestFormLimitsAndHealth(t *testing.T) {
	cfg := &nagiv1.ContainerConfig{Form: &nagiv1.ImageForm{
		Image: "nginx:alpine", MemoryMb: 256, Cpus: 0.5, HealthCommand: "wget -qO- localhost", HealthIntervalSeconds: 15,
	}}
	proj, err := Validate(context.Background(), cfg, "app", nil, t.TempDir(), "nagi-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := proj.Services["app"]
	if svc.MemLimit != 256<<20 || svc.CPUS != 0.5 {
		t.Fatalf("资源限制不对：mem=%d cpus=%v", svc.MemLimit, svc.CPUS)
	}
	if svc.HealthCheck == nil || len(svc.HealthCheck.Test) != 2 || svc.HealthCheck.Test[1] != "wget -qO- localhost" ||
		svc.HealthCheck.Interval == nil || time.Duration(*svc.HealthCheck.Interval) != 15*time.Second {
		t.Fatalf("健康检查不对：%+v", svc.HealthCheck)
	}
}

func TestInternalPorts(t *testing.T) {
	got, err := CleanInternalPorts([]string{" pg:5432 ", "db.umami:3306", "pg:5432", ""})
	if err != nil || strings.Join(got, ",") != "pg:5432,db.umami:3306" {
		t.Fatalf("clean: %v %v", got, err)
	}
	for _, bad := range []string{"pg", "pg:0", "pg:70000", "PG:1", "a.b.c:1"} {
		if _, err := CleanInternalPorts([]string{bad}); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if m := InternalPortMap(got); len(m["pg"]) != 1 || m["db.umami"][0] != 3306 {
		t.Fatalf("map: %v", m)
	}
}

// compose 里的端口：网站端口按 http，其余原样；没写对外端口的跳过。
func TestComposePorts(t *testing.T) {
	cp, err := LoadCompose(context.Background(),
		"services:\n  web:\n    image: x\n    ports: [\"38080:80\", \"35353:53/udp\", \"9000\"]\n  db:\n    image: y\n    network_mode: host\n    ports: [\"5432:5432\"]\n",
		nil, t.TempDir(), "nagi-p-web")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ComposePort{}
	for _, p := range ComposePorts("web", cp) {
		got[p.Key] = p
	}
	if len(got) != 3 {
		t.Fatalf("ports: %v", got)
	}
	if p := got["web/80/tcp"]; p.Skip != "" || p.Route.GetProtocol() != ProtoHTTP || !p.Guessed || p.Route.GetPublished() != 38080 {
		t.Fatalf("http: %+v", p)
	}
	if p := got["web/53/udp"]; p.Skip != "" || p.Route.GetProtocol() != ProtoUDP {
		t.Fatalf("udp: %+v", p)
	}
	if p := got["web/9000/tcp"]; p.Skip == "" {
		t.Fatalf("no published port should be skipped: %+v", p)
	}
	if NewRouteName(got["web/80/tcp"].Route, map[string]bool{"web-80": true}) != "web-80-2" {
		t.Fatal("route name")
	}
}

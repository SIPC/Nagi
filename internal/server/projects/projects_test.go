package projects

import (
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
)

// 一起部署时按依赖排序：被依赖的先部署，其余按创建顺序；有循环时报错。
func TestOrderJobs(t *testing.T) {
	mk := func(name string, pos int64, deps ...string) job {
		return job{c: store.Container{Name: name, Position: pos}, cfg: &nagiv1.ContainerConfig{DependsOn: deps}}
	}
	got, err := orderJobs([]job{mk("web", 1, "api"), mk("api", 2, "db"), mk("db", 3), mk("cron", 4)})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, j := range got {
		names = append(names, j.c.Name)
	}
	if strings.Join(names, ",") != "db,api,web,cron" {
		t.Fatalf("order: %v", names)
	}
	if _, err := orderJobs([]job{mk("a", 1, "b"), mk("b", 2, "a")}); err == nil {
		t.Fatal("cycle should fail")
	}
}

func TestDefaultNetworkName(t *testing.T) {
	cases := map[string]string{
		"Echo Server":           "echo-server",
		"商城 Shop 2":             "shang-cheng-shop-2",
		"全中文":                   "quan-zhong-wen",
		"!!!":                   "",
		"--a__b--":              "a-b",
		strings.Repeat("x", 40): strings.Repeat("x", 32),
	}
	for in, want := range cases {
		if got := DefaultNetworkName(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

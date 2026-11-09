package api

import (
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 操作员沿用已有配置时，现有目录和快照子目录也要不变（规范化以后比较）。
func TestSameDirs(t *testing.T) {
	cfg := func(dir string, paths ...string) *nagiv1.ContainerConfig {
		return &nagiv1.ContainerConfig{ProjectDir: dir, BackupPaths: paths}
	}
	cases := []struct {
		a, b *nagiv1.ContainerConfig
		want bool
	}{
		{cfg(""), cfg(""), true},
		{cfg(""), cfg("/run"), false},
		{cfg("/opt/app"), cfg(" /opt/app/ "), true},
		{cfg("/opt/app"), cfg("/opt/other"), false},
		{cfg("/opt/app", "db", "media"), cfg("/opt/app", "media/", "db"), true},
		{cfg("/opt/app", "db"), cfg("/opt/app", "db", "media"), false},
		{cfg("/opt/app"), cfg("relative"), false},
	}
	for i, c := range cases {
		if got := sameDirs(c.a, c.b); got != c.want {
			t.Errorf("%d: sameDirs = %v, want %v", i, got, c.want)
		}
	}
}

// 沿用有特权的配置时，容器变量和服务级变量都要不变。
func TestSameVars(t *testing.T) {
	env := func(kv ...string) []*nagiv1.EnvVar {
		var out []*nagiv1.EnvVar
		for i := 0; i < len(kv); i += 2 {
			out = append(out, &nagiv1.EnvVar{Key: kv[i], Value: kv[i+1]})
		}
		return out
	}
	base := &nagiv1.ContainerConfig{Env: env("A", "1"), ServiceEnv: map[string]*nagiv1.EnvList{"web": {Vars: env("B", "2")}}}
	cases := []struct {
		cfg  *nagiv1.ContainerConfig
		want bool
	}{
		{&nagiv1.ContainerConfig{Env: env("A", "1"), ServiceEnv: map[string]*nagiv1.EnvList{"web": {Vars: env("B", "2")}}}, true},
		{&nagiv1.ContainerConfig{Env: env("A", "2"), ServiceEnv: map[string]*nagiv1.EnvList{"web": {Vars: env("B", "2")}}}, false},
		{&nagiv1.ContainerConfig{Env: env("A", "1"), ServiceEnv: map[string]*nagiv1.EnvList{"web": {Vars: env("B", "2", "LD_PRELOAD", "/x.so")}}}, false},
		{&nagiv1.ContainerConfig{Env: env("A", "1"), ServiceEnv: map[string]*nagiv1.EnvList{"db": {Vars: env("B", "2")}}}, false},
		{&nagiv1.ContainerConfig{Env: env("A", "1")}, false},
	}
	for i, c := range cases {
		if got := sameVars(base, c.cfg); got != c.want {
			t.Errorf("%d: sameVars = %v, want %v", i, got, c.want)
		}
	}
}

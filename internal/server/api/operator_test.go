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

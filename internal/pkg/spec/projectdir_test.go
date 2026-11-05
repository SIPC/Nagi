package spec

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 现有目录：相对路径以这个目录为准，env_file 只记路径不读文件（面板上没有这个文件），只有管理员能设置。
func TestProjectDir(t *testing.T) {
	cfg := &nagiv1.ContainerConfig{ProjectDir: "/opt/immich/", Env: []*nagiv1.EnvVar{{Key: "UPLOAD_LOCATION", Value: "./library"}},
		Compose: "services:\n  server:\n    image: nginx\n    env_file: .env\n    volumes:\n      - ${UPLOAD_LOCATION}:/data\n      - ./postgres:/pg\n"}
	proj, err := Validate(context.Background(), cfg, "immich", nil, "/srv/nagi/x/immich/data", "check")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GetProjectDir() != "/opt/immich" {
		t.Errorf("project dir not cleaned: %q", cfg.GetProjectDir())
	}
	var sources []string
	for _, v := range proj.Services["server"].Volumes {
		sources = append(sources, filepath.ToSlash(strings.TrimPrefix(v.Source, filepath.VolumeName(v.Source))))
	}
	if !slices.Contains(sources, "/opt/immich/library") || !slices.Contains(sources, "/opt/immich/postgres") {
		t.Errorf("relative paths should resolve in the project dir: %v", sources)
	}
	if reasons, _ := PrivilegeReasons(cfg, "immich"); !slices.Contains(reasons, "project_dir") {
		t.Errorf("project_dir should need an administrator: %v", reasons)
	}
	for _, bad := range []string{"relative/dir", "/", "/opt/$X"} {
		if _, err := CleanProjectDir(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	single := &nagiv1.ContainerConfig{ProjectDir: "/opt/x", Form: &nagiv1.ImageForm{Image: "nginx"}}
	if _, err := Validate(context.Background(), single, "x", nil, "/d", "check"); err == nil {
		t.Error("an image-form container cannot use a project dir")
	}
}

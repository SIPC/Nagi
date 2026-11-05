package project

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"gopkg.in/yaml.v3"
)

// 现有目录：compose 里的 env_file: .env 改读面板的变量文件（!override 整个替换），别的 env_file 照旧。
func TestOverrideProjectDirEnvFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	projectDir := filepath.Join(root, "immich")
	mp := containerPaths{root: filepath.Join(root, "nagi")}
	cfg := &nagiv1.ContainerConfig{ProjectDir: projectDir,
		Compose: "services:\n  server:\n    image: nginx\n    env_file:\n      - .env\n      - extra.env\n  redis:\n    image: redis\n"}
	proj, err := spec.Load(ctx, cfg, "immich", nil, mp.dataDir(), "nagi-p-immich")
	if err != nil {
		t.Fatal(err)
	}
	ov, err := buildOverride(overrideInput{proj: proj, mp: mp, projectID: "p", container: "immich", version: 1, subnet: "10.200.0.64/26", projectDir: projectDir})
	if err != nil {
		t.Fatal(err)
	}
	svc := ov["services"].(map[string]any)["server"].(map[string]any)
	node, ok := svc["env_file"].(*yaml.Node)
	if !ok || node.Tag != "!override" || len(node.Content) != 2 {
		t.Fatalf("env_file = %#v", svc["env_file"])
	}
	if node.Content[0].Value != filepath.ToSlash(mp.envFile()) || node.Content[1].Value != filepath.ToSlash(filepath.Join(projectDir, "extra.env")) {
		t.Fatalf("env_file = %s, %s", node.Content[0].Value, node.Content[1].Value)
	}
	if _, ok := ov["services"].(map[string]any)["redis"].(map[string]any)["env_file"]; ok {
		t.Fatal("a service without env_file must not get one")
	}
}

package api

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 导入时，命名卷改成引用原来那个项目的卷；找不到原卷的保持原样，已经是 external 的不动。
func TestExternalVolumes(t *testing.T) {
	text := "services:\n  ml:\n    image: x\n    volumes:\n      - model-cache:/cache\nvolumes:\n  model-cache:\n  fresh: {}\n  ext:\n    external: true\n"
	out, notes, err := externalVolumes(context.Background(), text, map[string]string{"model-cache": "immich_model-cache", "ext": "other"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Volumes map[string]map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if v := doc.Volumes["model-cache"]; v["external"] != true || v["name"] != "immich_model-cache" {
		t.Errorf("model-cache = %v", v)
	}
	if v := doc.Volumes["fresh"]; v["external"] != nil {
		t.Errorf("fresh = %v", v)
	}
	if v := doc.Volumes["ext"]; v["name"] != nil {
		t.Errorf("ext = %v", v)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "immich_model-cache") {
		t.Errorf("notes = %v", notes)
	}
	if !strings.Contains(out, "model-cache:/cache") {
		t.Errorf("services changed: %s", out)
	}
}

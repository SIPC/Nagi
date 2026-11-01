package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
)

func TestVolumesFromCompose(t *testing.T) {
	mp := containerPaths{root: t.TempDir()}
	app := types.ServiceConfig{Name: "app"}
	app.Volumes = []types.ServiceVolumeConfig{
		{Type: types.VolumeTypeVolume, Source: "db", Target: "/var/lib/db"},
		{Type: types.VolumeTypeVolume, Source: "nfs", Target: "/mnt/nfs"},
		{Type: types.VolumeTypeBind, Source: filepath.Join(mp.dataDir(), "conf"), Target: "/etc/app"},
		{Type: types.VolumeTypeBind, Source: "/etc/passwd", Target: "/host"},
	}
	cp := &types.Project{
		Volumes:  types.Volumes{"db": {}, "nfs": {Driver: "nfs"}},
		Services: types.Services{"app": app},
	}
	got := map[string]*nagiv1.VolumeInfo{}
	for _, v := range volumesFromCompose(cp, mp) {
		got[v.GetName()] = v
	}
	if v := got["db"]; v == nil || v.GetKind() != "managed" || len(v.GetMounts()) != 1 || v.GetMounts()[0].GetTarget() != "/var/lib/db" {
		t.Errorf("db = %v", v)
	}
	if v := got["nfs"]; v == nil || v.GetKind() != "unmanaged" || v.GetHostPath() != "" {
		t.Errorf("nfs = %v", v)
	}
	if v := got[DataVolume]; v == nil || v.GetKind() != "data" || len(v.GetMounts()) != 1 || v.GetMounts()[0].GetTarget() != "/etc/app" {
		t.Errorf("data = %v", v)
	}
}

func TestMeasureVolume(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "f"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "g"), []byte("123"), 0o644); err != nil {
		t.Fatal(err)
	}
	v := &nagiv1.VolumeInfo{HostPath: dir}
	measureVolume(context.Background(), v)
	if v.GetSizeBytes() != 8 || v.GetFiles() != 2 || v.GetModified() == nil || v.GetPartial() {
		t.Errorf("v = %v", v)
	}
	// 目录不存在时不报错，都是 0。
	missing := &nagiv1.VolumeInfo{HostPath: filepath.Join(dir, "nope")}
	measureVolume(context.Background(), missing)
	if missing.GetSizeBytes() != 0 {
		t.Errorf("missing = %v", missing)
	}
}

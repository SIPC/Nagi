package project

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"site/index.html":       "hi",
		"site/../../escape.txt": "nope",
		"/abs/x.txt":            "abs",
		"__MACOSX/site/._a":     "junk",
	} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	h := &zip.FileHeader{Name: "site/link"}
	h.SetMode(os.ModeSymlink | 0o777)
	w, _ := zw.CreateHeader(h)
	_, _ = w.Write([]byte("/etc/passwd"))
	_ = zw.Close()
	archive := filepath.Join(dir, "a.zip")
	_ = os.WriteFile(archive, buf.Bytes(), 0o600)

	out := filepath.Join(dir, "out")
	n, err := extract(archive, out)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("应该解出 3 个文件，实际 %d", n)
	}
	// ".." 在根目录处截止，绝对路径放到 out 下面，都不会跑出 out。
	for _, p := range []string{"site/index.html", "escape.txt", "abs/x.txt"} {
		if _, err := os.Stat(filepath.Join(out, p)); err != nil {
			t.Errorf("缺少 %s", p)
		}
	}
	if _, err := os.Lstat(filepath.Join(out, "site", "link")); err == nil {
		t.Error("符号链接应该跳过")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err == nil {
		t.Error("文件跑到了解压目录外面")
	}
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "dist/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "dist/run.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: 2})
	_, _ = tw.Write([]byte("ok"))
	_ = tw.Close()
	_ = gz.Close()
	archive := filepath.Join(dir, "a.tgz")
	_ = os.WriteFile(archive, buf.Bytes(), 0o600)
	out := filepath.Join(dir, "out")
	if n, err := extract(archive, out); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(out, "dist", "run.sh")); err != nil {
		t.Fatal(err)
	}

	_ = os.WriteFile(archive, []byte("not an archive"), 0o600)
	if _, err := extract(archive, filepath.Join(dir, "bad")); err == nil {
		t.Fatal("不认识的格式应该报错")
	}
}

func TestFormLimitsAndHealth(t *testing.T) {
	cfg := &nagiv1.ContainerConfig{Form: &nagiv1.ImageForm{
		Image: "nginx:alpine", MemoryMb: 256, Cpus: 0.5, HealthCommand: "wget -qO- localhost", HealthIntervalSeconds: 15,
	}}
	proj, err := validate(context.Background(), cfg, "app", nil, t.TempDir(), "nagi-test")
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
	if !likelyHTTP(8080) || likelyHTTP(9000) || likelyHTTP(5432) {
		t.Fatal("端口猜测不对")
	}
}

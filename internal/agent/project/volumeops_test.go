package project

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/agent/fileops"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// volumeFixture 建一个只有缓存配置的容器，data/ 就是 data 返回的目录。
type volumeFixture struct {
	m    *Manager
	dir  string
	data string
}

func newVolumeFixture(t *testing.T) *volumeFixture {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	m, err := NewManager(d, nil, DefaultSubnetPool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := m.store(ctx, &nagiv1.ContainerSpec{ProjectId: "p1", Name: "web", Dir: dir, Config: &nagiv1.ContainerConfig{}}); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	return &volumeFixture{m: m, dir: dir, data: data}
}

func (f *volumeFixture) put(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(f.data, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *volumeFixture) get(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.data, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取 %s：%v", rel, err)
	}
	return string(b)
}

func (f *volumeFixture) missing(t *testing.T, rel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(f.data, filepath.FromSlash(rel))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s 不应该存在", rel)
	}
}

// stagingEmpty 确认 .staging 里没有留下操作的临时目录。
func (f *volumeFixture) stagingEmpty(t *testing.T) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(f.dir, ".staging"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "op-") {
			t.Fatalf("暂存目录里留下了 %s", e.Name())
		}
	}
	walk, _ := os.ReadDir(f.data)
	for _, e := range walk {
		if strings.HasPrefix(e.Name(), ".nagi-tmp-") {
			t.Fatalf("卷里留下了临时文件 %s", e.Name())
		}
	}
}

func ok() error { return nil }

func (f *volumeFixture) transfer(t *testing.T, req *nagiv1.VolumeTransferRequest) (*nagiv1.FileOpResult, error) {
	t.Helper()
	req.ProjectId, req.Container, req.Volume = "p1", "web", DataVolume
	return f.m.TransferVolumeFiles(context.Background(), req, nil, ok)
}

func TestTransferVolumeMoveAndRename(t *testing.T) {
	f := newVolumeFixture(t)
	f.put(t, "a.txt", "a")
	f.put(t, "dir/b.txt", "b")
	f.put(t, "dir/sub/c.txt", "c")

	if _, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"a.txt"}, NewName: "renamed.txt"}); err != nil {
		t.Fatal(err)
	}
	f.missing(t, "a.txt")
	if f.get(t, "renamed.txt") != "a" {
		t.Fatal("改名后的内容不对")
	}
	res, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"renamed.txt", "dir/sub"}, DestDir: "dir"})
	if paths := slices.Sorted(slices.Values(res.GetPaths())); err != nil || !slices.Equal(paths, []string{"dir/renamed.txt", "dir/sub"}) {
		t.Fatalf("移动：%v %v", res, err)
	}
	if f.get(t, "dir/renamed.txt") != "a" || f.get(t, "dir/sub/c.txt") != "c" {
		t.Fatal("移动后的内容不对")
	}
	// 同名时先问。
	f.put(t, "b.txt", "other")
	res, err = f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"b.txt"}, DestDir: "dir"})
	if err != nil || !slices.Equal(res.GetConflicts(), []string{"b.txt"}) || f.get(t, "b.txt") != "other" {
		t.Fatalf("同名时应该只返回冲突：%v %v", res, err)
	}
	res, err = f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"b.txt"}, DestDir: "dir", Conflict: fileops.ConflictRename})
	if err != nil || !slices.Equal(res.GetPaths(), []string{"dir/b (2).txt"}) || f.get(t, "dir/b.txt") != "b" {
		t.Fatalf("保留两者：%v %v", res, err)
	}
	// 目录不能移到自己里面。
	if _, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"dir"}, DestDir: "dir/sub"}); err == nil {
		t.Fatal("目录不能移到自己里面")
	}
	// 路径走不出卷。
	for _, req := range []*nagiv1.VolumeTransferRequest{
		{Paths: []string{"../x"}},
		{Paths: []string{"dir/b.txt"}, DestDir: "../.."},
		{Paths: []string{"dir/b.txt"}, NewName: "../escape"},
		{Paths: []string{"dir/b.txt"}, NewName: "a/b"},
		{Paths: []string{""}},
	} {
		if _, err := f.transfer(t, req); err == nil {
			t.Fatalf("%v 应该被拒绝", req)
		}
	}
}

func TestTransferVolumeCopy(t *testing.T) {
	f := newVolumeFixture(t)
	f.put(t, "site/index.html", "hi")
	f.put(t, "site/css/a.css", "css")
	res, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"site"}, Copy: true})
	if err != nil || !slices.Equal(res.GetPaths(), []string{"site (2)"}) {
		t.Fatalf("复制到原来的目录应该保留两者：%v %v", res, err)
	}
	if f.get(t, "site (2)/css/a.css") != "css" || f.get(t, "site/index.html") != "hi" {
		t.Fatal("复制的内容不对")
	}
	if err := os.Mkdir(filepath.Join(f.data, "backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"site/index.html"}, DestDir: "backup", Copy: true}); err != nil {
		t.Fatal(err)
	}
	f.put(t, "site/index.html", "new")
	if _, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"site/index.html"}, DestDir: "backup", Copy: true, Conflict: fileops.ConflictOverwrite}); err != nil {
		t.Fatal(err)
	}
	if f.get(t, "backup/index.html") != "new" {
		t.Fatal("覆盖后的内容不对")
	}
	res, err = f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"site/index.html"}, DestDir: "backup", Copy: true, Conflict: fileops.ConflictSkip})
	if err != nil || res.GetSkipped() != 1 {
		t.Fatalf("跳过：%v %v", res, err)
	}
	f.stagingEmpty(t)
}

func TestTransferVolumeSymlinks(t *testing.T) {
	f := newVolumeFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.put(t, "d/a", "a")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(f.data, "d", "link")); err != nil {
		t.Skipf("无法创建符号链接：%v", err)
	}
	if err := os.Symlink(outside, filepath.Join(f.data, "out")); err != nil {
		t.Fatal(err)
	}
	res, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"d", "out"}, NewName: "", DestDir: "", Copy: true, Conflict: fileops.ConflictRename})
	if err != nil || res.GetSkippedLinks() != 2 {
		t.Fatalf("复制应该跳过符号链接：%v %v", res, err)
	}
	f.missing(t, "d (2)/link")
	f.missing(t, "out (2)")
	// 经过指向卷外的链接：拒绝。
	if _, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"d/a"}, DestDir: "out", Copy: true}); err == nil {
		t.Fatal("目标经过指向卷外的链接应该拒绝")
	}
	if _, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"out/secret"}, Copy: true}); err == nil {
		t.Fatal("源经过指向卷外的链接应该拒绝")
	}
	// 移动链接只移动链接本身。
	if _, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"out"}, NewName: "moved"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(f.data, "moved")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("移动的应该是链接本身")
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Fatal("卷外的文件不应该被动")
	}
}

func TestVolumeOpsRefuseWhileLocked(t *testing.T) {
	f := newVolumeFixture(t)
	f.put(t, "a", "a")
	l := f.m.lock("p1", "web")
	l.Lock()
	defer l.Unlock()
	if _, err := f.transfer(t, &nagiv1.VolumeTransferRequest{Paths: []string{"a"}, NewName: "b"}); err == nil {
		t.Fatal("部署进行中应该拒绝")
	}
}

func TestVolumeOpsLeaseLost(t *testing.T) {
	f := newVolumeFixture(t)
	f.put(t, "a", "a")
	lost := func() error { return errors.New("容器当前由另一节点运行") }
	req := &nagiv1.VolumeTransferRequest{ProjectId: "p1", Container: "web", Volume: DataVolume, Paths: []string{"a"}, Copy: true}
	if _, err := f.m.TransferVolumeFiles(context.Background(), req, nil, lost); err == nil {
		t.Fatal("租约不在本节点时应该放弃")
	}
	f.missing(t, "a (2)")
	f.stagingEmpty(t)
}

func TestCompressAndExtractVolume(t *testing.T) {
	for _, format := range []string{fileops.FormatZip, fileops.FormatTarGz} {
		t.Run(format, func(t *testing.T) {
			f := newVolumeFixture(t)
			f.put(t, "site/index.html", "hi")
			f.put(t, "site/js/app.js", "js")
			f.put(t, "notes.txt", "n")
			dest := "out" + fileops.ArchiveExt(format)
			creq := &nagiv1.VolumeCompressRequest{ProjectId: "p1", Container: "web", Volume: DataVolume, Paths: []string{"site", "notes.txt"}, Dest: dest, Format: format}
			if _, err := f.m.CompressVolumeFiles(context.Background(), creq, nil, ok); err != nil {
				t.Fatal(err)
			}
			res, err := f.m.CompressVolumeFiles(context.Background(), creq, nil, ok)
			if err != nil || !slices.Equal(res.GetConflicts(), []string{dest}) {
				t.Fatalf("压缩包已存在时应该先问：%v %v", res, err)
			}
			// 解压到以压缩包命名的新目录。
			ereq := &nagiv1.VolumeExtractRequest{ProjectId: "p1", Container: "web", Volume: DataVolume, Path: dest, DestDir: "out", IntoNewDir: true}
			if _, err := f.m.ExtractVolumeFile(context.Background(), ereq, nil, ok); err != nil {
				t.Fatal(err)
			}
			if f.get(t, "out/site/js/app.js") != "js" || f.get(t, "out/notes.txt") != "n" {
				t.Fatal("解压的内容不对")
			}
			res, err = f.m.ExtractVolumeFile(context.Background(), ereq, nil, ok)
			if err != nil || !slices.Equal(res.GetConflicts(), []string{"out"}) {
				t.Fatalf("目录已存在时应该先问：%v %v", res, err)
			}
			ereq.Conflict = fileops.ConflictRename
			if res, err = f.m.ExtractVolumeFile(context.Background(), ereq, nil, ok); err != nil || !slices.Equal(res.GetPaths(), []string{"out (2)"}) {
				t.Fatalf("保留两者：%v %v", res, err)
			}
			// 解压到当前目录：顶层的每一项分别查同名。
			here := &nagiv1.VolumeExtractRequest{ProjectId: "p1", Container: "web", Volume: DataVolume, Path: dest, DestDir: ""}
			res, err = f.m.ExtractVolumeFile(context.Background(), here, nil, ok)
			if err != nil || len(res.GetConflicts()) != 2 {
				t.Fatalf("应该有两项同名：%v %v", res, err)
			}
			f.put(t, "notes.txt", "changed")
			here.Conflict = fileops.ConflictOverwrite
			if _, err := f.m.ExtractVolumeFile(context.Background(), here, nil, ok); err != nil {
				t.Fatal(err)
			}
			if f.get(t, "notes.txt") != "n" {
				t.Fatal("覆盖后应该是压缩包里的内容")
			}
			f.stagingEmpty(t)
		})
	}
}

func TestExtractVolumeZipSlip(t *testing.T) {
	f := newVolumeFixture(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"good.txt", "../../evil.txt"} {
		w, _ := zw.Create(n)
		w.Write([]byte("x"))
	}
	zw.Close()
	f.put(t, "slip.zip", buf.String())
	req := &nagiv1.VolumeExtractRequest{ProjectId: "p1", Container: "web", Volume: DataVolume, Path: "slip.zip", DestDir: "slip", IntoNewDir: true}
	if _, err := f.m.ExtractVolumeFile(context.Background(), req, nil, ok); err == nil || !strings.Contains(err.Error(), "不安全的路径") {
		t.Fatalf("应该拒绝：%v", err)
	}
	f.missing(t, "slip")
	f.missing(t, "good.txt")
	if _, err := os.Stat(filepath.Join(f.dir, "evil.txt")); err == nil {
		t.Fatal("写到了卷外面")
	}
	f.stagingEmpty(t)
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestVolumeChunkedUpload(t *testing.T) {
	f := newVolumeFixture(t)
	ctx := context.Background()
	content := bytes.Repeat([]byte("0123456789"), 3000)
	ref := &nagiv1.VolumeFileRef{ProjectId: "p1", Container: "web", Volume: DataVolume, Path: "up/big.bin"}
	info, err := f.m.BeginVolumeUpload(ctx, ref, int64(len(content)), "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.AppendVolumeUpload(ctx, ref, info.ID, 0, hexSum(content[:20000]), bytes.NewReader(content[:20000])); err != nil {
		t.Fatal(err)
	}
	// 断开后重新开始：接着传。
	again, err := f.m.BeginVolumeUpload(ctx, ref, int64(len(content)), "k")
	if err != nil || again.ID != info.ID || again.Offset != 20000 {
		t.Fatalf("应该接着传：%+v %v", again, err)
	}
	// 别的目标不能借用这个上传。
	other := &nagiv1.VolumeFileRef{ProjectId: "p1", Container: "web", Volume: DataVolume, Path: "other.bin"}
	if _, err := f.m.AppendVolumeUpload(ctx, other, info.ID, 20000, hexSum(content[20000:]), bytes.NewReader(content[20000:])); err == nil {
		t.Fatal("目标不一致应该拒绝")
	}
	if _, err := f.m.AppendVolumeUpload(ctx, ref, info.ID, 20000, hexSum(content[20000:]), bytes.NewReader(content[20000:])); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.CommitVolumeUpload(ctx, ref, info.ID, hexSum([]byte("bad")), ok); err == nil {
		t.Fatal("哈希不对应该拒绝")
	}
	f.missing(t, "up/big.bin")
	// 重新传一遍，提交成功。
	info, _ = f.m.BeginVolumeUpload(ctx, ref, int64(len(content)), "k")
	if info.Offset != 0 {
		t.Fatal("作废的上传应该从头开始")
	}
	if _, err := f.m.AppendVolumeUpload(ctx, ref, info.ID, 0, hexSum(content), bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	n, err := f.m.CommitVolumeUpload(ctx, ref, info.ID, hexSum(content), ok)
	if err != nil || n != int64(len(content)) || f.get(t, "up/big.bin") != string(content) {
		t.Fatalf("提交：%d %v", n, err)
	}
	entries, _ := os.ReadDir(filepath.Join(f.dir, ".staging", "uploads"))
	if len(entries) != 0 {
		t.Fatal("提交以后应该删掉暂存")
	}
}

func TestVolumeDirSizes(t *testing.T) {
	f := newVolumeFixture(t)
	f.put(t, "a/1", "12345")
	f.put(t, "a/b/2", "12")
	f.put(t, "c/3", "1")
	res, err := f.m.ListVolumeDirSizes(context.Background(), &nagiv1.VolumeDirSizesRequest{ProjectId: "p1", Container: "web", Volume: DataVolume, Names: []string{"a", "c"}})
	if err != nil || res.GetSizes()[0].GetSize() != 7 || res.GetSizes()[1].GetSize() != 1 || !res.GetSizes()[0].GetComplete() {
		t.Fatalf("目录大小：%v %v", res, err)
	}
	if _, err := f.m.ListVolumeDirSizes(context.Background(), &nagiv1.VolumeDirSizesRequest{ProjectId: "p1", Container: "web", Volume: DataVolume, Names: []string{"../.."}}); err == nil {
		t.Fatal("名字不能带路径")
	}
}

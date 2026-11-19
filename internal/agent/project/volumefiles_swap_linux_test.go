package project

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"golang.org/x/sys/unix"
)

// makeSpecialFile 在 p 建一个设备文件（kind 为 "device"，和 /dev/zero 一样是 c 1 5）或管道（"fifo"）。
// 没有 CAP_MKNOD 时跳过。
func makeSpecialFile(t *testing.T, kind, p string) {
	t.Helper()
	var err error
	if kind == "device" {
		err = unix.Mknod(p, unix.S_IFCHR|0o666, int(unix.Mkdev(1, 5)))
		if errors.Is(err, unix.EPERM) {
			t.Skip("没有 CAP_MKNOD，建不了设备文件")
		}
	} else {
		err = unix.Mkfifo(p, 0o666)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// swapFiles 像恶意容器那样不停原子地交换 a 和 b，直到测试结束。
func swapFiles(t *testing.T, a, b string) {
	t.Helper()
	if err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE); err != nil {
		t.Skipf("文件系统不支持 RENAME_EXCHANGE：%v", err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
		}
	}()
	t.Cleanup(func() {
		stop.Store(true)
		wg.Wait()
	})
}

// 容器在 Lstat 和打开之间把普通文件换成设备文件或管道（v0.35.0#2、#18）：
// 下载、打包下载、保存前核对都不能读出设备的内容、读出比原文件多的字节，也不能卡住。
func TestVolumeReadsRejectSwappedSpecial(t *testing.T) {
	for _, kind := range []string{"device", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			f := newVolumeFixture(t)
			want := strings.Repeat("A", 4096)
			f.put(t, "d/f", want)
			makeSpecialFile(t, kind, filepath.Join(f.data, "d", "x"))
			swapFiles(t, filepath.Join(f.data, "d", "f"), filepath.Join(f.data, "d", "x"))
			sum := sha256.Sum256([]byte(want))
			check := UploadCheck{ExpectedSHA256: hex.EncodeToString(sum[:])}
			root, err := os.OpenRoot(f.data)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()

			var served, rejected atomic.Int64
			done := make(chan struct{})
			go func() {
				defer close(done)
				for i := range 400 {
					ref := &nagiv1.VolumeFileRef{ProjectId: "p1", Container: "web", Volume: DataVolume, Path: "d/f"}
					if i%2 == 1 {
						ref.Length = 1 << 20 // 范围比文件大
					}
					var head *nagiv1.VolumeFileChunk
					var got []byte
					err := f.m.ReadVolumeFile(t.Context(), ref, func(ch *nagiv1.VolumeFileChunk) error {
						if head == nil {
							head = ch
						} else {
							got = append(got, ch.GetData()...)
						}
						return nil
					})
					if err != nil {
						rejected.Add(1)
						continue
					}
					served.Add(1)
					if head.GetSize() != int64(len(want)) || string(got) != want {
						t.Errorf("第 %d 次下载读到 %d 字节（size=%d），不是原文件", i, len(got), head.GetSize())
						return
					}
				}
				for i := range 100 {
					var buf bytes.Buffer
					ref := &nagiv1.VolumeFileRef{ProjectId: "p1", Container: "web", Volume: DataVolume, Path: "d"}
					err := f.m.ReadVolumeFile(t.Context(), ref, func(ch *nagiv1.VolumeFileChunk) error {
						buf.Write(ch.GetData())
						return nil
					})
					if err != nil {
						continue
					}
					if bad := badTarEntry(&buf, want); bad != "" {
						t.Errorf("第 %d 次打包下载：%s", i, bad)
						return
					}
				}
				for i := range 200 {
					if err := check.verify(root, filepath.Join("d", "f")); err != nil && !errors.Is(err, ErrFileChanged) {
						t.Errorf("第 %d 次核对：%v", i, err)
						return
					}
				}
			}()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("卡住了")
			}
			t.Logf("下载成功 %d 次，拒绝 %d 次", served.Load(), rejected.Load())
		})
	}
}

// badTarEntry 检查打包下载的内容：每个文件都应该是原文件。有问题时返回说明。
func badTarEntry(r io.Reader, want string) string {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err.Error()
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return ""
		}
		if err != nil {
			return err.Error()
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err.Error()
		}
		if string(data) != want {
			return h.Name + " 的内容不是原文件"
		}
	}
}

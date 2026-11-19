package metrics

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCheckDisk(t *testing.T) {
	defer func(f func(string) (int64, int64)) { diskSpace = f }(diskSpace)
	var total, free int64
	diskSpace = func(string) (int64, int64) { return total, free }

	if err := CheckDisk("/", 0); err != nil {
		t.Fatalf("读不到时不应该拦: %v", err)
	}
	total, free = 100<<30, 50<<30
	if err := CheckDisk("/", 0); err != nil {
		t.Fatalf("空间够: %v", err)
	}
	if err := CheckDisk("/", 47<<30); err == nil {
		t.Fatal("写入以后低于 5% 应该拦")
	}
	total, free = 1000<<30, 1<<30
	var le *LowDiskError
	if err := CheckDisk("/", 0); !errors.As(err, &le) || le.Free != 1<<30 {
		t.Fatalf("低于 2 GiB 应该拦: %v", err)
	}
}

// 空间还很多、inode 快用完时也要拦；不限制 inode 个数的文件系统（总数报 0）不拦。
func TestCheckDiskInodes(t *testing.T) {
	defer func(f func(string) (int64, int64)) { diskSpace = f }(diskSpace)
	defer func(f func(string) (int64, int64)) { diskInodes = f }(diskInodes)
	diskSpace = func(string) (int64, int64) { return 100 << 30, 50 << 30 }
	var inodes, free int64
	diskInodes = func(string) (int64, int64) { return inodes, free }

	if err := CheckDisk("/", 0); err != nil {
		t.Fatalf("读不到 inode 个数时不应该拦: %v", err)
	}
	inodes, free = 1_000_000, 500_000
	if err := CheckDisk("/", 0); err != nil {
		t.Fatalf("inode 够: %v", err)
	}
	inodes, free = 1_000_000, 40_000
	var le *LowDiskError
	if err := CheckDisk("/", 0); !errors.As(err, &le) || le.FreeInodes != 40_000 || le.Inodes != 1_000_000 {
		t.Fatalf("inode 低于 5%% 应该拦: %v", err)
	}
	inodes, free = 100_000, 9_000
	if err := CheckDisk("/", 0); !errors.As(err, &le) {
		t.Fatalf("inode 低于 %d 个应该拦: %v", MinFreeInodes, err)
	}
	if !strings.Contains(le.Error(), "inode") {
		t.Fatalf("应该说明是 inode 不够: %q", le.Error())
	}
}

func TestDiskGuard(t *testing.T) {
	defer func(f func(string) (int64, int64)) { diskSpace = f }(diskSpace)
	calls := 0
	free := int64(10 << 30)
	diskSpace = func(string) (int64, int64) { calls++; return 100 << 30, free }

	g := &DiskGuard{Dir: "/"}
	var buf bytes.Buffer
	w := g.Writer(&buf)
	chunk := make([]byte, 1<<20)
	for range 64 {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("64 MiB 以内应该只检查一次，检查了 %d 次", calls)
	}
	free = 1 << 30
	if _, err := w.Write(chunk); err == nil {
		t.Fatal("写满 64 MiB 后应该重新检查并拦下")
	}
	if buf.Len() != 64<<20 {
		t.Fatalf("拦下的那次不应该写入: %d", buf.Len())
	}
}

// v0.35.0#19：几路并发写共用一个 DiskGuard 时写入量合在一起算：8 路各写 16 MiB（共 128 MiB），
// 合计写满 64 MiB 后重新检查并拦下。各用各的检查时每路都只在第一次写时检查，全部能写完。
func TestDiskGuardShared(t *testing.T) {
	defer func(f func(string) (int64, int64)) { diskSpace = f }(diskSpace)
	var mu sync.Mutex
	calls := 0
	diskSpace = func(string) (int64, int64) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return 100 << 30, 10 << 30
		}
		return 100 << 30, 1 << 30 // 第一次检查以后磁盘满了
	}

	g := &DiskGuard{Dir: "/"}
	chunk := make([]byte, 1<<20)
	var wg sync.WaitGroup
	var written, blocked atomic.Int64
	for range 8 {
		wg.Go(func() {
			w := g.Writer(io.Discard)
			for range 16 {
				if _, err := w.Write(chunk); err != nil {
					blocked.Add(1)
					return
				}
				written.Add(int64(len(chunk)))
			}
		})
	}
	wg.Wait()
	// 合计只能写 64 MiB，最多 4 路写完，其余的被拦下。
	if blocked.Load() < 4 || written.Load() != DiskCheckEvery {
		t.Fatalf("合计写满 %d 后应该全部拦下：写了 %d，拦下 %d 路", DiskCheckEvery, written.Load(), blocked.Load())
	}
}

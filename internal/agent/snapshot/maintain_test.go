package snapshot

import (
	"context"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// 定期维护时，过期快照占用的空间会被回收，仓库大小保持有界，而不是随快照数量一直增长。
// kopia 按整个数据包回收：一个包里的内容全部过期才删除包，所以允许有少量浪费。
func TestMaintainKeepsStorageBounded(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storage := filepath.Join(root, "backups")
	r, err := Open(ctx, "p1", storage, filepath.Join(root, "backup.key"), filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)

	data := filepath.Join(root, "data")
	os.MkdirAll(data, 0o755)
	const n = 60 // 每份 1MB 的不同内容，只保留最近 20 份
	for i := 0; i < n; i++ {
		b := make([]byte, 1<<20)
		rand.Read(b)
		if err := os.WriteFile(filepath.Join(data, "blob.bin"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Create(ctx, Version, []Dir{{"data", data}}, nil, map[string]string{"version": strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
		if i%10 == 9 {
			time.Sleep(1100 * time.Millisecond) // kopia 的时间戳精确到秒
			if err := r.Maintain(ctx, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	size := dirSize(t, storage)
	t.Logf("storage after %d snapshots (20 kept): %dMB", n, size>>20)
	// 不维护时约 60MB；维护后应明显变小（保留的 20MB 加上未整包过期的少量浪费）。
	if size > 45<<20 {
		t.Fatalf("storage not reclaimed: %dMB for 20 kept snapshots of 1MB", size>>20)
	}
	list, err := r.List(ctx, Version)
	if err != nil || len(list) != 20 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	if err := r.Restore(ctx, list[len(list)-1].ID, "data", filepath.Join(root, "restore")); err != nil {
		t.Fatalf("restore oldest kept snapshot after maintenance: %v", err)
	}
}

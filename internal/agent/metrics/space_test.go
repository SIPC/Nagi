package metrics

import (
	"bytes"
	"errors"
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

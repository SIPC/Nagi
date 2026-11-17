package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

const indexTestPID = "fedcba9876543210"

func newIndexTestReplicas(t *testing.T) *replicas {
	t.Helper()
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "replicas"), 0o700); err != nil {
		t.Fatal(err)
	}
	return newReplicas(data)
}

// readIndex 读出磁盘上的 replica-dirs.json。
func readIndex(t *testing.T, r *replicas) replicaIndex {
	t.Helper()
	raw, err := os.ReadFile(r.file)
	if err != nil {
		t.Fatal(err)
	}
	var idx replicaIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

// 改名成功以后目录落盘失败，不算写入失败：调用方不回滚内存，内存和磁盘上的记录一致（审查 N3）。
func TestWriteAtomicDirSyncFailure(t *testing.T) {
	r := newIndexTestReplicas(t)
	orig := syncDir
	t.Cleanup(func() { syncDir = orig })
	id := indexTestPID + "-web"
	for i, cause := range []error{syscall.EIO, syscall.EINVAL, errors.ErrUnsupported} {
		calls := 0
		syncDir = func(dir string) error {
			calls++
			return &fs.PathError{Op: "sync", Path: dir, Err: cause}
		}
		base := filepath.Join(filepath.Dir(r.dir), fmt.Sprintf("loc%d", i))
		if err := r.setLocation(id, base, true); err != nil {
			t.Fatalf("%v：目录落盘失败不应算写入失败：%v", cause, err)
		}
		if calls == 0 {
			t.Fatal("应该让目录落盘")
		}
		if root, _ := r.root(indexTestPID, "web"); root != filepath.Join(base, id) {
			t.Fatalf("%v：内存里应是新位置，实际 %s", cause, root)
		}
		if l := readIndex(t, r).Locations[id]; l.Base != base {
			t.Fatalf("%v：磁盘上应是新位置，实际 %+v", cause, l)
		}
		mv := replicaMove{ID: id, From: base, To: r.dir}
		if err := r.markMove(mv, true); err != nil {
			t.Fatalf("%v：记下移动不应失败：%v", cause, err)
		}
		if n := len(readIndex(t, r).Moves); n != 1 || len(r.idx.Moves) != 1 {
			t.Fatalf("%v：内存和磁盘都应记着这次移动：%d %d", cause, n, len(r.idx.Moves))
		}
		if err := r.markMove(mv, false); err != nil {
			t.Fatal(err)
		}
	}
}

// markMove 写盘失败时内存不变（审查建议）。
func TestMarkMoveRollback(t *testing.T) {
	r := newIndexTestReplicas(t)
	block := r.file + ".tmp" // 占住临时文件名，记录写不进去
	other := filepath.Join(filepath.Dir(r.dir), "other")
	mv := replicaMove{ID: indexTestPID + "-web", From: r.dir, To: other}
	if err := os.Mkdir(block, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.markMove(mv, true); err == nil {
		t.Fatal("记录写不进去时应该失败")
	}
	if len(r.idx.Moving) != 0 || len(r.idx.Moves) != 0 {
		t.Fatalf("写盘失败时内存不能变：%+v", r.idx)
	}
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	if err := r.markMove(mv, true); err != nil {
		t.Fatal(err)
	}
	if len(r.idx.Moving) != 2 || len(r.idx.Moves) != 1 {
		t.Fatalf("原位置和目标位置都记进 Moving：%+v", r.idx)
	}
	if err := os.Mkdir(block, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.markMove(mv, false); err == nil {
		t.Fatal("记录写不进去时应该失败")
	}
	if len(r.idx.Moving) != 2 || len(r.idx.Moves) != 1 {
		t.Fatalf("去掉记录失败时内存里也留着（和磁盘一致）：%+v", r.idx)
	}
}

package snapshot

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/virtualfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/upload"
)

// writeTree 绕过 Create，直接把一棵目录树写成快照：模拟被攻破的源节点写出来的副本仓库。
func writeTree(t *testing.T, r *Repo, root fs.Directory) string {
	t.Helper()
	ctx := context.Background()
	var id manifest.ID
	err := repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "test"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		man, err := upload.NewUploader(w).Upload(ctx, root, policy.BuildTree(nil, policy.DefaultPolicy), r.source(Manual))
		if err != nil {
			return err
		}
		id, err = snapshot.SaveSnapshot(ctx, w, man)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(id)
}

func file(name, content string) fs.Entry {
	return virtualfs.StreamingFileFromReader(name, io.NopCloser(strings.NewReader(content)))
}

// 快照里的文件名带 .. 或 / 时，恢复失败，目标目录外面不出现文件。
// 重名也会被拒绝，但 kopia 的上传会合并重名项，这里造不出来。
func TestRestoreRejectsBadNames(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		entries []fs.Entry
	}{
		{"parent", []fs.Entry{file("ok", "1"), file("../../escape", "x")}},
		{"slash", []fs.Entry{virtualfs.NewStaticDirectory("sub", []fs.Entry{file("a/../../../escape", "x")})}},
		{"dot", []fs.Entry{file("..", "x")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			r, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close(ctx)
			id := writeTree(t, r, virtualfs.NewStaticDirectory("root", []fs.Entry{virtualfs.NewStaticDirectory("data", tc.entries)}))
			target := filepath.Join(root, "c", "data")
			if err := r.Restore(ctx, id, "data", target); !errors.Is(err, ErrBadName) {
				t.Fatalf("restore: %v", err)
			}
			for _, p := range []string{filepath.Join(root, "escape"), filepath.Join(root, "c", "escape")} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("%s was written", p)
				}
			}
		})
	}
}

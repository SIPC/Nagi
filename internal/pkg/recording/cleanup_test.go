package recording

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// 旧录制原件只有完整导入、逐字节与库里一致（或库里已按容量清理）才删除；
// 未导入完或不一致的保留并提示失败，修好后下次重启接着删；删完写完成标记，不再重复。
func TestCleanupLegacyRecordingsOnlyAfterVerifiedImport(t *testing.T) {
	r := testRecordings(t)
	ok := r.WriteOnce(t.Context(), "imported-and-equal")
	expired := r.WriteOnce(t.Context(), "pruned-in-database")
	bad := r.WriteOnce(t.Context(), "will-be-changed-after-import")
	want := readAll(t, r, ok.ID)
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	if err := r.UseDatabase(t.Context(), filepath.Join(t.TempDir(), "recordings.db"), nil); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dir := func(id string) string { return filepath.Join(r.Dir, id[:6], id) }
	exists := func(id string) bool { _, err := os.Stat(dir(id)); return err == nil }

	// 还没导入：一个都不删。
	if err := r.CleanupLegacy(t.Context(), logs); err == nil || !exists(ok.ID) || !exists(bad.ID) {
		t.Fatalf("未导入不能删：%v", err)
	}
	if m := logs.Migrations(); len(m) != 1 || m[0].ID != CleanupID || m[0].Phase != "failed" {
		t.Fatalf("应在日志页提示失败：%+v", m)
	}
	if err := r.ImportLegacy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`DELETE FROM recording_sessions WHERE id=?`, expired.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir(bad.ID), ChunkName(0))
	original, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(bytes.Clone(original), 'x'), 0o600); err != nil {
		t.Fatal(err)
	}

	// 一个与库里不一致：只删核验通过的两个，不一致的保留，整体算失败。
	if err := r.CleanupLegacy(t.Context(), logs); err == nil || exists(ok.ID) || exists(expired.ID) || !exists(bad.ID) {
		t.Fatalf("只删核验通过的：err=%v ok=%v expired=%v bad=%v", err, exists(ok.ID), exists(expired.ID), exists(bad.ID))
	}
	if got := readAll(t, r, ok.ID); got != want {
		t.Fatal("删原件后回放改变")
	}
	var uploaded int
	if err := r.SealedChunks(ok.ID, func(int, []byte) error { uploaded++; return nil }); err != nil || uploaded == 0 {
		t.Fatalf("删原件后外送读不到密文：%d %v", uploaded, err)
	}

	// 修好后重启：删掉剩下的，删空目录，写完成标记；再次运行不重复。
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.CleanupLegacy(t.Context(), logs); err != nil || exists(bad.ID) {
		t.Fatalf("修好后应删除：%v", err)
	}
	if _, err := os.Stat(r.Dir); !os.IsNotExist(err) {
		t.Fatalf("旧录制目录应删除：%v", err)
	}
	if b, _ := logs.Checkpoint(t.Context(), cleanupCheckpoint); string(b) != `{"phase":"done"}` {
		t.Fatalf("应写完成标记：%s", b)
	}
	if err := r.CleanupLegacy(t.Context(), logs); err != nil {
		t.Fatal(err)
	}
}

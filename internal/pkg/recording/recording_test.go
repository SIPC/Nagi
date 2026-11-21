package recording

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/server/sealer"
)

func testRecordings(t *testing.T) *Recordings {
	t.Helper()
	key, err := sealer.Load(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	return &Recordings{Dir: t.TempDir(), Sealer: key}
}

func readAll(t *testing.T, r *Recordings, id string) string {
	t.Helper()
	var out bytes.Buffer
	if err := r.Read(id, func(b []byte) error { out.Write(b); return nil }); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// 录制只有输出和窗口大小；中文被拆在两次输出之间也能完整还原。
func TestRecordingRoundTrip(t *testing.T) {
	r := testRecordings(t)
	rec := r.Start(context.Background(), 80, 24)
	word := []byte("密码已修改")
	rec.Output(word[:4])
	rec.Output(word[4:])
	rec.Resize(120, 40)
	s := rec.Close()
	if len(s.Chunks) == 0 || s.Chunks[0].Hash == "" {
		t.Fatalf("摘要 %+v", s)
	}
	got := readAll(t, r, s.ID)
	if !strings.Contains(got, `"version":2`) || !strings.Contains(got, `"密"`) || !strings.Contains(got, `"码已修改"`) || strings.Contains(got, "�") || !strings.Contains(got, `"r","120x40"`) {
		t.Fatalf("回放内容：%s", got)
	}
}

// 刷屏超过 64 MB 以后，最后的命令仍在录制里，开头也在，中间标记已截断。
func TestRecordingKeepsTail(t *testing.T) {
	r := testRecordings(t)
	rec := r.Start(context.Background(), 80, 24)
	rec.Output([]byte("$ first-command\r\n"))
	junk := bytes.Repeat([]byte("yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy\r\n"), 2000)
	for written := 0; written < 70<<20; written += len(junk) {
		rec.Output(junk)
	}
	rec.Output([]byte("$ rm -rf /important\r\n"))
	s := rec.Close()
	if !s.Truncated {
		t.Fatal("应标记已截断")
	}
	var kept int64
	for _, c := range s.Chunks {
		kept += c.Bytes
	}
	if kept > (headLimit+tailLimit)*11/10 {
		t.Fatalf("保留了 %d 字节", kept)
	}
	got := readAll(t, r, s.ID)
	for _, want := range []string{"first-command", "rm -rf /important", "录制已截断"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %s", want)
		}
	}
}

// 加密的块被改动后读不出来。
func TestRecordingTamper(t *testing.T) {
	r := testRecordings(t)
	s := r.WriteOnce(context.Background(), "hello")
	if err := r.Read("../../etc", func([]byte) error { return nil }); err == nil {
		t.Fatal("非法 ID")
	}
	path := filepath.Join(r.Dir, s.ID[:6], s.ID, "000000")
	if err := corrupt(path); err != nil {
		t.Fatal(err)
	}
	if err := r.Read(s.ID, func([]byte) error { return nil }); err == nil {
		t.Fatal("改动后的块应读不出来")
	}
}

func corrupt(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	b[len(b)-1] ^= 0xff
	return os.WriteFile(path, b, 0o600)
}

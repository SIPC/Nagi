package auditlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordRotateResume(t *testing.T) {
	old := maxSize
	maxSize = 2000
	defer func() { maxSize = old }()
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		l.Record(Event{Panel: "sha256//p", Actor: "admin", Action: "DeployContainer", Target: "p1/web", Detail: strings.Repeat("x", 50)})
	}
	if n := len(files(dir)); n != maxFiles {
		t.Fatalf("应该只留 %d 个文件，实际 %d", maxFiles, n)
	}
	// 太长的字段截断。
	seq := l.Record(Event{Action: "node_exec", Detail: strings.Repeat("长", 3000)})
	l.Close()

	// 重新打开接着序号写。
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if got := l2.Record(Event{Action: "x"}); got != seq+1 {
		t.Fatalf("重新打开后序号 %d，应该是 %d", got, seq+1)
	}
	lines, first, last := l2.After(seq-1, 10)
	if len(lines) != 2 || first != seq || last != seq+1 {
		t.Fatalf("After 不对：%d 行 %d-%d", len(lines), first, last)
	}
	var e Event
	_ = json.Unmarshal(lines[0], &e)
	if len(e.Detail) > maxField+10 {
		t.Fatalf("字段没有截断：%d", len(e.Detail))
	}
}

// H2：升级前节点保存了面板的外送凭据。升级后第一次打开时删掉，面板第一次读从以前外送到的位置开始。
func TestMigrateShipping(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(filepath.Join(dir, "audit"))
	defer l.Close()
	for range 5 {
		l.Record(Event{Action: "a"})
	}
	cfg := filepath.Join(dir, "auditsinks.json")
	_ = os.WriteFile(cfg, []byte(`{"sha256//p":[{"id":"s1","type":"s3","access_key":"ak","secret_key":"sk"}]}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "audit", "cursors.json"), []byte(`{"s1@abc":3,"s2@def":4}`), 0o600)
	if err := l.MigrateShipping(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatal("升级后要删掉带凭据的外送配置")
	}
	if got := l.Start(); got != 3 {
		t.Fatalf("面板第一次读应该从以前外送到的位置（3）开始，实际 %d", got)
	}
	// 没有旧配置的节点：从最新的记录开始。
	l2, _ := Open(filepath.Join(dir, "audit2"))
	defer l2.Close()
	l2.Record(Event{Action: "a"})
	if err := l2.MigrateShipping(filepath.Join(dir, "none.json")); err != nil || l2.Start() != 1 {
		t.Fatalf("没有旧配置时从最新的记录开始：%d %v", l2.Start(), err)
	}
}

func TestRead(t *testing.T) {
	l, _ := Open(t.TempDir())
	defer l.Close()
	for i := range 700 {
		l.Record(Event{Action: "a", Detail: strings.Repeat("x", i%3)})
	}
	lines, first, last := l.Read(0, 0)
	if len(lines) != ReadLimit || first != 1 || last != ReadLimit {
		t.Fatalf("每次最多 %d 条：%d 行 %d-%d", ReadLimit, len(lines), first, last)
	}
	lines, first, last = l.Read(last, 1000)
	if len(lines) != 200 || first != 501 || last != 700 {
		t.Fatalf("接着读：%d 行 %d-%d", len(lines), first, last)
	}
	if lines, first, last = l.Read(700, 10); len(lines) != 0 || first != 0 || last != 0 {
		t.Fatalf("没有新记录：%d 行 %d-%d", len(lines), first, last)
	}
	// 面板的游标比节点的序号还大（本地审计被清空过）：从头读。
	if lines, first, _ = l.Read(9999, 10); len(lines) != 10 || first != 1 {
		t.Fatalf("游标超过最新序号时从头读：%d 行，从 %d 开始", len(lines), first)
	}
	// 总大小有上限。
	old := readMaxBytes
	readMaxBytes = 50 << 10
	defer func() { readMaxBytes = old }()
	big, _ := Open(t.TempDir())
	defer big.Close()
	for range 300 {
		big.Record(Event{Action: "a", Detail: strings.Repeat("长", maxField/3)})
	}
	lines, _, last = big.Read(0, 0)
	size := 0
	for _, b := range lines {
		size += len(b)
	}
	if size > readMaxBytes || len(lines) >= 300 || last != int64(len(lines)) {
		t.Fatalf("超过大小上限：%d 字节，%d 行到 %d", size, len(lines), last)
	}
}

package auditlog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func openTest(t *testing.T, dir string) *Log {
	t.Helper()
	l, err := Open(filepath.Join(dir, "logs", "audit", "node-audit.db"), filepath.Join(dir, "audit"))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.ImportLegacy(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestIndependentHistorySurvivesStoreWriteFailure(t *testing.T) {
	dir := t.TempDir()
	history, err := logstore.Open(t.Context(), filepath.Join(dir, "history.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	l := openTest(t, dir)
	l.Capture = func(e Event) {
		history.Report(logstore.Event{Time: e.Time, Category: logstore.Audit, Kind: e.Action, Data: e})
	}
	if id := l.Record(Event{Action: "before", Result: "denied"}); id != 1 {
		t.Fatalf("序号: %d", id)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 21, 12, 3, 4, 0, time.FixedZone("local", 8*3600))
	if id := l.Record(Event{Time: at, Action: "after", Actor: strings.Repeat("人", 3000)}); id != 0 {
		t.Fatalf("写失败不能返回已保存的编号: %d", id)
	}
	if err := history.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := history.Query(t.Context(), logstore.Filter{Category: logstore.Audit})
	if err != nil || len(rows) != 2 || rows[0].Kind != "before" || rows[1].Kind != "after" || !rows[1].Time.Equal(at) {
		t.Fatalf("独立历史丢失或重复: %+v err=%v", rows, err)
	}
	var captured Event
	if err := json.Unmarshal(rows[1].Data.(json.RawMessage), &captured); err != nil || captured.Result != "ok" || captured.Seq != 0 || len(captured.Actor) > maxField+3 {
		t.Fatalf("独立历史没有沿用来源规范化，或冒充了序号: %+v err=%v", captured, err)
	}
}

// Every record is chained; reopening continues the numbering and the chain.
func TestRecordChainResume(t *testing.T) {
	dir := t.TempDir()
	l := openTest(t, dir)
	for range 50 {
		l.Record(Event{Panel: "sha256//p", Actor: "admin", Action: "DeployContainer", Target: "p1/web"})
	}
	seq := l.Record(Event{Action: "node_exec", Detail: strings.Repeat("长", 3000)})
	l.Close()
	l2 := openTest(t, dir)
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
	if r, err := l2.Verify(t.Context()); err != nil || !r.OK() || r.Records != seq+1 {
		t.Fatalf("%+v %v", r, err)
	}
}

// Tampering is detected: a changed line, a deleted record, and a deleted record
// hidden behind a forged tombstone.
func TestVerifyDetectsTampering(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"edit", `UPDATE audit_events SET line=replace(line,'"admin"','"mallory"') WHERE seq=5`},
		{"delete", `DELETE FROM audit_events WHERE seq=5`},
		{"forged tombstone", `INSERT INTO audit_tombstones SELECT seq, hash FROM audit_events WHERE seq=5; DELETE FROM audit_events WHERE seq=5`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := openTest(t, dir)
			for range 10 {
				l.Record(Event{Actor: "admin", Action: "DeployContainer"})
			}
			l.Close()
			d, err := db.Open(filepath.Join(dir, "logs", "audit", "node-audit.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			d.Close()
			r, err := VerifyFile(t.Context(), filepath.Join(dir, "logs", "audit", "node-audit.db"))
			if err != nil || r.OK() {
				t.Fatalf("tampering not detected: %+v %v", r, err)
			}
		})
	}
}

// Capacity cleanup leaves tombstones listed by a prune record on the chain; the
// chain still verifies, and keeps verifying after the prune record itself ages.
func TestTrimKeepsChainVerifiable(t *testing.T) {
	dir := t.TempDir()
	l := openTest(t, dir)
	for i := range 3*pruneBatch + 10 {
		l.Record(Event{Action: "a", Detail: strings.Repeat("x", i%7)})
	}
	for range 2 {
		if ok, err := l.trim(t.Context()); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	r, err := l.Verify(t.Context())
	if err != nil || !r.OK() {
		t.Fatalf("%+v %v", r, err)
	}
	lines, first, _ := l.Read(0, 10)
	if len(lines) == 0 || first != 2*pruneBatch+1 {
		t.Fatalf("oldest records not trimmed: first=%d", first)
	}
}

func TestRead(t *testing.T) {
	l := openTest(t, t.TempDir())
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
	if lines, first, _ = l.Read(9999, 10); len(lines) != 10 || first != 1 {
		t.Fatalf("游标超过最新序号时从头读：%d 行，从 %d 开始", len(lines), first)
	}
	old := readMaxBytes
	readMaxBytes = 50 << 10
	defer func() { readMaxBytes = old }()
	big := openTest(t, t.TempDir())
	for range 300 {
		big.Record(Event{Action: "a", Detail: strings.Repeat("长", maxField/3)})
	}
	lines, _, last = big.Read(0, 0)
	if len(bytes.Join(lines, nil)) > readMaxBytes || len(lines) >= 300 || last != int64(len(lines)) {
		t.Fatalf("超过大小上限：%d 行到 %d", len(lines), last)
	}
}

// H2：升级前节点保存了面板的外送凭据。升级后第一次打开时删掉，面板第一次读从以前外送到的位置开始。
func TestMigrateShipping(t *testing.T) {
	dir := t.TempDir()
	writeLegacy(t, filepath.Join(dir, "audit"), 1, 5)
	l := openTest(t, dir)
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
	l2 := openTest(t, t.TempDir())
	l2.Record(Event{Action: "a"})
	if err := l2.MigrateShipping(filepath.Join(dir, "none.json")); err != nil || l2.Start() != 1 {
		t.Fatalf("没有旧配置时从最新的记录开始：%d %v", l2.Start(), err)
	}
}

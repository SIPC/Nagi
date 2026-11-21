package audit

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 完成标准（docs/security.md 第 5 步）：备用面板按 id 增量同步，提升以后接着原来的哈希链写，校验连续。
func TestReplicaPromote(t *testing.T) {
	ctx := context.Background()
	primary, standby := openTestLog(t), openTestLog(t)
	recP, recS := testRecordings(t), testRecordings(t)
	recP.Sealer = recS.Sealer // 备用面板有同一个主密钥
	old := time.Now().Add(-100 * 24 * time.Hour)
	for i := range 6 {
		e := Event{Category: Operation, Action: "Deploy"}
		if i < 2 {
			e.Time = old
		}
		primary.Record(ctx, e)
	}
	s := recP.WriteOnce(ctx, "hello from primary")
	primary.Record(ctx, Event{Category: Session, Action: "NodeExec", RecordingID: s.ID})
	// 主面板按保留时间删掉了最旧的两条（留下墓碑）。
	if err := primary.Prune(ctx, DefaultRetention, time.Now()); err != nil {
		t.Fatal(err)
	}
	// 分两批同步，第二批从断点续传。
	for {
		_, frame, err := primary.ReadBatch(ctx, standby.MaxID(ctx), 3, recP)
		if err != nil {
			t.Fatal(err)
		}
		before := standby.MaxID(ctx)
		if _, err := standby.ApplyFrame(ctx, frame, recS); err != nil {
			t.Fatal(err)
		}
		if standby.MaxID(ctx) == before {
			break
		}
	}
	if standby.MaxID(ctx) != primary.MaxID(ctx) {
		t.Fatalf("同步到 %d，主面板 %d", standby.MaxID(ctx), primary.MaxID(ctx))
	}
	var out bytes.Buffer
	if err := recS.Read(s.ID, func(b []byte) error { out.Write(b); return nil }); err != nil || !bytes.Contains(out.Bytes(), []byte("hello from primary")) {
		t.Fatalf("录制没有同步过来：%v", err)
	}
	// Re-sending the same frame (a resumed sync) neither fails nor duplicates.
	_, frame, err := primary.ReadBatch(ctx, 0, 100, recP)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := standby.ApplyFrame(ctx, frame, recS); err != nil {
		t.Fatalf("重复同步：%v", err)
	}
	// 提升：重新打开备用面板的库，接着写。
	standby.Close()
	promoted, err := Open(ctx, standby.path, standby.log)
	if err != nil {
		t.Fatal(err)
	}
	defer promoted.Close()
	id := promoted.Record(ctx, Event{Category: Operation, Action: "PromoteStandby"})
	if id != primary.MaxID(ctx)+1 {
		t.Fatalf("提升后的编号 %d", id)
	}
	r, err := promoted.Verify(ctx)
	if err != nil || !r.OK() {
		t.Fatalf("提升后哈希链：%+v %v", r, err)
	}
}

// Since v0.37 recording chunks live in the recording database and legacy files
// are removed after a verified import. Replication must read chunks from the
// database, and the standby must store them where it can replay them.
func TestReplicaRecordingsFromDatabase(t *testing.T) {
	ctx := t.Context()
	primary, standby := openTestLog(t), openTestLog(t)
	recP, recS := testRecordings(t), testRecordings(t)
	recP.Sealer = recS.Sealer
	for _, r := range []*Recordings{recP, recS} {
		if err := r.UseDatabase(ctx, filepath.Join(t.TempDir(), "panel-recordings.db"), nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
	}
	s := recP.WriteOnce(ctx, "replicated from the database")
	primary.Record(ctx, Event{Category: Session, Action: "NodeExec", RecordingID: s.ID})
	if entries, _ := os.ReadDir(recP.Dir); len(entries) != 0 {
		t.Fatal("fixture: the primary must have no recording files")
	}
	_, frame, err := primary.ReadBatch(ctx, 0, 100, recP)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := standby.ApplyFrame(ctx, frame, recS); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := recS.Read(s.ID, func(b []byte) error { out.Write(b); return nil }); err != nil || !bytes.Contains(out.Bytes(), []byte("replicated from the database")) {
		t.Fatalf("standby cannot replay the replicated recording: %v", err)
	}
}

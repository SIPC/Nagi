package audit

import (
	"bytes"
	"context"
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

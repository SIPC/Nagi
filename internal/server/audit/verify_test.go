package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestIndependentHistorySurvivesLegacyWriteFailure(t *testing.T) {
	l := openTestLog(t)
	history, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	l.Capture = func(e Event) {
		history.Report(logstore.Event{Time: e.Time, Category: logstore.Audit, Kind: e.Action, Data: json.RawMessage(Canonical(e))})
	}
	if id := l.Record(t.Context(), Event{Action: "before"}); id != 1 {
		t.Fatalf("旧库编号发生变化: %d", id)
	}
	if _, err := l.DB().Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'test write failure'); END`); err != nil {
		t.Fatal(err)
	}
	// MCP 收集阶段不重复入库；最终父子记录即使旧库失败也各自留下事实。
	ctx, children := WithCollector(t.Context())
	l.Record(ctx, Event{Action: "child"})
	if id := l.RecordWithChildren(ctx, Event{Action: "parent"}, children); id != 0 {
		t.Fatalf("旧库写失败不能返回成功编号: %d", id)
	}
	if err := history.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := history.Query(t.Context(), logstore.Filter{Category: logstore.Audit})
	if err != nil || len(rows) != 3 || rows[0].Kind != "before" || rows[1].Kind != "parent" || rows[2].Kind != "child" {
		t.Fatalf("独立历史丢失或重复: %+v err=%v", rows, err)
	}
	for i, row := range rows {
		var saved Event
		if err := json.Unmarshal(row.Data.(json.RawMessage), &saved); err != nil {
			t.Fatal(err)
		}
		wantID := int64(0)
		if i == 0 {
			wantID = 1
		}
		if saved.ID != wantID || saved.Result != ResultOK {
			t.Fatalf("不能把旧库未提交的编号当成证据: %+v", saved)
		}
	}
	if result, err := l.Verify(t.Context()); err != nil || !result.OK() || result.Records != 1 {
		t.Fatalf("失败写入不能破坏旧哈希链: %+v err=%v", result, err)
	}
	if _, err := l.DB().Exec(`DROP TRIGGER reject_audit`); err != nil {
		t.Fatal(err)
	}
	history.Close()
	if id := l.Record(t.Context(), Event{Action: "journal-unavailable"}); id != 2 {
		t.Fatalf("新库失败不能阻断旧库或正常操作: %d", id)
	}
	if result, err := l.Verify(t.Context()); err != nil || !result.OK() || result.Records != 2 {
		t.Fatalf("独立历史失败破坏旧审计: %+v err=%v", result, err)
	}
}

func openTestLog(t *testing.T) *Log {
	t.Helper()
	l, err := Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestChildCounts(t *testing.T) {
	l := openTestLog(t)
	ctx := t.Context()
	a := l.Record(ctx, Event{Action: "parent-a"})
	b := l.Record(ctx, Event{Action: "parent-b"})
	c := l.Record(ctx, Event{Action: "parent-c"})
	for _, parent := range []int64{a, b, a} {
		if id := l.Record(ctx, Event{Action: "child", ParentID: parent}); id == 0 {
			t.Fatal("child not recorded")
		}
	}
	counts, err := l.ChildCounts(ctx, []int64{b, c, a, 999})
	if err != nil || len(counts) != 2 || counts[a] != 2 || counts[b] != 1 {
		t.Fatalf("child counts: %v, %v", counts, err)
	}
	// Explicitly asking for root records still counts parent_id=0 as before.
	counts, err = l.ChildCounts(ctx, []int64{0, a})
	if err != nil || len(counts) != 2 || counts[0] != 3 || counts[a] != 2 {
		t.Fatalf("root and child counts: %v, %v", counts, err)
	}
}

// 完成标准（docs/security.md 第 5 步）：删掉中间的一条记录，校验报错；按保留时间清理不影响校验。
func TestVerifyDetectsDeletion(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)
	old := time.Now().Add(-100 * 24 * time.Hour)
	for i := range 10 {
		e := Event{Category: Operation, Action: "Deploy", ActorName: "admin"}
		if i < 3 {
			e.Time = old
		}
		if i == 1 {
			e.Category = Login // 保留得更久
		}
		l.Record(ctx, e)
	}
	if r, err := l.Verify(ctx); err != nil || !r.OK() || r.Records != 10 {
		t.Fatalf("完好的链：%+v %v", r, err)
	}
	// 按保留时间清理：删掉第 1、3 条（普通记录），第 2 条（登录）留着，链靠墓碑接上；链上多一条清理记录。
	if err := l.Prune(ctx, DefaultRetention, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r, err := l.Verify(ctx); err != nil || !r.OK() || r.Records != 9 {
		t.Fatalf("清理之后：%+v %v", r, err)
	}
	// 删掉中间的一条。
	if _, err := l.DB().ExecContext(ctx, `DELETE FROM events WHERE id = 6`); err != nil {
		t.Fatal(err)
	}
	r, err := l.Verify(ctx)
	if err != nil || r.OK() || r.BadID != 7 || !strings.Contains(r.Reason, "缺少记录 6") {
		t.Fatalf("删除之后：%+v %v", r, err)
	}
	// 修改一条。
	l2 := openTestLog(t)
	for range 3 {
		l2.Record(ctx, Event{Category: Operation, Action: "Deploy"})
	}
	if _, err := l2.DB().ExecContext(ctx, `UPDATE events SET actor_name = 'someone' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if r, _ := l2.Verify(ctx); r.BadID != 2 {
		t.Fatalf("修改之后：%+v", r)
	}
}

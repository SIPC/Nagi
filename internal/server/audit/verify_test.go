package audit

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestLog(t *testing.T) *Log {
	t.Helper()
	l, err := Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
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
	// 按保留时间清理：删掉第 1、3 条（普通记录），第 2 条（登录）留着，链靠墓碑接上。
	if err := l.Prune(ctx, DefaultRetention, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r, err := l.Verify(ctx); err != nil || !r.OK() || r.Records != 8 {
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

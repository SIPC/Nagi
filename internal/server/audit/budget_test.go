package audit

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestLegacyAuditSharesBudgetWithoutBreakingChain(t *testing.T) {
	ctx := t.Context()
	l := openTestLog(t)
	for range 900 {
		if l.Record(ctx, Event{Category: Operation, Action: "Deploy", Params: strings.Repeat("x", MaxParams-20)}) == 0 {
			t.Fatal("seed")
		}
	}
	login := l.Record(ctx, Event{Category: Login, Action: "login", Time: time.Now().Add(-400 * 24 * time.Hour)})
	budget, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer budget.Close()
	if err := l.UseBudget(ctx, budget, false); err != nil {
		t.Fatal(err)
	}
	for range 30 {
		if err := budget.Record(ctx, logstore.Event{Category: logstore.Audit, Kind: "operation", Data: map[string]any{"message": strings.Repeat("a", 48<<10)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Prune(ctx, DefaultRetention, time.Now()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := l.db.QueryRow(`SELECT count(*) FROM events WHERE id=?`, login).Scan(&n); err != nil || n != 1 {
		t.Fatalf("普通历史未删完便按天数删审计: %d %v", n, err)
	}
	st, err := budget.State(ctx)
	if err != nil || st.DiskBytes > 4<<20 || len(st.Archives) != 1 || st.Archives[0].PrunedBatches == 0 {
		t.Fatalf("旧库没有按物理占盘受限: %+v %v", st, err)
	}
	if v, err := l.Verify(ctx); err != nil || !v.OK() {
		t.Fatalf("清理破坏审计链: %+v %v", v, err)
	}
	if l.Record(ctx, Event{Category: Operation, Action: "after-prune"}) <= login {
		t.Fatal("清理后序号倒退或无法继续")
	}
}

func TestQuotaPrunePreservesActiveRecording(t *testing.T) {
	r := testRecordings(t)
	active := r.Start(t.Context(), 80, 24)
	defer active.Close()
	active.Output([]byte("live"))
	if err := r.Prune(0, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	active.Output([]byte("after cleanup"))
	s := active.Close()
	if s.Truncated || !strings.Contains(readAll(t, r, s.ID), "after cleanup") {
		t.Fatalf("清理破坏正在录制的终端: %+v", s)
	}
}

package audit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// M12：能写 audit.db 的人删掉中间一条、补一个墓碑，之前链照样接得上、校验通过。现在墓碑没有对应的清理记录，校验报错。
func TestVerifyDetectsForgedTombstone(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)
	for range 6 {
		l.Record(ctx, Event{Category: Session, Action: "OpenNodeShell", ActorName: "attacker"})
	}
	if _, err := l.DB().Exec(`INSERT INTO tombstones (id, hash) SELECT id, hash FROM events WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := l.DB().Exec(`DELETE FROM events WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	r, err := l.Verify(ctx)
	if err != nil || r.OK() || r.BadID != 3 || !strings.Contains(r.Reason, "墓碑可能是伪造的") {
		t.Fatalf("删除中间一条并伪造墓碑：%+v %v", r, err)
	}
}

// 正常清理以后校验通过：清理记录列出删掉的 ID，交错删除（普通记录和保留更久的记录相间）时切成多条；
// 清理记录自己被删掉以后，墓碑还在的 ID 由新的清理记录接着列出；在清理过的范围里再伪造墓碑也能发现。
func TestPruneRecordsCoverTombstones(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)
	now := time.Now()
	old := now.Add(-100 * 24 * time.Hour)
	// 3000 条旧记录，普通和登录相间：删掉 1500 条不连续的，要切成多条清理记录。
	for i := range 3000 {
		e := Event{Time: old, Category: Operation, Action: "Deploy"}
		if i%2 == 1 {
			e.Category = Login
		}
		l.Record(ctx, e)
	}
	for range 3 {
		l.Record(ctx, Event{Category: Operation, Action: "Deploy"})
	}
	if err := l.Prune(ctx, DefaultRetention, now); err != nil {
		t.Fatal(err)
	}
	var prunes, total int
	rows, _ := l.DB().Query(`SELECT params FROM events WHERE action = 'audit_pruned'`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		var p prunedParams
		if err := json.Unmarshal([]byte(s), &p); err != nil || len(s) > MaxParams {
			t.Fatalf("清理记录的参数不对（%d 字节）：%v", len(s), err)
		}
		prunes++
		total += p.Count
	}
	rows.Close()
	if prunes < 2 || total != 1500 {
		t.Fatalf("清理记录 %d 条、共列出 %d 个 ID，应该是多条、1500 个", prunes, total)
	}
	// 第 1 条比剩下的记录都旧，它的墓碑一起删掉，剩 1499 个。
	if r, err := l.Verify(ctx); err != nil || !r.OK() || r.Tombstones != 1499 {
		t.Fatalf("清理之后：%+v %v", r, err)
	}
	// 再过一年：登录记录和清理记录都过期，墓碑跟着清掉，新的清理记录接上。
	later := now.Add(400 * 24 * time.Hour)
	l.Record(ctx, Event{Time: later, Category: Operation, Action: "Deploy"})
	if err := l.Prune(ctx, DefaultRetention, later); err != nil {
		t.Fatal(err)
	}
	if r, err := l.Verify(ctx); err != nil || !r.OK() {
		t.Fatalf("清理记录被清理以后：%+v %v", r, err)
	}

	// 普通记录保留得比登录记录还久时，清理记录被删掉了、它列出的墓碑还在：新的清理记录接着列出。
	l2 := openTestLog(t)
	for i := range 6 {
		e := Event{Time: old, Category: Login, Action: "login"}
		if i == 0 {
			e.Category = Operation
		}
		l2.Record(ctx, e)
	}
	l2.Record(ctx, Event{Time: now, Category: Operation, Action: "Deploy"})
	long := Retention{Normal: 1000 * 24 * time.Hour, Long: 30 * 24 * time.Hour}
	if err := l2.Prune(ctx, long, now); err != nil {
		t.Fatal(err)
	}
	if err := l2.Prune(ctx, long, now.Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r, err := l2.Verify(ctx); err != nil || !r.OK() {
		t.Fatalf("接着列出墓碑：%+v %v", r, err)
	}

	// 在清理记录列出的范围以外伪造：发现。
	l3 := openTestLog(t)
	for i := range 6 {
		e := Event{Time: now, Category: Operation, Action: "Deploy"}
		if i == 1 {
			e.Time = old
		}
		l3.Record(ctx, e)
	}
	if err := l3.Prune(ctx, DefaultRetention, now); err != nil {
		t.Fatal(err)
	}
	_, _ = l3.DB().Exec(`INSERT INTO tombstones (id, hash) SELECT id, hash FROM events WHERE id = 4`)
	_, _ = l3.DB().Exec(`DELETE FROM events WHERE id = 4`)
	if r, _ := l3.Verify(ctx); r.OK() || r.BadID != 4 {
		t.Fatalf("清理过以后再伪造墓碑：%+v", r)
	}
}

// 升级前的墓碑：主面板第一次启动时写一条 legacy 清理记录，只写一次；有多条 legacy 记录时校验报错。
func TestCoverLegacyTombstones(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)
	for range 5 {
		l.Record(ctx, Event{Category: Operation, Action: "Deploy"})
	}
	// 模拟旧版本的清理：只有墓碑，没有清理记录。
	_, _ = l.DB().Exec(`INSERT INTO tombstones (id, hash) SELECT id, hash FROM events WHERE id <= 2`)
	_, _ = l.DB().Exec(`DELETE FROM events WHERE id <= 2`)
	if r, _ := l.Verify(ctx); r.OK() {
		t.Fatal("没有清理记录的墓碑应该报错")
	}
	if err := l.CoverLegacyTombstones(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.CoverLegacyTombstones(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = l.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE action = 'audit_pruned'`).Scan(&n)
	if n != 1 {
		t.Fatalf("legacy 清理记录 %d 条，应该只写一次", n)
	}
	if r, err := l.Verify(ctx); err != nil || !r.OK() {
		t.Fatalf("写了 legacy 清理记录以后：%+v %v", r, err)
	}
	// 有人清掉状态让它再写一次（借机盖住伪造的墓碑）：多了一条 legacy 记录，校验报错。
	_, _ = l.DB().Exec(`DELETE FROM meta WHERE key = 'prune_records'`)
	_, _ = l.DB().Exec(`INSERT INTO tombstones (id, hash) SELECT id, hash FROM events WHERE id = 4`)
	_, _ = l.DB().Exec(`DELETE FROM events WHERE id = 4`)
	_ = l.CoverLegacyTombstones(ctx)
	if r, _ := l.Verify(ctx); r.OK() || !strings.Contains(r.Reason, "多条升级时写的清理记录") {
		t.Fatalf("多条 legacy 清理记录：%+v", r)
	}
}

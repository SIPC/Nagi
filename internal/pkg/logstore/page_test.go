package logstore_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestByteBoundedPagesDoNotSkipRecords(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), 8<<20)
	for range 28 {
		record(t, s, logstore.Event{Category: logstore.Access, Kind: "request", Data: map[string]any{"message": strings.Repeat("x", 48<<10)}})
	}
	first, err := s.ReadPage(t.Context(), logstore.Filter{Limit: 1000})
	if err != nil || !first.More || first.Next == 0 || first.Next >= 28 || first.State.Last != 28 {
		t.Fatalf("首屏没有按字节分页: %+v %v", first, err)
	}
	second, err := s.ReadPage(t.Context(), logstore.Filter{After: first.Next, Limit: 1000})
	if err != nil || second.More || second.Next != 28 || len(first.Events)+len(second.Events) != 28 {
		t.Fatalf("分页漏读: first=%d second=%d next=%d more=%v err=%v", len(first.Events), len(second.Events), second.Next, second.More, err)
	}
	all := append(first.Events, second.Events...)
	for i, e := range all {
		if e.Seq != int64(i+1) {
			t.Fatalf("序号不连续: %d %d", i, e.Seq)
		}
	}
	empty, err := s.ReadPage(t.Context(), logstore.Filter{Category: logstore.Metrics})
	if err != nil || len(empty.Events) != 0 || empty.Next != 28 {
		t.Fatalf("筛选空屏没有返回可安全追上的位置: %+v %v", empty, err)
	}
	if _, err := s.ReadPage(t.Context(), logstore.Filter{After: 29}); err == nil {
		t.Fatal("超过来源末尾的游标被当成没有日志")
	}
	first, err = s.ReadPage(t.Context(), logstore.Filter{NewestFirst: true, Limit: 1000})
	if err != nil || !first.More {
		t.Fatalf("倒序首屏没有按字节分页: %+v %v", first, err)
	}
	// A new arrival between pages must neither repeat nor shift older entries.
	record(t, s, logstore.Event{Category: logstore.Access, Kind: "new arrival", Data: "later"})
	second, err = s.ReadPage(t.Context(), logstore.Filter{NewestFirst: true, After: first.Next, Limit: 1000})
	if err != nil || second.More || len(first.Events)+len(second.Events) != 28 {
		t.Fatalf("倒序分页漏读: first=%d second=%d err=%v", len(first.Events), len(second.Events), err)
	}
	for i, e := range append(first.Events, second.Events...) {
		if e.Seq != int64(28-i) {
			t.Fatalf("倒序序号不连续: %d %d", i, e.Seq)
		}
	}
}

func TestLogSearchFiltersBeforePaginationIncludingCopies(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), 32<<20)
	c, err := s.OpenCopies(t.Context(), logstore.Config{RecentCapacityMiB: 16, RecentHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i, e := range []logstore.Event{
		{Category: logstore.Runtime, Kind: "program", Project: "p1", Data: "disk 20%"},
		{Category: logstore.Runtime, Kind: "host.journal", Project: "p2", Data: "disk 20%"},
		{Category: logstore.Runtime, Kind: "host.journal", Project: "p1", Data: "disk 200"},
		{Category: logstore.Runtime, Kind: "host.journal", Project: "p1", Data: "DISK 20%"},
		{Category: logstore.Runtime, Kind: "host.journal", Project: "p1", Data: "disk 20% second"},
	} {
		e.Time = time.Now()
		record(t, s, e)
		e.Seq = int64(i + 1)
		if _, err := c.Receive(t.Context(), "n", logstore.CopyBatch{Source: "remote", Category: logstore.Runtime, After: int64(i), Through: int64(i + 1), Events: []logstore.Event{e}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, copy := range []bool{false, true} {
		for _, newest := range []bool{false, true} {
			f := logstore.Filter{NewestFirst: newest, Category: logstore.Runtime, Project: "p1", Search: "disk 20%", KindPrefix: "host.", Limit: 1}
			wantSeqs := []int64{4, 5}
			if newest {
				wantSeqs = []int64{5, 4}
			}
			for _, want := range wantSeqs {
				var p logstore.Page
				if copy {
					p, _, err = c.Read(t.Context(), "n", "remote", f)
				} else {
					p, err = s.ReadPage(t.Context(), f)
				}
				if err != nil || len(p.Events) != 1 || p.Events[0].Seq != want {
					t.Fatalf("copy=%v want=%d got=%+v err=%v", copy, want, p, err)
				}
				f.After = p.Next
			}
		}
	}
}

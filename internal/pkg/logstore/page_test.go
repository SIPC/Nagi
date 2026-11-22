package logstore_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
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

// A Live page skips imported history in bounded scans and never skips a live
// record: callers resume at Next until they reach the live tail.
func TestLivePagesSkipLargeImportedHistoryInBoundedSteps(t *testing.T) {
	ctx := t.Context()
	t.Cleanup(logstore.SetLiveScan(1000))
	s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), 32<<20)
	record(t, s, logstore.Event{Category: logstore.Audit, Kind: "live.before"})
	const imported = 2500 // more than two scan bounds
	for i := 0; i < imported; i += 500 {
		var batch []logstore.Event
		for j := i; j < i+500; j++ {
			batch = append(batch, logstore.Event{Time: time.Unix(1700000000+int64(j), 0), Category: logstore.Audit, Kind: "node.Imported", Data: j})
		}
		if err := s.ImportBatch(ctx, batch, "legacy", i+500); err != nil {
			t.Fatal(err)
		}
	}
	record(t, s, logstore.Event{Category: logstore.Audit, Kind: "live.after"})
	var kinds []string
	after, pages := int64(0), 0
	for {
		p, err := s.ReadPage(ctx, logstore.Filter{Category: logstore.Audit, After: after, Limit: 128, Live: true})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range p.Events {
			kinds = append(kinds, e.Kind)
		}
		if p.Next <= after || pages > 10 {
			t.Fatalf("live pages did not advance: next=%d after=%d pages=%d", p.Next, after, pages)
		}
		after = p.Next
		if !p.More {
			if after != p.State.Last {
				t.Fatalf("last page stopped short of the tail: %d < %d", after, p.State.Last)
			}
			break
		}
	}
	if got := strings.Join(kinds, ","); got != "live.before,live.after" || pages < 3 {
		t.Fatalf("live pages: %s in %d pages", got, pages)
	}
	// The log page itself still shows imported history.
	all, err := s.ReadPage(ctx, logstore.Filter{Category: logstore.Audit, Limit: 10})
	if err != nil || len(all.Events) != 10 || all.Events[1].Kind != "node.Imported" {
		t.Fatalf("ordinary read lost imported history: %+v %v", all.Events, err)
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

// The logs page keeps one gap-evidence search per category mounted. Its matches
// are sparse, so evaluating the search on every record of the category reads
// that category's whole history: seconds on a large store with a cold page
// cache, all of it under the journal gate.
func TestGapEvidenceSearchDoesNotReadWholeCategory(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "logs.db")
	s := openStore(t, path, 256<<20)
	const marker = `"history_complete":false`
	gap := func(category, kind, object string, data any) {
		record(t, s, logstore.Event{Category: category, Kind: "gap." + kind, Object: object, Data: data})
	}
	fill := func(n int) {
		for range n / 100 {
			batch := make([]logstore.Event, 100)
			for i := range batch {
				batch[i] = logstore.Event{Category: logstore.Runtime, Kind: "program", Data: map[string]any{"msg": strings.Repeat("program output ", 256), "history_complete": true}}
			}
			s.ReportBatch(batch)
			if err := s.Flush(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	gap(logstore.Runtime, "first", "", map[string]any{"history_complete": false})
	fill(500)
	gap(logstore.Runtime, "mixed-case", "", json.RawMessage(`{"History_Complete":false}`))
	fill(500)
	gap(logstore.Access, "access", "", map[string]any{"history_complete": false})
	gap(logstore.Runtime, "expired", "expire-me", map[string]any{"history_complete": false})
	gap(logstore.System, "logstore", "", map[string]any{"history_complete": false})

	seqs := func(events []logstore.Event) []int64 {
		out := []int64{}
		for _, e := range events {
			out = append(out, e.Seq)
		}
		return out
	}
	// Ground truth without a text search: every gap.* record carries the marker.
	truth := func(category string) []int64 {
		return seqs(query(t, s, logstore.Filter{Category: category, KindPrefix: "gap.", Limit: 1000}))
	}
	read := func(f logstore.Filter) []int64 {
		t.Helper()
		p, err := s.ReadPage(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return seqs(p.Events)
	}
	check := func(stage string) {
		t.Helper()
		for _, category := range []string{logstore.Runtime, logstore.System, logstore.Access, ""} {
			want := truth(category)
			if len(want) == 0 {
				t.Fatalf("%s: fixture has no %q gaps", stage, category)
			}
			for _, search := range []string{marker, `"HISTORY_COMPLETE":FALSE`} {
				if got := read(logstore.Filter{Category: category, Search: search, Limit: 10}); !slices.Equal(got, want) {
					t.Fatalf("%s: %q search %q got %v want %v", stage, category, search, got, want)
				}
			}
			if got := read(logstore.Filter{Category: category, Search: marker, NewestFirst: true, Limit: 10}); !slices.Equal(got, reversed(want)) {
				t.Fatalf("%s: %q newest-first got %v want reversed %v", stage, category, got, want)
			}
			var walked []int64
			for f := (logstore.Filter{Category: category, Search: marker, Limit: 1}); ; {
				p, err := s.ReadPage(ctx, f)
				if err != nil {
					t.Fatal(err)
				}
				walked = append(walked, seqs(p.Events)...)
				if !p.More {
					break
				}
				f.After = p.Next
			}
			if !slices.Equal(walked, want) {
				t.Fatalf("%s: %q cursor pages got %v want %v", stage, category, walked, want)
			}
		}
		if got := read(logstore.Filter{Category: logstore.Runtime, Search: marker + " reason", Limit: 10}); len(got) != 0 {
			t.Fatalf("%s: search text beyond the marker was not applied: %v", stage, got)
		}
	}
	check("fresh")
	if err := s.ExpireObject(ctx, logstore.Runtime, "expire-me", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	check("after expiring a gap record")

	// History written by a binary that did not maintain gap evidence.
	s.Close()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecContext(ctx, `DROP TRIGGER IF EXISTS log_gaps_insert; DROP TRIGGER IF EXISTS log_gaps_delete; DROP TABLE IF EXISTS log_gaps;
		INSERT INTO log_events(time_ns,received_ns,category,priority,kind,project,object,data) VALUES (1,1,'runtime',0,'gap.legacy','','','{"history_complete":false}')`)
	if err := errors.Join(err, d.Close()); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, path, 256<<20)
	check("history from an older binary")

	fastest := func(search string) time.Duration {
		best := time.Hour
		for range 3 {
			began := time.Now()
			read(logstore.Filter{Category: logstore.Runtime, Search: search, Limit: 10})
			best = min(best, time.Since(began))
		}
		return best
	}
	scan, gaps := fastest("text that no record contains"), fastest(marker)
	t.Logf("full category search %v, gap evidence search %v", scan, gaps)
	if gaps*5 > scan {
		t.Fatalf("gap evidence search reads the whole category: %v vs full search %v", gaps, scan)
	}
}

func reversed(s []int64) []int64 {
	out := slices.Clone(s)
	slices.Reverse(out)
	return out
}

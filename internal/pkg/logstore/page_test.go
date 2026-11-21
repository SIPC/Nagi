package logstore_test

import (
	"path/filepath"
	"strings"
	"testing"

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
}

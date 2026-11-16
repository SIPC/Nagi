package auditlog

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/auditsink"
)

func TestRecordRotateResume(t *testing.T) {
	old := maxSize
	maxSize = 2000
	defer func() { maxSize = old }()
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		l.Record(Event{Panel: "sha256//p", Actor: "admin", Action: "DeployContainer", Target: "p1/web", Detail: strings.Repeat("x", 50)})
	}
	if n := len(files(dir)); n != maxFiles {
		t.Fatalf("应该只留 %d 个文件，实际 %d", maxFiles, n)
	}
	// 太长的字段截断。
	seq := l.Record(Event{Action: "node_exec", Detail: strings.Repeat("长", 3000)})
	l.Close()

	// 重新打开接着序号写。
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
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
}

func TestShip(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			http.Error(w, "down", 502)
			return
		}
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			var m map[string]any
			_ = json.Unmarshal(sc.Bytes(), &m)
			got = append(got, m)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	l, _ := Open(filepath.Join(dir, "audit"))
	defer l.Close()
	l.Record(Event{Action: "before"})
	s := &Shipper{Log: l, Node: func() string { return "node-a" }, ConfigPath: filepath.Join(dir, "auditsinks.json")}
	s.Load()
	sk := auditsink.Config{ID: "s1", Name: "recv", Type: "http", URL: srv.URL}
	// 两个面板配了同一个目标：只发一份。
	_ = s.SetSinks("p1", []auditsink.Config{sk})
	_ = s.SetSinks("p2", []auditsink.Config{sk})
	l.Record(Event{Action: "a"})
	l.Record(Event{Action: "b"})
	ctx := context.Background()
	s.ShipOnce(ctx)
	mu.Lock()
	if len(got) != 2 || got[0]["action"] != "a" || got[0]["source"] != "node" || got[0]["node"] != "node-a" {
		t.Fatalf("新目标只发设置以后的记录，并带上节点：%v", got)
	}
	fail = true
	mu.Unlock()
	l.Record(Event{Action: "c"})
	s.ShipOnce(ctx)
	if len(s.Status()) != 1 {
		t.Fatal("失败时要记下原因")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	// 重新加载（节点重启）以后从游标处补发。
	s2 := &Shipper{Log: l, ConfigPath: s.ConfigPath}
	s2.Load()
	s2.ShipOnce(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[2]["action"] != "c" {
		t.Fatalf("恢复后应该补发：%v", got)
	}
	if st, _ := os.Stat(s.ConfigPath); st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatal("外送配置里有密钥，文件权限应该是 0600")
	}
}

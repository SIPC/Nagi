package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
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

// External delivery continues across failover without gaps or duplicates: the
// standby receives the primary's sink cursor with the replication stream and
// the promoted panel ships from there, including records the old primary had
// not delivered yet and those written by the promotion. A cursor ahead of what
// the replica holds is clamped, so missing records are never marked sent.
func TestReplicaCarriesSinkCursorAcrossPromotion(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	got := map[float64]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil && m["type"] == nil {
				got[m["id"].(float64)]++
			}
		}
	}))
	defer srv.Close()
	sk := SinkConfig{ID: "h", Type: "http", URL: srv.URL}

	primary, standby := openTestLog(t), openTestLog(t)
	sync := func() {
		for {
			_, frame, err := primary.ReadBatch(ctx, standby.MaxID(ctx), 50, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := standby.MaxID(ctx)
			if _, err := standby.ApplyFrame(ctx, frame, nil); err != nil {
				t.Fatal(err)
			}
			if standby.MaxID(ctx) == before {
				break
			}
		}
		if _, err := standby.ApplyFrame(ctx, CursorFrame(primary.SinkCursors(ctx)), nil); err != nil {
			t.Fatal(err)
		}
	}
	primary.Record(ctx, Event{Category: Operation, Action: "BeforeSink"})
	sh := &Shipper{Log: primary}
	sh.Configure(ctx, []SinkConfig{sk})
	for range 20 {
		primary.Record(ctx, Event{Category: Operation, Action: "Deploy"})
	}
	if !sh.Flush(primary.MaxID(ctx), 20*time.Second) {
		t.Fatalf("primary did not deliver: %+v", sh.Status())
	}
	sh.Stop()
	sync()
	// Written on the primary after its last delivery, replicated, never sent.
	for range 3 {
		primary.Record(ctx, Event{Category: Operation, Action: "Undelivered"})
	}
	sync()

	// A forged cursor beyond the replica's head is clamped to it.
	if _, err := standby.ApplyFrame(ctx, CursorFrame(map[string]int64{"sink:h:cursor": 1 << 40}), nil); err != nil {
		t.Fatal(err)
	}
	if v := standby.Meta(ctx, "sink:h:cursor"); v != strconv.FormatInt(standby.MaxID(ctx), 10) {
		t.Fatalf("cursor past the replica's head: %s", v)
	}
	// Undo the forged cursor: in the real stream cursors come only from the primary.
	_ = standby.SetMeta(ctx, "sink:h:cursor", "21")
	standby.Close()
	promoted, err := Open(ctx, standby.path, standby.log)
	if err != nil {
		t.Fatal(err)
	}
	defer promoted.Close()
	promoted.Record(ctx, Event{Category: Operation, Action: "PromoteStandby"})
	ps := &Shipper{Log: promoted}
	ps.Configure(ctx, []SinkConfig{sk})
	defer ps.Stop()
	if !ps.Flush(promoted.MaxID(ctx), 20*time.Second) {
		t.Fatalf("promoted panel did not deliver: %+v", ps.Status())
	}
	mu.Lock()
	defer mu.Unlock()
	for id := int64(2); id <= promoted.MaxID(ctx); id++ {
		if got[float64(id)] != 1 {
			t.Fatalf("record %d delivered %d times across failover: %v", id, got[float64(id)], got)
		}
	}
}

func TestReplicaCarriesNodeSinkCursorsAcrossPromotion(t *testing.T) {
	ctx := t.Context()
	primary, standby := openTestLog(t), openTestLog(t)
	primary.Record(ctx, Event{Category: Operation, Action: "PanelHead"})
	for key, value := range map[string]string{"sink:h:node:a:cursor": "100", "sink:h:node:b:cursor": "103"} {
		if err := primary.SetMeta(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	_, frame, err := primary.ReadBatch(ctx, 0, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := standby.ApplyFrame(ctx, frame, nil); err != nil {
		t.Fatal(err)
	}
	for _, cursors := range []map[string]int64{primary.SinkCursors(ctx), {"sink:h:node:a:cursor": 99, "sink:h:node:b:cursor": 102}} {
		if _, err := standby.ApplyFrame(ctx, CursorFrame(cursors), nil); err != nil {
			t.Fatal(err)
		}
	}
	standby.Close()
	promoted, err := Open(ctx, standby.path, standby.log)
	if err != nil {
		t.Fatal(err)
	}
	defer promoted.Close()
	var mu sync.Mutex
	got := map[string]map[int64]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			var line struct {
				NodeID string `json:"node_id"`
				Seq    int64  `json:"seq"`
			}
			if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
				t.Error(err)
				continue
			}
			if got[line.NodeID] == nil {
				got[line.NodeID] = map[int64]int{}
			}
			got[line.NodeID][line.Seq]++
		}
	}))
	defer srv.Close()
	sk := SinkConfig{ID: "h", Type: "http", URL: srv.URL}
	sh := &Shipper{Log: promoted, client: srv.Client()}
	for node, start := range map[string]int64{"a": 100, "b": 103} {
		n := NodeSource{ID: node, Read: func(ctx context.Context, after int64, limit int) ([][]byte, int64, int64, int64, error) {
			if after < 0 {
				return nil, 0, 0, 110, nil // Old migration window expired: discovery would skip the backlog.
			}
			var lines [][]byte
			for seq := after + 1; seq <= 110; seq++ {
				lines = append(lines, []byte(`{"seq":`+strconv.FormatInt(seq, 10)+`}`))
			}
			return lines, after + 1, 110, 110, nil
		}}
		for range 2 {
			if err := sh.relayNode(ctx, sk, n); err != nil {
				t.Fatal(err)
			}
		}
		mu.Lock()
		for seq := start + 1; seq <= 110; seq++ {
			if got[node][seq] != 1 {
				t.Fatalf("node %s sequence %d delivered %d times: %v", node, seq, got[node][seq], got)
			}
		}
		if len(got[node]) != int(110-start) || promoted.Meta(ctx, nodeCursorKey("h", node)) != "110" {
			t.Fatalf("node %s cursor or delivered range is wrong: %v", node, got)
		}
		mu.Unlock()
	}
}

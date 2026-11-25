package logstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func openStore(t *testing.T, path string, budget int64) *logstore.Store {
	t.Helper()
	s, err := logstore.Open(t.Context(), path, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func query(t *testing.T, s *logstore.Store, f logstore.Filter) []logstore.Event {
	t.Helper()
	r, err := s.Query(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func record(t *testing.T, s *logstore.Store, e logstore.Event) {
	t.Helper()
	if err := s.Record(t.Context(), e); err != nil {
		t.Fatal(err)
	}
}

func TestNestedParamsAndCommandSecretsAreRedactedBeforeStorage(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	for _, input := range []string{
		`mysql -pSensitive123 db01`,
		`mysql -p Sensitive123 db01`,
		`mysql --password Sensitive123 db01`,
		"mysql --password=" + "Sensitive123 db01",
		`mysql -p'Sensitive 123' db01`,
		`mysql --password "Sensitive 123" db01`,
		`mysql --password=Sensi'tive 123' db01`,
		`mysql --password Sensi\ tive123 db01`,
		"tool --token Sensitive123 --api-key=" + "Sensitive456 db01",
		`tool -k Sensitive123 db01`,
		`openssl enc -K Sensitive123 -in db01`,
		`mysql --password "Sensi\"tive 123" db01`,
		`db01 mysql --password "Sensitive 123`,
		`db01 mysql --password 'Sensitive 123`,
		`db01 mysql --password "Sensitive123\`,
		`tool --access-token 'Sensitive 123' --secret-access-key Sensitive456 db01`,
		`tool --private-key=Sensitive123 --credential Sensitive456 db01`,
		`postgres://user:Sensitive123@db01:5432/app`,
		`mongodb+srv://user:Sensitive123@db01/app`,
		`user:Sensitive123@db01`,
		`https://user:Sensitive123@db01/private?token=Sensitive456`,
		"db01 Bearer\tSensitive123",
		"db01 Basic\fSensitive123",
		"password\rSensitive123 db01",
		"db01 -----BEGIN PRIVATE KEY-----\nSensitive123\n-----END PRIVATE KEY-----",
		`{"password":"Sensitive123", "target":"db01"`,
		`{"target":"db01"} --password Sensitive123`,
		`{"key":"Sensitive123","target":"db01"}`,
		`[{"command":"Sensitive123","target":"db01"}]`,
		`{"args":[{"private_key":"Sensitive123","target":"db01"}],"token":"Sensitive456"}`,
		`{"args":[{"api-key":"Sensitive123","credential":{"nested":"Sensitive456"}}],"target":"db01"}`,
		`{"\u0070assword":"Sensitive123","target":"db01","count":9007199254740993}`,
	} {
		t.Run(input, func(t *testing.T) {
			record(t, s, logstore.Event{Category: logstore.Audit, Kind: "params.test", Data: map[string]any{"params": input, "error": input}})
			rows := query(t, s, logstore.Filter{Category: logstore.Audit, Limit: 1000})
			var data map[string]string
			if err := json.Unmarshal(rows[len(rows)-1].Data.(json.RawMessage), &data); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"params", "error"} {
				if strings.Contains(data[field], "Sensitive") || strings.Contains(data[field], "Sensi") || !strings.Contains(data[field], "db01") || data[field] == input {
					t.Fatalf("%s retained a secret or lost safe context: %s", field, data[field])
				}
				if json.Valid([]byte(input)) && !json.Valid([]byte(data[field])) {
					t.Fatalf("JSON became invalid: %s", data[field])
				}
				if strings.Contains(input, "9007199254740993") && !strings.Contains(data[field], "9007199254740993") {
					t.Fatalf("JSON integer lost precision: %s", data[field])
				}
			}
		})
	}
	// 普通参数不是秘密；引用编号按原规则保留，数组和多层 JSON 字符串也递归检查。
	params := `{"target":"db01","token_id":"reference-only","args":[{"key":"Sensitive123","ok":true}]}`
	wrapper, _ := json.Marshal(map[string]any{"payload": params, "name": "readable"})
	record(t, s, logstore.Event{Category: logstore.Audit, Kind: "nested", Data: map[string]any{"params": string(wrapper), "error": "ordinary failure"}})
	rows := query(t, s, logstore.Filter{Category: logstore.Audit, Limit: 1000})
	body := string(rows[len(rows)-1].Data.(json.RawMessage))
	for _, want := range []string{"reference-only", "ordinary failure", "readable", "db01"} {
		if !strings.Contains(body, want) {
			t.Fatalf("safe context %q was lost: %s", want, body)
		}
	}
	if strings.Contains(body, "Sensitive") {
		t.Fatalf("nested JSON escaped redaction: %s", body)
	}
}

func TestRedactBareCredentialAssignments(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"password=Sensitive123", "password=[已隐藏]"},
		{"token=Sensitive123", "token=[已隐藏]"},
		{"command=Sensitive123", "command=[已隐藏]"},
		{"PASSWORD=Sensitive123", "PASSWORD=[已隐藏]"},
		{"private_key=Sensitive123", "private_key=[已隐藏]"},
		{"plainOutput123", "plainOutput123"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			input, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			out, err := logstore.RedactJSON(input)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("redacted string: got %q, want %q", got, tc.want)
			}
			s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), 2<<20)
			record(t, s, logstore.Event{Category: logstore.Runtime, Kind: "container.output", Data: map[string]string{"text": tc.input}})
			if err := s.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows := query(t, s, logstore.Filter{Category: logstore.Runtime})
			if len(rows) != 1 {
				t.Fatalf("stored rows: got %d, want 1", len(rows))
			}
			var stored map[string]string
			if err := json.Unmarshal(rows[0].Data.(json.RawMessage), &stored); err != nil {
				t.Fatal(err)
			}
			if stored["text"] != tc.want {
				t.Fatalf("stored text: got %q, want %q", stored["text"], tc.want)
			}
		})
	}
}

func TestLocalHistorySurvivesRestartAndUsesExclusiveCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	s := openStore(t, path, 2<<20)
	start := time.Date(2026, 11, 20, 1, 2, 3, 123456789, time.UTC)
	for i := range 3 {
		record(t, s, logstore.Event{Time: start.Add(time.Duration(i) * time.Second), Category: logstore.Access, Kind: "http", Project: "p", Data: map[string]any{"status": 401 + i, "bytes": int64(9007199254740993)}})
	}
	first := query(t, s, logstore.Filter{Project: "p", Limit: 1})[0]
	if first.Source == "" || !first.Time.Equal(start) || first.Received.IsZero() {
		t.Fatalf("缺失来源/精确事件时间/采集时间: %+v", first)
	}
	if !strings.Contains(string(first.Data.(json.RawMessage)), "9007199254740993") {
		t.Fatal("64 位计数被浮点数舍入")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, path, 2<<20)
	record(t, s, logstore.Event{Category: logstore.Audit, Kind: "restart", Data: "ok"})
	rows := query(t, s, logstore.Filter{After: first.Seq, Limit: 10})
	if len(rows) != 3 || rows[0].Seq != first.Seq+1 || rows[2].Seq != first.Seq+3 || rows[2].Source != first.Source {
		t.Fatalf("重启/翻页丢失、重复或来源变化: %+v", rows)
	}
	rows = query(t, s, logstore.Filter{From: start.Add(time.Second), To: start.Add(2 * time.Second), Category: logstore.Access, Project: "p"})
	if len(rows) != 1 || rows[0].Seq != first.Seq+1 {
		t.Fatalf("闭开时间边界错误: %+v", rows)
	}
	if _, err := s.Query(t.Context(), logstore.Filter{Limit: 1001}); err == nil {
		t.Fatal("查询无界")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Record(ctx, logstore.Event{Category: logstore.Audit, Kind: "canceled"}); err == nil {
		t.Fatal("取消的写入被当成成功")
	}
}

func TestCapacityPrunesOrdinaryBeforeAuditAndKeepsBoundedGaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	const budget = 2 << 20
	s := openStore(t, path, budget)
	if err := s.RecordCheckpoint(t.Context(), logstore.Event{Time: time.Now().Add(-24 * time.Hour), Category: logstore.Audit, Kind: "first.audit", Data: "must survive ordinary flood"}, "input", 7); err != nil {
		t.Fatal(err)
	}
	first := query(t, s, logstore.Filter{})[0]
	fill := func(category string, count int) {
		for i := range count {
			record(t, s, logstore.Event{Time: time.Now().Add(24 * time.Hour), Category: category, Kind: fmt.Sprint(i), Data: strings.Repeat("x", 32<<10)})
			state, err := s.State(t.Context())
			if err != nil || state.DiskBytes > budget {
				t.Fatalf("真实 DB/WAL/SHM 越额: %+v err=%v", state, err)
			}
		}
	}
	fill(logstore.Access, 70)
	rows := query(t, s, logstore.Filter{Category: logstore.Audit})
	if len(rows) != 1 || rows[0].Seq != first.Seq {
		t.Fatal("普通流量淘汰了旧审计")
	}
	fill(logstore.Audit, 75)
	state, err := s.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pruned) != 2 || len(query(t, s, logstore.Filter{Category: logstore.Access})) != 0 {
		t.Fatalf("没有按类别清理/缺口账本不对: %+v", state)
	}
	var removed int64
	for _, p := range state.Pruned {
		if p.Count <= 0 || p.Last < p.First {
			t.Fatalf("清理摘要错误: %+v", p)
		}
		removed += p.Count
	}
	if got := len(query(t, s, logstore.Filter{Limit: 1000})); int64(got)+removed != state.Last {
		t.Fatalf("清理有未记账丢失: retained=%d removed=%d last=%d", got, removed, state.Last)
	}
	s.Close()
	s = openStore(t, path, budget)
	record(t, s, logstore.Event{Category: logstore.Audit, Kind: "after.prune.restart"})
	rows = query(t, s, logstore.Filter{After: state.Last})
	if len(rows) != 1 || rows[0].Seq != state.Last+1 || rows[0].Source != state.Source {
		t.Fatalf("清理重启后复用了编号: %+v", rows)
	}
	if b, err := s.Checkpoint(t.Context(), "input"); err != nil || string(b) != "7" {
		t.Fatalf("历史清理顺便丢掉了来源读取位置: %s %v", b, err)
	}
}

func TestJournalRedactsAndPreservesLoggerGroups(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	var terminal bytes.Buffer
	logger := s.Logger(slog.New(slog.NewTextHandler(&terminal, nil)), logstore.DefaultConfig())
	logger.With("component", "agent").WithGroup("job").With("id", "task-7").With("phase", "pull").WithGroup("result").Info("failed token=topsecret", "status", 502, "authorization", "Bearer abcdef", "url", "https://bob:pass@example.com/private?code=unsafe")
	if err := s.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := query(t, s, logstore.Filter{Category: logstore.Runtime})
	if len(rows) != 1 || terminal.Len() == 0 {
		t.Fatal("结构化日志或原来的终端输出丢失")
	}
	b := rows[0].Data.(json.RawMessage)
	for _, secret := range []string{"topsecret", "abcdef", "bob:pass", "unsafe"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("敏感数据入库: %s", b)
		}
	}
	var data struct {
		Component string
		Job       struct {
			ID, Phase string
			Result    struct{ Status int }
		}
	}
	if err := json.Unmarshal(b, &data); err != nil || data.Component != "agent" || data.Job.ID != "task-7" || data.Job.Phase != "pull" || data.Job.Result.Status != 502 {
		t.Fatalf("With/WithGroup 改变字段层级或丢掉字段: %s err=%v", b, err)
	}
	record(t, s, logstore.Event{Category: logstore.Container, Kind: "stdout", Data: strings.Repeat("长输出", 32<<10)})
	rows = query(t, s, logstore.Filter{Category: logstore.Container})
	b = rows[0].Data.(json.RawMessage)
	if !json.Valid(b) || len(b) > 64<<10 || !bytes.Contains(b, []byte(`"truncated":true`)) {
		t.Fatalf("大记录没有有界且显式地截断: len=%d", len(b))
	}
	s.Close()
	s.Report(logstore.Event{Category: logstore.Runtime, Kind: "closed"})
	// 已关闭库的错误不能递归写日志；Report 自己返回即可。
}

func TestLocalConfigDefaultsAndCapacityRange(t *testing.T) {
	dir := t.TempDir()
	c, err := logstore.LoadConfig(dir)
	if err != nil || c.CapacityGiB != 5 || c.MetricsEverySeconds != 60 || c.TransferProgressSeconds != 5 || c.RecordingFlushSeconds != 5 {
		t.Fatalf("默认配置: %+v err=%v", c, err)
	}
	if !c.HostJournal || c.HostRetrySeconds != 30 || c.HostLookbackHours != 24 || c.HostSSH || c.HostSudo || c.HostSSHFailuresPerMinute != 100 || c.HostSSHSummarySeconds != 10 {
		t.Fatalf("宿主机默认配置: %+v", c)
	}
	for _, input := range []string{`{"host_retry_seconds":0}`, `{"host_retry_seconds":3601}`, `{"host_lookback_hours":0}`, `{"host_lookback_hours":169}`, `{"host_ssh_failures_per_minute":0}`, `{"host_ssh_failures_per_minute":1001}`, `{"host_ssh_summary_seconds":0}`, `{"host_ssh_summary_seconds":61}`} {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(input), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := logstore.LoadConfig(dir); err == nil {
			t.Errorf("接受了无效宿主机配置: %s", input)
		}
	}
	for _, tc := range []struct {
		json string
		ok   bool
	}{{`{"capacity_gib":10}`, true}, {`{"capacity_gib":4}`, false}, {`{"capacity_gib":11}`, false}, {`{"metrics_every_seconds":15}`, false}, {`{"metrics_every_seconds":0}`, false}, {`{"container_rescan_seconds":0}`, false}, {`{"container_retry_seconds":3601}`, false}, {`{"transfer_progress_seconds":0}`, false}, {`{"transfer_progress_seconds":3601}`, false}, {`{"transfer_progress_seconds":1}`, true}, {`{"recording_flush_seconds":0}`, false}, {`{"recording_flush_seconds":61}`, false}, {`{"recording_flush_seconds":1}`, true}, {`{"recording_flush_seconds":60}`, true}, {`{"runtime_level":"trace"}`, false}} {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.json), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := logstore.LoadConfig(dir)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v", tc.json, err)
		}
	}
}

func TestHistoryAndCheckpointCommitTogetherAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	s := openStore(t, path, 2<<20)
	e := logstore.Event{Category: logstore.Container, Kind: "output", Data: "first"}
	if err := s.RecordCheckpoint(t.Context(), e, "container-a", 7); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.RecordCheckpoint(ctx, e, "container-a", 8); err == nil {
		t.Fatal("失败记录却确认了位置")
	}
	// Waiting for a busy store wrote nothing and keeps the position, so the
	// collector retries from it: not a write failure, no alert count (裸金属).
	before, err := s.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	whileBusy := func(write func(context.Context) error) error {
		release := logstore.HoldGate(s)
		defer release()
		busy, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		return write(busy)
	}
	err = whileBusy(func(ctx context.Context) error { return s.RecordCheckpoint(ctx, e, "container-a", 8) })
	if !errors.Is(err, logstore.ErrBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for the gate should report ErrBusy: %v", err)
	}
	if st, _ := s.State(t.Context()); st.WriteErrors != before.WriteErrors {
		t.Fatalf("a busy write retried from its position counted as a failure: %d → %d", before.WriteErrors, st.WriteErrors)
	}
	// Without a position nothing retries the record: it is lost and counts.
	err = whileBusy(func(ctx context.Context) error {
		return s.Record(ctx, logstore.Event{Category: logstore.Task, Kind: "no.position"})
	})
	if st, _ := s.State(t.Context()); !errors.Is(err, logstore.ErrBusy) || st.WriteErrors != before.WriteErrors+1 {
		t.Fatalf("a busy write without a position must count: %v %d", err, st.WriteErrors)
	}
	s.Close()
	s = openStore(t, path, 2<<20)
	b, err := s.Checkpoint(t.Context(), "container-a")
	if err != nil || string(b) != "7" || len(query(t, s, logstore.Filter{})) != 1 {
		t.Fatalf("失败/重启后读取位置与正文脱节: %s %v", b, err)
	}
}

func TestImportReconcilesHistoryWithoutCollapsingIdenticalLiveFacts(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	e := logstore.Event{Time: time.Date(2026, 11, 20, 1, 2, 3, 123456789, time.UTC),
		Category: logstore.Audit, Kind: "panel.Deploy", Data: map[string]any{"id": 7, "time": 123}}
	record(t, s, e)
	old := e
	old.Time = e.Time.Truncate(time.Millisecond)
	if err := s.ImportCheckpoint(t.Context(), old, "legacy", 7); err != nil {
		t.Fatal(err)
	}
	if got := len(query(t, s, logstore.Filter{})); got != 1 {
		t.Fatalf("millisecond legacy copy duplicated live fact: %d", got)
	}
	if b, err := s.Checkpoint(t.Context(), "legacy"); err != nil || string(b) != "7" {
		t.Fatalf("reconciliation did not commit source position: %s %v", b, err)
	}
	// Same timestamp and payload is not enough to discard a live observation.
	record(t, s, e)
	old.Data = map[string]any{"id": 8, "time": 123}
	if err := s.ImportCheckpoint(t.Context(), old, "legacy", 8); err != nil {
		t.Fatal(err)
	}
	if got := len(query(t, s, logstore.Filter{})); got != 3 {
		t.Fatalf("distinct historical or identical live facts collapsed: %d", got)
	}
}

func TestExpireObjectPreservesOtherHistoryAndCheckpoint(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	ctx := t.Context()
	// Retention uses local receive time, not an untrusted event timestamp.
	if err := s.RecordCheckpoint(ctx, logstore.Event{Time: time.Now().Add(24 * time.Hour), Category: logstore.Task, Object: "reports", Kind: "old", Data: "old"}, "reports", 7); err != nil {
		t.Fatal(err)
	}
	old := query(t, s, logstore.Filter{})[0]
	record(t, s, logstore.Event{Time: time.Now().Add(-24 * time.Hour), Category: logstore.Task, Object: "reports", Kind: "recent", Data: "recent"})
	record(t, s, logstore.Event{Category: logstore.Task, Object: "other", Kind: "other", Data: "other"})
	record(t, s, logstore.Event{Category: logstore.Audit, Object: "reports", Kind: "audit", Data: "audit"})
	for range 2 {
		if err := s.ExpireObject(ctx, logstore.Task, "reports", old.Received.Add(time.Nanosecond)); err != nil {
			t.Fatal(err)
		}
	}
	remaining := query(t, s, logstore.Filter{})
	if len(remaining) != 3 || remaining[0].Kind != "recent" || remaining[1].Kind != "other" || remaining[2].Kind != "audit" {
		t.Fatalf("expired unrelated or recent history: %+v", remaining)
	}
	if b, err := s.Checkpoint(ctx, "reports"); err != nil || string(b) != "7" {
		t.Fatalf("expiration reset checkpoint: %s %v", b, err)
	}
	state, err := s.State(ctx)
	if err != nil || len(state.Pruned) != 1 || state.Pruned[0].Category != logstore.Task || state.Pruned[0].Count != 1 || state.Pruned[0].First != old.Seq || state.Pruned[0].Last != old.Seq {
		t.Fatalf("expiration envelope or repeated cleanup count incorrect: %+v %v", state.Pruned, err)
	}
}

func TestAlertLedgersShareQuotaAndFailAtomically(t *testing.T) {
	const budget = 2 << 20
	s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), budget)
	ctx := t.Context()
	ledger := []byte(`{"current":"` + strings.Repeat("x", 768<<10) + `"}`)
	committed := 0
	for i := range 5 {
		node := fmt.Sprint(i)
		err := s.SaveAlertLedger(ctx, node, ledger, logstore.Event{Kind: "alert.test", Object: node})
		state, stateErr := s.State(ctx)
		if stateErr != nil || state.DiskBytes > budget {
			t.Fatalf("alert ledger exceeded shared disk quota: %+v %v", state, stateErr)
		}
		b, readErr := s.Checkpoint(ctx, "alerts/state/"+node)
		if err != nil {
			if readErr != nil || len(b) != 0 {
				t.Fatalf("failed ledger left a partial checkpoint: %s %v", b, readErr)
			}
			if events := query(t, s, logstore.Filter{Category: logstore.Alert, Object: node}); len(events) != 0 {
				t.Fatalf("failed ledger left a history event: %v", events)
			}
			if committed == 0 {
				t.Fatalf("fresh store rejected the first bounded ledger: %v", err)
			}
			return
		}
		committed++
		if readErr != nil || string(b) != string(ledger) {
			t.Fatalf("committed alert ledger changed: %v", readErr)
		}
	}
	t.Fatal("checkpoints did not consume shared quota")
}

func BenchmarkLocalJournal(b *testing.B) {
	s, err := logstore.Open(context.Background(), filepath.Join(b.TempDir(), "logs.db"), 8<<20)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	logger := s.Logger(slog.New(slog.NewTextHandler(io.Discard, nil)), logstore.DefaultConfig())
	b.ResetTimer()
	for b.Loop() {
		logger.Info("request", "status", 401)
	}
}

// A history that sits past the cleanup line (CN-001 after the v0.37.1
// migration) must not make each write pay for the whole cleanup: the old code
// trimmed down to 80% inside every write, which took longer than any writer's
// deadline, so all writes failed. Writes now stay fast while the background
// cleanup brings usage back under the line.
func TestWritesStayFastWhileArchivePastCleanupLine(t *testing.T) {
	const budget = 50 << 20
	limit := int64(budget * 4 / 5)
	var size atomic.Int64
	size.Store(limit * 93 / 100)
	s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), budget)
	if err := s.RegisterArchive(t.Context(), logstore.Archive{Name: "history", Priority: 2,
		Size: func() (int64, error) { return size.Load(), nil },
		// One batch on a CN-001-sized history takes longer than a cleanup step,
		// and cancelling it mid-way rolls it back like SQLite does.
		Trim: func(ctx context.Context) (bool, error) {
			select {
			case <-time.After(600 * time.Millisecond):
			case <-ctx.Done():
				return false, ctx.Err()
			}
			size.Add(-limit / 200) // 0.5% of the limit per batch
			return true, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		started := time.Now()
		err := s.Record(ctx, logstore.Event{Category: logstore.Audit, Kind: "write", Data: i})
		cancel()
		if err != nil || time.Since(started) > 1500*time.Millisecond {
			t.Fatalf("write %d took %v: %v", i, time.Since(started), err)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for size.Load() > limit*82/100 {
		if time.Now().After(deadline) {
			t.Fatalf("background cleanup stalled at %.1f%% of the limit", 100*float64(size.Load())/float64(limit))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := s.Record(t.Context(), logstore.Event{Category: logstore.Audit, Kind: "after"}); err != nil {
		t.Fatal(err)
	}
}

// 裸金属 on v0.37.4 gave every freed page back to the disk after each cleanup
// (and truncated the WAL after every batch), so the file shrank and grew
// again every 3–4 hours and cleanup rewrote about 0.5 GB each time. Below the
// hard limit the freed pages now stay in the file and new records reuse them:
// after a cleanup the file keeps its size and stays within the limit while
// writing continues.
func TestCleanupKeepsFreedPagesForReuse(t *testing.T) {
	const budget = 8 << 20
	limit := int64(budget * 4 / 5)
	path := filepath.Join(t.TempDir(), "logs.db")
	s := openStore(t, path, budget)
	size := func() int64 {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.Size()
	}
	fill := func(prefix string, until func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for i := 0; !until(); i++ {
			record(t, s, logstore.Event{Category: logstore.Metrics, Kind: "sample", Data: strings.Repeat(prefix, 8<<10) + fmt.Sprint(i)})
			st, err := s.State(t.Context())
			if err != nil || st.DiskBytes > limit {
				t.Fatalf("files past the limit: %+v %v", st.DiskBytes, err)
			}
			if time.Now().After(deadline) {
				t.Fatal("cleanup never ran")
			}
		}
	}
	// Write until the background cleanup has pruned once (the line is 90%).
	pruned := func() bool {
		st, err := s.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return len(st.Pruned) > 0
	}
	fill("a", pruned)
	time.Sleep(500 * time.Millisecond) // let the cleanup finish its round
	before := size()
	if before < limit*80/100 {
		t.Fatalf("cleanup gave freed pages back to the disk: file %d of limit %d", before, limit)
	}
	// Another round of the same volume reuses the free pages: the file does
	// not grow by what was written, and stays under the limit.
	written := 0
	fill("b", func() bool { written++; return written > 200 })
	if after := size(); after > limit || after > before+before/10 {
		t.Fatalf("file grew from %d to %d instead of reusing free pages", before, after)
	}
}

// 裸金属 on v0.37.4: chart history (an archive at the metrics priority) was
// trimmed before the journal's own metrics records, so every cleanup emptied
// the charts while journal metrics records from days earlier stayed. At equal
// priority the journal's records go first now; the archive is trimmed only
// once the journal has none left at that priority.
func TestCleanupTrimsJournalBeforeArchiveAtEqualPriority(t *testing.T) {
	const budget = 8 << 20
	limit := int64(budget * 4 / 5)
	var size, trims atomic.Int64
	size.Store(limit / 2)
	s := openStore(t, filepath.Join(t.TempDir(), "logs.db"), budget)
	if err := s.RegisterArchive(t.Context(), logstore.Archive{Name: "charts", Priority: 2,
		Size: func() (int64, error) { return size.Load(), nil },
		Trim: func(context.Context) (bool, error) {
			trims.Add(1)
			size.Add(-limit / 50)
			return true, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Journal metrics records fill the rest past the cleanup line (90%).
	for i := 0; ; i++ {
		st, err := s.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if st.DiskBytes > limit*92/100 {
			break
		}
		record(t, s, logstore.Event{Category: logstore.Metrics, Kind: "sample", Data: strings.Repeat("m", 8<<10) + fmt.Sprint(i)})
	}
	first := query(t, s, logstore.Filter{Category: logstore.Metrics, Limit: 1})[0].Seq
	// Further writes keep usage past the line and wake the cleanup; wait until
	// it has freed something, then see what it freed.
	deadline := time.Now().Add(20 * time.Second)
	for i := 0; ; i++ {
		record(t, s, logstore.Event{Category: logstore.Metrics, Kind: "sample", Data: strings.Repeat("w", 8<<10) + fmt.Sprint(i)})
		if n := trims.Load(); n != 0 {
			t.Fatalf("chart history trimmed %d times while journal metrics records remained", n)
		}
		if now := query(t, s, logstore.Filter{Category: logstore.Metrics, Limit: 1}); len(now) == 0 || now[0].Seq != first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup never pruned the oldest journal metrics records")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

package logstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	if err != nil || c.CapacityGiB != 5 || c.MetricsEverySeconds != 10 {
		t.Fatalf("默认配置: %+v err=%v", c, err)
	}
	for _, tc := range []struct {
		json string
		ok   bool
	}{{`{"capacity_gib":10}`, true}, {`{"capacity_gib":4}`, false}, {`{"capacity_gib":11}`, false}, {`{"metrics_every_seconds":15}`, false}, {`{"metrics_every_seconds":0}`, false}, {`{"container_rescan_seconds":0}`, false}, {`{"container_retry_seconds":3601}`, false}, {`{"runtime_level":"trace"}`, false}} {
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

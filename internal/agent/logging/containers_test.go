package logging_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/logging"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

type historySource struct {
	instances []docker.Instance
	output    map[string][]docker.LogLine
	events    []docker.EngineEvent
	delivered chan string
}

func (s *historySource) AllInstances(context.Context) ([]docker.Instance, error) {
	return s.instances, nil
}
func (s *historySource) LogsSince(ctx context.Context, c docker.Instance, since time.Time, _ int, emit func(docker.LogLine) error) error {
	for _, l := range s.output[c.ID] {
		if !since.IsZero() && l.Time.Before(since) {
			continue
		}
		if err := emit(l); err != nil {
			return err
		}
	}
	select {
	case s.delivered <- c.ID:
	case <-ctx.Done():
	}
	<-ctx.Done()
	return ctx.Err()
}
func (s *historySource) EventsSince(ctx context.Context, since time.Time, emit func(docker.EngineEvent) error) error {
	for _, e := range s.events {
		if !since.IsZero() && e.Time.Before(since) {
			continue
		}
		if err := emit(e); err != nil {
			return err
		}
	}
	select {
	case s.delivered <- "events":
	case <-ctx.Done():
	}
	<-ctx.Done()
	return ctx.Err()
}

func runHistory(t *testing.T, src logging.ContainerSource, delivered <-chan string, count int, logs *logstore.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var wg sync.WaitGroup
	cfg := logstore.DefaultConfig()
	cfg.ContainerRetrySeconds = 1
	wg.Go(func() { logging.RunContainers(ctx, src, logs, cfg) })
	defer wg.Wait()
	defer cancel()
	for range count {
		select {
		case <-delivered:
		case <-ctx.Done():
			t.Fatal("后台采集没有完成", ctx.Err())
		}
	}
}

func TestBackgroundHistoryResumesAcrossRestartAndKeepsLegitimateDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	open := func() *logstore.Store {
		s, err := logstore.Open(t.Context(), path, 2<<20)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	logs := open()
	t.Cleanup(func() { logs.Close() })
	at := time.Date(2026, 11, 20, 0, 0, 0, 999, time.UTC)
	a := docker.Instance{ID: "old-instance", Project: "p", Container: "app", State: "running"}
	lines := []docker.LogLine{
		{Stream: "stderr", Time: at, Text: "older stderr"},
		{Stream: "stdout", Time: at.Add(time.Second), Text: "same"},
		{Stream: "stdout", Time: at.Add(time.Second), Text: "same"},
		{Stream: "stdout", Time: at.Add(2 * time.Second), Text: "last"},
		{Stream: "stdout", Time: at.Add(1500 * time.Millisecond), Text: "late line"},
	}
	events := []docker.EngineEvent{{ID: a.ID, Time: at, Type: "container", Action: "start"}, {ID: a.ID, Time: at, Type: "container", Action: "start"}}
	run := func(s *historySource) {
		t.Helper()
		runHistory(t, s, s.delivered, len(s.instances)+1, logs)
	}
	run(&historySource{instances: []docker.Instance{a}, output: map[string][]docker.LogLine{a.ID: lines}, events: events, delivered: make(chan string)})
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	logs = open()
	defer logs.Close()
	// 同一纳秒继续出现一条合法新行；重建后同组容器有另一个实例，不能沿用旧实例的读取位置。
	b := a
	b.ID = "new-instance"
	b.Project = ""
	lines = append(lines, docker.LogLine{Stream: "stdout", Time: at.Add(2 * time.Second), Text: "same timestamp new"}, docker.LogLine{Stream: "stderr", Time: at.Add(3 * time.Second), Text: "token=private-value"})
	events = append(events, docker.EngineEvent{ID: b.ID, Time: at, Type: "container", Action: "start"})
	run(&historySource{instances: []docker.Instance{a, b}, output: map[string][]docker.LogLine{
		a.ID: lines, b.ID: {{Stream: "stdout", Time: at, Text: "new instance"}},
	}, events: events, delivered: make(chan string)})
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Container})
	if err != nil || len(rows) != 8 {
		t.Fatalf("重启丢行或重放重复入库: rows=%d err=%v", len(rows), err)
	}
	texts := map[string]int{}
	for _, row := range rows {
		var data struct{ Text string }
		if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
			t.Fatal(err)
		}
		texts[data.Text]++
		if row.Object == b.ID && row.Project != "" {
			t.Fatal("未托管实例被错误归到项目")
		}
	}
	if texts["same"] != 2 || texts["same timestamp new"] != 1 || texts["new instance"] != 1 || texts["token=[已隐藏]"] != 1 || texts["late line"] != 1 {
		t.Fatalf("合法重复/边界追加/重建/脱敏错误: %+v", texts)
	}
	rows, err = logs.Query(t.Context(), logstore.Filter{Category: logstore.Task})
	if err != nil || len(rows) != 3 {
		t.Fatalf("事件重放去重错误: %+v %v", rows, err)
	}
}

type restartingSource struct {
	*historySource
	calls atomic.Int32
}

func (s *restartingSource) LogsSince(ctx context.Context, c docker.Instance, since time.Time, limit int, emit func(docker.LogLine) error) error {
	if s.calls.Add(1) == 1 {
		if err := emit(s.output[c.ID][0]); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	return s.historySource.LogsSince(ctx, c, since, limit, emit)
}

func TestBackgroundHistoryReconnectsWithoutConfirmingUnreadTail(t *testing.T) {
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	at := time.Now().Add(-time.Hour)
	src := &restartingSource{historySource: &historySource{
		instances: []docker.Instance{{ID: "a", State: "running"}},
		output: map[string][]docker.LogLine{"a": {
			{Stream: "stdout", Time: at, Text: "first"},
			{Stream: "stdout", Time: at.Add(time.Second), Text: "unread before disconnect"},
		}}, delivered: make(chan string),
	}}
	runHistory(t, src, src.delivered, 2, logs)
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Container})
	if err != nil || len(rows) != 2 {
		t.Fatalf("重连重放重复/丢掉未读尾部: %+v %v", rows, err)
	}
	var tail struct{ Text string }
	if err := json.Unmarshal(rows[1].Data.(json.RawMessage), &tail); err != nil || tail.Text != "unread before disconnect" {
		t.Fatalf("重连后的尾部错误: %+v %v", tail, err)
	}
}

// rescanSource 每次重扫时 Docker 的运行时长文字都在变；从第 exitAt 次起容器已退出。
type rescanSource struct {
	*historySource
	scans  atomic.Int32
	exitAt int32
}

func (s *rescanSource) AllInstances(context.Context) ([]docker.Instance, error) {
	n := s.scans.Add(1)
	c := s.instances[0]
	c.Status = fmt.Sprintf("Up %d seconds", n)
	if s.exitAt > 0 && n >= s.exitAt {
		c.State, c.Status = "exited", "Exited (0)"
	}
	return []docker.Instance{c}, nil
}

func TestContainerStateWrittenOnlyOnChangeAndExecDoesNotRescan(t *testing.T) {
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	run := func(src *rescanSource, rescan int, until func() bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		cfg := logstore.DefaultConfig()
		cfg.ContainerRescanSeconds, cfg.ContainerRetrySeconds = rescan, 1
		var wg sync.WaitGroup
		wg.Go(func() { logging.RunContainers(ctx, src, logs, cfg) })
		for !until() {
			if ctx.Err() != nil {
				t.Fatal("后台采集没有完成")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		wg.Wait()
		if err := logs.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	states := func() int {
		rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Metrics, KindPrefix: "container.state"})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	app := docker.Instance{ID: "a", Project: "p", Container: "app", State: "running"}

	// 健康检查每次都产生 exec_create/start/die，它们不改变容器清单，不能触发重扫。
	var events []docker.EngineEvent
	at := time.Now()
	for i := range 5 {
		for _, action := range []string{"exec_create: sh", "exec_start: sh", "exec_die"} {
			events = append(events, docker.EngineEvent{ID: app.ID, Time: at.Add(time.Duration(i) * time.Millisecond), Type: "container", Action: action})
		}
	}
	execs := &rescanSource{historySource: &historySource{instances: []docker.Instance{app}, events: events, delivered: make(chan string, 16)}}
	var settled time.Time
	run(execs, 3600, func() bool {
		select {
		case id := <-execs.delivered:
			if id == "events" {
				settled = time.Now().Add(300 * time.Millisecond) // 留时间给被误触发的重扫
			}
		default:
		}
		return !settled.IsZero() && time.Now().After(settled)
	})
	if n := execs.scans.Load(); n != 1 {
		t.Fatalf("exec 事件触发了 %d 次额外重扫", n-1)
	}

	// 只有运行时长文字在变时不重写快照；状态真的变化（退出）时立即写。
	before := states()
	src := &rescanSource{historySource: &historySource{instances: []docker.Instance{app}, delivered: make(chan string, 16)}, exitAt: 4}
	run(src, 1, func() bool { return src.scans.Load() >= 5 })
	if got := states() - before; got != 2 {
		t.Fatalf("5 次重扫（第 4 次起已退出）应写 2 条状态快照，实际 %d 条", got)
	}
}

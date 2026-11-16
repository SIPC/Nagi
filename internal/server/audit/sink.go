package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/auditsink"
)

// 日志外送（见 docs/audit.md#外送）：面板被攻破、机器被格式化以后，日志在别处还有一份。每个目标一个游标，
// 发送失败时游标不动，恢复后从游标处补发，不另存一份数据。每小时和每 1000 条把最新的哈希作为锚点发出去。

// SinkConfig 是一个外送目标（定义在 pkg/auditsink，节点外送本地审计也用它）。
type SinkConfig = auditsink.Config

// SinkStatus 是一个目标的发送状态。
type SinkStatus struct {
	Cursor      int64     `json:"cursor"`
	LastSuccess time.Time `json:"last_success"`
	LastError   string    `json:"last_error,omitempty"`
	FailingFrom time.Time `json:"failing_from"`
}

// Anchor 是发往外送目标的锚点：链上最新的一条记录。
type Anchor struct {
	Time time.Time `json:"time"`
	ID   int64     `json:"id"`
	Hash string    `json:"hash"`
	// Tombstones 是当时的墓碑个数：和外送目标上的清理记录对照，墓碑突然变多说明有人删了记录。
	Tombstones int64 `json:"tombstones"`
}

const (
	sinkInterval   = 2 * time.Second
	s3Interval     = time.Hour
	anchorInterval = time.Hour
	anchorEvery    = 1000
	sinkBatch      = 500
	// sinkAlertAfter：连续失败这么久告警。
	sinkAlertAfter = 10 * time.Minute
)

// Shipper 把审计记录外送到配置的目标。
type Shipper struct {
	Log        *Log
	Recordings *Recordings
	// OnFailure 在某个目标连续失败超过 10 分钟时调用（failing=true），恢复时再调用一次。
	OnFailure func(ctx context.Context, sink SinkConfig, failing bool, err string)

	mu      sync.Mutex
	sinks   []SinkConfig
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	status  map[string]*SinkStatus
	alerted map[string]bool
	client  *http.Client
}

// Configure 换成新的目标列表，重启发送。
func (s *Shipper) Configure(ctx context.Context, sinks []SinkConfig) {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		s.status, s.alerted = map[string]*SinkStatus{}, map[string]bool{}
		s.client = &http.Client{Timeout: 30 * time.Second}
	}
	s.sinks = sinks
	ctx, s.cancel = context.WithCancel(context.WithoutCancel(ctx))
	for _, sk := range sinks {
		st := s.status[sk.ID]
		if st == nil {
			st = &SinkStatus{}
			s.status[sk.ID] = st
		}
		if c, err := strconv.ParseInt(s.Log.Meta(ctx, "sink:"+sk.ID+":cursor"), 10, 64); err == nil {
			st.Cursor = c
		} else {
			// 新目标从现在开始发，不补发以前的记录（导出可以拿到全部）。
			st.Cursor = s.lastID(ctx)
			_ = s.Log.SetMeta(ctx, "sink:"+sk.ID+":cursor", strconv.FormatInt(st.Cursor, 10))
		}
		s.wg.Add(1)
		go s.run(ctx, sk)
	}
}

// Stop 停止发送。
func (s *Shipper) Stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Status 返回各目标的状态。
func (s *Shipper) Status() map[string]SinkStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]SinkStatus{}
	for id, st := range s.status {
		out[id] = *st
	}
	return out
}

// Flush 等所有目标把 ID 不大于 id 的记录发出去，最多等 timeout。
func (s *Shipper) Flush(id int64, timeout time.Duration) bool { return s.flush(id, timeout, false) }

// FlushHealthy 同 Flush，但不等正在失败的目标：反正发不出去，等满 timeout 只会让操作卡住。
// 修改外送配置前用它把这条变更发往旧目标。
func (s *Shipper) FlushHealthy(id int64, timeout time.Duration) bool {
	return s.flush(id, timeout, true)
}

func (s *Shipper) flush(id int64, timeout time.Duration, skipFailing bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		done := true
		s.mu.Lock()
		for _, sk := range s.sinks {
			st := s.status[sk.ID]
			if skipFailing && st != nil && st.LastError != "" {
				continue
			}
			if st == nil || st.Cursor < id {
				done = false
			}
		}
		s.mu.Unlock()
		if done {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func (s *Shipper) lastID(ctx context.Context) int64 {
	var id int64
	_ = s.Log.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&id)
	return id
}

func (s *Shipper) run(ctx context.Context, sk SinkConfig) {
	defer s.wg.Done()
	interval := sinkInterval
	if sk.Type == "s3" {
		interval = s3Interval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// 启动时先发一次锚点，之后每小时、每 1000 条一次。
	var lastAnchor time.Time
	sinceAnchor := 0
	for {
		n, err := s.sendBatch(ctx, sk, &lastAnchor, &sinceAnchor)
		s.mark(ctx, sk, err)
		if err == nil && n == sinkBatch {
			continue // 还有积压，接着发
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Shipper) mark(ctx context.Context, sk SinkConfig, err error) {
	s.mu.Lock()
	st := s.status[sk.ID]
	now := time.Now()
	var notify *bool
	if err != nil {
		if st.FailingFrom.IsZero() {
			st.FailingFrom = now
		}
		st.LastError = err.Error()
		if now.Sub(st.FailingFrom) > sinkAlertAfter && !s.alerted[sk.ID] {
			s.alerted[sk.ID] = true
			v := true
			notify = &v
		}
	} else {
		st.LastSuccess, st.LastError, st.FailingFrom = now, "", time.Time{}
		if s.alerted[sk.ID] {
			s.alerted[sk.ID] = false
			v := false
			notify = &v
		}
	}
	msg := st.LastError
	s.mu.Unlock()
	if notify != nil && s.OnFailure != nil {
		s.OnFailure(ctx, sk, *notify, msg)
	}
}

// sendBatch 发一批游标之后的记录，需要时带上锚点；成功后移动游标。返回发出的记录数。
func (s *Shipper) sendBatch(ctx context.Context, sk SinkConfig, lastAnchor *time.Time, sinceAnchor *int) (int, error) {
	s.mu.Lock()
	cursor := s.status[sk.ID].Cursor
	s.mu.Unlock()
	limit := sinkBatch
	if sk.Type == "s3" {
		limit = 1 << 20 // 每小时一个对象，装下这一个小时的全部记录
	}
	var events []Event
	rows, err := s.Log.db.QueryContext(ctx, `SELECT `+columns+` FROM events WHERE id > ? ORDER BY id LIMIT ?`, cursor, limit)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, e)
	}
	rows.Close()
	anchorDue := time.Since(*lastAnchor) >= anchorInterval || *sinceAnchor+len(events) >= anchorEvery
	if len(events) == 0 && !anchorDue {
		return 0, nil
	}
	var lines [][]byte
	for _, e := range events {
		b, _ := json.Marshal(ExportLine(e))
		lines = append(lines, b)
	}
	var anchor *Anchor
	if anchorDue {
		a := Anchor{Time: time.Now()}
		_ = s.Log.db.QueryRowContext(ctx, `SELECT id, hash FROM events ORDER BY id DESC LIMIT 1`).Scan(&a.ID, &a.Hash)
		_ = s.Log.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tombstones`).Scan(&a.Tombstones)
		if a.ID > 0 {
			anchor = &a
			b, _ := json.Marshal(map[string]any{"type": "anchor", "time": a.Time.UTC().Format(time.RFC3339Nano), "id": a.ID, "hash": a.Hash, "tombstones": a.Tombstones})
			lines = append(lines, b)
		}
	}
	if len(lines) == 0 {
		return 0, nil
	}
	if err := s.deliver(ctx, sk, events, lines); err != nil {
		return 0, err
	}
	if len(events) > 0 {
		last := events[len(events)-1].ID
		s.mu.Lock()
		s.status[sk.ID].Cursor = last
		s.mu.Unlock()
		_ = s.Log.SetMeta(ctx, "sink:"+sk.ID+":cursor", strconv.FormatInt(last, 10))
	}
	*sinceAnchor += len(events)
	if anchor != nil {
		*lastAnchor, *sinceAnchor = time.Now(), 0
		b, _ := json.Marshal(anchor)
		_ = s.Log.SetMeta(ctx, "last_anchor", string(b))
	}
	return len(events), nil
}

// LastAnchor 返回最近一次发出的锚点。
func (l *Log) LastAnchor(ctx context.Context) (Anchor, bool) {
	var a Anchor
	v := l.Meta(ctx, "last_anchor")
	return a, v != "" && json.Unmarshal([]byte(v), &a) == nil
}

// ExportLine 是外送和导出的一行：全部字段，时间用 RFC 3339 毫秒，带上哈希链。
func ExportLine(e Event) map[string]any {
	var m map[string]any
	b, _ := json.Marshal(e)
	_ = json.Unmarshal(b, &m)
	m["time"] = e.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	m["prev_hash"], m["hash"] = e.PrevHash, e.Hash
	return m
}

func (s *Shipper) deliver(ctx context.Context, sk SinkConfig, events []Event, lines [][]byte) error {
	switch sk.Type {
	case "http":
		return s.postHTTP(ctx, sk, lines)
	case "syslog":
		return auditsink.SendSyslog(ctx, sk, lines)
	case "s3":
		first := int64(0)
		if len(events) > 0 {
			first = events[0].ID
		}
		key := auditsink.ObjectKey(sk.Prefix, auditsink.PanelScope, first, time.Now())
		if err := auditsink.S3Put(ctx, s.client, sk, key, bytes.Join(lines, []byte("\n")), "application/x-ndjson"); err != nil {
			return err
		}
		if sk.Recordings && s.Recordings != nil {
			for _, e := range events {
				if e.RecordingID != "" {
					if err := s.uploadRecording(ctx, sk, e.RecordingID); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return fmt.Errorf("unknown sink type %q", sk.Type)
}

// TestSink 发一条测试消息。
func (s *Shipper) TestSink(ctx context.Context, sk SinkConfig) error {
	line, _ := json.Marshal(map[string]any{"type": "test", "time": time.Now().UTC().Format(time.RFC3339Nano), "message": "Nagi audit sink test"})
	client := &http.Client{Timeout: 15 * time.Second}
	switch sk.Type {
	case "http":
		return auditsink.PostHTTP(ctx, client, sk, [][]byte{line})
	case "syslog":
		return auditsink.SendSyslog(ctx, sk, [][]byte{line})
	case "s3":
		return auditsink.S3Put(ctx, client, sk, auditsink.ScopedKey(sk.Prefix, auditsink.PanelScope, "nagi-test.json"), line, "application/json")
	}
	return fmt.Errorf("unknown sink type %q", sk.Type)
}

func (s *Shipper) postHTTP(ctx context.Context, sk SinkConfig, lines [][]byte) error {
	return auditsink.PostHTTP(ctx, s.client, sk, lines)
}

// ---- S3（SigV4 签名的 PUT） ----

func (s *Shipper) uploadRecording(ctx context.Context, sk SinkConfig, id string) error {
	if !recordingIDRe.MatchString(id) {
		return nil
	}
	dir := filepath.Join(s.Recordings.Dir, id[:6], id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // 已经被清理
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		key := auditsink.ScopedKey(sk.Prefix, auditsink.PanelScope, "recordings/"+id+"/"+e.Name())
		if err := auditsink.S3Put(ctx, s.client, sk, key, b, "application/octet-stream"); err != nil {
			return err
		}
	}
	return nil
}

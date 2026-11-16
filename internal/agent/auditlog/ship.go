package auditlog

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/auditsink"
)

// 节点外送本地审计：面板下发外送目标（AgentService.SetAuditSink），节点按面板指纹分别保存在
// identity/auditsinks.json（0600），面板挂了照常外送。几个面板配了同一个目标（ID 相同）只发一份。
// 每个目标一个游标（audit/cursors.json），发送失败时游标不动，恢复后补发；新目标从现在开始发。

const (
	shipInterval = 5 * time.Second
	shipBatch    = 500
)

// Shipper 把节点本地审计外送到面板配置的目标。
type Shipper struct {
	Log *Log
	// Node 返回节点在面板上的名称，写进外送的每一行。
	Node func() string
	// NodeID 返回节点 ID（面板分配的），写进外送的每一行。
	NodeID func() string
	// ConfigPath 是保存外送目标的文件（identity/auditsinks.json）。
	ConfigPath string
	Logger     *slog.Logger

	mu      sync.Mutex
	byPanel map[string][]auditsink.Config
	cursors map[string]int64
	failing map[string]string
	client  *http.Client
}

// Load 读取保存的外送目标和游标。
func (s *Shipper) Load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byPanel, s.cursors, s.failing = map[string][]auditsink.Config{}, map[string]int64{}, map[string]string{}
	s.client = &http.Client{Timeout: 30 * time.Second}
	if b, err := os.ReadFile(s.ConfigPath); err == nil {
		_ = json.Unmarshal(b, &s.byPanel)
	}
	if b, err := os.ReadFile(s.cursorPath()); err == nil {
		_ = json.Unmarshal(b, &s.cursors)
	}
}

func (s *Shipper) cursorPath() string { return filepath.Join(s.Log.dir, "cursors.json") }

// SetSinks 保存一个面板下发的外送目标。
func (s *Shipper) SetSinks(panel string, sinks []auditsink.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(sinks) == 0 {
		delete(s.byPanel, panel)
	} else {
		s.byPanel[panel] = sinks
	}
	// 新目标从现在开始发，不补发以前的记录（本地文件里有全部）。
	for _, sk := range s.sinksLocked() {
		if _, ok := s.cursors[sk.ID]; !ok {
			s.cursors[sk.ID] = s.Log.Seq()
		}
	}
	b, err := json.Marshal(s.byPanel)
	if err != nil {
		return err
	}
	if err := writeFile(s.ConfigPath, b); err != nil {
		return err
	}
	return s.saveCursorsLocked()
}

// sinksLocked 合并各面板的目标，同一个 ID 只留一个。
func (s *Shipper) sinksLocked() []auditsink.Config {
	seen := map[string]bool{}
	var out []auditsink.Config
	panels := make([]string, 0, len(s.byPanel))
	for p := range s.byPanel {
		panels = append(panels, p)
	}
	sort.Strings(panels)
	for _, p := range panels {
		for _, sk := range s.byPanel[p] {
			if sk.ID == "" || seen[sk.ID] {
				continue
			}
			seen[sk.ID] = true
			out = append(out, sk)
		}
	}
	return out
}

func (s *Shipper) saveCursorsLocked() error {
	b, err := json.Marshal(s.cursors)
	if err != nil {
		return err
	}
	return writeFile(s.cursorPath(), b)
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Run 每 5 秒外送一次，直到 ctx 结束。
func (s *Shipper) Run(ctx context.Context) {
	t := time.NewTicker(shipInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.ShipOnce(ctx)
	}
}

// ShipOnce 把每个目标游标之后的记录发出去。
func (s *Shipper) ShipOnce(ctx context.Context) {
	s.mu.Lock()
	sinks := s.sinksLocked()
	s.mu.Unlock()
	for _, sk := range sinks {
		for {
			s.mu.Lock()
			cursor := s.cursors[sk.ID]
			s.mu.Unlock()
			lines, first, last := s.Log.After(cursor, shipBatch)
			if len(lines) == 0 {
				break
			}
			if err := auditsink.Send(ctx, s.client, sk, first, s.decorate(lines)); err != nil {
				s.mu.Lock()
				was := s.failing[sk.ID]
				s.failing[sk.ID] = err.Error()
				s.mu.Unlock()
				if was == "" && s.Logger != nil {
					s.Logger.Warn("外送本地审计失败，稍后重试", "sink", sk.Name, "err", err)
				}
				break
			}
			s.mu.Lock()
			if s.failing[sk.ID] != "" && s.Logger != nil {
				s.Logger.Info("外送本地审计已恢复", "sink", sk.Name)
			}
			delete(s.failing, sk.ID)
			s.cursors[sk.ID] = last
			_ = s.saveCursorsLocked()
			s.mu.Unlock()
			if len(lines) < shipBatch {
				break
			}
		}
	}
}

// decorate 给每一行加上来源（source: node）和节点，接收端能和面板的记录分开。
func (s *Shipper) decorate(lines [][]byte) [][]byte {
	name, id := "", ""
	if s.Node != nil {
		name = s.Node()
	}
	if s.NodeID != nil {
		id = s.NodeID()
	}
	out := make([][]byte, 0, len(lines))
	for _, l := range lines {
		var m map[string]any
		if json.Unmarshal(l, &m) != nil {
			continue
		}
		m["source"], m["node_id"], m["node"] = "node", id, name
		b, _ := json.Marshal(m)
		out = append(out, b)
	}
	return out
}

// Status 返回各目标的失败原因（正常的不在里面）。
func (s *Shipper) Status() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.failing {
		out[k] = v
	}
	return out
}

// Cursor 返回一个目标的游标，测试用。
func (s *Shipper) Cursor(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strconv.FormatInt(s.cursors[id], 10)
}

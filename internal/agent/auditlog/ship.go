package auditlog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// Trusted 判断面板还在不在信任列表里。移出信任列表的面板下发的目标不再外送（见 docs/audit.md#节点本地审计）：
	// 拿下面板的人配的目标，管理员把面板移除以后不能还一直收到节点的记录。为 nil 时不检查（测试）。
	Trusted func(fingerprint string) bool
	Logger  *slog.Logger

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
	// 新目标从现在开始发，不补发以前的记录（本地文件里有全部）。旧版本的游标按目标 ID 存，接着用。
	for _, sk := range s.sinksLocked() {
		k := sinkKey(sk)
		if _, ok := s.cursors[k]; !ok {
			if old, ok := s.cursors[sk.ID]; ok {
				s.cursors[k] = old
			} else {
				s.cursors[k] = s.Log.Seq()
			}
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

// sinksLocked 合并仍在信任列表里的面板的目标。主面板和备用面板配的是同一份（ID 和内容都相同），只留一个；
// ID 相同、内容不同的分开发：一个面板不能借同样的 ID 顶替别的面板的目标。
func (s *Shipper) sinksLocked() []auditsink.Config {
	seen := map[string]bool{}
	var out []auditsink.Config
	panels := make([]string, 0, len(s.byPanel))
	for p := range s.byPanel {
		if s.Trusted == nil || s.Trusted(p) {
			panels = append(panels, p)
		}
	}
	sort.Strings(panels)
	for _, p := range panels {
		for _, sk := range s.byPanel[p] {
			if sk.ID == "" || seen[sinkKey(sk)] {
				continue
			}
			seen[sinkKey(sk)] = true
			out = append(out, sk)
		}
	}
	return out
}

// sinkKey 是一个目标的游标和失败状态的键：ID 加上配置的摘要。
func sinkKey(sk auditsink.Config) string {
	b, _ := json.Marshal(sk)
	sum := sha256.Sum256(b)
	return sk.ID + "@" + hex.EncodeToString(sum[:6])
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
			key := sinkKey(sk)
			s.mu.Lock()
			cursor := s.cursors[key]
			s.mu.Unlock()
			lines, first, last := s.Log.After(cursor, shipBatch)
			if len(lines) == 0 {
				break
			}
			if err := auditsink.Send(ctx, s.client, sk, auditsink.NodeScope(s.nodeID()), first, s.decorate(lines)); err != nil {
				s.mu.Lock()
				was := s.failing[key]
				s.failing[key] = err.Error()
				s.mu.Unlock()
				if was == "" && s.Logger != nil {
					s.Logger.Warn("外送本地审计失败，稍后重试", "sink", sk.Name, "err", err)
				}
				break
			}
			s.mu.Lock()
			if s.failing[key] != "" && s.Logger != nil {
				s.Logger.Info("外送本地审计已恢复", "sink", sk.Name)
			}
			delete(s.failing, key)
			s.cursors[key] = last
			_ = s.saveCursorsLocked()
			s.mu.Unlock()
			if len(lines) < shipBatch {
				break
			}
		}
	}
}

func (s *Shipper) nodeID() string {
	if s.NodeID != nil {
		return s.NodeID()
	}
	return ""
}

// decorate 给每一行加上来源（source: node）和节点，接收端能和面板的记录分开。
func (s *Shipper) decorate(lines [][]byte) [][]byte {
	name, id := "", s.nodeID()
	if s.Node != nil {
		name = s.Node()
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
func (s *Shipper) Cursor(sk auditsink.Config) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strconv.FormatInt(s.cursors[sinkKey(sk)], 10)
}

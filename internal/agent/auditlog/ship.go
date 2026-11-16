package auditlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// 节点本地审计由面板代为外送（见 docs/audit.md#节点本地审计）：面板用 AgentService.ReadAuditLog 按游标读走记录，
// 写到 <前缀>/node/<节点 ID>/ 下。节点不持有外送凭据，被攻破的节点写不了面板的记录。面板连不上时记录留在本地
// 文件里（10 个 10 MB 的文件），恢复后面板从游标处接着读。
//
// v0.29.1 以前节点自己外送，凭据存在 identity/auditsinks.json。升级后第一次打开时删掉它，记下以前外送到的位置，
// 面板第一次读时从那里开始，升级前后的记录不会漏发。

const (
	// ReadLimit 是 ReadAuditLog 每次最多返回的条数。
	ReadLimit = 500
	// relayStartFor 是升级前外送位置的有效期：之后新加的外送目标从当时最新的记录开始。
	relayStartFor  = 7 * 24 * time.Hour
	relayStartFile = "relay-start.json"
)

// readMaxBytes 是每次返回的总大小上限，不超过 gRPC 的消息上限（测试里改小）。
var readMaxBytes = 2 << 20

type relayStart struct {
	Seq   int64     `json:"seq"`
	Until time.Time `json:"until"`
}

// MigrateShipping 删掉升级前保存的外送配置（里面有面板的外送凭据）和游标，记下以前外送到的位置。没有时什么也不做。
func (l *Log) MigrateShipping(configPath string) error {
	b, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return nil
	}
	var byPanel map[string][]json.RawMessage
	cursorPath := filepath.Join(l.dir, "cursors.json")
	if err == nil && json.Unmarshal(b, &byPanel) == nil && len(byPanel) > 0 {
		start := int64(-1)
		if cb, err := os.ReadFile(cursorPath); err == nil {
			var cursors map[string]int64
			if json.Unmarshal(cb, &cursors) == nil {
				for _, c := range cursors {
					if start < 0 || c < start {
						start = c
					}
				}
			}
		}
		if start >= 0 {
			rb, _ := json.Marshal(relayStart{Seq: start, Until: time.Now().Add(relayStartFor)})
			if err := writeFile(filepath.Join(l.dir, relayStartFile), rb); err != nil {
				return err
			}
		}
	}
	_ = os.Remove(cursorPath)
	if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Start 是面板第一次读时的起点：升级前外送到的位置（7 天内），否则是最新的序号（新目标不补发以前的记录）。
func (l *Log) Start() int64 {
	seq := l.Seq()
	if b, err := os.ReadFile(filepath.Join(l.dir, relayStartFile)); err == nil {
		var r relayStart
		if json.Unmarshal(b, &r) == nil && time.Now().Before(r.Until) && r.Seq <= seq {
			return r.Seq
		}
	}
	return seq
}

// Read 返回序号大于 after 的记录，最多 limit 条、总共不超过 2 MB。after 比最新的序号还大时（本地审计被清空过）从头返回。
func (l *Log) Read(after int64, limit int) (lines [][]byte, first, last int64) {
	if limit <= 0 || limit > ReadLimit {
		limit = ReadLimit
	}
	if after > l.Seq() {
		after = 0
	}
	all, first, _ := l.After(after, limit)
	size := 0
	for _, line := range all {
		if size+len(line) > readMaxBytes && len(lines) > 0 {
			break
		}
		size += len(line)
		lines = append(lines, line)
		var e struct {
			Seq int64 `json:"seq"`
		}
		_ = json.Unmarshal(line, &e)
		last = e.Seq
	}
	if len(lines) == 0 {
		first = 0
	}
	return lines, first, last
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Package auditlog 是节点本地审计（见 docs/audit.md#节点本地审计）：面板被攻破时，面板上的记录不可信，
// 所以节点自己也记一份——节点终端、node_exec、面板下发的修改类指令、被拒绝的越权请求。
//
// 写在 <数据目录>/audit/ 下，JSON Lines，按 10 MB 轮换，保留 10 个文件。节点也按面板下发的外送配置
// 直接外送（见 ship.go）：节点被攻破后可以删掉本地文件，但已经外送的记录还在。
package auditlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxSize 是每个文件的大小上限（测试里改小）。
var maxSize int64 = 10 << 20

const (
	current  = "audit.jsonl"
	maxFiles = 10
	// maxField 是每个字段的长度上限：来源于请求的内容都可能很长。
	maxField = 2000
)

// Event 是一条节点本地审计记录。
type Event struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	Panel  string    `json:"panel,omitempty"`  // 发起的面板指纹；越权请求是被拒绝的节点公钥
	Actor  string    `json:"actor,omitempty"`  // 面板上的成员（和令牌）
	IP     string    `json:"ip,omitempty"`     // 成员的来源地址
	Action string    `json:"action"`           // 接口方法，或 node_shell、node_exec、violation
	Target string    `json:"target,omitempty"` // 项目/容器、路径等
	Detail string    `json:"detail,omitempty"` // 命令、退出码等
	Result string    `json:"result"`           // ok、failed、denied
	Error  string    `json:"error,omitempty"`
}

// Log 是节点本地审计。零值不可用，用 Open 打开；nil 时 Record 什么也不做。
type Log struct {
	dir string

	mu   sync.Mutex
	f    *os.File
	size int64
	seq  int64
}

// Open 打开（必要时创建）审计目录，接着上次的序号写。
func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Log{dir: dir}
	l.seq = lastSeq(dir)
	if err := l.openCurrent(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) openCurrent() error {
	f, err := os.OpenFile(filepath.Join(l.dir, current), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

func clip(s string) string {
	if len(s) <= maxField {
		return s
	}
	// 按字节截，再去掉被截断的半个字符。
	return strings.ToValidUTF8(s[:maxField], "") + "…"
}

// Record 写一条记录，返回它的序号。写失败只返回 0：审计不能影响节点的正常工作。
func (l *Log) Record(e Event) int64 {
	if l == nil {
		return 0
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Time = e.Time.UTC()
	if e.Result == "" {
		e.Result = "ok"
	}
	e.Actor, e.Target, e.Detail, e.Error = clip(e.Actor), clip(e.Target), clip(e.Detail), clip(e.Error)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	b, err := json.Marshal(e)
	if err != nil {
		return 0
	}
	b = append(b, '\n')
	if l.size+int64(len(b)) > maxSize && l.size > 0 {
		l.rotate()
	}
	if l.f == nil {
		return 0
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	if err != nil {
		return 0
	}
	return e.Seq
}

// rotate 把 audit.jsonl 改名为 audit.<最后的序号>.jsonl，只留最新的 maxFiles 个文件（包括当前的）。调用方持有锁。
func (l *Log) rotate() {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	_ = os.Rename(filepath.Join(l.dir, current), filepath.Join(l.dir, fmt.Sprintf("audit.%020d.jsonl", l.seq-1)))
	old := rotated(l.dir)
	for len(old) > maxFiles-1 {
		_ = os.Remove(filepath.Join(l.dir, old[0]))
		old = old[1:]
	}
	_ = l.openCurrent()
}

// Close 关闭文件。
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Seq 返回最新一条的序号。
func (l *Log) Seq() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// rotated 返回轮换出去的文件名，旧的在前（文件名里的序号补齐了位数，按名字排序就是按时间）。
func rotated(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		n := e.Name()
		if n != current && strings.HasPrefix(n, "audit.") && strings.HasSuffix(n, ".jsonl") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// files 返回所有文件，旧的在前。
func files(dir string) []string {
	return append(rotated(dir), current)
}

func lastSeq(dir string) int64 {
	all := files(dir)
	for i := len(all) - 1; i >= 0; i-- {
		var last int64
		_ = scan(filepath.Join(dir, all[i]), func(e Event) bool {
			last = e.Seq
			return true
		})
		if last > 0 {
			return last
		}
		// 当前文件是空的（刚轮换）：从轮换出去的文件名里取。
		if n := strings.TrimSuffix(strings.TrimPrefix(all[i], "audit."), ".jsonl"); n != all[i] {
			if v, err := strconv.ParseInt(n, 10, 64); err == nil && v > 0 {
				return v
			}
		}
	}
	return 0
}

func scan(path string, fn func(Event) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if !fn(e) {
			return nil
		}
	}
	return sc.Err()
}

// After 返回序号大于 seq 的记录（原样的行），最多 limit 条。
func (l *Log) After(seq int64, limit int) (lines [][]byte, first, last int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, name := range files(l.dir) {
		f, err := os.Open(filepath.Join(l.dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() && len(lines) < limit {
			var e struct {
				Seq int64 `json:"seq"`
			}
			if json.Unmarshal(sc.Bytes(), &e) != nil || e.Seq <= seq {
				continue
			}
			if first == 0 {
				first = e.Seq
			}
			last = e.Seq
			lines = append(lines, append([]byte(nil), sc.Bytes()...))
		}
		f.Close()
		if len(lines) >= limit {
			break
		}
	}
	return lines, first, last
}

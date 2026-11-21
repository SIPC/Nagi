package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/klauspost/compress/zstd"
)

// 终端与命令录制（见 docs/audit.md#终端与命令录制）：录输出、窗口大小和输入（没有回显的字符只记个数，见 recording_input.go）。格式是 asciicast v2，
// 每 256 KB 或每 5 秒切一块，zstd 压缩后用主密钥加密（AES-256-GCM，附加数据是录制 ID 和序号），存为
// <数据目录>/audit/recordings/<年月>/<录制 ID>/<序号>。单个会话保留开头 8 MB 和最后 56 MB，中间丢掉并标记已截断。

const (
	chunkBytes    = 256 << 10
	chunkInterval = 5 * time.Second
	headLimit     = 8 << 20
	tailLimit     = 56 << 20
)

// Sealer 用主密钥加密和解密。
type Sealer interface {
	Seal(plaintext, additional []byte) ([]byte, error)
	Open(ciphertext, additional []byte) ([]byte, error)
}

// Recordings 管理录制文件。
type Recordings struct {
	Dir    string
	Sealer Sealer
	// Enabled 判断现在要不要录制（「设置 → 安全 → 终端录制」）。
	Enabled func(context.Context) bool
	budget  *logstore.Store
	active  sync.Map
}

var recordingIDRe = regexp.MustCompile(`^[0-9]{6}-[0-9a-f]{24}$`)

// Recorder 是一次会话的录制。
type Recorder struct {
	r     *Recordings
	id    string
	dir   string
	start time.Time

	mu        sync.Mutex
	buf       bytes.Buffer
	pending   []byte // 不完整的 UTF-8 字节，留到下一次输出
	seq       int
	headBytes int64
	tail      []chunkInfo
	head      []chunkInfo
	truncated bool
	closed    bool
	err       error
	stop      chan struct{}
	done      chan struct{}
	// typed 是等着回显的输入；lastT 是上一个事件的时间（秒），事件的时间不能倒退。
	typed []typedRun
	lastT float64
}

type chunkInfo struct {
	Seq   int    `json:"seq"`
	Bytes int64  `json:"bytes"`
	Hash  string `json:"sha256"`
}

// Summary 是会话结束时写进审计记录的内容：各块的哈希进入哈希链。
type Summary struct {
	ID        string      `json:"id"`
	Chunks    []chunkInfo `json:"chunks"`
	Bytes     int64       `json:"bytes"`
	Truncated bool        `json:"truncated,omitempty"`
	Seconds   int64       `json:"seconds"`
}

// Start 开始录制，没开录制时返回 nil。cols、rows 是初始的窗口大小。
func (r *Recordings) Start(ctx context.Context, cols, rows int) *Recorder {
	if r == nil || r.Enabled != nil && !r.Enabled(ctx) {
		return nil
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	now := time.Now()
	id := now.Format("200601") + "-" + hex.EncodeToString(b)
	rec := &Recorder{r: r, id: id, dir: filepath.Join(r.Dir, id[:6], id), start: now, stop: make(chan struct{}), done: make(chan struct{})}
	r.active.Store(id, true)
	if err := os.MkdirAll(rec.dir, 0o700); err != nil {
		r.active.Delete(id)
		return nil
	}
	header, _ := json.Marshal(map[string]any{"version": 2, "width": cols, "height": rows, "timestamp": now.Unix()})
	rec.buf.Write(header)
	rec.buf.WriteByte('\n')
	go rec.loop()
	return rec
}

// ID 返回录制 ID。
func (rec *Recorder) ID() string {
	if rec == nil {
		return ""
	}
	return rec.id
}

func (rec *Recorder) loop() {
	defer close(rec.done)
	t := time.NewTicker(chunkInterval)
	defer t.Stop()
	for {
		select {
		case <-rec.stop:
			return
		case <-t.C:
			rec.mu.Lock()
			rec.resolveStaleLocked(time.Now())
			rec.flushLocked()
			rec.mu.Unlock()
		}
	}
}

func (rec *Recorder) event(kind, data string) { rec.eventAt(time.Now(), kind, data) }

func (rec *Recorder) eventAt(at time.Time, kind, data string) {
	t := max(float64(at.Sub(rec.start).Microseconds())/1e6, rec.lastT)
	rec.lastT = t
	line, _ := json.Marshal([]any{t, kind, data})
	rec.buf.Write(line)
	rec.buf.WriteByte('\n')
	if rec.buf.Len() >= chunkBytes {
		rec.flushLocked()
	}
}

// Output 记下一段终端输出。
func (rec *Recorder) Output(b []byte) {
	if rec == nil || len(b) == 0 {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.closed {
		return
	}
	data := append(rec.pending, b...)
	// 末尾不完整的 UTF-8 字符留到下一次，asciicast 的数据必须是合法的字符串。
	cut := len(data)
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	rec.pending = append([]byte(nil), data[cut:]...)
	out := strings.ToValidUTF8(string(data[:cut]), "�")
	rec.matchEchoLocked(out, time.Now())
	rec.event("o", out)
}

// Resize 记下窗口大小的变化，回放时排版才正确。
func (rec *Recorder) Resize(cols, rows uint32) {
	if rec == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.closed {
		rec.event("r", formatSize(int(cols), int(rows)))
	}
}

func formatSize(cols, rows int) string { return fmt.Sprintf("%dx%d", cols, rows) }

// flushLocked 把缓冲写成一块。
func (rec *Recorder) flushLocked() {
	if rec.buf.Len() == 0 {
		return
	}
	plain := bytes.Clone(rec.buf.Bytes())
	rec.buf.Reset()
	info, err := rec.r.writeChunk(rec.id, rec.dir, rec.seq, plain)
	rec.seq++
	if err != nil {
		rec.err = err
		rec.truncated = true
		return
	}
	// 开头 8 MB 一直保留；之后的块只留最后 56 MB，攻击者先刷屏也挤不掉最后的操作。
	if rec.headBytes < headLimit {
		rec.headBytes += info.Bytes
		rec.head = append(rec.head, info)
		return
	}
	rec.tail = append(rec.tail, info)
	var total int64
	for _, c := range rec.tail {
		total += c.Bytes
	}
	for total > tailLimit && len(rec.tail) > 1 {
		old := rec.tail[0]
		rec.tail = rec.tail[1:]
		total -= old.Bytes
		_ = os.Remove(filepath.Join(rec.dir, chunkName(old.Seq)))
		rec.truncated = true
	}
}

func chunkName(seq int) string { return fmt.Sprintf("%06d", seq) }

func chunkAD(id string, seq int) []byte { return []byte("recording/" + id + "/" + strconv.Itoa(seq)) }

var (
	zenc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	zdec, _ = zstd.NewReader(nil)
)

func (r *Recordings) writeChunk(id, dir string, seq int, plain []byte) (chunkInfo, error) {
	sealed, err := r.Sealer.Seal(zenc.EncodeAll(plain, nil), chunkAD(id, seq))
	if err != nil {
		return chunkInfo{}, err
	}
	path := filepath.Join(dir, chunkName(seq))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := r.budget.WriteArchive(ctx, 3, int64(len(sealed))+4096, func() error { return os.WriteFile(path, sealed, 0o600) }); err != nil {
		return chunkInfo{}, err
	}
	sum := sha256.Sum256(sealed)
	return chunkInfo{Seq: seq, Bytes: int64(len(plain)), Hash: hex.EncodeToString(sum[:])}, nil
}

// Close 结束录制，返回摘要。
func (rec *Recorder) Close() Summary {
	if rec == nil {
		return Summary{}
	}
	rec.mu.Lock()
	if rec.closed {
		rec.mu.Unlock()
		return rec.summary()
	}
	rec.closed = true
	rec.hideAllLocked()
	if len(rec.pending) > 0 {
		rec.event("o", strings.ToValidUTF8(string(rec.pending), "�"))
		rec.pending = nil
	}
	rec.flushLocked()
	rec.r.active.Delete(rec.id)
	rec.mu.Unlock()
	close(rec.stop)
	<-rec.done
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.summary()
}

func (rec *Recorder) summary() Summary {
	s := Summary{ID: rec.id, Truncated: rec.truncated, Seconds: int64(time.Since(rec.start).Seconds())}
	s.Chunks = append(append(s.Chunks, rec.head...), rec.tail...)
	for _, c := range s.Chunks {
		s.Bytes += c.Bytes
	}
	return s
}

// WriteOnce 把一次命令的输出写成一个录制（exec、node_exec），返回摘要。没开录制时返回空。
func (r *Recordings) WriteOnce(ctx context.Context, output string) Summary {
	rec := r.Start(ctx, 200, 50)
	if rec == nil {
		return Summary{}
	}
	rec.Output([]byte(strings.ReplaceAll(output, "\n", "\r\n")))
	return rec.Close()
}

// truncatedMarker 是回放时插在开头和结尾之间的提示。
const truncatedMarker = "\r\n\x1b[33m[录制已截断：中间的输出超出上限，已丢弃]\x1b[0m\r\n"

// Read 按顺序读出录制的每一块（asciicast v2 的若干行），中间被丢掉的地方插入一条提示。
func (r *Recordings) Read(id string, fn func([]byte) error) error {
	if !recordingIDRe.MatchString(id) {
		return errors.New("录制不存在")
	}
	dir := filepath.Join(r.Dir, id[:6], id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return errors.New("录制不存在")
	}
	var seqs []int
	for _, e := range entries {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			seqs = append(seqs, n)
		}
	}
	sort.Ints(seqs)
	prev := -1
	var lastT float64
	for _, seq := range seqs {
		sealed, err := os.ReadFile(filepath.Join(dir, chunkName(seq)))
		if err != nil {
			return err
		}
		packed, err := r.Sealer.Open(sealed, chunkAD(id, seq))
		if err != nil {
			return errors.New("录制无法解密，可能已被修改")
		}
		plain, err := zdec.DecodeAll(packed, nil)
		if err != nil {
			return err
		}
		if prev >= 0 && seq != prev+1 {
			line, _ := json.Marshal([]any{lastT, "o", truncatedMarker})
			if err := fn(append(line, '\n')); err != nil {
				return err
			}
		}
		if t := lastEventTime(plain); t > 0 {
			lastT = t
		}
		prev = seq
		if err := fn(plain); err != nil {
			return err
		}
	}
	return nil
}

func lastEventTime(b []byte) float64 {
	b = bytes.TrimRight(b, "\n")
	i := bytes.LastIndexByte(b, '\n')
	var ev []any
	if json.Unmarshal(b[i+1:], &ev) == nil && len(ev) == 3 {
		if t, ok := ev[0].(float64); ok {
			return t
		}
	}
	return 0
}

// Prune 删掉早于 keep 的录制；总量超过 maxBytes 时从最旧的月份删起。
func (r *Recordings) Prune(keep time.Duration, maxBytes int64, now time.Time) error {
	if r == nil {
		return nil
	}
	if r.budget != nil {
		return r.budget.WriteArchive(context.Background(), 3, 0, func() error { return nil })
	}
	months, _ := os.ReadDir(r.Dir)
	type rec struct {
		path string
		mod  time.Time
		size int64
	}
	var all []rec
	var total int64
	for _, m := range months {
		dirs, _ := os.ReadDir(filepath.Join(r.Dir, m.Name()))
		for _, d := range dirs {
			p := filepath.Join(r.Dir, m.Name(), d.Name())
			var size int64
			var mod time.Time
			files, _ := os.ReadDir(p)
			for _, f := range files {
				if info, err := f.Info(); err == nil {
					size += info.Size()
					if info.ModTime().After(mod) {
						mod = info.ModTime()
					}
				}
			}
			all = append(all, rec{p, mod, size})
			total += size
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, x := range all {
		if _, active := r.active.Load(filepath.Base(x.path)); active {
			continue
		}
		if now.Sub(x.mod) > keep || maxBytes > 0 && total > maxBytes {
			if err := os.RemoveAll(x.path); err != nil {
				return err
			}
			total -= x.size
		}
	}
	return nil
}

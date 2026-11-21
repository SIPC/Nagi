// Package auditlog 是节点本地审计（见 docs/audit.md#节点本地审计）：面板被攻破时，面板上的记录不可信，
// 所以节点自己也记一份——节点终端、node_exec、面板下发的修改类指令、被拒绝的越权请求。
//
// v0.37.1 起记在本机日志目录的 audit/node-audit.db（SQLite），和节点业务库分开。每条记录保存原样的一行 JSON
// （ReadAuditLog 返回、面板外送的就是这一行）和哈希链：hash = SHA-256(prev_hash ‖ 这一行)。v0.37.0 及以前的
// JSON Lines 文件（<数据目录>/audit/）启动时按原序号、原样导入，核对后删除，见 legacy.go。
// 面板读走后代为外送（见 ship.go）：节点被攻破后可以删改本地记录，但已经外送的记录还在。
package auditlog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// maxField 是每个字段的长度上限：来源于请求的内容都可能很长。
const maxField = 2000

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

// Log 是节点本地审计。用 Open 打开；nil 时 Record 什么也不做。
type Log struct {
	db     *sql.DB
	path   string
	legacy string // v0.37.0 及以前的 JSON Lines 目录

	mu       sync.Mutex
	seq      int64
	lastHash string
	budget   *logstore.Store
	// pending：旧文件还没有导入完。这期间新记录照旧追加到旧文件（序号接着旧文件），导入时一起按序号进链，
	// 不会出现序号交错；导入完以后不再写旧文件。
	pending atomic.Bool
	// historyPending：统一日志页的旧副本还没导入完，容量清理先不删本库里的记录。
	historyPending atomic.Bool
	// Capture 独立采集规范化后的审计事实，不以本库写成功为前提。
	// 启动时设置，运行时不修改，不能递归写本 Log；序号不代表本库已保存。
	Capture func(Event)
}

// Path is the audit store below a machine's local log directory.
func Path(logsDir string) string { return filepath.Join(logsDir, "audit", "node-audit.db") }

// Open 打开（必要时创建）审计库 path。legacyDir 是旧版本的 JSON Lines 目录（没有时为空）。打开后调用
// ImportLegacy 导入旧文件。
func Open(path, legacyDir string) (*Log, error) {
	d, err := db.Open(path)
	if err != nil {
		return nil, err
	}
	// Incremental auto-vacuum lets capacity cleanup give space back without a
	// full-size VACUUM. db.Open already wrote a WAL header, so switching needs a
	// VACUUM, which is free while the database is still empty.
	var tables int
	if err := d.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil {
		d.Close()
		return nil, err
	}
	if tables == 0 {
		if _, err := d.Exec(`PRAGMA auto_vacuum=INCREMENTAL; VACUUM`); err != nil {
			d.Close()
			return nil, err
		}
	}
	if _, err := d.Exec(`PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS audit_events (
 seq INTEGER PRIMARY KEY, line BLOB NOT NULL, prev_hash TEXT NOT NULL, hash TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS audit_tombstones (seq INTEGER PRIMARY KEY, hash TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS audit_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`); err != nil {
		d.Close()
		return nil, err
	}
	l := &Log{db: d, path: path, legacy: legacyDir}
	if err := l.loadHead(); err != nil {
		d.Close()
		return nil, err
	}
	if legacyDir != "" && l.meta("legacy_imported") == "" && len(files(legacyDir)) > 0 {
		l.pending.Store(true)
		l.historyPending.Store(true)
		l.seq = max(l.seq, legacySeq(legacyDir))
	}
	return l, nil
}

func (l *Log) loadHead() error {
	l.seq, l.lastHash = 0, ""
	err := l.db.QueryRow(`SELECT seq, hash FROM (SELECT seq, hash FROM audit_events UNION ALL SELECT seq, hash FROM audit_tombstones) ORDER BY seq DESC LIMIT 1`).Scan(&l.seq, &l.lastHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (l *Log) meta(key string) string {
	var v string
	_ = l.db.QueryRow(`SELECT value FROM audit_meta WHERE key=?`, key).Scan(&v)
	return v
}

func (l *Log) setMeta(key, value string) error {
	_, err := l.db.Exec(`INSERT INTO audit_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func clip(s string) string {
	if len(s) <= maxField {
		return s
	}
	// 按字节截，再去掉被截断的半个字符。
	return strings.ToValidUTF8(s[:maxField], "") + "…"
}

// ChainHash 是一条记录的哈希：SHA-256(prev_hash ‖ 原样的一行)。
func ChainHash(prev string, line []byte) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write(line)
	return hex.EncodeToString(h.Sum(nil))
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
	// Only an exhausted quota rejects the record. A busy shared gate must not
	// cancel the append to the independent audit chain.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	err := l.budget.Reserve(ctx, 4, 64<<10)
	cancel()
	var seq int64
	if errors.Is(err, logstore.ErrCapacity) {
		l.budget.NoteWriteFailure(logstore.Audit, 0, 1, fmt.Errorf("节点审计库：%w", err))
	} else {
		l.mu.Lock()
		if l.db == nil {
			err = sql.ErrConnDone
		} else if l.pending.Load() {
			err = l.appendLegacyLocked(&e)
		} else {
			err = l.appendLocked(context.Background(), &e)
		}
		if err == nil {
			seq = e.Seq
		}
		l.mu.Unlock()
		if err != nil {
			l.budget.NoteWriteFailure(logstore.Audit, 1, 1, fmt.Errorf("节点审计库：%w", err))
		}
	}
	if seq == 0 {
		e.Seq = 0
	}
	if l.Capture != nil {
		l.Capture(e)
	}
	return seq
}

// appendLocked appends e to the chain; the caller holds l.mu.
func (l *Log) appendLocked(ctx context.Context, e *Event) error {
	e.Seq = l.seq + 1
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	hash := ChainHash(l.lastHash, line)
	if _, err := l.db.ExecContext(ctx, `INSERT INTO audit_events(seq,line,prev_hash,hash) VALUES(?,?,?,?)`, e.Seq, line, l.lastHash, hash); err != nil {
		return err
	}
	l.seq, l.lastHash = e.Seq, hash
	return nil
}

// Close 关闭审计库。
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return nil
	}
	_, _ = l.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	err := l.db.Close()
	l.db = nil
	return err
}

// Seq 返回最新一条的序号。
func (l *Log) Seq() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// After 返回序号大于 seq 的记录（原样的行），最多 limit 条。旧文件没导入完时没有记录可读，
// 面板的游标不动，导入完以后从同一个位置接着读。
func (l *Log) After(seq int64, limit int) (lines [][]byte, first, last int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil || l.pending.Load() {
		return nil, 0, 0
	}
	rows, err := l.db.Query(`SELECT seq, line FROM audit_events WHERE seq>? ORDER BY seq LIMIT ?`, seq, limit)
	if err != nil {
		return nil, 0, 0
	}
	defer rows.Close()
	for rows.Next() {
		var s int64
		var line []byte
		if rows.Scan(&s, &line) != nil {
			break
		}
		if first == 0 {
			first = s
		}
		last = s
		lines = append(lines, line)
	}
	return lines, first, last
}

// VerifyResult 是一次链校验的结果。
type VerifyResult struct {
	Records    int64  `json:"records"`
	Tombstones int64  `json:"tombstones"`
	LastSeq    int64  `json:"last_seq"`
	LastHash   string `json:"last_hash"`
	BadSeq     int64  `json:"bad_seq,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// OK 判断整条链完好。
func (r VerifyResult) OK() bool { return r.BadSeq == 0 }

// Verify 校验整条哈希链：每条的哈希对得上内容，相邻的记录（或墓碑）接得上，每个墓碑都被链上某条清理记录
// （audit_pruned）或旧文件导入记录（audit_legacy_import 的 gaps）列出。中间删一条、改一条、补一个假墓碑都能
// 发现；从某处起重写后面所有的记录、或在末尾追加，要靠外送出去的副本对比。
func (l *Log) Verify(ctx context.Context) (VerifyResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return VerifyResult{}, sql.ErrConnDone
	}
	return verifyDB(ctx, l.db)
}

func verifyDB(ctx context.Context, d *sql.DB) (VerifyResult, error) {
	var res VerifyResult
	tomb := map[int64]string{}
	trows, err := d.QueryContext(ctx, `SELECT seq, hash FROM audit_tombstones`)
	if err != nil {
		return res, err
	}
	for trows.Next() {
		var s int64
		var h string
		if err := trows.Scan(&s, &h); err != nil {
			trows.Close()
			return res, err
		}
		tomb[s] = h
	}
	if err := errors.Join(trows.Err(), trows.Close()); err != nil {
		return res, err
	}
	res.Tombstones = int64(len(tomb))
	bad := func(seq int64, format string, args ...any) {
		if res.BadSeq == 0 {
			res.BadSeq, res.Reason = seq, fmt.Sprintf(format, args...)
		}
	}
	rows, err := d.QueryContext(ctx, `SELECT seq, line, prev_hash, hash FROM audit_events ORDER BY seq`)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	prevSeq, prevHash := int64(-1), ""
	covered := map[int64]bool{}
	for rows.Next() {
		var seq int64
		var line []byte
		var prev, hash string
		if err := rows.Scan(&seq, &line, &prev, &hash); err != nil {
			return res, err
		}
		res.Records++
		var e Event
		if json.Unmarshal(line, &e) != nil || e.Seq != seq {
			bad(seq, "记录 %d 的内容和序号对不上，可能被修改过", seq)
		}
		if ChainHash(prev, line) != hash {
			bad(seq, "记录 %d 的内容和哈希对不上，可能被修改过", seq)
		}
		if prevSeq >= 0 {
			link := prevHash
			for s := prevSeq + 1; s < seq; s++ {
				h, ok := tomb[s]
				if !ok {
					bad(seq, "记录 %d 之前缺少记录 %d，可能被删除过", seq, s)
					break
				}
				link = h
			}
			if prev != link {
				bad(seq, "记录 %d 和上一条记录接不上，中间的记录可能被删除或修改过", seq)
			}
		}
		for _, s := range coveredBy(e) {
			covered[s] = true
		}
		prevSeq, prevHash = seq, hash
		res.LastSeq, res.LastHash = seq, hash
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	for s := range tomb {
		if !covered[s] {
			bad(s, "记录 %d 被删除了，但没有对应的清理记录，墓碑可能是伪造的", s)
			break
		}
	}
	if res.Records == 0 && len(tomb) > 0 {
		return res, errors.New("节点审计库里只剩墓碑，记录被清空了")
	}
	return res, nil
}

// coveredBy lists the tombstones a record accounts for.
func coveredBy(e Event) []int64 {
	switch e.Action {
	case actionPruned:
		v, _ := strings.CutPrefix(e.Detail, "ids=")
		return parseRanges(strings.Split(v, ","))
	case legacyImport:
		var s importSummary
		if json.Unmarshal([]byte(e.Detail), &s) == nil {
			return parseRanges(s.Gaps)
		}
	}
	return nil
}

func parseRanges(parts []string) []int64 {
	var out []int64
	for _, part := range parts {
		a, b, found := strings.Cut(part, "-")
		lo, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			continue
		}
		hi := lo
		if found {
			if hi, err = strconv.ParseInt(b, 10, 64); err != nil || hi < lo || hi-lo > 1<<24 {
				continue
			}
		}
		for s := lo; s <= hi; s++ {
			out = append(out, s)
		}
	}
	return out
}

func formatRanges(seqs []int64) []string {
	var out []string
	for i := 0; i < len(seqs); {
		j := i
		for j+1 < len(seqs) && seqs[j+1] == seqs[j]+1 {
			j++
		}
		if i == j {
			out = append(out, strconv.FormatInt(seqs[i], 10))
		} else {
			out = append(out, fmt.Sprintf("%d-%d", seqs[i], seqs[j]))
		}
		i = j + 1
	}
	return out
}

// VerifyFile 只读地校验一个节点审计库（nagi audit verify）。
func VerifyFile(ctx context.Context, path string) (VerifyResult, error) {
	if _, err := os.Stat(path); err != nil {
		return VerifyResult{}, err
	}
	d, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return VerifyResult{}, err
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	return verifyDB(ctx, d)
}

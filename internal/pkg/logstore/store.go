// Package logstore 保存本机日志。每个库有独立且持久的来源编号和只增序号；
// 不负责网络传输、身份、选路或通知，未来的 mesh 查询/复制复用这里的记录与缺口信息。
package logstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
)

// 类别决定清理顺序，先清低优先级，再按采集顺序清最旧的。
const (
	Runtime   = "runtime"
	Access    = "access"
	Container = "container"
	Metrics   = "metrics"
	Task      = "task"
	Audit     = "audit"
	Security  = "security"
	Alert     = "alert"
	System    = "logstore"
)

const maxRecordBytes = 64 << 10

var ErrCapacity = errors.New("本地日志额度不足")

func priority(category string) (int, bool) {
	switch category {
	case Runtime:
		return 0, true
	case Access, Container:
		return 1, true
	case Metrics:
		return 2, true
	case Task:
		return 3, true
	case Audit, Security, Alert, System:
		return 4, true
	}
	return 0, false
}

// Event 的 Time 是来源的事件时间；Received 是本机实际采到的时间，不用远端时间决定清理顺序。
// Data 入库前做通用脱敏；调用方仍须按语义隐藏命令、配置正文和无法自动识别的敏感内容。
type Event struct {
	Seq      int64     `json:"seq"`
	Source   string    `json:"source"`
	Time     time.Time `json:"time"`
	Received time.Time `json:"received"`
	Category string    `json:"category"`
	Kind     string    `json:"kind"`
	Project  string    `json:"project,omitempty"`
	Object   string    `json:"object,omitempty"`
	Data     any       `json:"data"`
}

// Filter 只查本库；After 为独占序号游标（倒序时为上界，0 从最新开始）。
// From/To 为事件时间的闭开区间；副本传输仍默认正序。
type Filter struct {
	After       int64
	NewestFirst bool
	From, To    time.Time
	Category    string
	Project     string
	Object      string
	Search      string
	KindPrefix  string
	Limit       int
}

// Pruned 是某类别累计主动清理的范围包络，范围内不表示每条都已删除。
// 每类别只占一行，不会因为清理日志而生成无限增长的清理日志。
type Pruned struct {
	Category string `json:"category"`
	First    int64  `json:"first"`
	Last     int64  `json:"last"`
	Count    int64  `json:"count"`
}

type State struct {
	Source      string         `json:"source"`
	Last        int64          `json:"last"`
	DiskBytes   int64          `json:"disk_bytes"`
	BudgetBytes int64          `json:"budget_bytes"`
	WriteErrors uint64         `json:"write_errors"`
	Failures    []WriteFailure `json:"failures"`
	Pruned      []Pruned       `json:"pruned"`
	Archives    []ArchiveUsage `json:"archives"`
}

// Migration describes live background work, not a durable checkpoint or a copy's state.
type Migration struct {
	ID        string
	Phase     string
	Completed int64
	Total     int64
	Error     string
}

type Store struct {
	gate    chan struct{}
	db      *sql.DB
	path    string
	source  string
	budget  int64
	dbLimit int64
	closed  bool
	w       writer
	errors  atomic.Uint64
	// lastDisk is the physical size last measured under the gate (see Reserve).
	lastDisk atomic.Int64
	// Report 的失败仅写原始 stderr logger，不能递归写回自身。
	lastError  atomic.Int64
	archives   []Archive
	migrations sync.Map // string → immutable Migration; independent of the disk writer gate.
}

func (s *Store) SetMigration(m Migration) {
	if s != nil {
		s.migrations.Store(m.ID, m)
	}
}

func (s *Store) Migrations() []Migration {
	var out []Migration
	if s != nil {
		s.migrations.Range(func(_, v any) bool {
			out = append(out, v.(Migration))
			return true
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Open 允许小额度供本地测试和未来近期副本使用；生产节点的 5～10 GiB 范围由 LoadConfig 校验。
// 额度包含此库的索引、WAL 和 SHM。20% 空间留给写事务/清理，不把逻辑正文大小冒充实际占盘。
func Open(ctx context.Context, path string, budget int64) (*Store, error) {
	return open(ctx, path, budget, true)
}

func open(ctx context.Context, path string, budget int64, pruneOnOpen bool) (*Store, error) {
	if budget < 2<<20 {
		return nil, fmt.Errorf("日志库额度不能小于 2 MiB")
	}
	d, err := db.Open(path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: d, path: path, budget: budget, dbLimit: budget * 4 / 5, gate: make(chan struct{}, 1)}
	fail := func(err error) (*Store, error) { d.Close(); return nil, err }
	var vacuum int
	if err := d.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&vacuum); err != nil {
		return fail(err)
	}
	if vacuum != 2 {
		// db.Open 已切换 WAL，空库也可能已有页头。此时仅设置 auto_vacuum 不会生效，
		// 必须在还没有任何历史时初始化页布局，不能在满库上做需要额外整库空间的 VACUUM。
		var tables int
		if err := d.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil {
			return fail(err)
		}
		if tables != 0 {
			return fail(fmt.Errorf("日志库不是增量回收格式，拒绝未经迁移覆盖历史"))
		}
		if _, err := d.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL; VACUUM`); err != nil {
			return fail(err)
		}
	}
	// 单连接且所有查询在锁内读完，任何 checkpoint 都不会被本模块自己的长游标阻塞。
	// 默认 WAL + NORMAL：进程崩溃不丢已提交记录，断电可能丢最后几次提交，但库不会损坏。
	// 审计、安全、告警和日志库自身记录的事务在 write 里临时切到 FULL，断电也不丢。
	// WAL 交给自动 checkpoint（不再每次提交后 TRUNCATE），阈值按额度缩放，
	// WAL 物理大小始终留在额度预留的 20% 以内。
	walPages := min(1000, max(16, budget/32/4096))
	for _, pragma := range []string{
		"PRAGMA auto_vacuum = INCREMENTAL", "PRAGMA synchronous = NORMAL",
		fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", walPages), "PRAGMA journal_size_limit = 0", "PRAGMA temp_store = MEMORY",
	} {
		if _, err := d.ExecContext(ctx, pragma); err != nil {
			return fail(err)
		}
	}
	if _, err := d.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS log_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS log_events (
			seq INTEGER PRIMARY KEY AUTOINCREMENT, time_ns INTEGER NOT NULL, received_ns INTEGER NOT NULL,
			category TEXT NOT NULL, priority INTEGER NOT NULL, kind TEXT NOT NULL,
			project TEXT NOT NULL, object TEXT NOT NULL, data BLOB NOT NULL);
		CREATE INDEX IF NOT EXISTS log_prune ON log_events(priority, seq);
		CREATE INDEX IF NOT EXISTS log_time ON log_events(time_ns, seq);
		CREATE TABLE IF NOT EXISTS log_pruned (
			category TEXT PRIMARY KEY, first_seq INTEGER NOT NULL, last_seq INTEGER NOT NULL, count INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS log_checkpoints (
			key TEXT PRIMARY KEY, data BLOB NOT NULL);
	`); err != nil {
		return fail(err)
	}
	var source [16]byte
	if _, err := rand.Read(source[:]); err != nil {
		return fail(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT OR IGNORE INTO log_meta VALUES ('source', ?)`, hex.EncodeToString(source[:])); err != nil {
		return fail(err)
	}
	if err := d.QueryRowContext(ctx, `SELECT value FROM log_meta WHERE key='source'`).Scan(&s.source); err != nil {
		return fail(err)
	}
	var pageSize int64
	if err := d.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return fail(err)
	}
	// 缩小配置时先清历史，再限制新页数；不以新配额静默启动一个已经超大的库。
	if pruneOnOpen {
		if err := s.prune(ctx, s.dbLimit*8/10, 4); err != nil {
			return fail(err)
		}
	}
	if err := s.compact(ctx); err != nil {
		return fail(err)
	}
	if n, err := s.diskBytes(); err != nil || n > s.budget {
		return fail(fmt.Errorf("日志库无法收缩到额度内: bytes=%d: %v", n, err))
	}
	if _, err := d.ExecContext(ctx, fmt.Sprintf("PRAGMA max_page_count = %d", s.dbLimit/pageSize)); err != nil {
		return fail(err)
	}
	return s, nil
}

// Close 等待正在写入的记录，刷新 WAL；关闭后不会生成第二个来源或复用已删除序号。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.stopWriter()
	s.gate <- struct{}{}
	defer func() { <-s.gate }()
	if s.closed {
		return nil
	}
	s.closed = true
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return errors.Join(err, s.db.Close())
}

// Record 同步落盘，调用方得到成功后记录已提交。失败不会外送，也不会自动改用控制连接。
func (s *Store) Record(ctx context.Context, e Event) error {
	return s.record(ctx, e, "", nil, false)
}

// RecordCheckpoint 把已采到的记录和来源读取位置放在同一事务内。
// 位置不是可轮换的历史，清理历史不能删除或倒退它；位置本身也受数据库页数硬上限约束。
func (s *Store) RecordCheckpoint(ctx context.Context, e Event, key string, position any) error {
	b, err := json.Marshal(position)
	if err != nil {
		return err
	}
	if key == "" || len(key) > 200 || len(b) > 4096 {
		return fmt.Errorf("无效或过大的日志采集断点")
	}
	return s.record(ctx, e, key, b, false)
}

// ImportCheckpoint atomically reconciles an identical normalized historical
// fact and advances its source position. Normal live writes never deduplicate:
// identical container output lines still represent separate observed facts.
// Data must include the original identity and timestamp. The indexed time match
// allows millisecond precision in legacy SQLite audit stores.
func (s *Store) ImportCheckpoint(ctx context.Context, e Event, key string, position any) error {
	b, err := json.Marshal(position)
	if err != nil {
		return err
	}
	if key == "" || len(key) > 200 || len(b) > 4096 {
		return fmt.Errorf("无效或过大的日志采集断点")
	}
	return s.record(ctx, e, key, b, true)
}

// ImportBatch is ImportCheckpoint for several facts in one transaction, so a
// startup import does not pay one synchronous commit per historical record.
func (s *Store) ImportBatch(ctx context.Context, events []Event, key string, position any) error {
	if s == nil || len(events) == 0 {
		return nil
	}
	b, err := json.Marshal(position)
	if err != nil {
		return err
	}
	if key == "" || len(key) > 200 || len(b) > 4096 {
		return fmt.Errorf("无效或过大的日志采集断点")
	}
	rows := make([]row, 0, len(events))
	for _, e := range events {
		r, err := prepareRow(e)
		if err != nil {
			s.fail(e.Category, 1, 0, err)
			return err
		}
		rows = append(rows, r)
	}
	if err := s.write(ctx, rows, key, b, true); err != nil {
		s.fail(events[0].Category, uint64(len(events)), 0, err)
		return err
	}
	return nil
}

// Checkpoint 返回最近一次已随记录提交的位置；没有位置不等于过去没有日志。
func (s *Store) Checkpoint(ctx context.Context, key string) (json.RawMessage, error) {
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.gate }()
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM log_checkpoints WHERE key=?`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return json.RawMessage(b), err
}

func prepareRow(e Event) (row, error) {
	p, ok := priority(e.Category)
	if !ok || e.Kind == "" {
		return row{}, fmt.Errorf("未知日志类别或空事件名称")
	}
	b, err := json.Marshal(e.Data)
	if err != nil {
		return row{}, err
	}
	b, err = sanitizeJSON(b)
	if err != nil {
		return row{}, err
	}
	if len(b) > maxRecordBytes {
		b, _ = json.Marshal(map[string]any{"truncated": true, "original_bytes": len(b), "head": string(b[:8<<10])})
	}
	e.Kind, e.Project, e.Object = clipText(e.Kind, 200), clipText(e.Project, 200), clipText(e.Object, 1000)
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	return row{e: e, p: p, data: b}, nil
}

func (s *Store) record(ctx context.Context, e Event, key string, position []byte, reconcile bool) error {
	if s == nil {
		return nil
	}
	r, err := prepareRow(e)
	if err == nil {
		err = s.write(ctx, []row{r}, key, position, reconcile)
	}
	if err != nil {
		s.fail(e.Category, 1, 0, err)
	}
	return err
}

// writeRows commits one queued batch in a single transaction (FULL sync when it holds important records).
func (s *Store) writeRows(ctx context.Context, rows []row) error {
	return s.write(ctx, rows, "", nil, false)
}

func (s *Store) write(ctx context.Context, rows []row, key string, position []byte, reconcile bool) error {
	reserve, maxP := int64(32<<10), 0
	for _, r := range rows {
		reserve += int64(len(r.data)) * 2
		maxP = max(maxP, r.p)
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	if s.closed {
		return sql.ErrConnDone
	}
	if err := s.prepare(ctx, maxP, reserve); err != nil {
		return err
	}
	if slices.ContainsFunc(rows, func(r row) bool { return important(r.e.Category) }) {
		if _, err := s.db.ExecContext(ctx, `PRAGMA synchronous = FULL`); err != nil {
			return err
		}
		defer s.db.Exec(`PRAGMA synchronous = NORMAL`)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	received := time.Now().UTC().UnixNano()
	for _, r := range rows {
		e := r.e
		query := `INSERT INTO log_events(time_ns, received_ns, category, priority, kind, project, object, data)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?`
		args := []any{e.Time.UnixNano(), received, e.Category, r.p, e.Kind, e.Project, e.Object, r.data}
		if reconcile {
			at := e.Time.Truncate(time.Millisecond)
			query += ` WHERE NOT EXISTS (SELECT 1 FROM log_events WHERE time_ns>=? AND time_ns<? AND category=? AND kind=? AND project=? AND object=? AND data=?)`
			args = append(args, at.UnixNano(), at.Add(time.Millisecond).UnixNano(), e.Category, e.Kind, e.Project, e.Object, r.data)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	if key != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO log_checkpoints(key,data) VALUES (?,?)
			ON CONFLICT(key) DO UPDATE SET data=excluded.data`, key, position); err != nil {
			return err
		}
	}
	// 提交后不再每次 TRUNCATE checkpoint：实测它占接收副本耗时六成以上，把整机写锁拖到数百毫秒。
	// WAL 交给自动 checkpoint；只有大事务把 WAL 撑过额度的 1/16 时才截断，保证物理额度。
	if err := tx.Commit(); err != nil {
		return err
	}
	s.trimWAL(ctx)
	return nil
}

// trimWAL truncates an oversized WAL after a commit. The commit already
// succeeded, so a failed truncation is not a write failure; the next large
// commit or cleanup tries again, and prepare still counts the WAL's bytes.
func (s *Store) trimWAL(ctx context.Context) {
	if st, err := os.Stat(s.path + "-wal"); err == nil && st.Size() > s.budget/16 {
		_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	}
}

func (s *Store) liveBytes(ctx context.Context) (int64, error) {
	var pages, free, size int64
	for _, v := range []struct {
		query string
		to    *int64
	}{{"PRAGMA page_count", &pages}, {"PRAGMA freelist_count", &free}, {"PRAGMA page_size", &size}} {
		if err := s.db.QueryRowContext(ctx, v.query).Scan(v.to); err != nil {
			return 0, err
		}
	}
	total := (pages - free) * size
	for _, a := range s.archives {
		n, err := a.Size()
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func (s *Store) prune(ctx context.Context, target int64, maxPriority int) error {
	exhausted := map[string]bool{}
	for {
		live, err := s.liveBytes(ctx)
		if err != nil || live <= target {
			return err
		}
		// 小批次删除，给 WAL 留足预算；每批与缺口摘要原子提交。
		var seq int64
		var category string
		var p int
		err = s.db.QueryRowContext(ctx, `SELECT seq, category, priority FROM log_events WHERE priority<=? ORDER BY priority, seq LIMIT 1`, maxPriority).Scan(&seq, &category, &p)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			p = maxPriority + 1
		}
		chosen := -1
		for i, a := range s.archives {
			if !exhausted[a.Name] && a.Priority <= maxPriority && a.Priority <= p {
				chosen, p = i, a.Priority
			}
		}
		if chosen >= 0 {
			a := s.archives[chosen]
			changed, err := a.Trim(ctx)
			if err != nil {
				return err
			}
			if !changed {
				exhausted[a.Name] = true
			}
			if changed {
				if _, err := s.db.ExecContext(ctx, `INSERT INTO log_meta(key,value) VALUES (?, '1') ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1`, "archive_pruned:"+a.Name); err != nil {
					return err
				}
				if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
					return err
				}
			}
			continue
		}
		if seq == 0 {
			return ErrCapacity
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM log_events WHERE seq=?`, seq)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO log_pruned(category,first_seq,last_seq,count) VALUES (?,?,?,1)
				ON CONFLICT(category) DO UPDATE SET first_seq=min(first_seq,excluded.first_seq), last_seq=max(last_seq,excluded.last_seq), count=count+1`, category, seq, seq)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return err
		}
	}
}

func (s *Store) compact(ctx context.Context) error {
	return CompactHistory(ctx, s.db)
}

// CompactHistory incrementally reclaims a history-only SQLite database without
// creating a second full-size copy. Its owner must serialize writers.
func CompactHistory(ctx context.Context, d *sql.DB) error {
	previous := int(^uint(0) >> 1)
	for {
		var free int
		if err := d.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
			return err
		}
		if free == 0 {
			break
		}
		if free >= previous {
			return fmt.Errorf("日志库增量空间回收未推进")
		}
		previous = free
		if _, err := d.ExecContext(ctx, `PRAGMA incremental_vacuum(128)`); err != nil {
			return err
		}
		if _, err := d.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return err
		}
	}
	_, err := d.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

func (s *Store) diskBytes() (int64, error) {
	size, err := FileBytes(s.path)
	if err != nil {
		return 0, err
	}
	for _, a := range s.archives {
		n, err := a.Size()
		if err != nil {
			return 0, err
		}
		size += n
	}
	return size, nil
}

// Query first waits (bounded by ctx) for records already queued by Report, so
// a caller sees its own earlier reports. Paged readers do not wait.
func (s *Store) Query(ctx context.Context, f Filter) ([]Event, error) {
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = s.Flush(wait)
	cancel()
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.gate }()
	return s.query(ctx, f)
}

func (s *Store) query(ctx context.Context, f Filter) ([]Event, error) {
	if f.After < 0 || f.Limit < 0 || f.Limit > 1000 || (!f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To)) {
		return nil, fmt.Errorf("无效日志查询范围")
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	where, args := []string{"seq > ?"}, []any{f.After}
	order := "seq"
	if f.NewestFirst {
		order = "seq DESC"
		where, args = []string{"1=1"}, nil
		if f.After > 0 {
			where, args = []string{"seq < ?"}, []any{f.After}
		}
	}
	for _, v := range []struct{ col, value string }{{"category", f.Category}, {"project", f.Project}, {"object", f.Object}} {
		if v.value != "" {
			where, args = append(where, v.col+"=?"), append(args, v.value)
		}
	}
	if !f.From.IsZero() {
		where, args = append(where, "time_ns>=?"), append(args, f.From.UnixNano())
	}
	if !f.To.IsZero() {
		where, args = append(where, "time_ns<?"), append(args, f.To.UnixNano())
	}
	if f.Search != "" {
		where = append(where, "instr(lower(kind || ' ' || project || ' ' || object || ' ' || CAST(data AS TEXT)),lower(?))>0")
		args = append(args, f.Search)
	}
	if f.KindPrefix != "" {
		where = append(where, "substr(kind,1,length(?))=?")
		args = append(args, f.KindPrefix, f.KindPrefix)
	}
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, `SELECT seq,time_ns,received_ns,category,kind,project,object,data FROM log_events WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e := Event{Source: s.source}
		var ts, received int64
		var data []byte
		if err := rows.Scan(&e.Seq, &ts, &received, &e.Category, &e.Kind, &e.Project, &e.Object, &data); err != nil {
			return nil, err
		}
		e.Time, e.Received, e.Data = time.Unix(0, ts).UTC(), time.Unix(0, received).UTC(), json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) State(ctx context.Context) (State, error) {
	if err := s.lock(ctx); err != nil {
		return State{}, err
	}
	defer func() { <-s.gate }()
	return s.state(ctx)
}

func (s *Store) state(ctx context.Context) (State, error) {
	st := State{Source: s.source, BudgetBytes: s.budget, WriteErrors: s.errors.Load(), Failures: s.WriteFailures(), Pruned: []Pruned{}}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='log_events'),0)`).Scan(&st.Last); err != nil {
		return st, err
	}
	var err error
	if st.DiskBytes, err = s.diskBytes(); err != nil {
		return st, err
	}
	for _, a := range s.archives {
		n, err := a.Size()
		if err != nil {
			return st, err
		}
		u := ArchiveUsage{Name: a.Name, DiskBytes: n}
		if err := s.db.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM log_meta WHERE key=?`, "archive_pruned:"+a.Name).Scan(&u.PrunedBatches); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return st, err
		}
		st.Archives = append(st.Archives, u)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT category,first_seq,last_seq,count FROM log_pruned ORDER BY category`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Pruned
		if err := rows.Scan(&p.Category, &p.First, &p.Last, &p.Count); err != nil {
			return st, err
		}
		st.Pruned = append(st.Pruned, p)
	}
	return st, rows.Err()
}

func (s *Store) lock(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.gate
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Directory is the machine-local history directory, outside role snapshots.
func (s *Store) Directory() string { return filepath.Dir(s.path) }

// OpenLocal 只打开本机的配置和日志库；面板的角色变化不应重建它。
func OpenLocal(ctx context.Context, dir string) (*Store, Config, error) {
	c, err := LoadConfig(dir)
	if err != nil {
		return nil, c, err
	}
	s, err := Open(ctx, filepath.Join(dir, "logs.db"), int64(c.CapacityGiB)*GiB)
	return s, c, err
}

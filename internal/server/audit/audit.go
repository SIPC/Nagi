// Package audit 是面板的审计日志（见 docs/audit.md）：记录存在单独的 audit.db 里，只追加，每条带哈希链。
package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

//go:embed migrations/*.sql
var migrations embed.FS

// 类别。
const (
	Operation = "operation"
	Read      = "read"
	MCP       = "mcp"
	Session   = "session"
	Login     = "login"
	Alert     = "alert"
	Security  = "security"
)

// Categories 是所有类别，界面的筛选按这个顺序。
var Categories = []string{Operation, Read, MCP, Session, Login, Alert, Security}

// 结果。
const (
	ResultOK     = "ok"
	ResultFailed = "failed"
	ResultDenied = "denied"
)

// 操作者类型。
const (
	ActorUser   = "user"
	ActorToken  = "token"
	ActorNode   = "node"
	ActorPanel  = "panel"
	ActorSystem = "system"
)

// 来源。
const (
	ViaWeb    = "web"
	ViaApp    = "app" // 安卓 App（见 docs/android.md#审计）
	ViaMCP    = "mcp"
	ViaCLI    = "cli"
	ViaSystem = "system"
	ViaNode   = "node"
)

// Event 是一条审计记录。字段的含义见 docs/audit.md#字段。
type Event struct {
	ID          int64     `json:"id"`
	Time        time.Time `json:"-"`
	Category    string    `json:"category"`
	Action      string    `json:"action"`
	ActorType   string    `json:"actor_type"`
	ActorID     string    `json:"actor_id"`
	ActorName   string    `json:"actor_name"`
	Via         string    `json:"via"`
	TokenID     int64     `json:"token_id"`
	Client      string    `json:"client"`
	Session     string    `json:"session"`
	IP          string    `json:"ip"`
	UserAgent   string    `json:"user_agent"`
	ProjectID   string    `json:"project_id"`
	TargetType  string    `json:"target_type"`
	TargetID    string    `json:"target_id"`
	TargetName  string    `json:"target_name"`
	Params      string    `json:"params"`
	Result      string    `json:"result"`
	Error       string    `json:"error"`
	DurationMS  int64     `json:"duration_ms"`
	ParentID    int64     `json:"parent_id"`
	RecordingID string    `json:"recording_id"`
	PrevHash    string    `json:"-"`
	Hash        string    `json:"-"`
}

// 各字段的长度上限：来源于请求的内容都可能很长。
const (
	maxShort  = 200
	maxError  = 1000
	MaxParams = 4 << 10
)

// Log 是审计日志。
type Log struct {
	db  *sql.DB
	log *slog.Logger

	mu       sync.Mutex
	lastHash string

	hooksMu sync.RWMutex
	hooks   []func(Event)

	path           string
	budget         *logstore.Store
	historyPending atomic.Bool
	// Capture 独立采集规范化后的本机审计事实，旧库写失败仍会调用。
	// 启动时设置，运行时不修改，不能递归写本 Log；旧库失败时 ID/哈希为空。
	// 检测规则和旧外送继续使用仅在旧库提交成功后运行的 OnRecord。
	Capture func(Event)
}

// Open 打开（必要时创建）审计库。
func Open(ctx context.Context, path string, log *slog.Logger) (*Log, error) {
	d, err := db.Open(path)
	if err != nil {
		return nil, err
	}
	// 按总量上限删除以后要能把空间还回去（见 Prune）；只在建表之前设置才生效。
	if _, err := d.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		d.Close()
		return nil, err
	}
	sub, _ := fs.Sub(migrations, "migrations")
	if err := db.Migrate(ctx, d, sub); err != nil {
		d.Close()
		return nil, err
	}
	l := &Log{db: d, log: log, path: path}
	if err := d.QueryRowContext(ctx, `SELECT hash FROM events ORDER BY id DESC LIMIT 1`).Scan(&l.lastHash); err != nil && !errors.Is(err, sql.ErrNoRows) {
		d.Close()
		return nil, err
	}
	l.historyPending.Store(l.lastHash != "")
	return l, nil
}

// Close 关闭审计库。
func (l *Log) Close() error { return l.db.Close() }

// DB 返回底层的数据库，给迁移和测试用。
func (l *Log) DB() *sql.DB { return l.db }

// OnRecord 注册写入每条记录之后调用的函数（检测规则、外送）。函数要很快返回，不能再写审计。
func (l *Log) OnRecord(f func(Event)) {
	l.hooksMu.Lock()
	l.hooks = append(l.hooks, f)
	l.hooksMu.Unlock()
}

// Record 写入一条记录，返回它的 ID。写入失败只记日志：审计不能挡住正常的操作。
func (l *Log) Record(ctx context.Context, e Event) int64 {
	if l == nil {
		return 0
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	// MCP 工具执行期间的记录先收集起来，工具结束后挂在它下面写入（见 collect.go）。
	if c, _ := ctx.Value(collectorKey{}).(*Collector); c != nil {
		c.add(e)
		return 0
	}
	normalize(&e)
	if l.Capture != nil {
		defer func() { l.Capture(e) }()
	}
	persistCtx := context.WithoutCancel(ctx)
	if l.budget != nil {
		var cancel context.CancelFunc
		persistCtx, cancel = context.WithTimeout(persistCtx, 500*time.Millisecond)
		defer cancel()
	}
	var id int64
	err := l.budget.WriteArchive(persistCtx, 4, 64<<10, func() error {
		var err error
		id, err = l.insert(persistCtx, &e)
		if err == nil && l.budget != nil {
			_, err = l.db.ExecContext(persistCtx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		}
		return err
	})
	if err != nil {
		// insertTx 可能已经分配了 ID/计算了哈希，但事务没有提交，不能当成旧库证据。
		e.ID, e.Hash, e.PrevHash = 0, "", ""
		l.log.Warn("写入审计记录失败", "err", err, "action", e.Action)
		return 0
	}
	l.hooksMu.RLock()
	hooks := l.hooks
	l.hooksMu.RUnlock()
	for _, f := range hooks {
		f(e)
	}
	return id
}

func normalize(e *Event) {
	if e.Result == "" {
		e.Result = ResultOK
	}
	e.Action = clip(e.Action, maxShort)
	e.ActorID = clip(e.ActorID, maxShort)
	e.ActorName = clip(e.ActorName, maxShort)
	e.Client = clip(e.Client, maxShort)
	e.IP = clip(e.IP, 64)
	e.UserAgent = clip(e.UserAgent, 400)
	e.TargetID = clip(e.TargetID, maxShort)
	e.TargetName = clip(e.TargetName, maxShort)
	e.Error = clip(e.Error, maxError)
	if len(e.Params) > MaxParams {
		e.Params = TruncateParams(e.Params)
	}
}

// TruncateParams 截断超过 4 KB 的参数，标记「已截断」。截断后不再是合法的 JSON，所以包成一个对象。
func TruncateParams(p string) string {
	if len(p) <= MaxParams {
		return p
	}
	cut := p[:MaxParams-64]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	b, _ := json.Marshal(map[string]any{"truncated": true, "head": cut})
	return string(b)
}

func clip(v string, n int) string {
	if len(v) <= n {
		return v
	}
	r := []rune(v)
	if len(r) > n {
		r = r[:n]
	}
	for len(string(r)) > n*4 {
		r = r[:len(r)-1]
	}
	return string(r)
}

// Canonical 是计算哈希用的规范化 JSON：固定的字段顺序（结构体的顺序），时间用毫秒。
func Canonical(e Event) []byte {
	type canon struct {
		Event
		TimeMS int64 `json:"time"`
	}
	b, _ := json.Marshal(canon{Event: e, TimeMS: e.Time.UnixMilli()})
	return b
}

// ChainHash 计算 hash = SHA-256(prev_hash ‖ 规范化 JSON)。
func ChainHash(prev string, e Event) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write(Canonical(e))
	return hex.EncodeToString(h.Sum(nil))
}

func (l *Log) insert(ctx context.Context, e *Event) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// 先占一个 ID：哈希里要包含 ID，删掉中间一条以后后面的哈希对不上。
	if err := l.insertTx(ctx, tx, l.lastHash, e); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	l.lastHash = e.Hash
	return e.ID, nil
}

const columns = `id, time, category, action, actor_type, actor_id, actor_name, via, token_id, client, session, ip, user_agent,
	project_id, target_type, target_id, target_name, params, result, error, duration_ms, parent_id, recording_id, prev_hash, hash`

type scanner interface{ Scan(...any) error }

func scan(r scanner) (Event, error) {
	var e Event
	var ms int64
	err := r.Scan(&e.ID, &ms, &e.Category, &e.Action, &e.ActorType, &e.ActorID, &e.ActorName, &e.Via, &e.TokenID, &e.Client,
		&e.Session, &e.IP, &e.UserAgent, &e.ProjectID, &e.TargetType, &e.TargetID, &e.TargetName, &e.Params, &e.Result,
		&e.Error, &e.DurationMS, &e.ParentID, &e.RecordingID, &e.PrevHash, &e.Hash)
	e.Time = time.UnixMilli(ms)
	return e, err
}

// Filter 是查询条件，零值表示不限。
type Filter struct {
	From, To   time.Time
	ActorID    string
	Via        string
	Categories []string
	ProjectID  string
	// Projects 不为 nil 时只返回这些项目的记录（非管理员的访问范围），空列表表示什么都看不到。
	Projects []string
	// RequireProject 只返回属于某个项目的记录（访问全部项目的非管理员）。
	RequireProject bool
	NodeID         string
	TargetType     string
	TargetID       string
	// TargetMatch 按对象名称匹配，部署的对象是逗号分隔的几个容器，也算匹配。
	TargetMatch string
	FailedOnly  bool
	Search      string
	// SearchFields 是 Search 匹配的列，按查看者能看到的字段定（见 SearchViewer 等）。为空时匹配全部列（管理员）。
	// 搜索不能匹配查看者看不到的列，否则按有没有结果就能逐字猜出隐藏的内容。
	SearchFields []string
	// SearchSkipNodes：操作者、对象是节点的记录，名称不参与搜索。查看者有看不到的节点时用：这些名称显示时会换成
	// 「其他节点」，搜索匹配的却是原文，按有没有结果能一段一段拼出名称（2026-11-19-402545d#2）。
	SearchSkipNodes bool
	// SearchFilePaths 不为 nil 时，对象是文件、动作不在其中的记录，名称只按容器那一段匹配（对象名称是 <容器>:<路径>）：
	// 查看者看不到这些记录的路径，显示时只留下容器，按原文匹配的话，按有没有结果能一段一段拼出路径。
	SearchFilePaths []string
	ParentID        int64
	// Before 只返回 ID 比它小的（翻页）。
	Before int64
	Limit  int
	// TopLevel 不返回有上级的记录（界面上收在上级下面）。
	TopLevel bool
}

// 搜索匹配的列：管理员全部；操作员看不到 IP 和浏览器；只读成员还看不到参数和失败详情（见 docs/audit.md#权限）。
var (
	SearchAll      = []string{"action", "actor_name", "target_name", "client", "params", "error", "ip"}
	SearchOperator = []string{"action", "actor_name", "target_name", "client", "params", "error"}
	SearchViewer   = []string{"action", "actor_name", "target_name", "client"}
	// SearchNoNames：搜索的词和查看者看不到的节点的名称沾边时，连名称也不匹配。
	SearchNoNames = []string{"action", "client"}
)

func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(c string, a ...any) {
		conds = append(conds, c)
		args = append(args, a...)
	}
	if !f.From.IsZero() {
		add("time >= ?", f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		add("time < ?", f.To.UnixMilli())
	}
	if f.ActorID != "" {
		add("actor_id = ?", f.ActorID)
	}
	if f.Via != "" {
		add("via = ?", f.Via)
	}
	if len(f.Categories) > 0 {
		add("category IN ("+placeholders(len(f.Categories))+")", anySlice(f.Categories)...)
	}
	if f.ProjectID != "" {
		add("project_id = ?", f.ProjectID)
	}
	if f.Projects != nil {
		if len(f.Projects) == 0 {
			add("0")
		} else {
			add("project_id IN ("+placeholders(len(f.Projects))+")", anySlice(f.Projects)...)
		}
	}
	if f.RequireProject {
		add("project_id <> ''")
	}
	if f.NodeID != "" {
		add("target_type = 'node' AND target_id = ?", f.NodeID)
	}
	if f.TargetType != "" {
		add("target_type = ?", f.TargetType)
	}
	if f.TargetID != "" {
		add("target_id = ?", f.TargetID)
	}
	if f.TargetMatch != "" {
		add("(target_name = ? OR ',' || replace(target_name, ' ', '') || ',' LIKE '%,' || ? || ',%')", f.TargetMatch, f.TargetMatch)
	}
	if f.FailedOnly {
		add("result <> 'ok'")
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		like := "%" + escapeLike(s) + "%"
		cols := f.SearchFields
		if cols == nil {
			cols = SearchAll
		}
		var or []string
		var a []any
		for _, c := range cols {
			if !slices.Contains(SearchAll, c) {
				continue
			}
			col := c
			if c == "target_name" && f.SearchFilePaths != nil {
				hidden := "target_type = 'file'"
				if len(f.SearchFilePaths) > 0 {
					hidden += " AND action NOT IN (" + placeholders(len(f.SearchFilePaths)) + ")"
					a = append(a, anySlice(f.SearchFilePaths)...)
				}
				col = "(CASE WHEN " + hidden + " THEN " + fileContainerSQL + " ELSE target_name END)"
			}
			cond := col + ` LIKE ? ESCAPE '\'`
			if f.SearchSkipNodes && c == "actor_name" {
				cond = "(actor_type <> 'node' AND " + cond + ")"
			}
			if f.SearchSkipNodes && c == "target_name" {
				cond = "(target_type <> 'node' AND " + cond + ")"
			}
			or = append(or, cond)
			a = append(a, like)
		}
		if len(or) == 0 {
			add("0")
		} else {
			add("("+strings.Join(or, " OR ")+")", a...)
		}
	}
	if f.ParentID != 0 {
		add("parent_id = ?", f.ParentID)
	}
	if f.TopLevel {
		add("parent_id = 0")
	}
	if f.Before > 0 {
		add("id < ?", f.Before)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// fileContainerSQL 是文件操作的记录里文件所在的容器：对象名称以 :<路径> 结尾时去掉这一段，否则为空（和 api 的 fileContainer 一样）。
const fileContainerSQL = "(CASE WHEN substr(target_name, -length(target_id) - 1) = ':' || target_id " +
	"THEN substr(target_name, 1, length(target_name) - length(target_id) - 1) ELSE '' END)"

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func anySlice(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

// List 按条件返回记录，新的在前。
func (l *Log) List(ctx context.Context, f Filter) ([]Event, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	where, args := f.where()
	rows, err := l.db.QueryContext(ctx, `SELECT `+columns+` FROM events`+where+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Each 按 ID 从小到大逐条读出符合条件的记录（导出用）。
func (l *Log) Each(ctx context.Context, f Filter, fn func(Event) error) error {
	where, args := f.where()
	rows, err := l.db.QueryContext(ctx, `SELECT `+columns+` FROM events`+where+` ORDER BY id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Get 返回一条记录。
func (l *Log) Get(ctx context.Context, id int64) (Event, error) {
	return scan(l.db.QueryRowContext(ctx, `SELECT `+columns+` FROM events WHERE id = ?`, id))
}

// ChildCounts 返回这些记录各有几条下级记录。
func (l *Log) ChildCounts(ctx context.Context, ids []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := l.db.QueryContext(ctx, `SELECT parent_id, COUNT(*) FROM events WHERE parent_id IN (`+placeholders(len(ids))+`) GROUP BY parent_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// Meta 读审计库自己的一项状态。
func (l *Log) Meta(ctx context.Context, key string) string {
	var v string
	_ = l.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}

// SetMeta 写审计库自己的一项状态。
func (l *Log) SetMeta(ctx context.Context, key, value string) error {
	_, err := l.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---- 保留 ----

// Retention 是保留时间和总量上限（见 docs/audit.md#存储保留与同步）。
type Retention struct {
	Normal   time.Duration // 普通记录
	Long     time.Duration // security、session、login、mcp
	MaxBytes int64         // 数据库总量
}

// DefaultRetention 是默认的保留设置。
var DefaultRetention = Retention{Normal: 90 * 24 * time.Hour, Long: 365 * 24 * time.Hour, MaxBytes: 2 << 30}

// LongCategories 是保留得更久的类别。
var LongCategories = []string{Security, Session, Login, MCP}

// Prune 删掉过期的记录；超过总量上限时再从最旧的普通记录删起。
func (l *Log) Prune(ctx context.Context, r Retention, now time.Time) error {
	if l.budget != nil {
		return l.budget.WriteArchive(ctx, 4, 0, func() error { return nil })
	}
	long := "category IN (" + placeholders(len(LongCategories)) + ")"
	args := anySlice(LongCategories)
	if err := l.deleteWhere(ctx, `NOT `+long+` AND time < ?`, append(args, now.Add(-r.Normal).UnixMilli())...); err != nil {
		return err
	}
	if err := l.deleteWhere(ctx, long+` AND time < ?`, append(anySlice(LongCategories), now.Add(-r.Long).UnixMilli())...); err != nil {
		return err
	}
	if r.MaxBytes <= 0 {
		return nil
	}
	for range 100 {
		size := l.size(ctx)
		if size <= r.MaxBytes {
			return nil
		}
		// 每次删最旧的 1%（至少 1000 条），普通记录删完了才删保留更久的。
		var n int64
		_ = l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n)
		batch := max(n/100, 1000)
		var c int64
		_ = l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE NOT `+long, anySlice(LongCategories)...).Scan(&c)
		if c > 0 {
			if err := l.deleteWhere(ctx, `id IN (SELECT id FROM events WHERE NOT `+long+` ORDER BY id LIMIT ?)`, append(anySlice(LongCategories), batch)...); err != nil {
				return err
			}
		} else if err := l.deleteWhere(ctx, `id IN (SELECT id FROM events ORDER BY id LIMIT ?)`, batch); err != nil {
			return err
		}
		if _, err := l.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return err
		}
	}
	return nil
}

// deleteWhere 删除符合条件的记录，留下墓碑（ID 和哈希），哈希链还能接上；比剩下的记录都旧的墓碑一起删掉。
func (l *Log) deleteWhere(ctx context.Context, cond string, args ...any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ids, err := queryIDs(ctx, tx, `SELECT id FROM events WHERE `+cond+` ORDER BY id`, args...)
	if err != nil || len(ids) == 0 {
		return err
	}
	// 要删掉的清理记录列出的 ID，墓碑还在的由这次的清理记录接着列出（见 pruned.go）。
	var carried coverage
	if rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM events WHERE action = 'audit_pruned' AND (`+cond+`)`, args...); err == nil {
		for rows.Next() {
			if e, err := scan(rows); err == nil {
				carried.add(e)
			}
		}
		rows.Close()
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO tombstones (id, hash) SELECT id, hash FROM events WHERE `+cond, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE `+cond, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tombstones WHERE id < (SELECT MIN(id) FROM events)`); err != nil {
		return err
	}
	if len(carried.ranges) > 0 {
		left, err := queryIDs(ctx, tx, `SELECT id FROM tombstones ORDER BY id`)
		if err != nil {
			return err
		}
		for _, id := range left {
			if carried.covers(id) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		ids = slices.Compact(ids)
	}
	// 清理记录和删除在同一个事务里：中途崩溃不会留下没有清理记录的墓碑。
	last, err := l.writePruneRecordsTx(ctx, tx, ids, false)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	l.lastHash = last
	return nil
}

// size 是数据库实际占用的字节数（不含空闲页）。
func (l *Log) size(ctx context.Context) int64 {
	var pages, free, pageSize int64
	_ = l.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages)
	_ = l.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free)
	_ = l.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize)
	if pages == 0 {
		if st, err := os.Stat(l.path); err == nil {
			return st.Size()
		}
	}
	return (pages - free) * pageSize
}

// PruneLoop 每小时清理一次，直到 ctx 取消。retention 每次调用时读取，设置改了下一次就生效。
func (l *Log) PruneLoop(ctx context.Context, retention func(context.Context) Retention) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := l.Prune(ctx, retention(ctx), time.Now()); err != nil && ctx.Err() == nil {
			l.log.Warn("清理审计记录失败", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- ctx ----

type parentKey struct{}

// WithParent 标记 ctx 里的调用是 id 这条记录触发的（MCP 工具调用触发的接口调用）。
func WithParent(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, parentKey{}, id)
}

// Parent 返回 ctx 里的上级记录。
func Parent(ctx context.Context) int64 {
	id, _ := ctx.Value(parentKey{}).(int64)
	return id
}

// SessionTag 是记录里的 session 字段：会话 ID 的哈希前 12 位。
func SessionTag(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("nagi-session:" + sessionID))
	return hex.EncodeToString(sum[:])[:12]
}

// Itoa 是 strconv.FormatInt 的简写。
func Itoa(v int64) string { return strconv.FormatInt(v, 10) }

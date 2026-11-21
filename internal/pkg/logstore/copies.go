package logstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Copies owns a separate, bounded recent-history database. Authentication belongs
// to its mesh receiver: node is always resolved from the caller's canonical key.
// The source cannot delete copies or change already acknowledged evidence.
type Copies struct {
	s, budget *Store
	retention time.Duration
}

type CopyBatch struct {
	Source, Category string
	After, Through   int64
	Events           []Event
	Pruned           []Pruned
	WriteErrors      uint64
}

type CopyReceipt struct {
	Node, Source, Category string
	Received               int64
	After                  int64
	// Last includes panel-to-panel additions; only After acknowledges the node's prefix.
	Last        int64
	Pruned      []Pruned
	WriteErrors uint64
}

var ErrCopyCursor = errors.New("近期副本确认位置不匹配，请重新读取确认位置")
var ErrCopyConflict = errors.New("重发的日志正文与面板已保存证据冲突")

// OpenCopies puts recent.db beside this machine's logs.db, outside role-swapped
// panel configuration directories. The 1 GiB physical cap includes WAL/SHM;
// this is a sub-budget, not another allowance on top of the machine's capacity.
func (s *Store) OpenCopies(ctx context.Context, cfg Config) (*Copies, error) {
	if s == nil {
		return nil, fmt.Errorf("本机日志容量管理没有启动")
	}
	if cfg.RecentCapacityMiB == 0 {
		cfg.RecentCapacityMiB = 1024
	}
	if cfg.RecentHours == 0 {
		cfg.RecentHours = 24
	}
	if cfg.RecentCapacityMiB < 16 || cfg.RecentCapacityMiB > 1024 || cfg.RecentHours < 1 || cfg.RecentHours > 24 {
		return nil, fmt.Errorf("无效的近期副本容量或保留时间")
	}
	path := filepath.Join(filepath.Dir(s.path), "recent.db")
	// Open at the fixed maximum before applying a smaller configured quota.
	// Only this owner may prune copies: generic journal pruning would discard
	// the original remote identities needed by the deletion summaries.
	dest, err := open(ctx, path, GiB, false)
	if err != nil {
		return nil, err
	}
	c := &Copies{s: dest, budget: s, retention: time.Duration(cfg.RecentHours) * time.Hour}
	var deletedTable bool
	if err := dest.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='copy_deleted')`).Scan(&deletedTable); err != nil {
		dest.Close()
		return nil, err
	}
	tx, err := dest.db.BeginTx(ctx, nil)
	if err != nil {
		dest.Close()
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS copy_received ON log_events(priority,received_ns,seq);
		CREATE TABLE IF NOT EXISTS copy_origins (
		 local_seq INTEGER PRIMARY KEY REFERENCES log_events(seq) ON DELETE CASCADE,
		 node TEXT NOT NULL, origin TEXT NOT NULL, remote_seq INTEGER NOT NULL,
		 UNIQUE(node,origin,remote_seq));
		CREATE TABLE IF NOT EXISTS copy_pruned (
		 node TEXT NOT NULL, origin TEXT NOT NULL, category TEXT NOT NULL,
		 first_seq INTEGER NOT NULL, last_seq INTEGER NOT NULL, count INTEGER NOT NULL,
		 PRIMARY KEY(node,origin,category));
		CREATE TABLE IF NOT EXISTS copy_deleted (
		 node TEXT NOT NULL, origin TEXT NOT NULL, category TEXT NOT NULL,
		 first_seq INTEGER NOT NULL, last_seq INTEGER NOT NULL,
		 PRIMARY KEY(node,origin,category,first_seq));`)
	if err == nil && !deletedTable {
		// Old versions retained envelopes only. Conservatively protect those
		// envelopes from resurrection; new deletions have exact ranges.
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO copy_deleted SELECT node,origin,category,first_seq,last_seq FROM copy_pruned`)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		_ = tx.Rollback()
		dest.Close()
		return nil, err
	}
	dest.budget = int64(cfg.RecentCapacityMiB) << 20
	dest.dbLimit = dest.budget * 4 / 5
	if err = c.expire(ctx); err != nil {
		dest.Close()
		return nil, err
	}
	for level := 0; level < 5; level++ {
		for {
			n, e := dest.diskBytes()
			if e != nil {
				dest.Close()
				return nil, e
			}
			if n <= dest.dbLimit {
				break
			}
			changed, e := c.trim(ctx, level, time.Time{})
			if e != nil {
				dest.Close()
				return nil, e
			}
			if !changed {
				break
			}
		}
	}
	if n, e := dest.diskBytes(); e != nil || n > dest.dbLimit {
		dest.Close()
		return nil, fmt.Errorf("近期副本确认元数据超过容量，保留原库: bytes=%d: %v", n, e)
	}
	var pageSize int64
	if err = dest.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err == nil {
		_, err = dest.db.ExecContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", dest.dbLimit/pageSize))
	}
	if err != nil {
		dest.Close()
		return nil, err
	}
	// One size owner, five independent priority-specific reclaimers. Counting
	// physical bytes five times or assigning all copies audit priority is wrong.
	for p := range 5 {
		size := func() (int64, error) { return 0, nil }
		if p == 0 {
			size = func() (int64, error) { return FileBytes(path) }
		}
		err = s.ReplaceArchive(ctx, Archive{Name: fmt.Sprintf("%s#%d", path, p), Priority: p, Size: size,
			Trim: func(ctx context.Context) (bool, error) {
				if err := dest.lock(ctx); err != nil {
					return false, err
				}
				defer func() { <-dest.gate }()
				if dest.closed {
					return false, nil
				}
				return c.trim(ctx, p, time.Time{})
			}})
		if err != nil {
			dest.Close()
			return nil, err
		}
	}
	return c, nil
}

func (c *Copies) Close() error {
	if c == nil {
		return nil
	}
	return c.s.Close()
}

func copyKey(node, source, category string) string {
	h := sha256.Sum256([]byte(node + "\x00" + source + "\x00" + category))
	return fmt.Sprintf("copy.%x", h)
}

func (c *Copies) Receipt(ctx context.Context, node, source, category string) (CopyReceipt, error) {
	var r CopyReceipt
	if c == nil {
		return r, fmt.Errorf("近期副本没有启动")
	}
	if err := validCopyOrigin(node, source, category); err != nil {
		return r, err
	}
	b, err := c.s.Checkpoint(ctx, copyKey(node, source, category))
	if err == nil && len(b) > 0 {
		err = json.Unmarshal(b, &r)
	}
	return r, err
}

func validCopyOrigin(node, source, category string) error {
	_, ok := priority(category)
	if node == "" || len(node) > 200 || source == "" || len(source) > 100 || strings.ContainsRune(node+source, '\x00') || !ok {
		return fmt.Errorf("无效的近期副本来源或类别")
	}
	return nil
}

// Receive atomically commits the whole batch and its acknowledgement. Replays
// never rewrite the first saved copy; a lost reply is safe to retry. Category
// streams have independent positions, so noisy stdout cannot hold back audit.
func (c *Copies) Receive(ctx context.Context, node string, b CopyBatch) (CopyReceipt, error) {
	return c.receive(ctx, node, b, false)
}

func (c *Copies) receive(ctx context.Context, node string, b CopyBatch, fromPanel bool) (CopyReceipt, error) {
	var receipt CopyReceipt
	if c == nil {
		return receipt, fmt.Errorf("近期副本没有启动")
	}
	if err := validCopyOrigin(node, b.Source, b.Category); err != nil {
		return receipt, err
	}
	if b.After < 0 || b.Through < b.After || len(b.Events) > 256 || len(b.Pruned) > 1 {
		return receipt, fmt.Errorf("无效的近期副本批次")
	}
	type encoded struct {
		e    Event
		data []byte
	}
	var entries []encoded
	var totalBytes int64
	last := b.After
	for _, e := range b.Events {
		if e.Seq <= last || e.Seq > b.Through || e.Category != b.Category || e.Kind == "" || len(e.Kind) > 256 || len(e.Project) > 256 || len(e.Object) > 1064 || e.Time.IsZero() || !time.Unix(0, e.Time.UnixNano()).Equal(e.Time) {
			return receipt, fmt.Errorf("无效或乱序的近期副本记录")
		}
		data, err := json.Marshal(e.Data)
		if err != nil || len(data) > maxRecordBytes {
			return receipt, fmt.Errorf("无效或过大的近期副本正文")
		}
		data, err = sanitizeJSON(data)
		if err != nil {
			return receipt, err
		}
		totalBytes += int64(len(data) + len(e.Kind) + len(e.Project) + len(e.Object) + 512)
		if totalBytes > 1<<20 {
			return receipt, fmt.Errorf("近期副本批次过大")
		}
		entries = append(entries, encoded{e, data})
		last = e.Seq
	}
	for _, p := range b.Pruned {
		if p.Category != b.Category || p.First <= 0 || p.Last < p.First || p.Last > b.Through || p.Count < 1 {
			return receipt, fmt.Errorf("无效的来源清理摘要")
		}
	}
	p, _ := priority(b.Category)
	// The machine-wide gate is held only for the capacity check, never for the
	// copy transaction: one panel receives every node's copies and must not
	// starve its own audit and alert writes (v0.37.0 field failure).
	if err := c.budget.Reserve(ctx, p, totalBytes*2+(64<<10)); err != nil {
		return receipt, err
	}
	err := func() error {
		if err := c.s.lock(ctx); err != nil {
			return err
		}
		defer func() { <-c.s.gate }()
		if c.s.closed {
			return sql.ErrConnDone
		}
		key := copyKey(node, b.Source, b.Category)
		var old []byte
		err := c.s.db.QueryRowContext(ctx, `SELECT data FROM log_checkpoints WHERE key=?`, key).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if len(old) > 0 {
			if err := json.Unmarshal(old, &receipt); err != nil {
				return err
			}
		}
		if !fromPanel && b.Through <= receipt.After {
			for _, v := range entries {
				var at int64
				var category, kind, project, object string
				var data []byte
				err := c.s.db.QueryRowContext(ctx, `SELECT e.time_ns,e.category,e.kind,e.project,e.object,e.data FROM log_events e JOIN copy_origins o ON o.local_seq=e.seq WHERE o.node=? AND o.origin=? AND o.remote_seq=?`, node, b.Source, v.e.Seq).Scan(&at, &category, &kind, &project, &object, &data)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				} // Expired evidence is never resurrected.
				if err != nil {
					return err
				}
				if at != v.e.Time.UnixNano() || category != v.e.Category || kind != clipText(v.e.Kind, 200) || project != clipText(v.e.Project, 200) || object != clipText(v.e.Object, 1000) || !bytes.Equal(data, v.data) {
					return ErrCopyConflict
				}
			}
			return nil
		}
		if !fromPanel && b.After != receipt.After {
			return ErrCopyCursor
		}
		for {
			n, err := c.s.diskBytes()
			if err != nil {
				return err
			}
			if n+totalBytes*2+(64<<10) <= c.s.dbLimit {
				break
			}
			changed := false
			for level := 0; level <= p; level++ {
				var err error
				changed, err = c.trim(ctx, level, time.Time{})
				if err != nil {
					return err
				}
				if changed {
					break
				}
			}
			if !changed {
				return ErrCapacity
			}
		}
		tx, err := c.s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		now := time.Now().UTC().UnixNano()
		for _, v := range entries {
			e := v.e
			var at int64
			var category, kind, project, object string
			var data []byte
			err := tx.QueryRowContext(ctx, `SELECT e.time_ns,e.category,e.kind,e.project,e.object,e.data FROM log_events e JOIN copy_origins o ON o.local_seq=e.seq WHERE o.node=? AND o.origin=? AND o.remote_seq=?`, node, b.Source, e.Seq).Scan(&at, &category, &kind, &project, &object, &data)
			if err == nil {
				if at != e.Time.UnixNano() || category != e.Category || kind != clipText(e.Kind, 200) || project != clipText(e.Project, 200) || object != clipText(e.Object, 1000) || !bytes.Equal(data, v.data) {
					return ErrCopyConflict
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			received := now
			if fromPanel {
				received = e.Received.UnixNano()
				if e.Received.IsZero() || received > now {
					return fmt.Errorf("无效或超前的面板接收时间")
				}
				if received <= now-int64(c.retention) {
					continue
				}
			}
			// A node may reconnect behind a peer-recovered record that this
			// panel has since pruned. Neither upload path may resurrect it.
			var deleted bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM copy_deleted WHERE node=? AND origin=? AND category=? AND first_seq<=? AND last_seq>=?)`, node, b.Source, b.Category, e.Seq, e.Seq).Scan(&deleted); err != nil {
				return err
			}
			if deleted {
				continue
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO log_events(time_ns,received_ns,category,priority,kind,project,object,data) VALUES (?,?,?,?,?,?,?,?)`, e.Time.UnixNano(), received, e.Category, p, clipText(e.Kind, 200), clipText(e.Project, 200), clipText(e.Object, 1000), v.data)
			if err != nil {
				return err
			}
			seq, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO copy_origins VALUES (?,?,?,?)`, seq, node, b.Source, e.Seq); err != nil {
				return err
			}
		}
		pruned := receipt.Pruned
		if len(b.Pruned) > 0 {
			pruned = b.Pruned
		}
		after := receipt.After
		if !fromPanel {
			after = b.Through
		}
		receipt = CopyReceipt{Node: node, Source: b.Source, Category: b.Category, Received: now, After: after, Last: max(receipt.Last, b.Through), Pruned: pruned, WriteErrors: max(receipt.WriteErrors, b.WriteErrors)}
		data, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO log_checkpoints(key,data) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data`, key, data); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		c.s.trimWAL(ctx)
		return nil
	}()
	return receipt, err
}

// Expire uses panel receipt time, never a node-controlled clock. Confirmation
// positions survive expiry and capacity pruning, so reconnects don't resurrect
// deleted evidence or copy the same old records forever.
// Each batch takes recent.db's gate briefly, so receipts interleave with a
// large expiry instead of waiting for the whole sweep.
func (c *Copies) Expire(ctx context.Context) error {
	for p := range 5 {
		for {
			changed, err := func() (bool, error) {
				if err := c.s.lock(ctx); err != nil {
					return false, err
				}
				defer func() { <-c.s.gate }()
				if c.s.closed {
					return false, sql.ErrConnDone
				}
				return c.trim(ctx, p, time.Now().Add(-c.retention))
			}()
			if err != nil || !changed {
				if err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}
func (c *Copies) expire(ctx context.Context) error {
	for p := range 5 {
		for {
			changed, err := c.trim(ctx, p, time.Now().Add(-c.retention))
			if err != nil {
				return err
			}
			if !changed {
				break
			}
		}
	}
	return nil
}

// Caller holds c.s.gate. No callback may acquire the machine budget again.
func (c *Copies) trim(ctx context.Context, p int, before time.Time) (bool, error) {
	cutoff := int64(1<<63 - 1)
	if !before.IsZero() {
		cutoff = before.UnixNano()
	}
	tx, err := c.s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT e.seq,o.node,o.origin,o.remote_seq,e.category FROM log_events e JOIN copy_origins o ON o.local_seq=e.seq WHERE e.priority=? AND e.received_ns<? ORDER BY e.received_ns,e.seq LIMIT 64`, p, cutoff)
	if err != nil {
		return false, err
	}
	type removed struct {
		seq, remote            int64
		node, origin, category string
	}
	var batch []removed
	for rows.Next() {
		var r removed
		if err := rows.Scan(&r.seq, &r.node, &r.origin, &r.remote, &r.category); err != nil {
			rows.Close()
			return false, err
		}
		batch = append(batch, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(batch) == 0 {
		return false, nil
	}
	for _, r := range batch {
		// Merge adjacent exact tombstones, never the unreceived holes between
		// them. This keeps sequential deletion metadata compact across restarts.
		first, last := r.remote, r.remote
		if err := tx.QueryRowContext(ctx, `SELECT min(?,coalesce(min(first_seq),?)),max(?,coalesce(max(last_seq),?)) FROM copy_deleted WHERE node=? AND origin=? AND category=? AND first_seq<=? AND last_seq>=?`, first, first, last, last, r.node, r.origin, r.category, last+1, first-1).Scan(&first, &last); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM copy_deleted WHERE node=? AND origin=? AND category=? AND first_seq<=? AND last_seq>=?`, r.node, r.origin, r.category, last, first); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO copy_deleted VALUES (?,?,?,?,?)`, r.node, r.origin, r.category, first, last); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO copy_pruned VALUES (?,?,?,?,?,1) ON CONFLICT(node,origin,category) DO UPDATE SET first_seq=min(first_seq,excluded.first_seq),last_seq=max(last_seq,excluded.last_seq),count=count+1`, r.node, r.origin, r.category, r.remote, r.remote); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM log_events WHERE seq=?`, r.seq); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, c.s.compact(ctx)
}

// Read returns remote source/sequence values, not recent.db's local row IDs.
// Pruned describes local copy pruning; source pruning is returned separately.
func (c *Copies) Read(ctx context.Context, node, source string, f Filter) (Page, []Pruned, error) {
	var page Page
	if c == nil {
		return page, nil, fmt.Errorf("近期副本没有启动")
	}
	// Independent category streams can be at different watermarks. Never let
	// one fast category advance a merged cursor past another category's backlog.
	if err := validCopyOrigin(node, "source", f.Category); err != nil {
		return page, nil, err
	}
	if f.After < 0 || f.After > 0 && source == "" || f.Limit < 0 || f.Limit > 1000 || !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return page, nil, fmt.Errorf("无效的近期副本查询")
	}
	if err := c.s.lock(ctx); err != nil {
		return page, nil, err
	}
	defer func() { <-c.s.gate }()
	if source == "" {
		err := c.s.db.QueryRowContext(ctx, `SELECT json_extract(data,'$.Source') FROM log_checkpoints WHERE key LIKE 'copy.%' AND json_extract(data,'$.Node')=? AND json_extract(data,'$.Category')=? ORDER BY json_extract(data,'$.Received') DESC LIMIT 1`, node, f.Category).Scan(&source)
		if err != nil {
			return page, nil, err
		}
	}
	if err := validCopyOrigin(node, source, f.Category); err != nil {
		return page, nil, err
	}
	where, args := []string{"o.node=?", "o.origin=?", "o.remote_seq>?"}, []any{node, source, f.After}
	for _, v := range []struct{ key, value string }{{"category", f.Category}, {"project", f.Project}, {"object", f.Object}} {
		if v.value != "" {
			where = append(where, "e."+v.key+"=?")
			args = append(args, v.value)
		}
	}
	if !f.From.IsZero() {
		where = append(where, "e.time_ns>=?")
		args = append(args, f.From.UnixNano())
	}
	if !f.To.IsZero() {
		where = append(where, "e.time_ns<?")
		args = append(args, f.To.UnixNano())
	}
	if f.Search != "" {
		where = append(where, "instr(lower(e.kind || ' ' || e.project || ' ' || e.object || ' ' || CAST(e.data AS TEXT)),lower(?))>0")
		args = append(args, f.Search)
	}
	if f.KindPrefix != "" {
		where = append(where, "substr(e.kind,1,length(?))=?")
		args = append(args, f.KindPrefix, f.KindPrefix)
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	args = append(args, f.Limit)
	rows, err := c.s.db.QueryContext(ctx, `SELECT o.remote_seq,e.time_ns,e.received_ns,e.category,e.kind,e.project,e.object,e.data FROM log_events e JOIN copy_origins o ON o.local_seq=e.seq WHERE `+strings.Join(where, " AND ")+` ORDER BY o.remote_seq LIMIT ?`, args...)
	if err != nil {
		return page, nil, err
	}
	var readBytes int
	for rows.Next() {
		e := Event{Source: source}
		var at, received int64
		var data []byte
		if err := rows.Scan(&e.Seq, &at, &received, &e.Category, &e.Kind, &e.Project, &e.Object, &data); err != nil {
			rows.Close()
			return page, nil, err
		}
		readBytes += len(data) + len(e.Kind) + len(e.Object) + len(e.Project) + 512
		if readBytes > 1<<20 {
			page.More = true
			break
		}
		e.Time, e.Received, e.Data = time.Unix(0, at).UTC(), time.Unix(0, received).UTC(), json.RawMessage(data)
		page.Events = append(page.Events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, nil, err
	}
	page.State.Source = source
	page.Next = f.After
	page.More = page.More || len(page.Events) == f.Limit
	if len(page.Events) > 0 {
		page.Next = page.Events[len(page.Events)-1].Seq
	}
	var sourcePruned []Pruned
	found := false
	for _, category := range []string{Runtime, Access, Container, Metrics, Task, Audit, Security, Alert, System} {
		if f.Category != "" && f.Category != category {
			continue
		}
		var b []byte
		err := c.s.db.QueryRowContext(ctx, `SELECT data FROM log_checkpoints WHERE key=?`, copyKey(node, source, category)).Scan(&b)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return page, nil, err
		}
		var r CopyReceipt
		if err := json.Unmarshal(b, &r); err != nil {
			return page, nil, err
		}
		found = true
		page.State.Last = max(page.State.Last, r.After, r.Last)
		page.State.WriteErrors = max(page.State.WriteErrors, r.WriteErrors)
		sourcePruned = append(sourcePruned, r.Pruned...)
	}
	if !found {
		return page, nil, sql.ErrNoRows
	}
	if f.After > page.State.Last {
		return page, nil, fmt.Errorf("日志游标超过近期副本已确认位置")
	}
	if !page.More {
		page.Next = max(page.Next, page.State.Last)
	}
	rows, err = c.s.db.QueryContext(ctx, `SELECT category,first_seq,last_seq,count FROM copy_pruned WHERE node=? AND origin=?`, node, source)
	if err != nil {
		return page, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Pruned
		if err := rows.Scan(&p.Category, &p.First, &p.Last, &p.Count); err != nil {
			return page, nil, err
		}
		page.State.Pruned = append(page.State.Pruned, p)
	}
	return page, sourcePruned, rows.Err()
}

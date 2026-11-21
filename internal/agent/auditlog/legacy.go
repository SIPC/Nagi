package auditlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// 旧版本（v0.37.0 及以前）的 JSON Lines 文件：<数据目录>/audit/audit.jsonl，满 10 MB 改名为
// audit.<最后的序号>.jsonl。v0.37.1 起：
//
//  1. ImportLegacy 按原序号把每一行原样（字节不变）导入新库，依次算哈希链。旧文件之间缺的序号（旧版本按容量
//     删掉的文件）留下墓碑，链上的导入记录 audit_legacy_import 列出这些序号，校验时据此认可墓碑；最早一个文件
//     之前缺的不需要接，链从第一行开始。行的字节和序号都不变，面板的外送游标照用：不重发、不漏发。导入完以后
//     新记录只写新库，接着旧序号。
//     导入完成之前（例如旧文件有坏行）新记录照旧追加到旧文件，下次启动一起导入，不会丢也不会乱序；这期间
//     ReadAuditLog 不返回记录，面板的游标不动。中途被杀：下次从库里已有的最大序号接着导入（按批提交，链头
//     总是接得上）。
//  2. CleanupLegacy 在升级确认（连上面板）以后才删除旧文件：节点升级失败会自动换回旧版本，旧版本还要用这些文件。
//     删除前再核对一遍：新库整链校验通过，旧文件的每一行都和库里同一序号的内容逐字节相同。核对不通过就保留
//     旧文件，日志页提示，下次启动重试。删除前后各记一条日志，检查点记下完成。
//  3. 删除以后旧文件又出现（退回过旧版本、旧版本又写了一些）：这些行的序号和新库冲突，所以作为新记录追加
//     （action legacy_line，detail 是原样的一行，target 是文件名），同样核对后删除。

const (
	current      = "audit.jsonl"
	legacyImport = "audit_legacy_import"
	legacyLine   = "legacy_line"
	// relayStartFile 是 v0.29.1 以前节点自己外送时留下的起点（见 ship.go），导入时存进本库的 meta。
	relayStartFile = "relay-start.json"
)

// CleanupID is the live migration entry that reports the cleanup to the log page.
const CleanupID = "cleanup.nodeAudit"

const cleanupCheckpoint = "legacy.cleanup.node.audit"

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

// files 返回所有存在的旧文件，旧的在前。
func files(dir string) []string {
	out := rotated(dir)
	if st, err := os.Stat(filepath.Join(dir, current)); err == nil && !st.IsDir() {
		out = append(out, current)
	}
	return out
}

// legacySeq 是旧文件里最大的序号，没有旧文件时为 0。
func legacySeq(dir string) int64 {
	all := files(dir)
	for i := len(all) - 1; i >= 0; i-- {
		var last int64
		_ = scanLines(filepath.Join(dir, all[i]), func(line []byte) error {
			if s := lineSeq(line); s > last {
				last = s
			}
			return nil
		})
		if last > 0 {
			return last
		}
		if n := strings.TrimSuffix(strings.TrimPrefix(all[i], "audit."), ".jsonl"); n != all[i] {
			if v, err := strconv.ParseInt(n, 10, 64); err == nil && v > 0 {
				return v
			}
		}
	}
	return 0
}

func lineSeq(line []byte) int64 {
	var e struct {
		Seq int64 `json:"seq"`
	}
	if json.Unmarshal(line, &e) != nil {
		return 0
	}
	return e.Seq
}

func scanLines(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// appendLegacyLocked appends e to the legacy file while its import is pending;
// the caller holds l.mu.
func (l *Log) appendLegacyLocked(e *Event) error {
	e.Seq = l.seq + 1
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.legacy, current), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	l.seq = e.Seq
	return nil
}

type importSummary struct {
	Through  int64    `json:"through"`
	Lines    int64    `json:"lines"`
	Gaps     []string `json:"gaps,omitempty"`
	Files    []string `json:"files"`
	Rollback bool     `json:"rollback,omitempty"`
}

// ImportLegacy imports the legacy JSON Lines files into the chain. Call it after
// Open; it is idempotent and resumes after a kill. On error the files stay the
// write target and the import is retried on the next start.
func (l *Log) ImportLegacy(ctx context.Context) error {
	if l == nil || l.legacy == "" {
		return nil
	}
	names := files(l.legacy)
	if len(names) == 0 {
		return nil
	}
	if l.meta("legacy_imported") != "" {
		return l.importRollback(ctx, names)
	}
	// Bulk pass without the lock: Record keeps appending to the legacy file
	// meanwhile (pending), so the node serves requests during a long import.
	// The final pass below holds the lock, takes the few lines written since,
	// and switches new records over to the chain.
	if err := l.importLegacy(ctx, names, false); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.importLegacy(ctx, files(l.legacy), true)
}

// importLegacy imports legacy lines after the chain head. Without final it
// reads only up to each file's current end and leaves pending set; with final
// the caller holds l.mu and the import is completed.
func (l *Log) importLegacy(ctx context.Context, names []string, final bool) error {
	var head struct {
		seq  int64
		hash string
	}
	err := l.db.QueryRowContext(ctx, `SELECT seq, hash FROM (SELECT seq, hash FROM audit_events UNION ALL SELECT seq, hash FROM audit_tombstones) ORDER BY seq DESC LIMIT 1`).Scan(&head.seq, &head.hash)
	if err != nil && !errors.Is(err, sqlNoRows) {
		return err
	}
	lastHash, lastSeq := head.hash, head.seq
	after := lastSeq
	expect := after + 1
	var batch [][]byte
	var seqs []int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		tx, err := l.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		prev, last := lastHash, lastSeq
		for i, line := range batch {
			// Lines lost before the first surviving one need no bridge: the chain starts there.
			for s := last + 1; last > 0 && s < seqs[i]; s++ {
				if _, err := tx.ExecContext(ctx, `INSERT INTO audit_tombstones(seq,hash) VALUES(?,?)`, s, prev); err != nil {
					return err
				}
			}
			hash := ChainHash(prev, line)
			if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(seq,line,prev_hash,hash) VALUES(?,?,?,?)`, seqs[i], line, prev, hash); err != nil {
				return err
			}
			prev, last = hash, seqs[i]
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		lastHash, lastSeq = prev, last
		batch, seqs = batch[:0], seqs[:0]
		return nil
	}
	for _, name := range names {
		if !final && name == current {
			// Record may be appending to it right now; a half-written last
			// line must not be mistaken for a corrupt one. Read it under the lock.
			continue
		}
		err := scanLines(filepath.Join(l.legacy, name), func(raw []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			s := lineSeq(raw)
			if s <= 0 {
				return fmt.Errorf("旧节点审计 %s 含无法识别的行，保留旧文件，导入停在序号 %d", name, expect-1)
			}
			if s <= after {
				return nil
			}
			if s < expect {
				return fmt.Errorf("旧节点审计 %s 序号乱序（%d），保留旧文件", name, s)
			}
			batch = append(batch, append([]byte(nil), raw...))
			seqs = append(seqs, s)
			expect = s + 1
			if len(batch) == 512 {
				return flush()
			}
			return nil
		})
		if err != nil {
			// Keep what was read before the bad line; the rest waits for a retry.
			return errors.Join(err, flush())
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if !final {
		return nil
	}
	l.lastHash, l.seq = lastHash, lastSeq
	// Every tombstone so far comes from gaps between legacy files, including
	// gaps found by earlier, interrupted runs.
	rows, err := l.db.QueryContext(ctx, `SELECT seq FROM audit_tombstones ORDER BY seq`)
	if err != nil {
		return err
	}
	var gaps []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		gaps = append(gaps, s)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var lines int64
	_ = l.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events`).Scan(&lines)
	sum := importSummary{Through: l.seq, Lines: lines, Gaps: formatRanges(gaps), Files: names}
	if b, err := os.ReadFile(filepath.Join(l.legacy, relayStartFile)); err == nil {
		if err := l.setMeta("relay_start", string(b)); err != nil {
			return err
		}
	}
	if err := l.appendSystemLocked(ctx, legacyImport, sum); err != nil {
		return err
	}
	if err := l.setMeta("legacy_imported", strconv.FormatInt(sum.Through, 10)); err != nil {
		return err
	}
	l.pending.Store(false)
	return nil
}

// importRollback appends lines an older version wrote after the import, as new
// records carrying each original line unchanged.
func (l *Log) importRollback(ctx context.Context, names []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, name := range names {
		var fresh [][]byte
		err := scanLines(filepath.Join(l.legacy, name), func(raw []byte) error {
			var n int
			if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE line=?`, raw).Scan(&n); err != nil || n > 0 {
				return err // imported before, with its original number
			}
			if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE json_extract(line,'$.action')=? AND json_extract(line,'$.detail')=?`, legacyLine, string(raw)).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				fresh = append(fresh, append([]byte(nil), raw...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(fresh) == 0 {
			continue
		}
		for _, raw := range fresh {
			if err := l.appendRawLocked(ctx, Event{Action: legacyLine, Target: name, Detail: string(raw), Result: "ok"}); err != nil {
				return err
			}
		}
		if err := l.appendSystemLocked(ctx, legacyImport, importSummary{Rollback: true, Lines: int64(len(fresh)), Files: []string{name}, Through: l.seq}); err != nil {
			return err
		}
	}
	return nil
}

func (l *Log) appendSystemLocked(ctx context.Context, action string, data any) error {
	b, _ := json.Marshal(data)
	return l.appendRawLocked(ctx, Event{Action: action, Detail: string(b), Result: "ok"})
}

// appendRawLocked appends e unclipped; the caller holds l.mu.
func (l *Log) appendRawLocked(ctx context.Context, e Event) error {
	e.Time = nowUTC()
	return l.appendLocked(ctx, &e)
}

// LegacyPending reports whether legacy files are still waiting to be imported.
func (l *Log) LegacyPending() bool { return l != nil && l.pending.Load() }

// CleanupLegacy deletes the legacy files once the store verifies on its own and
// holds every legacy line unchanged. The unified-log copy for the log page is
// read from the store, not from these files, so it does not have to finish
// first. Call only after the upgrade is confirmed, so an automatic rollback
// still finds its files.
func (l *Log) CleanupLegacy(ctx context.Context, logs *logstore.Store) (err error) {
	if l == nil || l.legacy == "" || logs == nil || l.pending.Load() {
		return nil
	}
	names := files(l.legacy)
	if len(names) == 0 {
		removeLeftovers(l.legacy)
		return nil
	}
	logs.SetMigration(logstore.Migration{ID: CleanupID, Phase: "cleanup"})
	defer func() {
		if err != nil && ctx.Err() == nil {
			logs.SetMigration(logstore.Migration{ID: CleanupID, Phase: "failed", Error: err.Error()})
		}
	}()

	res, err := l.Verify(ctx)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("新审计库校验失败（%s），保留旧文件", res.Reason)
	}
	compared, err := l.compareLegacy(ctx, names)
	if err != nil {
		return err
	}
	data := map[string]any{"dir": l.legacy, "files": names, "lines": compared, "records": res.Records,
		"tombstones": res.Tombstones, "last_seq": res.LastSeq, "last_hash": res.LastHash}
	if err := logs.Record(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.node.audit.deleting", Data: data}); err != nil {
		return err
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(l.legacy, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	removeLeftovers(l.legacy)
	if err := logs.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.node.audit.done", Data: data},
		cleanupCheckpoint, map[string]any{"phase": "done", "last_seq": res.LastSeq}); err != nil {
		return err
	}
	logs.SetMigration(logstore.Migration{ID: CleanupID, Phase: "complete"})
	return nil
}

// removeLeftovers removes the old shipping position files (their content is in
// the store's metadata) and the directory once empty.
func removeLeftovers(dir string) {
	for _, n := range []string{relayStartFile, relayStartFile + ".tmp", "cursors.json"} {
		_ = os.Remove(filepath.Join(dir, n))
	}
	_ = os.Remove(dir)
}

// compareLegacy checks every legacy line against the store, byte for byte.
func (l *Log) compareLegacy(ctx context.Context, names []string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	first := l.firstSeq(ctx)
	var n int64
	for _, name := range names {
		err := scanLines(filepath.Join(l.legacy, name), func(raw []byte) error {
			s := lineSeq(raw)
			if s <= 0 {
				return fmt.Errorf("%s 含无法识别的行，保留旧文件", name)
			}
			var saved []byte
			err := l.db.QueryRowContext(ctx, `SELECT line FROM audit_events WHERE seq=?`, s).Scan(&saved)
			if err == nil && bytes.Equal(saved, raw) {
				n++
				return nil
			}
			// Written by an older version after the import: carried verbatim.
			var c int
			if qerr := l.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE json_extract(line,'$.action')=? AND json_extract(line,'$.detail')=?`, legacyLine, string(raw)).Scan(&c); qerr == nil && c > 0 {
				n++
				return nil
			}
			// Removed from the store by its own capacity cleanup: a tombstone the
			// chain check accepted, or older than everything the chain still bridges.
			if errors.Is(err, sqlNoRows) {
				var t int
				if l.db.QueryRowContext(ctx, `SELECT 1 FROM audit_tombstones WHERE seq=?`, s).Scan(&t) == nil || s < first {
					n++
					return nil
				}
			}
			return fmt.Errorf("%s 的第 %d 条与新审计库不一致或缺失，保留旧文件", name, s)
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (l *Log) firstSeq(ctx context.Context) int64 {
	var s int64
	_ = l.db.QueryRowContext(ctx, `SELECT coalesce(min(seq),0) FROM (SELECT seq FROM audit_events UNION ALL SELECT seq FROM audit_tombstones)`).Scan(&s)
	return s
}

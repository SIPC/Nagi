package auditlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// 容量清理（见 docs/audit.md#节点本地审计）：节点审计库计入本机日志的共享额度，属于最高优先级，别的日志清完
// 仍超额时才删；本库自己超过 maxBytes 时也删。每次删最旧的一批，留下墓碑（序号和哈希），并在同一个事务里往链上
// 写一条 audit_pruned，detail 列出现存的全部墓碑。校验时每个墓碑都要被链上的清理记录列出：删一条再补个墓碑也能
// 发现。比剩下的所有记录都旧的墓碑不再需要，一起删掉。

const actionPruned = "audit_pruned"

const pruneBatch = 256

var sqlNoRows = sql.ErrNoRows

// maxBytes 是本库自己的大小上限（测试里改小）。
var maxBytes int64 = 100 << 20

func nowUTC() time.Time { return time.Now().UTC() }

// trim 删掉最旧的一批记录，返回有没有删。
func (l *Log) trim(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil || l.pending.Load() || l.historyPending.Load() {
		return false, nil
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var lo, hi int64
	err = tx.QueryRowContext(ctx, `SELECT min(seq), max(seq) FROM (SELECT seq FROM audit_events WHERE seq < (SELECT max(seq) FROM audit_events) ORDER BY seq LIMIT ?)`, pruneBatch).Scan(&lo, &hi)
	if err != nil || lo == 0 {
		return false, nil
	}
	for _, q := range []string{
		`INSERT OR REPLACE INTO audit_tombstones(seq,hash) SELECT seq,hash FROM audit_events WHERE seq BETWEEN ? AND ?`,
		`DELETE FROM audit_events WHERE seq BETWEEN ? AND ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, lo, hi); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_tombstones WHERE seq < (SELECT min(seq) FROM audit_events)`); err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq FROM audit_tombstones ORDER BY seq`)
	if err != nil {
		return false, err
	}
	var all []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return false, err
		}
		all = append(all, s)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	e := Event{Seq: l.seq + 1, Time: nowUTC(), Action: actionPruned, Detail: "ids=" + strings.Join(formatRanges(all), ","), Result: "ok"}
	line, err := json.Marshal(e)
	if err != nil {
		return false, err
	}
	hash := ChainHash(l.lastHash, line)
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(seq,line,prev_hash,hash) VALUES(?,?,?,?)`, e.Seq, line, l.lastHash, hash); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	l.seq, l.lastHash = e.Seq, hash
	return true, logstore.CompactHistory(ctx, l.db)
}

// UseBudget 把本库接入本机日志的共享额度（最高优先级）。
func (l *Log) UseBudget(ctx context.Context, budget *logstore.Store) error {
	if budget == nil {
		return nil
	}
	if err := budget.RegisterArchive(ctx, logstore.Archive{
		Name: l.path, Priority: 4,
		Size: func() (int64, error) { return logstore.FileBytes(l.path) },
		Trim: l.trim,
	}); err != nil {
		return err
	}
	l.budget = budget
	return nil
}

// PruneLoop 每小时检查一次本库大小，超过 maxBytes 时从最旧的删起。
func (l *Log) PruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		for ctx.Err() == nil {
			n, err := logstore.FileBytes(l.path)
			if err != nil || n <= maxBytes {
				break
			}
			var changed bool
			err = l.budget.WriteArchive(ctx, 4, 0, func() error {
				var err error
				changed, err = l.trim(ctx)
				return err
			})
			if err != nil || !changed {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

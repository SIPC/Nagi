package auditlog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

const historyCheckpoint = "legacy.node.audit"

// MigrateHistory imports a startup snapshot, without modifying the original
// files still needed by legacy audit readers. Take through=Seq before enabling
// live capture, and run one importer per Log. Both copies share the local quota.
// A bad source or full destination leaves the source protected and the durable
// position unchanged for the failed record; restart resumes from that position.
func (l *Log) MigrateHistory(ctx context.Context, dest *logstore.Store, through int64) error {
	if dest == nil {
		return nil
	}
	l.historyPending.Store(true)
	b, err := dest.Checkpoint(ctx, historyCheckpoint)
	if err != nil {
		return err
	}
	var saved struct{ Seq int64 }
	if len(b) != 0 {
		if err := json.Unmarshal(b, &saved); err != nil {
			return err
		}
	}
	if saved.Seq < 0 || saved.Seq > through {
		return fmt.Errorf("旧节点审计序号已倒退，拒绝沿用迁移位置")
	}
	if saved.Seq == through {
		l.historyPending.Store(false)
		return nil
	}
	after := saved.Seq
	var cursor historyCursor
	for after < through {
		batch, err := l.historyBatch(ctx, after, through, &cursor)
		if err != nil {
			return err
		}
		if len(batch) == 0 || batch[0].Seq > after+1 {
			end := through
			if len(batch) > 0 {
				end = batch[0].Seq - 1
			}
			if err := dest.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.audit.gap",
				Data: map[string]any{"from": after + 1, "to": end, "source": "旧节点审计文件；不代表独立采集库也缺失"}}, historyCheckpoint, struct{ Seq int64 }{end}); err != nil {
				return err
			}
			after = end
		}
		for _, e := range batch {
			if e.Seq > after+1 {
				// Re-enter through the same bounded reader so internal gaps get an
				// explicit record too, not just a jump in the durable position.
				cursor = historyCursor{}
				break
			}
			if err := dest.ImportCheckpoint(ctx, logstore.Event{Time: e.Time, Category: logstore.Audit,
				Kind: "node." + e.Action, Object: e.Target, Data: e}, historyCheckpoint, struct{ Seq int64 }{e.Seq}); err != nil {
				return err
			}
			after = e.Seq
		}
	}
	// Also checkpoint a suffix already independently captured before upgrade.
	if err := dest.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.audit.imported",
		Data: map[string]any{"through": through}}, historyCheckpoint, struct{ Seq int64 }{through}); err != nil {
		return err
	}
	l.historyPending.Store(false)
	return nil
}

// The in-memory offset avoids rescanning each file for every 64-record batch.
// On restart or concurrent rotation the durable sequence remains authoritative.
type historyCursor struct {
	file   string
	offset int64
}

func (l *Log) historyBatch(ctx context.Context, after, through int64, cursor *historyCursor) ([]Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := os.ReadDir(l.dir); err != nil {
		return nil, err
	}
	var batch []Event
	var lastRead int64
	for _, name := range files(l.dir) {
		if name != current {
			last, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(name, "audit."), ".jsonl"), 10, 64)
			if err == nil && last <= after {
				continue
			}
		}
		f, err := os.Open(filepath.Join(l.dir, name))
		if err != nil {
			return nil, err
		}
		offset := int64(0)
		if cursor.file == name {
			offset = cursor.offset
			if offset > 0 {
				lastRead = after
			}
		}
		if _, err := f.Seek(offset, 0); err != nil {
			f.Close()
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
			n, token, err := bufio.ScanLines(data, atEOF)
			offset += int64(n)
			return n, token, err
		})
		for sc.Scan() {
			if err := ctx.Err(); err != nil {
				f.Close()
				return nil, err
			}
			var e Event
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Seq <= 0 || e.Time.IsZero() || !time.Unix(0, e.Time.UnixNano()).Equal(e.Time) {
				f.Close()
				return nil, fmt.Errorf("旧节点审计 %s 含无效记录，迁移未越过该位置", name)
			}
			if e.Seq <= lastRead {
				f.Close()
				return nil, fmt.Errorf("旧节点审计 %s 序号乱序", name)
			}
			lastRead = e.Seq
			if e.Seq <= after {
				continue
			}
			if e.Seq > through {
				f.Close()
				return batch, nil
			}
			batch = append(batch, e)
			*cursor = historyCursor{name, offset}
			if len(batch) == 64 {
				f.Close()
				return batch, nil
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return batch, nil
}

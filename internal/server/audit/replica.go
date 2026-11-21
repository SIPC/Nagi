package audit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// 备用面板的增量同步（见 docs/audit.md#备用面板）：主面板按 id 把记录（连同墓碑和录制的块）原样发给备用面板，
// 备用面板按原来的 id、prev_hash 和 hash 写进自己的 audit.db。提升以后接着这条链写。

// Batch 是一次同步的内容。
type Batch struct {
	Events     []Event     `json:"events,omitempty"`
	Tombstones []Tombstone `json:"tombstones,omitempty"`
	Chunks     []Chunk     `json:"chunks,omitempty"`
}

// Tombstone 是按保留时间删掉的记录留下的 ID 和哈希。
type Tombstone struct {
	ID   int64  `json:"id"`
	Hash string `json:"hash"`
}

// Chunk 是录制的一块（加密后的原样内容）。
type Chunk struct {
	Recording string `json:"recording"`
	Seq       int    `json:"seq"`
	Data      []byte `json:"data"`
}

// replicaEvent 是同步时带上 prev_hash 和 hash 的记录（Event 序列化时不带这两个字段）。
type replicaEvent struct {
	Event
	TimeMS   int64  `json:"time_ms"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// MaxID 返回最大的记录 ID（墓碑也算）。
func (l *Log) MaxID(ctx context.Context) int64 {
	var a, b int64
	_ = l.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&a)
	_ = l.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM tombstones`).Scan(&b)
	return max(a, b)
}

// ReadBatch 读出 id 大于 after 的一批记录，连同这段里的墓碑和录制的块。
func (l *Log) ReadBatch(ctx context.Context, after int64, limit int, rec *Recordings) (Batch, []byte, error) {
	var b Batch
	rows, err := l.db.QueryContext(ctx, `SELECT `+columns+` FROM events WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return b, nil, err
	}
	var out []replicaEvent
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			rows.Close()
			return b, nil, err
		}
		b.Events = append(b.Events, e)
		out = append(out, replicaEvent{Event: e, TimeMS: e.Time.UnixMilli(), PrevHash: e.PrevHash, Hash: e.Hash})
	}
	rows.Close()
	upTo := after + int64(limit)
	if len(b.Events) > 0 {
		upTo = b.Events[len(b.Events)-1].ID
	}
	trows, err := l.db.QueryContext(ctx, `SELECT id, hash FROM tombstones WHERE id > ? AND id <= ? ORDER BY id`, after, upTo)
	if err == nil {
		for trows.Next() {
			var t Tombstone
			if trows.Scan(&t.ID, &t.Hash) == nil {
				b.Tombstones = append(b.Tombstones, t)
			}
		}
		trows.Close()
	}
	if rec != nil {
		for _, e := range b.Events {
			if !recordingIDRe.MatchString(e.RecordingID) {
				continue
			}
			dir := filepath.Join(rec.Dir, e.RecordingID[:6], e.RecordingID)
			entries, _ := os.ReadDir(dir)
			for _, f := range entries {
				seq, err := strconv.Atoi(f.Name())
				if err != nil {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, f.Name()))
				if err == nil {
					b.Chunks = append(b.Chunks, Chunk{Recording: e.RecordingID, Seq: seq, Data: data})
				}
			}
		}
	}
	return b, marshalReplica(out, b), nil
}

// WriteBatch 把同步来的一批写进备用面板的审计库：按原来的 id、prev_hash、hash 写入，录制的块写进录制目录。
func (l *Log) WriteBatch(ctx context.Context, events []replicaEvent, tombs []Tombstone, chunks []Chunk, rec *Recordings) error {
	reserve := int64(64<<10) + int64(len(tombs))*256
	for _, e := range events {
		reserve += int64(len(Canonical(e.Event)))*2 + 4096
	}
	for _, c := range chunks {
		reserve += int64(len(c.Data)) + 4096
	}
	return l.budget.WriteArchive(ctx, 4, reserve, func() error {
		if err := l.writeBatch(ctx, events, tombs, chunks, rec); err != nil {
			return err
		}
		if l.budget != nil {
			_, err := l.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
			return err
		}
		return nil
	})
}

func (l *Log) writeBatch(ctx context.Context, events []replicaEvent, tombs []Tombstone, chunks []Chunk, rec *Recordings) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range tombs {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO tombstones (id, hash) VALUES (?, ?)`, t.ID, t.Hash); err != nil {
			return err
		}
	}
	last := l.lastHash
	for _, r := range events {
		e := r.Event
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO events (id, time, category, action, actor_type, actor_id, actor_name, via, token_id, client,
			session, ip, user_agent, project_id, target_type, target_id, target_name, params, result, error, duration_ms, parent_id,
			recording_id, prev_hash, hash) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID, r.TimeMS, e.Category, e.Action, e.ActorType, e.ActorID, e.ActorName, e.Via, e.TokenID, e.Client,
			e.Session, e.IP, e.UserAgent, e.ProjectID, e.TargetType, e.TargetID, e.TargetName, e.Params, e.Result, e.Error,
			e.DurationMS, e.ParentID, e.RecordingID, r.PrevHash, r.Hash)
		if err != nil {
			return err
		}
		last = r.Hash
	}
	// 自增序号跟上同步来的最大 ID，提升以后新记录接着编号。
	if n := len(events); n > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = MAX(seq, ?) WHERE name = 'events'`, events[n-1].ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	l.lastHash = last
	if rec != nil {
		for _, c := range chunks {
			if !recordingIDRe.MatchString(c.Recording) {
				continue
			}
			dir := filepath.Join(rec.Dir, c.Recording[:6], c.Recording)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, chunkName(c.Seq)), c.Data, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

// wireBatch 是同步连接上的一帧。
type wireBatch struct {
	Events     []replicaEvent `json:"events,omitempty"`
	Tombstones []Tombstone    `json:"tombstones,omitempty"`
	Chunks     []Chunk        `json:"chunks,omitempty"`
}

func marshalReplica(events []replicaEvent, b Batch) []byte {
	out, _ := json.Marshal(wireBatch{Events: events, Tombstones: b.Tombstones, Chunks: b.Chunks})
	return out
}

// ApplyFrame 解开同步连接上的一帧并写入，返回写入后最大的 ID。
func (l *Log) ApplyFrame(ctx context.Context, frame []byte, rec *Recordings) (int64, error) {
	var w wireBatch
	if err := json.Unmarshal(frame, &w); err != nil {
		return 0, err
	}
	for i := range w.Events {
		w.Events[i].Time = time.UnixMilli(w.Events[i].TimeMS)
	}
	if err := l.WriteBatch(ctx, w.Events, w.Tombstones, w.Chunks, rec); err != nil {
		return 0, err
	}
	return l.MaxID(ctx), nil
}

// ImportFrom 把另一个审计库里的记录作为新记录追加进来（备用面板提升以后，并入它做备用面板时自己的记录）。
func (l *Log) ImportFrom(ctx context.Context, path string) (int, error) {
	other, err := Open(ctx, path, l.log)
	if err != nil {
		return 0, err
	}
	defer other.Close()
	n := 0
	err = other.Each(ctx, Filter{}, func(e Event) error {
		e.ID, e.ParentID = 0, 0
		l.Record(ctx, e)
		n++
		return nil
	})
	return n, err
}

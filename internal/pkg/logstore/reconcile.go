package logstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// CopyIndex advertises identities, not bodies. Cursor belongs to this copy
// database; Source and Seq remain the original producer's identifiers.
type CopyIndex struct {
	Cursor                 int64
	Node, Source, Category string
	Seq                    int64
	Received               time.Time
	Hash                   []byte
}

type CopyItem struct {
	Cursor int64
	Node   string
	Event  Event
}

func (v CopyItem) Index() CopyIndex {
	e := v.Event
	return CopyIndex{Cursor: v.Cursor, Node: v.Node, Source: e.Source, Category: e.Category, Seq: e.Seq, Received: e.Received, Hash: copyHash(e)}
}

type CopyPosition struct {
	Source string
	After  int64
}

func copyHash(e Event) []byte {
	b, _ := json.Marshal([]any{e.Time.UnixNano(), e.Category, e.Kind, e.Project, e.Object, e.Data})
	h := sha256.Sum256(b)
	return h[:]
}

// Inventory scans a bounded page of retained originals. Its database source
// prevents an old reconciliation cursor from skipping a rebuilt database.
func (c *Copies) Inventory(ctx context.Context, pos CopyPosition) (CopyPosition, []CopyIndex, error) {
	st, err := c.s.State(ctx)
	if err != nil {
		return CopyPosition{}, nil, err
	}
	if pos.Source != st.Source {
		pos = CopyPosition{Source: st.Source}
	}
	if pos.After < 0 || pos.After > st.Last {
		return pos, nil, ErrCopyCursor
	}
	items, err := c.copyItems(ctx, `e.seq>? AND e.seq<=? ORDER BY e.seq LIMIT 128`, pos.After, st.Last)
	if err != nil {
		return pos, nil, err
	}
	var out []CopyIndex
	for _, v := range items {
		out = append(out, v.Index())
		pos.After = v.Cursor
	}
	if len(items) < 128 {
		pos.After = st.Last
	}
	return pos, out, nil
}

// Fetch never extends retention and cannot select log bodies outside recent.db.
func (c *Copies) Fetch(ctx context.Context, source string, cursors []int64) ([]CopyItem, error) {
	if len(cursors) > 8 {
		return nil, fmt.Errorf("日志补缺请求过大")
	}
	st, err := c.s.State(ctx)
	if err != nil {
		return nil, err
	}
	if source != st.Source {
		return nil, ErrCopyCursor
	}
	var out []CopyItem
	size := 0
	for _, cursor := range cursors {
		items, err := c.copyItems(ctx, `e.seq=?`, cursor)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 || time.Since(items[0].Event.Received) >= c.retention {
			continue
		}
		b, _ := json.Marshal(items[0])
		size += len(b)
		if size > 1<<20 {
			break
		}
		out = append(out, items[0])
	}
	return out, nil
}

func (c *Copies) copyItems(ctx context.Context, where string, args ...any) ([]CopyItem, error) {
	if err := c.s.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.s.gate }()
	rows, err := c.s.db.QueryContext(ctx, `SELECT e.seq,o.node,o.origin,o.remote_seq,e.time_ns,e.received_ns,e.category,e.kind,e.project,e.object,e.data FROM log_events e JOIN copy_origins o ON o.local_seq=e.seq WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CopyItem
	for rows.Next() {
		var v CopyItem
		var at, received int64
		var data []byte
		e := &v.Event
		if err := rows.Scan(&v.Cursor, &v.Node, &e.Source, &e.Seq, &at, &received, &e.Category, &e.Kind, &e.Project, &e.Object, &data); err != nil {
			return nil, err
		}
		e.Time, e.Received, e.Data = time.Unix(0, at).UTC(), time.Unix(0, received).UTC(), json.RawMessage(data)
		out = append(out, v)
	}
	return out, rows.Err()
}

// Needs distinguishes a never-received hole from local expiry/capacity pruning.
// A source's pruning summary is not a local tombstone.
func (c *Copies) Needs(ctx context.Context, v CopyIndex) (bool, error) {
	if err := validCopyOrigin(v.Node, v.Source, v.Category); err != nil {
		return false, err
	}
	if v.Seq < 1 || v.Received.IsZero() || len(v.Hash) != sha256.Size {
		return false, fmt.Errorf("无效的日志补缺索引")
	}
	if time.Since(v.Received) >= c.retention {
		return false, nil
	}
	items, err := c.copyItems(ctx, `o.node=? AND o.origin=? AND o.remote_seq=?`, v.Node, v.Source, v.Seq)
	if err != nil {
		return false, err
	}
	if len(items) > 0 {
		if string(copyHash(items[0].Event)) != string(v.Hash) {
			return false, ErrCopyConflict
		}
		return false, nil
	}
	if err := c.s.lock(ctx); err != nil {
		return false, err
	}
	defer func() { <-c.s.gate }()
	var deleted bool
	err = c.s.db.QueryRowContext(ctx, deletedQuery, v.Node, v.Source, v.Category, v.Seq, v.Seq).Scan(&deleted)
	return !deleted, err
}

// Merge adds still-retained evidence without moving the node's durable receipt.
// Node reconnects may therefore fill unreceived holes below the highest copy.
func (c *Copies) Merge(ctx context.Context, node string, b CopyBatch) error {
	_, err := c.receive(ctx, node, b, true)
	return err
}

func (c *Copies) SyncPosition(ctx context.Context, peer string) (CopyPosition, error) {
	var pos CopyPosition
	b, err := c.s.Checkpoint(ctx, "sync."+peer)
	if err == nil && len(b) > 0 {
		err = json.Unmarshal(b, &pos)
	}
	return pos, err
}

func (c *Copies) SaveSyncPosition(ctx context.Context, peer string, pos CopyPosition) error {
	b, err := json.Marshal(pos)
	if err != nil {
		return err
	}
	if err := c.budget.Reserve(ctx, 4, 64<<10); err != nil {
		return err
	}
	if err := c.s.lock(ctx); err != nil {
		return err
	}
	defer func() { <-c.s.gate }()
	if c.s.closed {
		return sql.ErrConnDone
	}
	_, err = c.s.db.ExecContext(ctx, `INSERT INTO log_checkpoints(key,data) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data`, "sync."+peer, b)
	return err
}

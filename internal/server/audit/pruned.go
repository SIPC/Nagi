package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 清理记录（见 docs/audit.md#防篡改）：按保留时间删掉记录时留下墓碑，链还能接上；但墓碑本身没有认证，能写数据库的人
// 删掉中间一条再补一个墓碑，链照样接得上。所以每次清理都在链上写一条 audit_pruned，列出这次删掉的 ID（连续的写成区间），
// 随外送发出；校验时要求每个墓碑都被它后面的某条清理记录列出。被删掉的清理记录列出的、墓碑还在的 ID，由新的清理记录接着列出。

const (
	actionPruned = "audit_pruned"
	// prunedChunk 是一条清理记录里 ID 列表的最大长度，参数不能超过 MaxParams。
	prunedChunk = MaxParams - 256
	// metaPruneRecords 表示已经为升级前的墓碑写过清理记录（legacy）。
	metaPruneRecords = "prune_records"
)

type prunedParams struct {
	IDs    string `json:"ids"`
	Count  int    `json:"count"`
	Legacy bool   `json:"legacy,omitempty"`
}

// idRanges 把排好序的 ID 写成 "1-5"、"7" 这样的区间。
func idRanges(ids []int64) []string {
	var out []string
	for i := 0; i < len(ids); {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if i == j {
			out = append(out, strconv.FormatInt(ids[i], 10))
		} else {
			out = append(out, fmt.Sprintf("%d-%d", ids[i], ids[j]))
		}
		i = j + 1
	}
	return out
}

// parseRange 解析 idRanges 的一项。
func parseRange(part string) (int64, int64, bool) {
	a, b, found := strings.Cut(part, "-")
	lo, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	hi := lo
	if found {
		if hi, err = strconv.ParseInt(b, 10, 64); err != nil || hi < lo {
			return 0, 0, false
		}
	}
	return lo, hi, true
}

// pruneRecords 把 ID 列表切成若干条清理记录（每条的参数不超过 MaxParams）。
func pruneRecords(ids []int64, legacy bool, now time.Time) []Event {
	var out []Event
	var cur []string
	size, count := 0, 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		b, _ := json.Marshal(prunedParams{IDs: strings.Join(cur, ","), Count: count, Legacy: legacy})
		out = append(out, Event{Time: now, Category: Security, Action: actionPruned, ActorType: ActorSystem, Via: ViaSystem,
			Result: ResultOK, Params: string(b)})
		cur, size, count = nil, 0, 0
	}
	for _, r := range idRanges(ids) {
		if size+len(r)+1 > prunedChunk {
			flush()
		}
		cur = append(cur, r)
		size += len(r) + 1
		lo, hi, _ := parseRange(r)
		count += int(hi - lo + 1)
	}
	flush()
	return out
}

// insertTx 在事务里按哈希链写一条记录（prev 是上一条的哈希）。调用方持有 l.mu，提交以后把 l.lastHash 设成 e.Hash。
func (l *Log) insertTx(ctx context.Context, tx *sql.Tx, prev string, e *Event) error {
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'events'), 0) + 1`).Scan(&next); err != nil {
		return err
	}
	e.ID = next
	e.PrevHash = prev
	e.Hash = ChainHash(e.PrevHash, *e)
	_, err := tx.ExecContext(ctx, `INSERT INTO events (id, time, category, action, actor_type, actor_id, actor_name, via, token_id, client,
		session, ip, user_agent, project_id, target_type, target_id, target_name, params, result, error, duration_ms, parent_id,
		recording_id, prev_hash, hash) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Time.UnixMilli(), e.Category, e.Action, e.ActorType, e.ActorID, e.ActorName, e.Via, e.TokenID, e.Client,
		e.Session, e.IP, e.UserAgent, e.ProjectID, e.TargetType, e.TargetID, e.TargetName, e.Params, e.Result, e.Error,
		e.DurationMS, e.ParentID, e.RecordingID, e.PrevHash, e.Hash)
	return err
}

// writePruneRecordsTx 在事务里写清理记录，返回最后一条的哈希（没有要写的时返回 l.lastHash）。
func (l *Log) writePruneRecordsTx(ctx context.Context, tx *sql.Tx, ids []int64, legacy bool) (string, error) {
	prev := l.lastHash
	for _, e := range pruneRecords(ids, legacy, time.Now()) {
		if err := l.insertTx(ctx, tx, prev, &e); err != nil {
			return "", err
		}
		prev = e.Hash
	}
	return prev, nil
}

// CoverLegacyTombstones 为升级前留下的墓碑写 legacy 清理记录。只在主面板第一次启动新版本时做一次。
func (l *Log) CoverLegacyTombstones(ctx context.Context) error {
	if l.Meta(ctx, metaPruneRecords) != "" {
		return nil
	}
	if err := l.coverLegacy(ctx); err != nil {
		return err
	}
	return l.SetMeta(ctx, metaPruneRecords, "1")
}

func (l *Log) coverLegacy(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ids, err := queryIDs(ctx, tx, `SELECT id FROM tombstones ORDER BY id`)
	if err != nil || len(ids) == 0 {
		return err
	}
	last, err := l.writePruneRecordsTx(ctx, tx, ids, true)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	l.lastHash = last
	return nil
}

func queryIDs(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// coverage 是清理记录列出的 ID 区间。先 add 完，查的时候排序合并，用二分查找。
type coverage struct {
	ranges [][2]int64
	legacy int
	done   bool
}

func (c *coverage) add(e Event) {
	var p prunedParams
	if json.Unmarshal([]byte(e.Params), &p) != nil {
		return
	}
	if p.Legacy {
		c.legacy++
	}
	for _, part := range strings.Split(p.IDs, ",") {
		if lo, hi, ok := parseRange(part); ok {
			c.ranges = append(c.ranges, [2]int64{lo, hi})
		}
	}
	c.done = false
}

func (c *coverage) finish() {
	if c.done {
		return
	}
	sort.Slice(c.ranges, func(i, j int) bool { return c.ranges[i][0] < c.ranges[j][0] })
	merged := c.ranges[:0]
	for _, r := range c.ranges {
		if n := len(merged); n > 0 && r[0] <= merged[n-1][1]+1 {
			merged[n-1][1] = max(merged[n-1][1], r[1])
			continue
		}
		merged = append(merged, r)
	}
	c.ranges, c.done = merged, true
}

// covers 判断 id 有没有被清理记录列出。
func (c *coverage) covers(id int64) bool {
	c.finish()
	i := sort.Search(len(c.ranges), func(i int) bool { return c.ranges[i][1] >= id })
	return i < len(c.ranges) && c.ranges[i][0] <= id
}

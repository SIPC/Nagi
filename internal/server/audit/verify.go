package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
)

// 哈希链校验（见 docs/audit.md#防篡改）：每条记录的 hash = SHA-256(prev_hash ‖ 规范化 JSON)，prev_hash 是上一条
// 记录（或它的墓碑）的 hash，每个墓碑都要被清理记录列出（见 pruned.go）。从某处起重写后面所有的记录、在末尾追加伪造的记录发现不了，要靠外送的副本对比。

// VerifyResult 是一次校验的结果。
type VerifyResult struct {
	Time    time.Time `json:"time"`
	Records int64     `json:"records"`
	// BadID 是第一条对不上的记录，0 表示整条链完好。
	BadID  int64  `json:"bad_id,omitempty"`
	Reason string `json:"reason,omitempty"`
	// LastID、LastHash 是链上最新的一条。
	LastID   int64  `json:"last_id"`
	LastHash string `json:"last_hash"`
	// Tombstones 是墓碑的个数。
	Tombstones int64 `json:"tombstones,omitempty"`
}

// OK 判断整条链完好。
func (r VerifyResult) OK() bool { return r.BadID == 0 }

// metaLastVerify 记着最近一次校验的结果。
const metaLastVerify = "last_verify"

// Verify 校验整条哈希链。
func (l *Log) Verify(ctx context.Context) (VerifyResult, error) {
	res, err := verifyDB(ctx, l.db)
	if err == nil {
		b, _ := json.Marshal(res)
		_ = l.SetMeta(ctx, metaLastVerify, string(b))
	}
	return res, err
}

// LastVerify 返回最近一次校验的结果。
func (l *Log) LastVerify(ctx context.Context) (VerifyResult, bool) {
	var r VerifyResult
	v := l.Meta(ctx, metaLastVerify)
	return r, v != "" && json.Unmarshal([]byte(v), &r) == nil
}

// VerifyFile 只读地校验一个审计库文件（nagi audit verify，面板运行中也能用）。
func VerifyFile(ctx context.Context, path string) (VerifyResult, error) {
	d, err := db.Open(path)
	if err != nil {
		return VerifyResult{}, err
	}
	defer d.Close()
	return verifyDB(ctx, d)
}

func verifyDB(ctx context.Context, d *sql.DB) (VerifyResult, error) {
	res := VerifyResult{Time: time.Now()}
	tomb := map[int64]string{}
	if rows, err := d.QueryContext(ctx, `SELECT id, hash FROM tombstones`); err == nil {
		for rows.Next() {
			var id int64
			var h string
			if rows.Scan(&id, &h) == nil {
				tomb[id] = h
			}
		}
		rows.Close()
	}
	rows, err := d.QueryContext(ctx, `SELECT `+columns+` FROM events ORDER BY id`)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	var prevID int64 = -1
	var prevHash string
	var cov coverage
	bad := func(id int64, reason string) {
		if res.BadID == 0 {
			res.BadID, res.Reason = id, reason
		}
	}
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return res, err
		}
		res.Records++
		if e.Action == actionPruned {
			cov.add(e)
		}
		if ChainHash(e.PrevHash, e) != e.Hash {
			bad(e.ID, fmt.Sprintf("记录 %d 的内容和哈希对不上，可能被修改过", e.ID))
		}
		if prevID >= 0 {
			// 中间缺的记录必须都有墓碑（按保留时间删掉的），并且接得上。
			link := prevHash
			for id := prevID + 1; id < e.ID; id++ {
				h, ok := tomb[id]
				if !ok {
					bad(e.ID, fmt.Sprintf("记录 %d 之前缺少记录 %d，可能被删除过", e.ID, id))
					break
				}
				link = h
			}
			if e.PrevHash != link && res.BadID == 0 {
				bad(e.ID, fmt.Sprintf("记录 %d 和上一条记录接不上，中间的记录可能被删除或修改过", e.ID))
			}
		}
		prevID, prevHash = e.ID, e.Hash
		res.LastID, res.LastHash = e.ID, e.Hash
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	res.Tombstones = int64(len(tomb))
	// 每个墓碑都要被清理记录列出：否则是有人删了记录、补了一个墓碑（见 pruned.go）。
	if res.BadID == 0 && res.Records > 0 {
		ids := make([]int64, 0, len(tomb))
		for id := range tomb {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			if !cov.covers(id) {
				bad(id, fmt.Sprintf("记录 %d 被删除了，但没有对应的清理记录，墓碑可能是伪造的", id))
				break
			}
		}
	}
	if res.BadID == 0 && cov.legacy > 1 {
		bad(res.LastID, "有多条升级时写的清理记录，审计库可能被改过")
	}
	if res.Records == 0 && res.BadID == 0 {
		var n int64
		_ = d.QueryRowContext(ctx, `SELECT COUNT(*) FROM tombstones`).Scan(&n)
		if n > 0 {
			return res, errors.New("审计库里只剩墓碑，记录被清空了")
		}
	}
	return res, nil
}

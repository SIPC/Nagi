package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// 审计库与业务库分离（见 docs/audit.md#存储保留与同步）：v0.37.1 起面板审计链在本机日志目录
// <数据目录>/logs/audit/ 下，不在面板目录（业务库、主备快照）旁边。升级时把面板目录里的旧审计库整份搬过去：
//
//   - 同一个文件系统上：先把预写日志合并进主文件，再整个改名过去（原子操作，被杀时文件只在两处之一）。文件的字节不变，
//     原 id、prev_hash、hash、墓碑、清理记录和外送游标原样保留，不重算哈希，旧链完整接上；什么都没删，所以不必先校验，
//     入口不等：面板起来以后在后台校验整条链（VerifyMoved），不通过时日志页提示，文件原样保留。
//   - 跨文件系统时用 VACUUM INTO 复制：副本独立通过整条链的校验、并且和原库逐行一致（摘要相同），先把原库的摘要记进
//     检查点，再把副本改名为正式文件；
//   - 正式文件已在时，只要原库仍与检查点里的摘要一致，就记日志、删除原库；不一致（例如退回旧版本后又写过一条新链）
//     时返回 ErrWrittenAfterMove，调用方把它当作另一份归档（audit-rollback-*.db）同样核验后搬走。
//   - 任何一步之间被杀都安全：正式文件不在就丢掉临时副本重来；在就以它为准，不再退回原库。
//   - 原库自己校验不通过时不搬：本次继续用原库，下次启动重试，日志页提示。

// RelocateID is the live migration entry for the log page.
const RelocateID = "cleanup.audit"

// VerifyMovedID reports the background chain check of renamed databases.
const VerifyMovedID = "verify.audit"

// The checkpoint is keyed by the destination name: each destination is filled
// from exactly one original, and is never filled again once done.
func relocateCheckpoint(dst string) string { return "legacy.cleanup.audit." + filepath.Base(dst) }

// ErrWrittenAfterMove means the original changed after its verified copy was
// put in place, for example because an older version ran in between. The
// caller should move it under RollbackName instead.
var ErrWrittenAfterMove = errors.New("旧审计库在迁移后又被写入过")

// RollbackName is a stable destination name for an original that changed after
// its move: derived from its content, so a restart picks the same name.
func RollbackName(ctx context.Context, old string) (string, error) {
	d, err := openRead(old)
	if err != nil {
		return "", err
	}
	defer d.Close()
	sum, err := summarize(ctx, d)
	if err != nil {
		return "", err
	}
	return "audit-rollback-" + sum.Digest[:16] + ".db", nil
}

type relocateState struct {
	Phase  string `json:"phase"` // copied, done
	Digest string `json:"digest"`
}

// Relocate moves the legacy audit database at old to dst and returns the path
// to open. It returns old only while no verified copy exists; once dst exists
// it is authoritative. A returned error with dst means the original was kept.
func Relocate(ctx context.Context, old, dst string, logs *logstore.Store) (string, error) {
	_, dstErr := os.Stat(dst)
	if dstErr != nil && !errors.Is(dstErr, os.ErrNotExist) {
		return dst, dstErr
	}
	haveDst := dstErr == nil
	if _, err := os.Stat(old); errors.Is(err, os.ErrNotExist) {
		return dst, nil
	} else if err != nil {
		return pick(haveDst, dst, old), err
	}
	if logs == nil {
		// Nowhere to record the move: keep using whatever exists, delete nothing.
		return pick(haveDst, dst, old), nil
	}
	key := relocateCheckpoint(dst)
	var st relocateState
	if b, err := logs.Checkpoint(ctx, key); err != nil {
		return pick(haveDst, dst, old), err
	} else if len(b) != 0 {
		if err := json.Unmarshal(b, &st); err != nil {
			return pick(haveDst, dst, old), err
		}
	}
	if !haveDst {
		// Same file system: rename the file itself. Its bytes, and so every id,
		// hash, tombstone and cursor, stay exactly as they were; nothing is
		// deleted, so no copy has to be verified first. The chain is verified
		// in the background afterwards (VerifyMoved).
		if err := renameDB(old, dst); err == nil {
			return dst, logs.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.audit.moved",
				Object: filepath.Base(old), Data: map[string]any{"from": old, "to": dst}}, key, relocateState{Phase: "done"})
		} else if !errors.Is(err, errCrossDevice) {
			return old, err
		}
		sum, err := copyVerified(ctx, old, dst, func(sum ChainSummary) error {
			st = relocateState{Phase: "copied", Digest: sum.Digest}
			return logs.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.audit.copied",
				Object: filepath.Base(old), Data: summaryData(old, dst, sum)}, key, st)
		})
		if err != nil {
			return old, err
		}
		return dst, deleteOriginal(ctx, logs, key, old, dst, sum)
	}
	if st.Phase != "copied" {
		return dst, ErrWrittenAfterMove
	}
	d, err := openRead(old)
	if err != nil {
		return dst, err
	}
	sum, err := summarize(ctx, d)
	d.Close()
	if err != nil {
		return dst, err
	}
	if sum.Digest != st.Digest {
		return dst, ErrWrittenAfterMove
	}
	return dst, deleteOriginal(ctx, logs, key, old, dst, sum)
}

var errCrossDevice = errors.New("cross-device")

func isCrossDevice(err error) bool {
	var e syscall.Errno
	if !errors.As(err, &e) {
		return false
	}
	if runtime.GOOS == "windows" {
		return e == 17 // ERROR_NOT_SAME_DEVICE
	}
	return e == syscall.EXDEV
}

// renameDB folds the WAL into the main file and renames it to dst. A rename
// is atomic: a kill leaves the file at exactly one of the two paths.
func renameDB(old, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	d, err := db.Open(old)
	if err != nil {
		return err
	}
	_, err = d.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	if err = errors.Join(err, d.Close()); err != nil {
		return err
	}
	if st, err := os.Stat(old + "-wal"); err == nil && st.Size() > 0 {
		return fmt.Errorf("%s: 预写日志没能合并，暂不迁移", filepath.Base(old))
	}
	if err := os.Rename(old, dst); err != nil {
		var le *os.LinkError
		if errors.As(err, &le) && isCrossDevice(le.Err) {
			return errCrossDevice
		}
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(old + suffix)
	}
	for _, dir := range []string{filepath.Dir(dst), filepath.Dir(old)} {
		if f, err := os.Open(dir); err == nil {
			_ = f.Sync() // best effort: Windows cannot fsync a directory
			f.Close()
		}
	}
	return nil
}

// VerifyMoved verifies a chain that was moved by rename. It runs after the
// panel is up; a failure is the same failure VerifyAudit reports.
func VerifyMoved(ctx context.Context, path string) error {
	r, err := VerifyFile(ctx, path)
	if err != nil {
		return err
	}
	if !r.OK() {
		return fmt.Errorf("审计链校验失败：%s", r.Reason)
	}
	return nil
}

func pick(useDst bool, dst, old string) string {
	if useDst {
		return dst
	}
	return old
}

func summaryData(old, dst string, sum ChainSummary) map[string]any {
	return map[string]any{"from": old, "to": dst, "records": sum.Records, "tombstones": sum.Tombstones,
		"last_id": sum.LastID, "last_hash": sum.LastHash, "sha256": sum.Digest}
}

func deleteOriginal(ctx context.Context, logs *logstore.Store, key, old, dst string, sum ChainSummary) error {
	data := summaryData(old, dst, sum)
	if err := logs.Record(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.audit.deleting",
		Object: filepath.Base(old), Data: data}); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", ""} {
		if err := os.Remove(old + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return logs.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.audit.done",
		Object: filepath.Base(old), Data: data}, key, relocateState{Phase: "done", Digest: sum.Digest})
}

// copyVerified writes a consistent copy next to dst, checks it, calls commit,
// and only then renames it into place.
func copyVerified(ctx context.Context, old, dst string, commit func(ChainSummary) error) (ChainSummary, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return ChainSummary{}, err
	}
	tmp := dst + ".tmp"
	clear := func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(tmp + suffix)
		}
	}
	clear()
	n, err := logstore.FileBytes(old)
	if err != nil {
		return ChainSummary{}, err
	}
	if err := metrics.CheckDisk(dst, n); err != nil {
		return ChainSummary{}, err
	}
	// VACUUM INTO needs a writable connection; it does not modify the source.
	src, err := db.Open(old)
	if err != nil {
		return ChainSummary{}, err
	}
	_, err = src.ExecContext(ctx, `VACUUM INTO ?`, tmp)
	src.Close()
	if err != nil {
		clear()
		return ChainSummary{}, err
	}
	sum, err := compareChains(ctx, old, tmp)
	if err == nil {
		err = syncFile(tmp)
	}
	if err == nil {
		err = commit(sum)
	}
	if err != nil {
		clear()
		return ChainSummary{}, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return ChainSummary{}, err
	}
	// Best effort: Windows cannot fsync a directory handle.
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return sum, nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// openRead opens an audit database without writing to it; a leftover WAL from
// a crash is included (mode=ro, not immutable).
func openRead(path string) (*sql.DB, error) {
	q := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(5000)"}}
	d, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	if err := d.PingContext(context.Background()); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// ChainSummary identifies a whole chain: its verification result plus a digest
// over every event, tombstone and meta row.
type ChainSummary struct {
	VerifyResult
	Digest string
}

// summarize verifies the chain in d on its own and digests its full contents.
func summarize(ctx context.Context, d *sql.DB) (ChainSummary, error) {
	var s ChainSummary
	res, err := verifyDB(ctx, d)
	if err != nil {
		return s, err
	}
	if !res.OK() {
		return s, fmt.Errorf("审计链校验失败：%s", res.Reason)
	}
	s.VerifyResult = res
	h := sha256.New()
	rows, err := d.QueryContext(ctx, `SELECT `+columns+` FROM events ORDER BY id`)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			rows.Close()
			return s, err
		}
		fmt.Fprintf(h, "e%d\x00%s\x00%s\x00%s\n", e.ID, e.PrevHash, e.Hash, Canonical(e))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return s, err
	}
	for _, q := range []string{`SELECT 't', id, hash FROM tombstones ORDER BY id`, `SELECT 'm', key, value FROM meta ORDER BY key`} {
		rows, err := d.QueryContext(ctx, q)
		if err != nil {
			return s, err
		}
		for rows.Next() {
			var kind, a, b string
			if err := rows.Scan(&kind, &a, &b); err != nil {
				rows.Close()
				return s, err
			}
			fmt.Fprintf(h, "%s%s\x00%s\n", kind, a, b)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return s, err
		}
	}
	s.Digest = hex.EncodeToString(h.Sum(nil))
	return s, nil
}

// compareChains requires both databases to verify on their own and to hold
// exactly the same events, tombstones and metadata.
func compareChains(ctx context.Context, a, b string) (ChainSummary, error) {
	var sums [2]ChainSummary
	for i, path := range []string{a, b} {
		d, err := openRead(path)
		if err != nil {
			return ChainSummary{}, err
		}
		sums[i], err = summarize(ctx, d)
		d.Close()
		if err != nil {
			return ChainSummary{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
	}
	if sums[0].Digest != sums[1].Digest {
		return ChainSummary{}, fmt.Errorf("新审计库与原库内容不一致（%d/%d 条），保留原库", sums[0].Records, sums[1].Records)
	}
	return sums[1], nil
}

package recording

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// CleanupID is the live migration entry that reports this cleanup to the log page.
const CleanupID = "cleanup.recordings"

const cleanupCheckpoint = "legacy.cleanup.recordings"

// CleanupLegacy deletes legacy recording directories whose import is complete
// and whose every file still matches, byte for byte, the ciphertext committed
// to the database. A file whose chunk the database no longer holds counts as
// verified only when retention already removed that chunk (the whole session,
// or a prefix of it). Anything else keeps the directory for the next start.
// Each deletion is logged; RemoveAll is idempotent, so an interrupted delete
// simply finishes on the next run. Run only where ImportLegacy runs.
func (r *Recordings) CleanupLegacy(ctx context.Context, logs *logstore.Store) (err error) {
	if r == nil || r.db == nil || r.Dir == "" || logs == nil {
		return nil
	}
	if b, err := logs.Checkpoint(ctx, cleanupCheckpoint); err != nil || len(b) != 0 {
		return err
	}
	months, err := os.ReadDir(r.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	logs.SetMigration(logstore.Migration{ID: CleanupID, Phase: "cleanup"})
	defer func() {
		if err != nil && ctx.Err() == nil {
			logs.SetMigration(logstore.Migration{ID: CleanupID, Phase: "failed", Error: err.Error()})
		}
	}()
	var failures []error
	deleted := 0
	for _, month := range months {
		if !month.IsDir() || len(month.Name()) != 6 {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(r.Dir, month.Name()))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			id := entry.Name()
			if !entry.IsDir() || !ValidID(id) || id[:6] != month.Name() {
				continue
			}
			ok, err := r.cleanupSession(ctx, logs, id)
			if err != nil {
				failures = append(failures, fmt.Errorf("录制 %s: %w", id, err))
			} else if ok {
				deleted++
			}
		}
		// Empty month directories go too; a non-empty one fails harmlessly.
		_ = os.Remove(filepath.Join(r.Dir, month.Name()))
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	if left, err := os.ReadDir(r.Dir); err != nil || len(left) != 0 {
		// Unrecognized files are not ours to delete; report and keep them.
		if err == nil {
			err = fmt.Errorf("旧录制目录里还有无法识别的 %d 项，已保留", len(left))
		}
		return err
	}
	if err := os.Remove(r.Dir); err != nil {
		return err
	}
	if err := logs.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.recordings.done",
		Data: map[string]any{"dir": r.Dir, "sessions_this_run": deleted}}, cleanupCheckpoint, map[string]string{"phase": "done"}); err != nil {
		return err
	}
	logs.SetMigration(logstore.Migration{ID: CleanupID, Phase: "complete"})
	return nil
}

func (r *Recordings) cleanupSession(ctx context.Context, logs *logstore.Store, id string) (bool, error) {
	if _, loaded := r.active.LoadOrStore(id, true); loaded {
		return false, nil
	}
	defer r.active.Delete(id)
	var complete int
	err := r.db.QueryRowContext(ctx, `SELECT complete FROM recording_imports WHERE id=?`, id).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) || err == nil && complete != 1 {
		return false, errors.New("尚未完整导入，保留原件")
	}
	if err != nil {
		return false, err
	}
	dir := filepath.Join(r.Dir, id[:6], id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	var session int
	minSeq := -1
	if err := r.db.QueryRowContext(ctx, `SELECT count(*), coalesce((SELECT min(seq) FROM recording_chunks WHERE recording=?),-1) FROM recording_sessions WHERE id=?`, id, id).Scan(&session, &minSeq); err != nil {
		return false, err
	}
	var bytesTotal int64
	matched, expired := 0, 0
	for _, entry := range entries {
		seq, err := strconv.Atoi(entry.Name())
		if err != nil || seq < 0 || entry.Name() != ChunkName(seq) || !entry.Type().IsRegular() {
			return false, errors.New("含无法识别的文件，保留原件")
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return false, err
		}
		bytesTotal += int64(len(body))
		var saved []byte
		err = r.db.QueryRowContext(ctx, `SELECT body FROM recording_chunks WHERE recording=? AND seq=?`, id, seq).Scan(&saved)
		switch {
		case err == nil && bytes.Equal(saved, body):
			matched++
		case err == nil:
			return false, errors.New("与数据库中的副本不一致，保留原件")
		case !errors.Is(err, sql.ErrNoRows):
			return false, err
		case session == 0 || minSeq < 0 || seq < minSeq:
			expired++ // retention already removed this body from the database
		default:
			return false, errors.New("数据库缺少这一块，保留原件")
		}
	}
	if err := logs.Record(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.recordings.deleting", Object: id,
		Data: map[string]any{"chunks": len(entries), "verified_equal": matched, "expired_in_database": expired, "bytes": bytesTotal}}); err != nil {
		return false, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	return true, nil
}

package recording

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ImportLegacy copies this machine's closed legacy files without re-encrypting,
// re-signing audit history or deleting originals. Do not run on a standby whose
// legacy files are still being appended by the old replication protocol.
func (r *Recordings) ImportLegacy(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.db == nil || r.Dir == "" {
		return nil
	}
	months, err := os.ReadDir(r.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
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
			if err := r.importLegacySession(ctx, id); err != nil {
				failures = append(failures, fmt.Errorf("录制 %s: %w", id, err))
			}
		}
	}
	return errors.Join(failures...)
}

func (r *Recordings) importLegacySession(ctx context.Context, id string) error {
	if _, loaded := r.active.LoadOrStore(id, true); loaded {
		return nil
	}
	defer r.active.Delete(id)
	var last, complete int
	err := r.db.QueryRowContext(ctx, `SELECT last_seq,complete FROM recording_imports WHERE id=?`, id).Scan(&last, &complete)
	if err == nil && complete == 1 {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		last = -1
		err = r.budget.WriteArchive(ctx, 3, 32<<10, func() error {
			tx, err := r.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			at, err := time.Parse("200601", id[:6])
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO recording_sessions(id,started_ns) VALUES(?,?)`, id, at.UnixNano()); err != nil {
				return err // a native recording with the same ID is not overwritten
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO recording_imports(id) VALUES(?)`, id); err != nil {
				return err
			}
			return tx.Commit()
		})
		if err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(filepath.Join(r.Dir, id[:6], id))
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := os.ReadDir(filepath.Join(r.Dir, id[:6], id))
	if err != nil {
		return err
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(128<<20))
	if err != nil {
		return err
	}
	defer decoder.Close()
	if len(entries) == 0 || entries[0].Name() != ChunkName(0) {
		return errors.New("旧录制开头缺失，保留原件")
	}
	seenImported := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		seq, err := strconv.Atoi(entry.Name())
		if err != nil || seq < 0 || entry.Name() != ChunkName(seq) || !entry.Type().IsRegular() {
			return errors.New("旧录制含无法识别的文件，保留原件")
		}
		f, err := root.Open(entry.Name())
		if err != nil {
			return err
		}
		// Legacy command chunks can exceed the usual 256 KiB. Refuse enormous
		// or corrupt sources without unbounded allocations, never delete them.
		body, err := io.ReadAll(io.LimitReader(f, (128<<20)+1))
		f.Close()
		if err != nil {
			return err
		}
		if len(body) > 128<<20 {
			return errors.New("旧录制块过大，保留原件")
		}
		packed, err := r.Sealer.Open(body, chunkAD(id, seq))
		if err != nil {
			return errors.New("旧录制校验失败，保留原件")
		}
		if _, err := decoder.DecodeAll(packed, nil); err != nil {
			return errors.New("旧录制压缩块无效，保留原件")
		}
		if seq <= last {
			var saved []byte
			if err := r.db.QueryRowContext(ctx, `SELECT body FROM recording_chunks WHERE recording=? AND seq=?`, id, seq).Scan(&saved); err != nil {
				return err
			}
			if !bytes.Equal(saved, body) {
				return errors.New("旧录制与已提交副本冲突，保留双方")
			}
			seenImported++
			continue
		}
		err = r.budget.WriteArchive(ctx, 3, int64(len(body))*2+(32<<10), func() error {
			tx, err := r.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, `INSERT INTO recording_chunks(recording,seq,body) VALUES(?,?,?)`, id, seq, body); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE recording_imports SET last_seq=? WHERE id=?`, seq, id); err != nil {
				return err
			}
			return tx.Commit()
		})
		if err != nil {
			return err
		}
	}
	var storedImported int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recording_chunks WHERE recording=? AND seq<=?`, id, last).Scan(&storedImported); err != nil {
		return err
	}
	if seenImported != storedImported {
		return errors.New("已导入的旧块后来缺失，保留双方")
	}
	return r.budget.WriteArchive(ctx, 3, 16<<10, func() error {
		_, err := r.db.ExecContext(ctx, `UPDATE recording_imports SET complete=1 WHERE id=?`, id)
		return err
	})
}

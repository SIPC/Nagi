package logstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// Archive is a legacy history owner sharing this machine's budget. Size includes
// its indexes/WAL/attachments; Trim removes a bounded oldest batch and reclaims
// physical space. Callbacks run under the journal gate: never log back into it.
// Writers must enter WriteArchive before acquiring their own transaction/lock.
type Archive struct {
	Name     string
	Priority int
	Size     func() (int64, error)
	Trim     func(context.Context) (bool, error)
}

type ArchiveUsage struct {
	Name          string `json:"name"`
	DiskBytes     int64  `json:"disk_bytes"`
	PrunedBatches int64  `json:"pruned_batches"`
}

func (s *Store) RegisterArchive(ctx context.Context, a Archive) error {
	if s == nil {
		return nil
	}
	if a.Name == "" || a.Priority < 0 || a.Priority > 4 || a.Size == nil || a.Trim == nil {
		return fmt.Errorf("无效的历史存储所有者")
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	for _, old := range s.archives {
		if old.Name == a.Name {
			return fmt.Errorf("重复历史存储: %s", a.Name)
		}
	}
	s.archives = append(s.archives, a)
	return nil
}

// WriteArchive reserves bounded transaction headroom, then serializes legacy
// writes with journal writes and cleanup. It never retries a side-effecting fn.
func (s *Store) WriteArchive(ctx context.Context, priority int, reserve int64, fn func() error) error {
	if s == nil {
		return fn()
	}
	if priority < 0 || priority > 4 || reserve < 0 || reserve > s.budget/5 {
		return ErrCapacity
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	if s.closed {
		return sql.ErrConnDone
	}
	if err := s.prepare(ctx, priority, reserve); err != nil {
		return err
	}
	return fn()
}

func (s *Store) prepare(ctx context.Context, p int, reserve int64) error {
	live, err := s.liveBytes(ctx)
	if err != nil {
		return err
	}
	physical, err := s.diskBytes()
	if err != nil {
		return err
	}
	maxPriority := min(p, 3)
	if p == 4 {
		maxPriority = 4
	}
	if live+reserve > s.dbLimit*9/10 || physical+reserve > s.dbLimit {
		if err := s.prune(ctx, s.dbLimit*8/10, maxPriority); err != nil {
			return err
		}
		if err := s.compact(ctx); err != nil {
			return err
		}
	}
	physical, err = s.diskBytes()
	if err != nil {
		return err
	}
	if physical+reserve > s.dbLimit {
		return ErrCapacity
	}
	return nil
}

// FileBytes accounts for physical SQLite files, not just live payload bytes.
func FileBytes(path string) (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		st, err := os.Stat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += st.Size()
	}
	return total, nil
}

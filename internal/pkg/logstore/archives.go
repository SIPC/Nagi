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
	return s.registerArchive(ctx, a, false)
}

// ReplaceArchive transfers an existing history owner's accounting at startup,
// for example from protected migration files to a reclaimable history database.
func (s *Store) ReplaceArchive(ctx context.Context, a Archive) error {
	return s.registerArchive(ctx, a, true)
}

func (s *Store) registerArchive(ctx context.Context, a Archive, replace bool) error {
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
	s.lastDisk.Store(0) // a new owner's bytes are not in the cached size yet
	for i, old := range s.archives {
		if old.Name == a.Name {
			if replace {
				s.archives[i] = a
				return nil
			}
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

// Reserve makes room for a write to a separate store that has its own writer
// lock (recent copies). It holds the journal gate only while checking or
// reclaiming capacity; the caller's transaction then runs without it. The
// usual case (enough space) costs a few PRAGMA reads.
//
// Far below the quota it does not take the gate at all: each admission adds
// its reserve (an upper bound of what that write adds) to the size last
// measured under the gate, so admissions accumulate until the estimate
// reaches 70% of the limit and the next caller measures again. The 30% margin
// covers journal writes made since that measurement. A busy journal therefore
// cannot stall callers that only need to know there is room.
func (s *Store) Reserve(ctx context.Context, priority int, reserve int64) error {
	if s != nil && priority >= 0 && priority <= 4 && reserve >= 0 && reserve <= s.budget/5 {
		for {
			n := s.lastDisk.Load()
			if n <= 0 || n+reserve > s.dbLimit*7/10 {
				break
			}
			if s.lastDisk.CompareAndSwap(n, n+reserve) {
				return nil
			}
		}
	}
	return s.WriteArchive(ctx, priority, reserve, func() error { return nil })
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
	s.lastDisk.Store(physical)
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

package recording

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func (r *Recordings) UseBudget(ctx context.Context, budget *logstore.Store) error {
	if budget == nil {
		return nil
	}
	err := budget.RegisterArchive(ctx, logstore.Archive{Name: r.Dir, Priority: 3,
		Size: func() (int64, error) {
			var total int64
			err := filepath.WalkDir(r.Dir, func(_ string, d fs.DirEntry, err error) error {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if !d.IsDir() {
					s, err := d.Info()
					if err != nil {
						return err
					}
					total += s.Size()
				}
				return nil
			})
			return total, err
		},
		Trim: func(ctx context.Context) (bool, error) {
			var oldest string
			var at time.Time
			err := filepath.WalkDir(r.Dir, func(path string, d fs.DirEntry, err error) error {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if !d.IsDir() || !recordingIDRe.MatchString(d.Name()) {
					return nil
				}
				if _, active := r.active.Load(d.Name()); active {
					return filepath.SkipDir
				}
				s, err := d.Info()
				if err != nil {
					return err
				}
				if oldest == "" || s.ModTime().Before(at) {
					oldest, at = path, s.ModTime()
				}
				return filepath.SkipDir
			})
			if err != nil || oldest == "" {
				return false, err
			}
			return true, os.RemoveAll(oldest)
		},
	})
	if err == nil {
		r.budget = budget
	}
	return err
}

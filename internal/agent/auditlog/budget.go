package auditlog

import (
	"context"
	"os"
	"path/filepath"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func (l *Log) UseBudget(ctx context.Context, budget *logstore.Store) error {
	if budget == nil {
		return nil
	}
	if err := budget.RegisterArchive(ctx, logstore.Archive{
		Name: l.dir, Priority: 4,
		Size: func() (int64, error) {
			entries, err := os.ReadDir(l.dir)
			if err != nil {
				return 0, err
			}
			var total int64
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return 0, err
				}
				total += info.Size()
			}
			return total, nil
		},
		Trim: func(ctx context.Context) (bool, error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			old := rotated(l.dir)
			// 留最后一个轮换文件：当前文件可能刚轮换即写失败，重启仍需恢复旧序号。
			if len(old) <= 1 {
				return false, nil
			}
			return true, os.Remove(filepath.Join(l.dir, old[0]))
		},
	}); err != nil {
		return err
	}
	l.budget = budget
	return nil
}

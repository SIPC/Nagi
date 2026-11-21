package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/server/audit"
)

// 面板审计库的位置（见 docs/audit.md#存储保留与同步）：v0.37.1 起在 <数据目录>/logs/audit/，
// 和面板业务库（panel.db）、主备快照分开。旧版本放在面板目录里的审计库启动时逐个核验后搬过去，见 audit.Relocate。

// auditDir is where audit databases live. Without the local log store there is
// nowhere to record the move, so the files stay in the panel directory.
func (s *Server) auditDir() string {
	if s.cfg.Logs == nil {
		return s.cfg.DataDir
	}
	return filepath.Join(s.cfg.Logs.Directory(), "audit")
}

// auditPath returns where to open an audit database called name: the new
// directory once a verified copy is there, the legacy file while its move has
// not succeeded yet, and the new directory for a file that exists nowhere yet.
func (s *Server) auditPath(name string) string {
	path := filepath.Join(s.auditDir(), name)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if old := filepath.Join(s.cfg.DataDir, name); old != path {
		if _, err := os.Stat(old); err == nil {
			return old
		}
	}
	return path
}

// auditGlob lists audit archives matching pattern in both directories.
func (s *Server) auditGlob(pattern string) ([]string, error) {
	var out []string
	for _, dir := range []string{s.auditDir(), s.cfg.DataDir} {
		paths, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, err
		}
		out = append(out, paths...)
		if dir == s.cfg.DataDir && dir == s.auditDir() {
			break
		}
	}
	return out, nil
}

// relocateAudit moves every legacy audit database out of the panel directory.
// A file that cannot be verified stays where it is and keeps being used; the
// failure is shown on the log page and retried on the next start.
func (s *Server) relocateAudit(ctx context.Context, dir string) error {
	logs := s.cfg.Logs
	if logs == nil || dir == s.cfg.DataDir {
		return nil
	}
	if err := s.relocateLegacyAudit(ctx, dir, logs); err != nil {
		return err
	}
	return nil
}

// verifyMovedAudit verifies, after startup, every chain moved by rename this
// start. Nothing was deleted by the move, so the entrance did not wait for it;
// a chain that fails stays as it is and is reported on the log page.
func (s *Server) verifyMovedAudit(ctx context.Context) {
	if len(s.movedAudit) == 0 {
		return
	}
	logs := s.cfg.Logs
	logs.SetMigration(logstore.Migration{ID: audit.VerifyMovedID, Phase: "cleanup"})
	var failures []error
	for _, path := range s.movedAudit {
		if err := audit.VerifyMoved(ctx, path); err != nil {
			if ctx.Err() != nil {
				return
			}
			failures = append(failures, fmt.Errorf("%s: %w", filepath.Base(path), err))
			continue
		}
		_ = logs.Record(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.audit.verified", Object: filepath.Base(path)})
	}
	if err := errors.Join(failures...); err != nil {
		s.log.Error("迁移后的审计库校验不通过，文件原样保留，请检查是否被篡改", "err", err)
		logs.SetMigration(logstore.Migration{ID: audit.VerifyMovedID, Phase: "failed", Error: err.Error()})
		return
	}
	s.log.Info("迁移后的审计库整链校验通过", "count", len(s.movedAudit))
	logs.SetMigration(logstore.Migration{ID: audit.VerifyMovedID, Phase: "complete"})
}

func (s *Server) relocateLegacyAudit(ctx context.Context, dir string, logs *logstore.Store) error {
	var legacy []string
	for _, pattern := range []string{"audit.db", "audit-standby.db", "audit-term*.db", "audit-standby-*.imported.db", "audit-rollback-*.db"} {
		paths, err := filepath.Glob(filepath.Join(s.cfg.DataDir, pattern))
		if err != nil {
			return err
		}
		legacy = append(legacy, paths...)
	}
	if len(legacy) == 0 {
		return nil
	}
	logs.SetMigration(logstore.Migration{ID: audit.RelocateID, Phase: "cleanup"})
	var failures []error
	for _, old := range legacy {
		dst := filepath.Join(dir, filepath.Base(old))
		_, err := audit.Relocate(ctx, old, dst, logs)
		if errors.Is(err, audit.ErrWrittenAfterMove) {
			// An older version wrote a new chain here after the move. Keep it as
			// its own archive; it is verified and moved the same way.
			var name string
			if name, err = audit.RollbackName(ctx, old); err == nil {
				dst = filepath.Join(dir, name)
				s.log.Warn("旧审计库在迁移后又被写入过（可能曾退回旧版本），作为单独的归档核验后迁移", "path", old, "to", dst)
				_, err = audit.Relocate(ctx, old, dst, logs)
			}
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", filepath.Base(old), err))
			continue
		}
		s.movedAudit = append(s.movedAudit, dst)
		s.log.Info("面板旧审计库已迁到本机日志目录", "from", old, "to", dst)
	}
	if err := errors.Join(failures...); err != nil {
		s.log.Error("面板旧审计库迁移未完成，未核验通过的原库已保留并继续使用，下次启动重试", "err", err)
		logs.SetMigration(logstore.Migration{ID: audit.RelocateID, Phase: "failed", Error: err.Error()})
		return nil
	}
	logs.SetMigration(logstore.Migration{ID: audit.RelocateID, Phase: "complete"})
	return nil
}

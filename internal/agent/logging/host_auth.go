package logging

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

var sudoCommand = regexp.MustCompile(`(?is)\bCOMMAND\s*[:=].*`)

func hostAuthEvent(entry journalEntry, cfg logstore.Config) (logstore.Event, bool, bool) {
	unit, comm, message := entry.text("_SYSTEMD_UNIT"), entry.text("_COMM"), entry.text("MESSAGE")
	lower := strings.ToLower(message)
	failure, kind := false, ""
	if cfg.HostSSH && (unit == "ssh.service" || unit == "sshd.service" || strings.HasPrefix(unit, "sshd@") || strings.HasPrefix(unit, "ssh@") || comm == "sshd" || comm == "sshd-session" || comm == "sshd-auth") {
		failure = containsAny(lower, "failed password", "failed publickey", "failed keyboard-interactive", "authentication failure", "invalid user", "maximum authentication attempts exceeded")
		if failure {
			kind = "host.ssh.failed"
		} else if containsAny(lower, "accepted password", "accepted publickey", "accepted keyboard-interactive", "disconnected from user") {
			kind = "host.ssh.login"
		}
	}
	if cfg.HostSudo && (comm == "sudo" || comm == "su") {
		kind = "host.sudo"
		// Do not retain arbitrary command arguments, even ones the generic
		// password/token recognizer cannot understand.
		message = sudoCommand.ReplaceAllString(message, "COMMAND=[已隐藏]")
		if containsAny(strings.ToLower(message), "command") && !strings.Contains(message, "COMMAND=[已隐藏]") {
			message = "[已隐藏包含命令的认证消息]"
		}
	}
	if kind == "" {
		return logstore.Event{}, false, false
	}
	e := journalEvent(entry, message, logstore.Security, kind)
	e.Data.(map[string]any)["count"] = 1
	return e, failure, true
}

// hostWriter serializes journal facts and rate-limit summaries with one durable
// cursor. A later ordinary event cannot acknowledge past unsaved SSH counts.
type hostWriter struct {
	mu         sync.Mutex
	logs       *logstore.Store
	cfg        logstore.Config
	position   string
	window     time.Time
	detailed   int
	pending    uint64
	first      time.Time
	last       logstore.Event
	lastCursor string
}

func newHostWriter(ctx context.Context, logs *logstore.Store, cfg logstore.Config, position string) (*hostWriter, func()) {
	w := &hostWriter{logs: logs, cfg: cfg, position: position, window: time.Now()}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !cfg.HostSSH {
			return
		}
		ticker := time.NewTicker(time.Duration(cfg.HostSSHSummarySeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.flush(ctx); err != nil && ctx.Err() == nil {
					logs.Report(logstore.Event{Category: logstore.System, Kind: "host.journal.unavailable", Data: map[string]any{"error": err.Error(), "history_complete": false}})
				}
			}
		}
	}()
	return w, func() {
		cancel()
		<-done
		if err := w.flush(context.Background()); err != nil {
			logs.Report(logstore.Event{Category: logstore.System, Kind: "host.journal.unavailable", Data: map[string]any{"error": err.Error(), "history_complete": false}})
		}
	}
}

func (w *hostWriter) current() string { w.mu.Lock(); defer w.mu.Unlock(); return w.position }
func (w *hostWriter) resetPosition()  { w.mu.Lock(); defer w.mu.Unlock(); w.position = "" }

func (w *hostWriter) write(ctx context.Context, e logstore.Event, cursor string) error {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := w.logs.RecordCheckpoint(ctx, e, "host.journal", cursor); err != nil {
		return err
	}
	w.position = cursor
	return nil
}

func (w *hostWriter) append(ctx context.Context, entry journalEntry) error {
	e, failure, keep := hostAuthEvent(entry, w.cfg)
	if !keep {
		e, keep = hostEvent(entry)
	}
	if !keep {
		return nil
	}
	cursor := entry.text("__CURSOR")
	if cursor == "" || len(cursor) > 2048 {
		return fmt.Errorf("journal 记录缺少有效游标")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if time.Since(w.window) >= time.Minute {
		if err := w.flushLocked(ctx); err != nil {
			return err
		}
		w.window, w.detailed = time.Now(), 0
	}
	if failure && w.detailed >= w.cfg.HostSSHFailuresPerMinute {
		if w.pending == 0 {
			w.first = e.Time
		}
		w.pending++
		w.last, w.lastCursor = e, cursor
		return nil
	}
	if err := w.flushLocked(ctx); err != nil {
		return err
	}
	if err := w.write(ctx, e, cursor); err != nil {
		return err
	}
	if failure {
		w.detailed++
	}
	return nil
}

func (w *hostWriter) flush(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked(ctx)
}

func (w *hostWriter) flushLocked(ctx context.Context) error {
	if w.pending == 0 {
		return nil
	}
	e := logstore.Event{Category: logstore.Security, Kind: "host.ssh.failed.summary", Time: w.last.Time,
		Data: map[string]any{"count": w.pending, "details_omitted": w.pending, "first_time": w.first, "last_time": w.last.Time, "counter": "observed_failure_records", "cursor": w.lastCursor}}
	if err := w.write(ctx, e, w.lastCursor); err != nil {
		return err
	}
	w.pending = 0
	return nil
}

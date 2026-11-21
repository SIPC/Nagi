package logging

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// journalEntry deliberately excludes environment, command lines and arbitrary
// journal fields. Only the selected message passes through the store's redactor.
type journalEntry map[string]json.RawMessage

func (e journalEntry) text(key string) string {
	var s string
	_ = json.Unmarshal(e[key], &s)
	return s
}

type journalSource interface {
	check(context.Context, string) error
	follow(context.Context, string, time.Time, func(journalEntry) error) error
}

var errJournalCursor = errors.New("journal 已不保留上次读取位置，历史存在缺口")

// RunHost reads the existing system journal only. It neither enables new host
// logging nor falls back to duplicate syslog files or the panel control channel.
func RunHost(ctx context.Context, logs *logstore.Store, cfg logstore.Config) {
	if !cfg.HostJournal {
		return
	}
	if runtime.GOOS != "linux" {
		logs.Report(logstore.Event{Category: logstore.System, Kind: "host.journal.unavailable", Data: map[string]any{"error": "宿主机 journal 采集仅支持 Linux", "history_complete": false}})
		return
	}
	runHost(ctx, logs, cfg, systemJournal{})
}

func runHost(ctx context.Context, logs *logstore.Store, cfg logstore.Config, src journalSource) {
	const key = "host.journal"
	var position string
	b, err := logs.Checkpoint(ctx, key)
	if err == nil && len(b) != 0 {
		err = json.Unmarshal(b, &position)
	}
	if err != nil {
		logs.Report(logstore.Event{Category: logstore.System, Kind: "host.journal.unavailable", Data: map[string]any{"error": err.Error(), "history_complete": false}})
		return // Never silently reset an unreadable durable position.
	}
	lastError := ""
	for ctx.Err() == nil {
		err := src.check(ctx, position)
		if errors.Is(err, errJournalCursor) {
			// Keep the old durable cursor until a new selected fact is committed.
			logs.Report(logstore.Event{Category: logstore.System, Kind: "host.journal.gap", Data: map[string]any{"error": err.Error(), "history_complete": false}})
			position = ""
			err = nil
		}
		if err == nil {
			logs.Report(logstore.Event{Category: logstore.System, Kind: "host.journal.attaching", Data: map[string]any{"history_complete": false, "lookback_hours": cfg.HostLookbackHours, "resuming": position != ""}})
			err = src.follow(ctx, position, time.Now().Add(-time.Duration(cfg.HostLookbackHours)*time.Hour), func(entry journalEntry) error {
				e, keep := hostEvent(entry)
				if !keep {
					return nil
				}
				cursor := entry.text("__CURSOR")
				if cursor == "" || len(cursor) > 2048 {
					return fmt.Errorf("journal 记录缺少有效游标")
				}
				writeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
				defer cancel()
				if err := logs.RecordCheckpoint(writeCtx, e, key, cursor); err != nil {
					return err
				}
				position = cursor
				return nil
			})
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		if err.Error() != lastError {
			logs.Report(logstore.Event{Category: logstore.System, Kind: "host.journal.unavailable", Data: map[string]any{"error": err.Error(), "history_complete": false}})
			lastError = err.Error()
		}
		timer := time.NewTimer(time.Duration(cfg.HostRetrySeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

var hostUnits = []string{
	"docker.service", "containerd.service", "nagi-agent.service", "nagi-server.service",
	"NetworkManager.service", "systemd-networkd.service", "systemd-resolved.service",
	"networking.service", "dhcpcd.service", "dhclient.service", "chrony.service", "chronyd.service", "systemd-timesyncd.service", "ntp.service", "ntpd.service",
	"systemd-journald.service", "rsyslog.service", "logrotate.service",
}

func listedUnit(unit string) bool {
	for _, u := range hostUnits {
		if unit == u {
			return true
		}
	}
	return false
}

func containsAny(text string, words ...string) bool {
	for _, word := range words {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

func hostEvent(entry journalEntry) (logstore.Event, bool) {
	message := entry.text("MESSAGE")
	unit := entry.text("_SYSTEMD_UNIT")
	lower := strings.ToLower(message)
	priority, err := strconv.Atoi(entry.text("PRIORITY"))
	warning := err == nil && priority >= 0 && priority <= 4
	kind := ""
	switch {
	case entry.text("_TRANSPORT") == "kernel":
		switch {
		case containsAny(lower, "out of memory", "oom-kill", "oom_reaper", "killed process", "kernel panic", "soft lockup", "hard lockup", "hung task", "blocked for more than"):
			kind = "kernel.memory"
		case containsAny(lower, "i/o error", "i/o timeout", "buffer i/o", "read-only file system", "remounting filesystem read-only", "remounting filesystem read only", "ext4-fs error", "xfs corruption", "btrfs error", "nvme timeout", "resetting controller", "filesystem has been shut down") || (containsAny(lower, "xfs", "btrfs", "nvme", "ata", "scsi") && containsAny(lower, "corrupt", "error", "failed", "resetting")):
			kind = "kernel.storage"
		case containsAny(lower, "link is down", "link is up", "link down", "link up", "netdev watchdog", "nf_conntrack: table full", "transmit queue timed out", "nic reset"):
			kind = "kernel.network"
		}
	case entry.text("_PID") == "1":
		target := entry.text("UNIT")
		if warning || containsAny(lower, "failed to start", "failed with result", "start request repeated too quickly", "dependency failed", "timed out") {
			kind = "service.failed"
		} else if containsAny(lower, "startup finished", "shutting down", "rebooting", "powering off", "system shutdown") {
			kind = "host.boot"
		} else if listedUnit(target) && containsAny(lower, "starting", "started", "stopping", "stopped", "deactivated") {
			kind = "service.state"
		}
	case listedUnit(unit):
		switch unit {
		case "docker.service", "containerd.service":
			if warning {
				kind = "daemon.warning"
			}
		case "nagi-agent.service", "nagi-server.service":
			// The structured logger already writes the same fact to logs.db.
			var structured struct {
				Level string `json:"level"`
				Time  string `json:"time"`
				Msg   string `json:"msg"`
			}
			if json.Unmarshal([]byte(message), &structured) == nil && structured.Level != "" && structured.Time != "" {
				return logstore.Event{}, false
			}
			if strings.HasPrefix(message, "time=") && strings.Contains(message, " level=") && strings.Contains(message, " msg=") && !containsAny(message, "本地日志入库失败", "无法打开本机日志") {
				return logstore.Event{}, false
			}
			kind = "nagi.output"
		default:
			if warning || containsAny(lower, "failed", "failure", "unreachable", "lost", "disconnected", "link down", "carrier", "clock change", "time jump", "time has been changed", "synchronized", "synchronised", "suppressed", "dropped", "corrupt", "no space left") {
				kind = "service.warning"
			}
		}
	}
	if kind == "" {
		return logstore.Event{}, false
	}
	at, timeErr := strconv.ParseInt(entry.text("__REALTIME_TIMESTAMP"), 10, 64)
	var eventTime time.Time
	if timeErr == nil && at > 0 {
		eventTime = time.UnixMicro(at)
	}
	truncated := len(message) > 8192
	if truncated {
		message = strings.ToValidUTF8(message[:8192], "")
	}
	return logstore.Event{Time: eventTime, Category: logstore.Runtime, Kind: "host." + kind, Object: unit,
		Data: map[string]any{"message": message, "priority": entry.text("PRIORITY"), "unit": unit, "target_unit": entry.text("UNIT"), "boot_id": entry.text("_BOOT_ID"), "cursor": entry.text("__CURSOR"), "truncated": truncated, "timestamp_missing": eventTime.IsZero()}}, true
}

type systemJournal struct{}

// cappedOutput bounds stderr without making a chatty child block on its pipe.
type cappedOutput struct{ text []byte }

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.text) < 8192 {
		b.text = append(b.text, p[:min(len(p), 8192-len(b.text))]...)
	}
	return n, nil
}

const journalFields = "__CURSOR,__REALTIME_TIMESTAMP,_BOOT_ID,_TRANSPORT,_SYSTEMD_UNIT,_PID,UNIT,PRIORITY,MESSAGE,SYSLOG_IDENTIFIER"

func (systemJournal) check(ctx context.Context, cursor string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := []string{"--system", "--no-pager", "--output=json", "--output-fields=__CURSOR", "--lines=1"}
	if cursor != "" {
		args = append(args, "--cursor="+cursor)
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	var out, diagnostic cappedOutput
	cmd.Stdout, cmd.Stderr = &out, &diagnostic
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("读取系统 journal: %w: %s", err, diagnostic.text)
	}
	// journalctl may exit successfully while showing only an unauthorized user's
	// empty/partial journal. Treat any access diagnostic as unavailable.
	if len(diagnostic.text) != 0 {
		return fmt.Errorf("系统 journal 不可完整读取: %s", diagnostic.text)
	}
	var entry journalEntry
	if err := json.Unmarshal(out.text, &entry); err != nil {
		return fmt.Errorf("系统 journal 不可用、为空或探测记录过大: %w", err)
	}
	if cursor != "" && entry.text("__CURSOR") != cursor {
		return errJournalCursor
	}
	return nil
}

func (systemJournal) follow(ctx context.Context, cursor string, since time.Time, emit func(journalEntry) error) error {
	args := []string{"--system", "--no-pager", "--no-tail", "--all", "--output=json", "--output-fields=" + journalFields}
	// OR source matches, never a global priority filter: link/time/state events
	// can be informational. Container output is not a separate source here.
	args = append(args, "_TRANSPORT=kernel", "+", "_PID=1")
	for _, unit := range hostUnits {
		args = append(args, "+", "_SYSTEMD_UNIT="+unit)
	}
	read := func(follow bool) error {
		opts := append([]string{}, args...)
		if cursor != "" {
			opts = append(opts, "--after-cursor="+cursor)
		} else {
			opts = append(opts, "--since="+since.Format("2006-01-02 15:04:05"))
		}
		if follow {
			opts = append(opts, "--follow")
		}
		return readJournal(ctx, opts, func(entry journalEntry) error {
			if err := emit(entry); err != nil {
				return err
			}
			// Even a deliberately excluded line can bound the handover, but a
			// failed selected write must not advance either cursor.
			if next := entry.text("__CURSOR"); next != "" {
				cursor = next
			}
			return nil
		})
	}
	// Follow mode defaults to the current boot. Read retained history without
	// follow first, then use its last cursor to close the handover race.
	if err := read(false); err != nil {
		return err
	}
	return read(true)
}

func readJournal(ctx context.Context, args []string, emit func(journalEntry) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	var diagnostic cappedOutput
	cmd.Stderr = &diagnostic
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 8192), 1<<20)
	for scanner.Scan() {
		var entry journalEntry
		if err = json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			break
		}
		if err = emit(entry); err != nil {
			break
		}
	}
	if err == nil {
		err = scanner.Err()
	}
	cancel()
	waitErr := cmd.Wait()
	if err != nil {
		return err
	}
	if len(diagnostic.text) != 0 {
		return fmt.Errorf("journal 读取中断: %s", diagnostic.text)
	}
	return waitErr
}

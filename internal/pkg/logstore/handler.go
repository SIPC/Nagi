package logstore

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

type handler struct {
	base    slog.Handler
	persist slog.Handler
}

// Logger 保留原有终端输出，同时保存结构化运行日志；With/WithGroup 的语义不变。
func (s *Store) Logger(base *slog.Logger, cfg Config) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.RuntimeLevel)) // LoadConfig 已校验。
	return slog.New(&handler{base: base.Handler(), persist: slog.NewJSONHandler(logWriter{s}, &slog.HandlerOptions{Level: level})})
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.persist.Enabled(ctx, level) || h.base.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.base.Enabled(ctx, r.Level) {
		err = h.base.Handle(ctx, r)
	}
	if h.persist.Enabled(ctx, r.Level) {
		err = errors.Join(err, h.persist.Handle(ctx, r))
	}
	return err
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{base: h.base.WithAttrs(attrs), persist: h.persist.WithAttrs(attrs)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &handler{base: h.base.WithGroup(name), persist: h.persist.WithGroup(name)}
}

type logWriter struct{ store *Store }

func (w logWriter) Write(b []byte) (int, error) {
	var meta struct{ Time time.Time }
	if err := json.Unmarshal(b, &meta); err != nil {
		return 0, err
	}
	w.store.Report(Event{Time: meta.Time, Category: Runtime, Kind: "program", Data: json.RawMessage(b)})
	return len(b), nil
}

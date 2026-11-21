package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// AllInstances 包括未托管容器；没有项目归属的日志只能按节点权限读取。
func (d *Client) AllInstances(ctx context.Context) ([]Instance, error) {
	return d.listInstances(ctx, nil, "nagi.project")
}

// LogsSince 供后台持久采集使用。Docker 时间边界是包含关系，由调用方按流去重。
// 回调串行执行，失败立即停读，不把没有持久保存的行当作已消费。
func (d *Client) LogsSince(ctx context.Context, c Instance, since time.Time, lineBytes int, onLine func(LogLine) error) error {
	info, err := d.c.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	opts := client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: c.State == "running", Tail: "all", Timestamps: true}
	if !since.IsZero() {
		opts.Since = since.Format(time.RFC3339Nano)
	}
	rc, err := d.c.ContainerLogs(ctx, c.ID, opts)
	if err != nil {
		return err
	}
	defer rc.Close()
	stdout := &historyLines{stream: "stdout", instance: c, limit: lineBytes, emit: onLine}
	if info.Container.Config != nil && info.Container.Config.Tty {
		_, err = io.Copy(stdout, rc)
		if err == nil {
			err = stdout.flush()
		}
		return err
	}
	stderr := &historyLines{stream: "stderr", instance: c, limit: lineBytes, emit: onLine}
	_, err = stdcopy.StdCopy(stdout, stderr, rc)
	if err == nil {
		err = errors.Join(stdout.flush(), stderr.flush())
	}
	return err
}

// 每行最多保留 8 KiB 原文前缀，给 JSON 转义和通用脱敏留空间；超长行仍消费完整，不静默丢掉后续行。
// 不用 Scanner 的最大 token 上限，否则一行过长会把整个余下输出吞掉。
type historyLines struct {
	stream   string
	instance Instance
	limit    int
	emit     func(LogLine) error
	head     []byte
	bytes    int64
}

func (w *historyLines) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) != 0 {
		i := bytes.IndexByte(p, '\n')
		piece := p
		if i >= 0 {
			piece = p[:i]
		}
		w.bytes += int64(len(piece))
		if left := w.limit - len(w.head); left > 0 {
			w.head = append(w.head, piece[:min(left, len(piece))]...)
		}
		if i < 0 {
			break
		}
		if err := w.flush(); err != nil {
			return 0, err
		}
		p = p[i+1:]
	}
	return total, nil
}

func (w *historyLines) flush() error {
	if w.bytes == 0 && len(w.head) == 0 {
		return nil
	}
	ts, text := splitTimestamp(strings.ToValidUTF8(string(w.head), ""))
	err := w.emit(LogLine{Container: w.instance.Container, Service: w.instance.Service, Stream: w.stream,
		Time: ts, Text: text, Bytes: w.bytes, Truncated: w.bytes > int64(len(w.head))})
	w.head, w.bytes = w.head[:0], 0
	return err
}

// EngineEvent 只保留事件白名单，不复制可能包含凭据的任意容器标签、命令或健康检查输出。
type EngineEvent struct {
	Time       time.Time         `json:"time"`
	Type       string            `json:"type"`
	Action     string            `json:"action"`
	ID         string            `json:"id"`
	Project    string            `json:"project,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// EventsSince 不隐藏断线：Docker 只保留有限事件回放，重连不能宣称历史必定齐全。
func (d *Client) EventsSince(ctx context.Context, since time.Time, emit func(EngineEvent) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opts := client.EventsListOptions{}
	if !since.IsZero() {
		opts.Since = since.Format(time.RFC3339Nano)
	}
	res := d.c.Events(ctx, opts)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-res.Err:
			if !ok || err == nil {
				return io.EOF
			}
			return err
		case e, ok := <-res.Messages:
			if !ok {
				return io.EOF
			}
			action, detail, _ := strings.Cut(string(e.Action), ":")
			a := map[string]string{}
			for _, key := range []string{"name", "image", "exitCode", "signal", "nagi.container", labelService} {
				if v := e.Actor.Attributes[key]; v != "" {
					a[key] = v
				}
			}
			if action == "health_status" {
				switch status := strings.TrimSpace(detail); status {
				case "healthy", "unhealthy", "starting", "running":
					a["health"] = status
				}
			}
			var at time.Time
			if e.TimeNano != 0 {
				at = time.Unix(0, e.TimeNano)
			} else if e.Time != 0 {
				at = time.Unix(e.Time, 0)
			}
			if err := emit(EngineEvent{Time: at, Type: string(e.Type), Action: action, ID: e.Actor.ID,
				Project: e.Actor.Attributes["nagi.project"], Attributes: a}); err != nil {
				return err
			}
		}
	}
}

// Package logging 将节点自己的采集来源接入本地日志，不处理 mesh 身份、远端复制和告警判定。
package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// ContainerSource 是 Docker 的只读采集面；不会启动/停止容器或改变 Docker 自己的日志轮换。
type ContainerSource interface {
	AllInstances(context.Context) ([]docker.Instance, error)
	LogsSince(context.Context, docker.Instance, time.Time, int, func(docker.LogLine) error) error
	EventsSince(context.Context, time.Time, func(docker.EngineEvent) error) error
}

// RunContainers 后台采集本机全部容器的输出、状态快照和引擎事件；不依赖有人打开日志页面。
// 返回前取消并等待所有读取者，不能在日志库关闭后继续确认读取位置。
func RunContainers(ctx context.Context, src ContainerSource, logs *logstore.Store, cfg logstore.Config) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	type worker struct {
		state  string
		cancel context.CancelFunc
		done   chan struct{}
	}
	workers := map[string]worker{}
	type snapshot struct {
		data map[string]any
		at   time.Time
	}
	snapshots := map[string]snapshot{}
	kick := make(chan struct{}, 1)
	defer func() { cancel(); wg.Wait() }()
	wg.Go(func() { collectEvents(ctx, src, logs, cfg, kick) })
	rescan := time.NewTicker(time.Duration(cfg.ContainerRescanSeconds) * time.Second)
	defer rescan.Stop()
	lastErr := ""
	for ctx.Err() == nil {
		listCtx, done := context.WithTimeout(ctx, 10*time.Second)
		instances, err := src.AllInstances(listCtx)
		done()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if lastErr != err.Error() {
				logs.Report(logstore.Event{Category: logstore.Runtime, Kind: "docker.inventory.interrupted", Data: map[string]any{"error": err.Error(), "history_complete": false}})
			}
			lastErr = err.Error()
		} else {
			if lastErr != "" {
				logs.Report(logstore.Event{Category: logstore.Runtime, Kind: "docker.inventory.resumed", Data: map[string]any{"history_complete": false}})
			}
			lastErr = ""
			present := map[string]bool{}
			for _, c := range instances {
				if ctx.Err() != nil {
					return
				}
				present[c.ID] = true
				// 状态快照只在内容变化时写，没变化的最多每 stateRefresh 写一次。每个 Docker 事件都会触发重扫，
				// 原来每次重扫给每个容器各写一行，占日志库新增行数的七成。
				state := map[string]any{"name": c.Name, "container": c.Container, "service": c.Service, "state": c.State,
					"status": c.Status, "health": c.Health, "image": c.Image, "image_id": c.ImageID}
				// status 是 Docker 的运行时长文字（Up 5 minutes），每分钟都会变，不算状态变化。
				key := map[string]any{}
				maps.Copy(key, state)
				delete(key, "status")
				if last, ok := snapshots[c.ID]; !ok || !maps.Equal(last.data, key) || time.Since(last.at) >= stateRefresh {
					logs.Report(logstore.Event{Category: logstore.Metrics, Kind: "container.state", Project: c.Project, Object: c.ID, Data: state})
					snapshots[c.ID] = snapshot{data: key, at: time.Now()}
				}
				w, exists := workers[c.ID]
				if exists && w.state == c.State {
					continue
				}
				if exists {
					w.cancel()
					<-w.done
				}
				child, stop := context.WithCancel(ctx)
				finished := make(chan struct{})
				workers[c.ID] = worker{state: c.State, cancel: stop, done: finished}
				wg.Go(func() {
					defer close(finished)
					collectOutput(child, src, logs, cfg, c)
				})
			}
			for id, w := range workers {
				if !present[id] {
					w.cancel()
					<-w.done
					logs.Report(logstore.Event{Category: logstore.Runtime, Kind: "container.disappeared", Object: id,
						Data: map[string]any{"history_complete": false, "reason": "来源已不在 Docker 清单，未采到的尾部不保证能补回"}})
					delete(workers, id)
				}
			}
			for id := range snapshots {
				if !present[id] {
					delete(snapshots, id)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-rescan.C:
		case <-kick: // 新实例出现时尽早接输出，不只等待定时发现；自动删除的容器仍可能来不及读。
		}
	}
}

type position struct {
	Time  time.Time `json:"time"`
	Count int64     `json:"count"`
}

// cursor 的计数仅用于重放时间边界，同一纳秒的重复行/同文事件不能按内容哈希误删。
// Docker 没有持久行号；轮换、有限事件回放及源时钟回拨均不能据此承诺绝对无缺口。
type cursor map[string]position

func loadCursor(ctx context.Context, logs *logstore.Store, key string) (cursor, error) {
	b, err := logs.Checkpoint(ctx, key)
	if err != nil {
		return nil, err
	}
	c := cursor{}
	if len(b) != 0 {
		err = json.Unmarshal(b, &c)
	}
	return c, err
}

func (c cursor) since() time.Time {
	var earliest time.Time
	for _, p := range c {
		if !p.Time.IsZero() && (earliest.IsZero() || p.Time.Before(earliest)) {
			earliest = p.Time
		}
	}
	return earliest
}

func (c cursor) append(ctx context.Context, logs *logstore.Store, key, stream string, replay cursor, e logstore.Event) error {
	if p, exists := replay[stream]; exists && !e.Time.IsZero() {
		if e.Time.Before(p.Time) {
			return nil
		}
		if e.Time.Equal(p.Time) && p.Count > 0 {
			p.Count--
			replay[stream] = p
			return nil
		}
	}
	next := maps.Clone(c)
	p := c[stream]
	if !e.Time.IsZero() {
		if e.Time.After(p.Time) {
			p = position{Time: e.Time}
		}
		if e.Time.Equal(p.Time) {
			p.Count++
		}
		next[stream] = p
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := logs.RecordCheckpoint(ctx, e, key, next); err != nil {
		return err
	}
	for k, v := range next {
		c[k] = v
	}
	// 第一个新记录之后视为实时流。随后同文/同时间或时钟回拨的记录照记，不能当重放静默丢掉。
	delete(replay, stream)
	return nil
}

func collectOutput(ctx context.Context, src ContainerSource, logs *logstore.Store, cfg logstore.Config, instance docker.Instance) {
	key := "docker.logs/" + instance.ID
	lastErr := ""
	for ctx.Err() == nil {
		c, err := loadCursor(ctx, logs, key)
		if err == nil {
			logs.Report(logstore.Event{Category: logstore.Runtime, Kind: "container.output.attaching", Project: instance.Project, Object: instance.ID,
				Data: map[string]any{"since": c.since(), "history_complete": false, "reason": "只读取 Docker 仍保留的输出，无法证明更早历史完整"}})
			replay := maps.Clone(c)
			err = src.LogsSince(ctx, instance, c.since(), cfg.ContainerLineBytes, func(line docker.LogLine) error {
				return c.append(ctx, logs, key, line.Stream, replay, logstore.Event{Time: line.Time, Category: logstore.Container,
					Kind: "container.output", Project: instance.Project, Object: instance.ID, Data: map[string]any{
						"container": instance.Container, "service": instance.Service, "stream": line.Stream, "text": line.Text,
						"truncated": line.Truncated, "original_bytes": line.Bytes, "timestamp_missing": line.Time.IsZero()}})
			})
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil && instance.State != "running" {
			<-ctx.Done()
			return
		}
		message := "输出流结束，将从已保存位置重新连接"
		if err != nil {
			message = err.Error()
		}
		if message != lastErr {
			logs.Report(logstore.Event{Category: logstore.Runtime, Kind: "container.output.interrupted", Project: instance.Project, Object: instance.ID,
				Data: map[string]any{"error": message, "history_complete": false}})
		}
		lastErr = message
		if !retry(ctx, cfg) {
			return
		}
	}
}

func collectEvents(ctx context.Context, src ContainerSource, logs *logstore.Store, cfg logstore.Config, kick chan<- struct{}) {
	const key = "docker.events"
	lastErr := ""
	for ctx.Err() == nil {
		c, err := loadCursor(ctx, logs, key)
		if err == nil {
			logs.Report(logstore.Event{Category: logstore.Runtime, Kind: "docker.events.attaching", Data: map[string]any{
				"since": c.since(), "history_complete": false, "reason": "Docker 事件回放有数量上限，断线期间可能有不可补回的历史"}})
			replay := maps.Clone(c)
			err = src.EventsSince(ctx, c.since(), func(e docker.EngineEvent) error {
				err := c.append(ctx, logs, key, "events", replay, logstore.Event{Time: e.Time, Category: logstore.Task,
					Kind: "docker." + e.Type + "." + e.Action, Project: e.Project, Object: e.ID, Data: e})
				// exec_* 是健康检查和终端里执行的命令，不改变容器清单，不触发重扫。
				if err == nil && e.Type == "container" && !strings.HasPrefix(e.Action, "exec_") {
					select {
					case kick <- struct{}{}:
					default:
					}
				}
				return err
			})
		}
		if ctx.Err() != nil {
			return
		}
		message := fmt.Sprint(err)
		if message != lastErr {
			logs.Report(logstore.Event{Category: logstore.Runtime, Kind: "docker.events.interrupted", Data: map[string]any{"error": message, "history_complete": false}})
		}
		lastErr = message
		if !retry(ctx, cfg) {
			return
		}
	}
}

// stateRefresh 是容器状态没有变化时重写快照的间隔，按时间查看日志时仍能找到附近的快照。
const stateRefresh = 10 * time.Minute

func retry(ctx context.Context, cfg logstore.Config) bool {
	t := time.NewTimer(time.Duration(cfg.ContainerRetrySeconds) * time.Second)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

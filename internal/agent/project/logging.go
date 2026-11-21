package project

import (
	"crypto/rand"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// taskLog 记在执行任务的节点，不依赖面板或输出订阅者。只记归属、进度和结果，不复制配置/环境/命令。
// 返回的输出仍原样交给现有调用者；日志失败不改变任务结果。
func (m *Manager) taskLog(action, projectID, container string, onLine func(string)) (func(string), func(error)) {
	if m.Journal == nil {
		return onLine, func(error) {}
	}
	id, started := rand.Text(), time.Now()
	record := func(phase string, data map[string]any) {
		data["task_id"], data["action"], data["phase"] = id, action, phase
		m.Journal.Report(logstore.Event{Category: logstore.Task, Kind: "project.task", Project: projectID, Object: container, Data: data})
	}
	record("started", map[string]any{})
	return func(line string) {
			record("output", map[string]any{"text": line})
			if onLine != nil {
				onLine(line)
			}
		}, func(err error) {
			data := map[string]any{"result": "ok", "duration_ms": time.Since(started).Milliseconds()}
			if err != nil {
				data["result"], data["error"] = "failed", err.Error()
			}
			record("finished", data)
		}
}

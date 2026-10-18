package project

import (
	"context"
	"fmt"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
)

// startWatch 是部署后观察容器的时长。compose up 成功只说明容器创建并启动了，
// 程序可能几秒后就崩溃退出、被 Docker 反复重启，不观察的话这次部署显示"成功"，用户要等告警才发现。
var startWatch = 10 * time.Second

// startLogLines 是容器起不来时附上的日志行数。
const startLogLines = 20

// watchStart 观察一个 compose 项目的容器：观察期内有容器以非 0 退出码退出，或者被 Docker 重启了，返回错误并输出它最后的日志。
// 以 0 退出、不重启的容器（例如执行一次就结束的任务）不算失败。观察期开始前已经有的重启次数不算。
func (m *Manager) watchStart(ctx context.Context, composeProject string, onLine func(string)) error {
	instances, err := m.docker.ProjectInstances(ctx, composeProject)
	if err != nil || len(instances) == 0 {
		return nil
	}
	base := map[string]int{}
	for _, c := range instances {
		if st, err := m.docker.StartInfo(ctx, c.ID); err == nil {
			base[c.ID] = st.RestartCount
		}
	}
	onLine(fmt.Sprintf("观察 %d 秒，确认容器正常运行", int(startWatch/time.Second)))
	deadline := time.Now().Add(startWatch)
	for {
		for _, c := range instances {
			st, err := m.docker.StartInfo(ctx, c.ID)
			if err != nil {
				continue // 容器可能刚被替换
			}
			var reason string
			switch {
			case st.RestartCount > base[c.ID]:
				reason = fmt.Sprintf("启动后退出、被重启了 %d 次（退出码 %d）", st.RestartCount-base[c.ID], st.ExitCode)
			case st.State == "exited" && st.ExitCode != 0:
				reason = fmt.Sprintf("启动后退出了（退出码 %d）", st.ExitCode)
			}
			if reason == "" {
				continue
			}
			onLine(fmt.Sprintf("容器 %s %s，最后 %d 行日志：", c.Service, reason, startLogLines))
			_ = m.docker.Logs(ctx, c, startLogLines, false, func(l docker.LogLine) { onLine("  " + l.Text) })
			return fmt.Errorf("容器 %s %s，部署算作失败。看上面的日志修改配置后重新部署，或者回滚到之前的版本", c.Service, reason)
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

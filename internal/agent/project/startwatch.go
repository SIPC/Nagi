package project

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
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
// hasPrevious 表示容器之前部署过别的版本，提示里才说可以回滚。
func (m *Manager) watchStart(ctx context.Context, composeProject string, hasPrevious bool, onLine func(string)) error {
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
			case st.State == "restarting":
				// 反复崩溃以后 Docker 会把重启间隔拉长到几十秒，观察期里重启次数可能不变，但容器一直在等着重启。
				reason = fmt.Sprintf("退出了、正在等待重启（退出码 %d）", st.ExitCode)
			}
			if reason == "" {
				continue
			}
			onLine(fmt.Sprintf("容器 %s %s，最后 %d 行日志：", c.Service, reason, startLogLines))
			// stdout 和 stderr 是分开读的，按时间排好再输出。
			var lines []docker.LogLine
			var mu sync.Mutex
			_ = m.docker.Logs(ctx, c, startLogLines, false, func(l docker.LogLine) {
				mu.Lock()
				lines = append(lines, l)
				mu.Unlock()
			})
			mu.Lock()
			sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time.Before(lines[j].Time) })
			for _, l := range lines {
				onLine("  " + l.Text)
			}
			mu.Unlock()
			next := "看容器的日志找原因，修改配置后重新部署"
			if hasPrevious {
				next += "，或者在版本列表里回滚到之前的版本"
			}
			return fmt.Errorf("容器 %s %s，部署算作失败。%s", c.Service, reason, next)
		}
		if !time.Now().Before(deadline) {
			m.warnAnonymousVolumes(ctx, instances, onLine)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// warnAnonymousVolumes 提示数据写在匿名卷里的容器：镜像声明了数据目录（VOLUME），配置里却没有把它设为持久化目录，
// Docker 自动建了一个卷放在别处。这些数据不在容器目录里，快照、备份、迁移和顶替都不包含它们。
func (m *Manager) warnAnonymousVolumes(ctx context.Context, instances []docker.Instance, onLine func(string)) {
	for _, c := range instances {
		dirs, err := m.docker.AnonymousVolumes(ctx, c.ID)
		if err != nil || len(dirs) == 0 {
			continue
		}
		onLine(fmt.Sprintf("注意：容器 %s 的 %s 是镜像声明的数据目录，没有设为持久化目录，数据在 Docker 的匿名卷里，"+
			"快照、备份、迁移和顶替都不包含它。在配置的「持久化目录」里加上 %s（compose 里挂到 ./volumes/ 下）后重新部署。",
			c.Service, strings.Join(dirs, "、"), strings.Join(dirs, "、")))
	}
}

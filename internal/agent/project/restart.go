package project

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

// 节点代管重启：设置了顶替的项目，容器的重启策略一律是 no，Docker 开机时不会自己把它们拉起来，
// 免得机器重启后和备用节点同时运行。持有租约的节点按用户原来的重启策略代为重启退出的容器（带退避）。

// restartPolicy 是用户在 compose 里写的（或默认的）重启策略。
type restartPolicy struct {
	mode       string // no、always、unless-stopped、on-failure
	maxRetries int    // on-failure 的次数上限，0 表示不限
}

// policyOf 读出服务的重启策略。没写时默认 unless-stopped（见 buildOverride）。
func policyOf(svc types.ServiceConfig) restartPolicy {
	if svc.Restart != "" {
		mode, n, _ := strings.Cut(svc.Restart, ":")
		max, _ := strconv.Atoi(n)
		return restartPolicy{mode: mode, maxRetries: max}
	}
	if svc.Deploy != nil && svc.Deploy.RestartPolicy != nil {
		rp := svc.Deploy.RestartPolicy
		max := 0
		if rp.MaxAttempts != nil {
			max = int(*rp.MaxAttempts)
		}
		switch rp.Condition {
		case "none":
			return restartPolicy{mode: "no"}
		case "on-failure":
			return restartPolicy{mode: "on-failure", maxRetries: max}
		default:
			return restartPolicy{mode: "always", maxRetries: max}
		}
	}
	return restartPolicy{mode: "unless-stopped"}
}

// policies 返回项目每个服务的重启策略，键是"容器/服务"。按各容器部署的版本（没部署过时用最新版本）读。
func (m *Manager) policies(ctx context.Context, id string) (map[string]restartPolicy, error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	containers, err := pdb.q.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	current := pdb.settings(ctx).GetSharedEnv()
	p := paths{root: row.Dir}
	out := map[string]restartPolicy{}
	for _, mem := range containers {
		cfg, err := pdb.current(ctx, mem)
		if err != nil {
			continue
		}
		shared := current
		if v, err := pdb.deployed(ctx, mem); err == nil && v != nil {
			shared = unmarshalEnv(v.SharedEnv)
		}
		cp, err := loadContainer(ctx, cfg, mem.Name, shared, p.container(mem.Name).dataDir(), composeProjectName(id, mem.Name))
		if err != nil {
			continue
		}
		for name, svc := range cp.Services {
			out[mem.Name+"/"+name] = policyOf(svc)
		}
	}
	return out, nil
}

// SetManagedRestart 修改正在运行的容器的重启策略（不重启容器）：managed 时改成 no，否则改回用户原来的策略。
// 以后部署时由 ManagedRestart 决定写进叠加配置的策略。
func (m *Manager) SetManagedRestart(ctx context.Context, id string, managed bool) error {
	pol, err := m.policies(ctx, id)
	if err != nil {
		return err
	}
	containers, err := m.docker.Instances(ctx, projectLabel(id))
	if err != nil {
		return err
	}
	for _, c := range containers {
		p := restartPolicy{mode: "no"}
		if !managed {
			p = pol[c.Container+"/"+c.Service]
		}
		if err := m.docker.SetRestartPolicy(ctx, c.ID, p.mode, p.maxRetries); err != nil {
			return err
		}
	}
	return nil
}

// restartState 记录节点代为重启某个容器的情况，用于退避。
type restartState struct {
	count   int       // 连续重启的次数
	next    time.Time // 下次允许重启的时间
	retries int       // on-failure 已经重启的次数
}

type managedRestarts struct {
	mu    sync.Mutex
	state map[string]*restartState // 容器 ID → 状态
	total map[string]int           // 容器 ID → 节点代为重启的总次数（计入监控的重启次数）
}

// ManagedRestartCount 返回节点代为重启某个容器的次数。
func (m *Manager) ManagedRestartCount(containerID string) int {
	m.restarts.mu.Lock()
	defer m.restarts.mu.Unlock()
	return m.restarts.total[containerID]
}

// Supervise 按用户原来的重启策略重启项目里退出的容器。由持有租约的节点定期调用。
// 项目正在部署等操作时跳过这一轮。
func (m *Manager) Supervise(ctx context.Context, id string) {
	l := m.lock(id)
	if !l.TryLock() {
		return
	}
	defer l.Unlock()
	containers, err := m.docker.Instances(ctx, projectLabel(id))
	if err != nil {
		return
	}
	var pol map[string]restartPolicy
	now := time.Now()
	r := &m.restarts
	for _, c := range containers {
		if c.State == "running" || c.State == "restarting" || c.State == "paused" {
			r.mu.Lock()
			if st := r.state[c.ID]; st != nil && now.After(st.next.Add(time.Minute)) {
				st.count = 0 // 稳定运行了一段时间，退避重新开始
			}
			r.mu.Unlock()
			continue
		}
		if c.State != "exited" {
			continue // created（从未启动）、dead 等不处理
		}
		if pol == nil {
			if pol, err = m.policies(ctx, id); err != nil {
				return
			}
		}
		p := pol[c.Container+"/"+c.Service]
		code, _, err := m.docker.ExitInfo(ctx, c.ID)
		if err != nil {
			continue
		}
		r.mu.Lock()
		if r.state == nil {
			r.state, r.total = map[string]*restartState{}, map[string]int{}
		}
		st := r.state[c.ID]
		if st == nil {
			st = &restartState{}
			r.state[c.ID] = st
		}
		ok := now.After(st.next)
		switch p.mode {
		case "always", "unless-stopped":
		case "on-failure":
			ok = ok && code != 0 && (p.maxRetries == 0 || st.retries < p.maxRetries)
		default:
			ok = false
		}
		if ok {
			st.count++
			if p.mode == "on-failure" {
				st.retries++
			}
			st.next = now.Add(min(time.Duration(1<<min(st.count, 6))*time.Second, time.Minute))
			r.total[c.ID]++
		}
		r.mu.Unlock()
		if ok {
			_ = m.docker.StartContainer(ctx, c.ID)
		}
	}
}

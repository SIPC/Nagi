package projects

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
)

// ---- 快照（按容器，在容器所在的节点上） ----

// Snapshots 列出容器的快照。
func (s *Service) Snapshots(ctx context.Context, projectID, name string) ([]*nagiv1.Snapshot, error) {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	agent, err := s.agent(ctx, c.NodeID)
	if err != nil {
		return nil, err
	}
	resp, err := agent.ListSnapshots(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: name})
	if err != nil {
		return nil, err
	}
	return resp.GetSnapshots(), nil
}

// CreateSnapshot 给容器做一份手动快照；name 为空时给项目里所有部署过的容器各做一份。
func (s *Service) CreateSnapshot(ctx context.Context, projectID, name string, out Output) error {
	ctx = context.WithoutCancel(ctx)
	var targets []store.Container
	if name != "" {
		c, err := s.container(ctx, projectID, name)
		if err != nil {
			return err
		}
		targets = append(targets, c)
	} else {
		rows, err := s.Q.ListContainers(ctx, projectID)
		if err != nil {
			return err
		}
		for _, c := range rows {
			if c.DeployedVersion > 0 {
				targets = append(targets, c)
			}
		}
		if len(targets) == 0 {
			return invalid("项目还没有部署过的容器")
		}
	}
	var failed i18n.List
	for _, c := range targets {
		if len(targets) > 1 {
			out(i18n.T(ctx, "==== 容器 %s ====", c.Name))
		}
		err := func() error {
			agent, err := s.agent(ctx, c.NodeID)
			if err != nil {
				return err
			}
			stream, err := agent.CreateSnapshot(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: c.Name})
			if err != nil {
				return err
			}
			_, err = recvResult(stream.Recv, out)
			return err
		}()
		if err != nil {
			if len(targets) == 1 {
				return err
			}
			out(i18n.T(ctx, "容器 %s 快照失败：%v", c.Name, err))
			failed = append(failed, c.Name)
		}
	}
	if len(failed) > 0 {
		return i18n.Errorf("这些容器快照失败：%s", failed)
	}
	return nil
}

// RestoreSnapshot 从定时或手动快照恢复容器的 data/ 和 volumes/。
func (s *Service) RestoreSnapshot(ctx context.Context, projectID, name, snapshotID string, out Output) error {
	ctx = context.WithoutCancel(ctx)
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return err
	}
	agent, err := s.agent(ctx, c.NodeID)
	if err != nil {
		return err
	}
	stream, err := agent.RestoreSnapshot(ctx, &nagiv1.RestoreSnapshotRequest{ProjectId: projectID, Container: name, SnapshotId: snapshotID})
	if err != nil {
		return err
	}
	_, err = recvResult(stream.Recv, out)
	return err
}

// Logs 打开容器日志的流。
func (s *Service) Logs(ctx context.Context, projectID, name, service string, tail int32, follow bool) (nagiv1.AgentService_StreamLogsClient, error) {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	agent, err := s.agent(ctx, c.NodeID)
	if err != nil {
		return nil, err
	}
	return agent.StreamLogs(ctx, &nagiv1.StreamLogsRequest{ProjectId: projectID, Container: name, Service: service, Tail: tail, Follow: follow})
}

// CheckImages 立即检查项目里各容器的镜像有没有新版本。不在线的节点跳过。
func (s *Service) CheckImages(ctx context.Context, projectID string) ([]*nagiv1.ImageUpdate, error) {
	rows, err := s.Q.ListContainers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var out []*nagiv1.ImageUpdate
	for _, c := range rows {
		if c.DeployedVersion == 0 {
			continue
		}
		agent, err := s.agent(ctx, c.NodeID)
		if err != nil {
			continue
		}
		resp, err := agent.CheckImages(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: c.Name})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetUpdates()...)
	}
	return out, nil
}

// ---- 私有仓库认证 ----

// dockerAuths 是 docker 的 config.json 里的 auths。
type dockerAuths struct {
	Auths map[string]json.RawMessage `json:"auths"`
}

// Registries 列出项目登录过的仓库。
func (s *Service) Registries(ctx context.Context, projectID string) ([]string, error) {
	p, err := s.project(ctx, projectID)
	if err != nil {
		return nil, err
	}
	cfg, err := s.dockerConfig(p)
	if err != nil || len(cfg) == 0 {
		return nil, err
	}
	var a dockerAuths
	if json.Unmarshal(cfg, &a) != nil {
		return nil, nil
	}
	out := make([]string, 0, len(a.Auths))
	for k := range a.Auths {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// LoginRegistry 登录私有仓库：由一个在线节点向仓库验证账号（优先用项目容器所在的节点），登录信息存在面板上，
// 推给项目所有容器所在的节点。
func (s *Service) LoginRegistry(ctx context.Context, projectID, registry, username, password string) ([]string, error) {
	l := s.lock(projectID)
	l.Lock()
	p, err := s.project(ctx, projectID)
	if err != nil {
		l.Unlock()
		return nil, err
	}
	agent, err := s.anyAgent(ctx, projectID)
	if err != nil {
		l.Unlock()
		return nil, err
	}
	resp, err := agent.LoginRegistry(ctx, &nagiv1.LoginRegistryRequest{Registry: registry, Username: username, Password: password})
	if err != nil {
		l.Unlock()
		return nil, err
	}
	merged, err := s.mergeDocker(p, resp.GetDockerConfig(), "")
	if err == nil {
		err = s.saveDocker(ctx, projectID, merged)
	}
	l.Unlock()
	if err != nil {
		return nil, err
	}
	s.pushProject(ctx, projectID)
	return s.Registries(ctx, projectID)
}

// LogoutRegistry 删除项目某个仓库的登录信息。
func (s *Service) LogoutRegistry(ctx context.Context, projectID, registry string) ([]string, error) {
	l := s.lock(projectID)
	l.Lock()
	p, err := s.project(ctx, projectID)
	if err != nil {
		l.Unlock()
		return nil, err
	}
	merged, err := s.mergeDocker(p, nil, registry)
	if err == nil {
		err = s.saveDocker(ctx, projectID, merged)
	}
	l.Unlock()
	if err != nil {
		return nil, err
	}
	s.pushProject(ctx, projectID)
	return s.Registries(ctx, projectID)
}

// mergeDocker 把新的登录信息合并进项目的 docker 配置，或者删除一个仓库的登录信息。
func (s *Service) mergeDocker(p store.Project, add []byte, remove string) ([]byte, error) {
	cfg, err := s.dockerConfig(p)
	if err != nil {
		return nil, err
	}
	cur := dockerAuths{Auths: map[string]json.RawMessage{}}
	if len(cfg) > 0 {
		_ = json.Unmarshal(cfg, &cur)
		if cur.Auths == nil {
			cur.Auths = map[string]json.RawMessage{}
		}
	}
	if len(add) > 0 {
		var n dockerAuths
		if err := json.Unmarshal(add, &n); err != nil || len(n.Auths) == 0 {
			return nil, i18n.New("节点返回的登录信息无法识别")
		}
		for k, v := range n.Auths {
			cur.Auths[k] = v
		}
	}
	if remove != "" {
		delete(cur.Auths, remove)
	}
	if len(cur.Auths) == 0 {
		return nil, nil
	}
	return json.Marshal(cur)
}

func (s *Service) saveDocker(ctx context.Context, projectID string, cfg []byte) error {
	var sealed []byte
	if len(cfg) > 0 {
		var err error
		if sealed, err = s.Sealer.Seal(cfg, []byte(dockerCtx(projectID))); err != nil {
			return err
		}
	}
	return s.Q.SetProjectDockerConfig(ctx, store.SetProjectDockerConfigParams{DockerConfig: sealed, ID: projectID})
}

// anyAgent 返回一个在线节点：优先用项目容器所在的节点。
func (s *Service) anyAgent(ctx context.Context, projectID string) (nagiv1.AgentServiceClient, error) {
	if rows, err := s.Q.ListContainers(ctx, projectID); err == nil {
		for _, c := range rows {
			if conn, err := s.Nodes.Get(c.NodeID); err == nil {
				return conn.Agent, nil
			}
		}
	}
	for _, c := range s.Nodes.Conns() {
		if !c.Pending() {
			return c.Agent, nil
		}
	}
	return nil, nodes.ErrOffline
}

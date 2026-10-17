package project

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 网络（见 docs/agent.md#网络）：
//   - 项目内网 nagi-<项目 ID>：默认开启。单容器成员由 compose 加入（别名是成员名），
//     compose 成员由节点用 network connect 接入，别名是"服务.成员"。
//   - 共享网络 nagi-shared-<名字>：节点上的命名网络，项目在设置里加入；成员由节点接入，
//     别名是"成员.网络名"（单容器）或"服务.成员.网络名"（compose）。
// compose 会把服务原名注册到它加入的每个网络上，所以 compose 成员不能在 compose 里加入这些网络，
// 否则两份 compose 都有 db 时就会撞名。容器重建后由 SyncNetworks 重新接入（节点监听 Docker 事件）。

const sharedNetworkPrefix = "nagi-shared-"

func projectNetwork(id string) string     { return "nagi-" + id }
func sharedNetwork(name string) string    { return sharedNetworkPrefix + name }
func sharedOwner(name string) string      { return "s:" + name }
func sharedNetworkName(net string) string { return strings.TrimPrefix(net, sharedNetworkPrefix) }

// ensureProjectNetwork 建好项目内网（不存在时）。
func (m *Manager) ensureProjectNetwork(ctx context.Context, id string) error {
	subnet, err := m.subnetFor(ctx, projectOwner(id))
	if err != nil {
		return err
	}
	return m.docker.EnsureNetwork(ctx, projectNetwork(id), subnet, map[string]string{LabelProject: id})
}

func (m *Manager) netLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.netLocks[id]
	if !ok {
		l = &sync.Mutex{}
		m.netLocks[id] = l
	}
	return l
}

// SyncNetworks 让项目所有容器的网络连接和项目设置一致：compose 成员接入项目内网（内网开启时），
// 所有成员接入项目加入的共享网络，别名按规则设置；多余的连接断开。单容器成员的项目内网由 compose 管理。
func (m *Manager) SyncNetworks(ctx context.Context, id string) error {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return err
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return err
	}
	settings := pdb.settings(ctx)
	pdb.Close()

	l := m.netLock(id)
	l.Lock()
	defer l.Unlock()
	containers, err := m.docker.Containers(ctx, projectLabel(id))
	if err != nil {
		return err
	}
	intranet := !settings.GetIntranetDisabled()
	if intranet {
		hasCompose := false
		for _, c := range containers {
			hasCompose = hasCompose || !c.Single
		}
		if hasCompose {
			if err := m.ensureProjectNetwork(ctx, id); err != nil {
				return err
			}
		}
	}
	var firstErr error
	for _, c := range containers {
		if c.Member == "" || c.Service == "" {
			continue
		}
		current, err := m.docker.NetworkAliases(ctx, c.ID)
		if err != nil {
			continue // 容器可能刚被删除
		}
		if _, ok := current["host"]; ok || len(current) == 0 {
			continue // host 网络、或者共用别的容器网络（network_mode: service:x）的服务不能再加入其他网络
		}
		single := c.Single
		want := map[string][]string{}
		if intranet && !single {
			want[projectNetwork(id)] = []string{c.Service + "." + c.Member}
		}
		if nn := settings.GetNetworkName(); nn != "" {
			for _, s := range settings.GetSharedNetworks() {
				alias := c.Member + "." + nn
				if !single {
					alias = c.Service + "." + alias
				}
				want[sharedNetwork(s)] = []string{alias}
			}
		}
		// 断开不该连着的（项目内网对单容器成员由 compose 管理，不碰）。
		for net := range current {
			managed := strings.HasPrefix(net, sharedNetworkPrefix) || (net == projectNetwork(id) && !single)
			if _, ok := want[net]; managed && !ok {
				if err := m.docker.Disconnect(ctx, net, c.ID); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
		for net, aliases := range want {
			if have, ok := current[net]; ok {
				if containsAll(have, aliases) {
					continue
				}
				_ = m.docker.Disconnect(ctx, net, c.ID)
			}
			if err := m.docker.Connect(ctx, net, c.ID, aliases); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("把 %s/%s 接入网络 %s: %w", c.Member, c.Service, net, err)
			}
		}
	}
	return firstErr
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// SyncAllNetworks 对本节点所有项目执行 SyncNetworks。节点在容器有变化（例如重建）时调用。
func (m *Manager) SyncAllNetworks(ctx context.Context) {
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return
	}
	for _, r := range rows {
		_ = m.SyncNetworks(ctx, r.ID)
	}
}

// ---- 共享网络 ----

// SharedNetworks 列出本节点的共享网络，以及加入了它们的项目。
func (m *Manager) SharedNetworks(ctx context.Context) ([]*nagiv1.SharedNetwork, error) {
	rows, err := m.nodeQ.ListSharedNetworks(ctx)
	if err != nil {
		return nil, err
	}
	joined := m.sharedMembers(ctx)
	out := make([]*nagiv1.SharedNetwork, 0, len(rows))
	for _, r := range rows {
		n := &nagiv1.SharedNetwork{Name: r.Name, Projects: joined[r.Name]}
		n.Subnet, _ = m.nodeQ.GetSubnet(ctx, sharedOwner(r.Name))
		out = append(out, n)
	}
	return out, nil
}

// sharedMembers 返回每个共享网络里加入的项目。
func (m *Manager) sharedMembers(ctx context.Context) map[string][]*nagiv1.SharedNetworkProject {
	out := map[string][]*nagiv1.SharedNetworkProject{}
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return out
	}
	for _, r := range rows {
		pdb, err := openProject(ctx, r.Dir)
		if err != nil {
			continue
		}
		s := pdb.settings(ctx)
		name := pdb.meta(ctx, metaName)
		pdb.Close()
		for _, n := range s.GetSharedNetworks() {
			out[n] = append(out[n], &nagiv1.SharedNetworkProject{ProjectId: r.ID, ProjectName: name, NetworkName: s.GetNetworkName()})
		}
	}
	return out
}

// CreateSharedNetwork 在节点上建一个共享网络。
func (m *Manager) CreateSharedNetwork(ctx context.Context, name string) ([]*nagiv1.SharedNetwork, error) {
	name = strings.TrimSpace(name)
	if !nameRe.MatchString(name) {
		return nil, invalid("网络名只能用小写字母、数字和 -，不超过 32 个字符")
	}
	rows, err := m.nodeQ.ListSharedNetworks(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Name == name {
			return nil, invalid("共享网络 %s 已经存在", name)
		}
	}
	subnet, err := m.subnetFor(ctx, sharedOwner(name))
	if err != nil {
		return nil, err
	}
	if err := m.docker.EnsureNetwork(ctx, sharedNetwork(name), subnet, map[string]string{"nagi.shared": name}); err != nil {
		_ = m.nodeQ.DeleteSubnet(ctx, sharedOwner(name))
		return nil, err
	}
	if err := m.nodeQ.InsertSharedNetwork(ctx, nodestore.InsertSharedNetworkParams{Name: name, CreatedAt: time.Now().Unix()}); err != nil {
		return nil, err
	}
	return m.SharedNetworks(ctx)
}

// DeleteSharedNetwork 删除共享网络。还有项目加入时拒绝。
func (m *Manager) DeleteSharedNetwork(ctx context.Context, name string) ([]*nagiv1.SharedNetwork, error) {
	if p := m.sharedMembers(ctx)[name]; len(p) > 0 {
		return nil, invalid("还有 %d 个项目加入了共享网络 %s（例如 %s），请先在项目设置里退出", len(p), name, p[0].GetProjectName())
	}
	if err := m.docker.RemoveNetwork(ctx, sharedNetwork(name)); err != nil {
		return nil, err
	}
	if err := m.nodeQ.DeleteSharedNetwork(ctx, name); err != nil {
		return nil, err
	}
	_ = m.nodeQ.DeleteSubnet(ctx, sharedOwner(name))
	return m.SharedNetworks(ctx)
}

// checkSharedSettings 检查项目加入共享网络的设置：网络存在、网络名格式正确且本节点唯一。
func (m *Manager) checkSharedSettings(ctx context.Context, id string, s *nagiv1.ProjectSettings) error {
	if len(s.GetSharedNetworks()) == 0 {
		return nil
	}
	if !nameRe.MatchString(s.GetNetworkName()) {
		return invalid("加入共享网络需要设置网络名（小写字母、数字和 -，不超过 32 个字符）")
	}
	rows, err := m.nodeQ.ListSharedNetworks(ctx)
	if err != nil {
		return err
	}
	exists := map[string]bool{}
	for _, r := range rows {
		exists[r.Name] = true
	}
	for _, n := range s.GetSharedNetworks() {
		if !exists[n] {
			return invalid("本节点没有共享网络 %s", n)
		}
	}
	projects, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return err
	}
	for _, r := range projects {
		if r.ID == id {
			continue
		}
		pdb, err := openProject(ctx, r.Dir)
		if err != nil {
			continue
		}
		other := pdb.settings(ctx)
		name := pdb.meta(ctx, metaName)
		pdb.Close()
		if other.GetNetworkName() == s.GetNetworkName() && len(other.GetSharedNetworks()) > 0 {
			return invalid("网络名 %s 已经被项目 %s 使用", s.GetNetworkName(), name)
		}
	}
	return nil
}

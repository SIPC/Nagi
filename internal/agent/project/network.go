package project

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"
)

// 项目内网（见 docs/agent.md#项目内网）：节点为每个在本节点上有容器的项目建一个 Docker 网络 nagi-<项目 ID>。
//   - 单个容器由 compose 加入（别名是容器名）；
//   - 容器组由节点用 network connect 接入，别名是"服务.容器组"。compose 会把服务原名注册到它加入的每个网络上，
//     所以容器组不能在 compose 里加入项目内网，否则两份 compose 都有 db 时就会撞名。
// 容器重建后由 SyncNetworks 重新接入（节点监听 Docker 事件）。

func projectNetwork(id string) string { return "nagi-" + id }

var netLocks sync.Map // 项目 ID → *sync.Mutex

func netLock(id string) *sync.Mutex {
	l, _ := netLocks.LoadOrStore(id, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// ProjectNetwork 返回项目内网的 Docker 网络名。
func ProjectNetwork(id string) string { return projectNetwork(id) }

// EnsureProjectNetwork 建好项目内网（不存在时）。放代理容器前调用。
func (m *Manager) EnsureProjectNetwork(ctx context.Context, id string) error {
	return m.ensureProjectNetwork(ctx, id)
}

// ensureProjectNetwork 建好项目内网（不存在时）。
func (m *Manager) ensureProjectNetwork(ctx context.Context, id string) error {
	subnet, err := m.subnetFor(ctx, projectOwner(id), projectSubnetBits)
	if err != nil {
		return err
	}
	return m.docker.EnsureNetwork(ctx, projectNetwork(id), subnet, map[string]string{LabelProject: id})
}

// SyncNetworks 把项目里容器组的容器接入项目内网（别名"服务.容器组"）。单个容器的项目内网由 compose 管理。
func (m *Manager) SyncNetworks(ctx context.Context, id string) error {
	l := netLock(id)
	l.Lock()
	defer l.Unlock()
	instances, err := m.docker.Instances(ctx, projectLabel(id))
	if err != nil {
		return err
	}
	hasGroup := false
	for _, c := range instances {
		hasGroup = hasGroup || !c.Single
	}
	if !hasGroup {
		return nil
	}
	if err := m.ensureProjectNetwork(ctx, id); err != nil {
		return err
	}
	var firstErr error
	for _, c := range instances {
		if c.Single || c.Container == "" || c.Service == "" {
			continue
		}
		current, err := m.docker.NetworkAliases(ctx, c.ID)
		if err != nil {
			continue // 容器可能刚被删除
		}
		if _, ok := current["host"]; ok || len(current) == 0 {
			continue // host 网络、或者共用别的容器网络（network_mode: service:x）的服务不能再加入其他网络
		}
		alias := c.Service + "." + c.Container
		if have, ok := current[projectNetwork(id)]; ok {
			if slices.Contains(have, alias) {
				continue
			}
			_ = m.docker.Disconnect(ctx, projectNetwork(id), c.ID)
		}
		if err := m.docker.Connect(ctx, projectNetwork(id), c.ID, []string{alias}); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("把 %s/%s 接入项目内网: %w", c.Container, c.Service, err)
		}
	}
	return firstErr
}

// Endpoint 是本节点上项目内网里一个正在运行的容器：名字、地址、声明的端口。
type Endpoint struct {
	IP       netip.Addr
	Ports    []uint32
	UDPPorts []uint32
}

// FindEndpoint 找本节点上项目 projectID 里名字是 name 的正在运行的容器（单个容器是"容器名"，容器组里的是"服务.容器组"）。
func (m *Manager) FindEndpoint(ctx context.Context, projectID, name string) (Endpoint, bool) {
	instances, err := m.docker.Instances(ctx, projectLabel(projectID))
	if err != nil {
		return Endpoint{}, false
	}
	for _, c := range instances {
		alias := c.Container
		if !c.Single {
			alias = c.Service + "." + c.Container
		}
		if alias != name || c.State != "running" {
			continue
		}
		ip, ok := c.IPs[projectNetwork(projectID)]
		if !ok {
			for _, a := range c.IPs {
				ip, ok = a, true
				break
			}
		}
		if !ok {
			continue
		}
		return Endpoint{IP: ip, Ports: ports32(c.Ports), UDPPorts: ports32(c.UDPPorts)}, true
	}
	return Endpoint{}, false
}

func ports32(in []uint16) []uint32 {
	var out []uint32
	for _, p := range in {
		out = append(out, uint32(p))
	}
	return out
}

// SyncAllNetworks 对本节点上的所有项目执行 SyncNetworks。节点在容器有变化（例如重建）时调用。
func (m *Manager) SyncAllNetworks(ctx context.Context) {
	seen := map[string]bool{}
	for _, c := range m.List(ctx) {
		id := c.Spec.GetProjectId()
		if !seen[id] {
			seen[id] = true
			_ = m.SyncNetworks(ctx, id)
		}
	}
}

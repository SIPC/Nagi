package project

import (
	"context"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 跨节点连接共享网络（见 docs/agent.md#跨节点连接共享网络）：这里管连接关系（存在节点数据库里）
// 和本节点网络里的容器；代理容器、容器列表的同步和连接转发由节点（agent 包）负责。

// LinkPeer 是和本节点某个共享网络相连的节点。
type LinkPeer struct {
	Key    []byte
	Name   string
	NodeID string
}

// SharedNetworkLinks 返回每个共享网络相连的节点。
func (m *Manager) SharedNetworkLinks(ctx context.Context) (map[string][]LinkPeer, error) {
	rows, err := m.nodeQ.ListSharedNetworkLinks(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]LinkPeer{}
	for _, r := range rows {
		out[r.Network] = append(out[r.Network], LinkPeer{Key: r.PeerKey, Name: r.PeerName, NodeID: r.PeerNode})
	}
	return out, nil
}

// SetSharedNetworkPeers 整个替换和共享网络 name 相连的节点。本节点没有这个网络时先建。
func (m *Manager) SetSharedNetworkPeers(ctx context.Context, name string, peers []LinkPeer) error {
	exists, err := m.sharedNetworkExists(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		if len(peers) == 0 {
			return nil
		}
		if _, err := m.CreateSharedNetwork(ctx, name); err != nil {
			return err
		}
	}
	tx, err := m.nodeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := m.nodeQ.WithTx(tx)
	if err := q.DeleteSharedNetworkLinks(ctx, name); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range peers {
		if len(p.Key) != 32 || seen[string(p.Key)] {
			continue
		}
		seen[string(p.Key)] = true
		if err := q.InsertSharedNetworkLink(ctx, nodestore.InsertSharedNetworkLinkParams{
			Network: name, PeerKey: p.Key, PeerName: p.Name, PeerNode: p.NodeID,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *Manager) sharedNetworkExists(ctx context.Context, name string) (bool, error) {
	rows, err := m.nodeQ.ListSharedNetworks(ctx)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// SharedNetworkProjects 返回加入了共享网络 name 的项目（不管是否在运行）。
func (m *Manager) SharedNetworkProjects(ctx context.Context, name string) []*nagiv1.SharedNetworkProject {
	return m.sharedContainers(ctx)[name]
}

// LinkEndpoint 是本节点共享网络里的一个容器（一个容器）。
type LinkEndpoint struct {
	Alias       string
	ProjectID   string
	ProjectName string
	IP          netip.Addr
	Ports       []uint32
}

// LinkEndpoints 返回本节点共享网络 name 里正在运行的容器：名字、地址和开着的端口
// （容器声明的端口，加上项目设置里补的跨节点端口）。
func (m *Manager) LinkEndpoints(ctx context.Context, name string) ([]LinkEndpoint, error) {
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []LinkEndpoint
	for _, r := range rows {
		pdb, err := openProject(ctx, r.Dir)
		if err != nil {
			continue
		}
		s := pdb.settings(ctx)
		pname := pdb.meta(ctx, metaName)
		pdb.Close()
		nn := s.GetNetworkName()
		if nn == "" || !slices.Contains(s.GetSharedNetworks(), name) {
			continue
		}
		extra := linkPortMap(s.GetLinkPorts())
		containers, err := m.docker.Instances(ctx, projectLabel(r.ID))
		if err != nil {
			return nil, err
		}
		for _, c := range containers {
			ip, ok := c.IPs[sharedNetwork(name)]
			if c.State != "running" || !ok || c.Container == "" || c.Service == "" {
				continue
			}
			key := c.Container
			if !c.Single {
				key = c.Service + "." + c.Container
			}
			var ports []uint32
			for _, p := range c.Ports {
				ports = append(ports, uint32(p))
			}
			for _, p := range extra[key] {
				if !slices.Contains(ports, p) {
					ports = append(ports, p)
				}
			}
			slices.Sort(ports)
			out = append(out, LinkEndpoint{Alias: key + "." + nn, ProjectID: r.ID, ProjectName: pname, IP: ip, Ports: ports})
		}
	}
	return out, nil
}

var linkPortRe = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)?):([0-9]{1,5})$`)

// cleanLinkPorts 检查并整理项目设置里的跨节点端口（"容器:端口"或"服务.容器组:端口"）。
func cleanLinkPorts(in []string) ([]string, error) {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		m := linkPortRe.FindStringSubmatch(s)
		if m == nil {
			return nil, invalid("跨节点端口 %q 的格式不对，应该是\"容器:端口\"或\"服务.容器组:端口\"", s)
		}
		if p, _ := strconv.Atoi(m[3]); p < 1 || p > 65535 {
			return nil, invalid("跨节点端口 %q 的端口号不对", s)
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

func linkPortMap(list []string) map[string][]uint32 {
	out := map[string][]uint32{}
	for _, s := range list {
		if m := linkPortRe.FindStringSubmatch(s); m != nil {
			p, _ := strconv.Atoi(m[3])
			out[m[1]] = append(out[m[1]], uint32(p))
		}
	}
	return out
}

// checkRemoteNames 检查网络名在相连的节点上有没有被别的项目使用。strict 时连不上相连的节点也算失败。
func (m *Manager) checkRemoteNames(ctx context.Context, id string, networkName string, networks []string, strict bool) error {
	if m.RemoteProjects == nil || networkName == "" {
		return nil
	}
	for _, n := range networks {
		list, err := m.RemoteProjects(ctx, n)
		if err != nil && strict {
			return invalid("共享网络 %s 和其他节点相连，但%v，暂时检查不了网络名是否重复", n, err)
		}
		for _, p := range list {
			if p.GetProjectId() != id && p.GetNetworkName() == networkName {
				return invalid("网络名 %s 已经被相连节点上的项目 %s 使用（共享网络 %s）", networkName, p.GetProjectName(), n)
			}
		}
	}
	return nil
}

// linkedNetworkError 是删除仍和其他节点相连的共享网络时的错误。
func linkedNetworkError(name string, peers []LinkPeer) error {
	names := make([]string, 0, len(peers))
	for _, p := range peers {
		names = append(names, p.Name)
	}
	return invalid("共享网络 %s 还和这些节点相连：%s，请先断开连接", name, strings.Join(names, "、"))
}

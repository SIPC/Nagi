package projects

import (
	"bytes"
	"context"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 从节点缓存拼回的顶替设置和备份节点（见 docs/panel.md#项目数据）：先记成待确认，管理员确认后才生效。
// 它们会让另一个节点登记接收这个容器的副本、在顶替时以缓存里的配置运行它；只凭一个节点的缓存就生效的话，
// 被攻破的节点可以伪造一个带特权 compose 和顶替设置的项目，借顶替在别的节点上以 root 运行它。
// 容器本身照常拼回：它只运行在上报的节点上，不会影响别的节点。

// pendingHAOf 返回容器待确认的设置，没有时为 nil。
func pendingHAOf(c store.Container) *nagiv1.PendingHA {
	if len(c.PendingHa) == 0 {
		return nil
	}
	p := &nagiv1.PendingHA{}
	if proto.Unmarshal(c.PendingHa, p) != nil {
		return nil
	}
	return p
}

// rebuiltHA 从节点缓存里取出可以待确认的设置：备份节点和顶替设置里的节点都要是面板上登记过的，
// 顶替的一方必须是上报的节点，两方的公钥要和登记的一致。对不上的直接丢掉。都没有时返回 nil。
func (s *Service) rebuiltHA(ctx context.Context, nodeID string, sp *nagiv1.ContainerSpec, fo *nagiv1.FailoverStatus) *nagiv1.PendingHA {
	p := &nagiv1.PendingHA{}
	if t := sp.GetBackup(); t != nil && t.GetNodeId() != nodeID {
		if _, err := s.Q.GetNode(ctx, t.GetNodeId()); err == nil {
			p.BackupNodeId = t.GetNodeId()
		}
	}
	if cfg := fo.GetConfig(); cfg != nil && s.failoverMatches(ctx, nodeID, cfg) {
		p.Failover, p.FailoverEpoch = cfg, fo.GetEpoch()
	}
	if p.GetBackupNodeId() == "" && p.GetFailover() == nil {
		return nil
	}
	p.Privileged, _ = spec.PrivilegeReasons(sp.GetConfig(), sp.GetName())
	return p
}

// failoverMatches 核对顶替设置：A、B 是两个不同的、登记过的节点，其中一个是 nodeID，公钥和登记的 mesh 公钥一致。
func (s *Service) failoverMatches(ctx context.Context, nodeID string, cfg *nagiv1.FailoverConfig) bool {
	a, b := cfg.GetA(), cfg.GetB()
	if a.GetNodeId() == "" || a.GetNodeId() == b.GetNodeId() || (a.GetNodeId() != nodeID && b.GetNodeId() != nodeID) {
		return false
	}
	for _, p := range []*nagiv1.FailoverParticipant{a, b} {
		n, err := s.Q.GetNode(ctx, p.GetNodeId())
		if err != nil || len(n.MeshKey) == 0 || !bytes.Equal(n.MeshKey, p.GetKey()) {
			return false
		}
	}
	return true
}

// keepUnsetFailover 判断面板上没有顶替设置的容器，节点上的顶替设置要不要先留着（不让节点删掉）：
//   - 项目刚从节点缓存拼回时，容器可能还没从持有者那里拼回顶替设置（例如先从热备拼回的）；
//   - 拼回的顶替设置在等管理员确认：节点上的那份要留到确认，确认后照常运行；放弃以后才删。
func (s *Service) keepUnsetFailover(ctx context.Context, c store.Container) bool {
	if pendingHAOf(c).GetFailover() != nil {
		return true
	}
	p, err := s.project(ctx, c.ProjectID)
	return err == nil && recentlyRebuilt(p)
}

// ConfirmPendingHA 管理员确认（或放弃）从节点缓存拼回的设置。确认时按当前的登记重新核对一遍。
func (s *Service) ConfirmPendingHA(ctx context.Context, projectID, name string, discard bool) error {
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return err
	}
	p := pendingHAOf(c)
	if p == nil {
		return invalid("容器 %s 没有待确认的顶替或备份设置", name)
	}
	if !discard {
		if len(c.Failover) > 0 || c.BackupNodeID != "" {
			return invalid("容器 %s 已经设置了顶替或备份节点，请先放弃待确认的设置", name)
		}
		if f := p.GetFailover(); f != nil {
			if !s.failoverMatches(ctx, c.NodeID, f) {
				return invalid("顶替设置里的节点和面板上登记的不一致，不能恢复，请放弃后重新设置")
			}
			raw, err := proto.Marshal(f)
			if err != nil {
				return err
			}
			if err := s.Q.SetContainerFailover(ctx, store.SetContainerFailoverParams{Failover: raw, FailoverEpoch: int64(p.GetFailoverEpoch()), ProjectID: projectID, Name: name}); err != nil {
				return err
			}
			// 见证人还是拼回之前的旧面板：换成本面板（在锁外进行；节点下次上报时也会触发）。
			defer s.adoptWitness(c, f)
		} else if id := p.GetBackupNodeId(); id != "" {
			if id == c.NodeID {
				return invalid("备份节点不能是容器所在的节点")
			}
			if _, err := s.Q.GetNode(ctx, id); err != nil {
				return invalid("备份节点已被移除，请放弃后重新设置")
			}
			if err := s.Q.SetContainerBackupNode(ctx, store.SetContainerBackupNodeParams{BackupNodeID: id, ProjectID: projectID, Name: name}); err != nil {
				return err
			}
		}
	}
	if err := s.Q.SetContainerPendingHA(ctx, store.SetContainerPendingHAParams{ProjectID: projectID, Name: name}); err != nil {
		return err
	}
	if !discard {
		_ = s.push(context.WithoutCancel(ctx), projectID, name)
		s.refreshLinks()
	}
	return nil
}

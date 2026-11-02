package projects

import (
	"context"
	"sort"

	"github.com/SIPC/nagi/internal/server/store"
)

// UpgradeOrder 排好一键升级节点的顺序：先升级只做热备或备份的节点，再升级容器所在的节点。
// 节点之间同步容器时，对方不认识新的目录格式就会拒绝（见 docs/control-plane.md）。
func (s *Service) UpgradeOrder(ctx context.Context, ids []string) ([]string, error) {
	rows, err := s.Q.ListAllContainers(ctx)
	if err != nil {
		return nil, err
	}
	return orderUpgrades(ids, rows), nil
}

func orderUpgrades(ids []string, rows []store.Container) []string {
	hosts, spares := map[string]bool{}, map[string]bool{}
	for _, c := range rows {
		hosts[c.NodeID] = true
		if c.BackupNodeID != "" {
			spares[c.BackupNodeID] = true
		}
		if cfg := failoverOf(c); cfg != nil {
			spares[standbyOf(cfg, c.NodeID).GetNodeId()] = true
		}
	}
	rank := func(id string) int {
		switch {
		case spares[id] && !hosts[id]:
			return 0
		case spares[id]:
			return 1
		case hosts[id]:
			return 2
		}
		return 3
	}
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

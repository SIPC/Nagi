package server

import (
	"context"
	"errors"
	"sync"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/standby"
)

// 主备面板的角色变化（见 docs/standby.md）：备用面板提升为主面板、落后的面板改为跟随新的主面板。
// 角色变了以后 Run 返回 ErrRoleChanged，调用方用同一个数据目录重新 New、Run，按新角色启动。

// ErrRoleChanged 表示面板的角色变了，需要按新角色重新启动。
var ErrRoleChanged = errors.New("面板角色已变化，按新角色重新启动")

type role struct {
	once sync.Once
	ch   chan struct{}
}

func (s *Server) changeRole() {
	s.role.once.Do(func() { close(s.role.ch) })
}

// behind 检查节点上报的任期（节点见过的最大任期和那个任期的主面板）。本面板落后时改为跟随那个主面板：
//   - 主面板：任期比节点见过的小，或者任期一样但节点认的是别的面板（同时提升了两个），降为备用面板；
//   - 备用面板：跟随的不是节点认的主面板，改为跟随它。
//
// 返回 true 表示本面板已经落后，不再处理这次上报。
func (s *Server) behind(ctx context.Context, req *nagiv1.ReportRequest) bool {
	term, primary, address := req.GetTerm(), req.GetPrimary(), req.GetPrimaryAddress()
	if term == 0 || primary == "" || primary == s.fingerprint {
		return false
	}
	own, _ := standby.Term(ctx, s.q)
	if s.standby == nil {
		if term < own {
			return false
		}
	} else if term < own || s.standby.Info.PrimaryFingerprint == primary {
		return false
	}
	if address == "" {
		if sp, err := s.q.GetStandbyPanelByServerFP(ctx, primary); err == nil {
			address = sp.Address
		}
	}
	if address == "" {
		s.log.Warn("节点认的主面板不是本面板，但不知道它的地址", "term", term, "primary", primary)
		return true
	}
	s.role.once.Do(func() {
		if err := standby.Follow(s.cfg.DataDir, address, primary); err != nil {
			s.log.Error("改为跟随新的主面板失败", "err", err)
			return
		}
		if s.standby == nil {
			s.log.Warn("已有新的主面板，本面板降为备用面板：离开期间的修改作废，从新的主面板同步", "term", term, "primary", address)
		} else {
			s.log.Info("改为跟随新的主面板", "term", term, "primary", address)
		}
		close(s.role.ch)
	})
	return true
}

// Promote 把备用面板提升为主面板：停止同步，任期加一，然后按主面板重新启动。
func (s *Server) Promote(ctx context.Context) (uint64, error) {
	if s.standby == nil {
		return 0, errors.New("本面板已经是主面板")
	}
	s.standby.Stop()
	term, err := standby.Promote(ctx, s.q, s.cfg.DataDir, s.fingerprint, s.standby.Info)
	if err != nil {
		// 没提升成：按备用面板重新启动，恢复同步。
		go s.changeRole()
		return 0, err
	}
	s.log.Warn("本面板已提升为主面板", "term", term)
	// 先让接口的回复发出去再重启。
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.changeRole()
	}()
	return term, nil
}

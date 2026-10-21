package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/standby"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 主备面板的角色变化（见 docs/standby.md）：备用面板提升为主面板、落后的面板改为跟随新的主面板。
// 角色变了以后 Run 返回 ErrRoleChanged，调用方用同一个数据目录重新 New、Run，按新角色启动。

// ErrRoleChanged 表示面板的角色变了，需要按新角色重新启动。
var ErrRoleChanged = errors.New("面板角色已变化，按新角色重新启动")

type role struct {
	mu      sync.Mutex
	changed bool
	ch      chan struct{}

	// checking：正在向某个面板核实它是不是主面板（公钥指纹 → 开始的时间），避免每次上报都去问。
	checking sync.Map
}

func (s *Server) changeRole() {
	s.role.mu.Lock()
	defer s.role.mu.Unlock()
	if !s.role.changed {
		s.role.changed = true
		close(s.role.ch)
	}
}

// behind 检查节点上报的任期（节点见过的最大任期和那个任期的主面板）。本面板可能已经落后时，
// 不直接相信节点的说法（节点可能被攻破）：只考虑本组登记过的面板，用数据库里记的地址、钉住它的公钥指纹，
// 去问它自己是不是主面板、任期多少，确认了才改为跟随它：
//   - 主面板：对方的任期比本面板大，或者一样大（同时提升了两个，节点认的是对方），降为备用面板；
//   - 备用面板：跟随的不是对方，改为跟随它。
//
// 返回 true 表示节点认的主面板不是本面板（这次上报不再处理）。
func (s *Server) behind(ctx context.Context, req *nagiv1.ReportRequest) bool {
	term, primary := req.GetTerm(), req.GetPrimary()
	if term == 0 || primary == "" || primary == s.fingerprint {
		return false
	}
	own, _ := standby.Term(ctx, s.q)
	if term < own || (s.standby != nil && s.standby.Info.PrimaryFingerprint == primary) {
		return true // 节点还没收到本面板的任期，或者本面板已经在跟随它认的主面板
	}
	sp, err := s.q.GetStandbyPanelByServerFP(ctx, primary)
	if err != nil {
		s.log.Warn("节点认的主面板不在本组登记的面板里，不跟随", "term", term, "primary", primary)
		return true
	}
	if at, ok := s.role.checking.Load(primary); ok && time.Since(at.(time.Time)) < 30*time.Second {
		return true
	}
	s.role.checking.Store(primary, time.Now())
	go s.verifyAndFollow(sp.Address, primary, own)
	return true
}

// verifyAndFollow 向 address 上的面板（钉住 fingerprint）核实它是主面板、任期不比本面板小，然后改为跟随它。
func (s *Server) verifyAndFollow(address, fingerprint string, own uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := standby.FetchStatus(ctx, address, fingerprint)
	if err != nil {
		s.log.Warn("无法向节点认的主面板核实，暂不跟随", "primary", address, "err", err)
		return
	}
	// 记下核实过的任期（节点上报的任期不算：节点可能被攻破，随便报一个很大的数）。以后提升时任期比它大。
	if err := standby.SeeTerm(ctx, s.q, st.Term); err != nil {
		s.log.Warn("保存见过的任期失败", "err", err)
	}
	if !st.IsPrimary || st.Primary != fingerprint || st.Term < own {
		s.log.Warn("节点认的面板自己说不是主面板（或任期落后），不跟随", "primary", address, "term", st.Term)
		return
	}
	if err := standby.Follow(s.cfg.DataDir, address, fingerprint); err != nil {
		s.log.Error("改为跟随新的主面板失败", "err", err)
		return
	}
	if s.standby == nil {
		s.log.Warn("已有新的主面板，本面板降为备用面板：离开期间的修改作废，从新的主面板同步", "term", st.Term, "primary", address)
	} else {
		s.log.Info("改为跟随新的主面板", "term", st.Term, "primary", address)
	}
	s.changeRole()
}

// handleStatus 提供 GET /api/standby/status：本面板的任期、主面板和是不是主面板。别的面板据此核实谁是主面板。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	term, primary := standby.Term(r.Context(), s.q)
	st := standby.PanelStatus{Term: term, Primary: primary, IsPrimary: s.standby == nil && primary == s.fingerprint}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// Promote 把备用面板提升为主面板：停止同步，任期加一（至少比节点上报里见过的大），然后按主面板重新启动。
func (s *Server) Promote(ctx context.Context) (uint64, error) {
	if s.standby == nil {
		return 0, errors.New("本面板已经是主面板")
	}
	s.standby.Stop()
	term, err := standby.Promote(ctx, s.db, s.q, s.cfg.DataDir, s.fingerprint, s.standby.Info)
	if err != nil {
		// 没提升成（数据库没有改动）：按备用面板重新启动，恢复同步。
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

// watchPrimary：备用面板连不上主面板超过 1 分钟时发告警，重新连上时发恢复通知（见 docs/standby.md#同步）。
// 告警渠道用同步过来的那份配置。
func (s *Server) watchPrimary(ctx context.Context) {
	const after = time.Minute
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	lastOK := time.Now()
	firing := false
	var notified time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st := s.standby.Status()
		now := time.Now()
		if st.Connected {
			lastOK = now
			if firing {
				firing = false
				s.nodes.SendPanelAlert(ctx, &nagiv1.Alert{Key: "standby_sync", Status: "resolved", Title: "备用面板已重新连上主面板",
					Node: "备用面板 " + s.publicAddress, Time: timestamppb.New(now)})
			}
			continue
		}
		if now.Sub(lastOK) < after || (firing && now.Sub(notified) < alert.RepeatInterval) {
			continue
		}
		firing, notified = true, now
		msg := "主面板 " + s.standby.Info.PrimaryAddress
		if st.LastError != "" {
			msg += "：" + st.LastError
		}
		if !st.LastSync.IsZero() {
			msg += "。最近一次同步：" + st.LastSync.Local().Format("2006-01-02 15:04:05")
		}
		s.nodes.SendPanelAlert(ctx, &nagiv1.Alert{Key: "standby_sync", Status: "firing",
			Title:   fmt.Sprintf("备用面板连不上主面板超过 %d 分钟", int(now.Sub(lastOK).Minutes())),
			Message: msg + "。主面板确实坏了时，可以在备用面板上提升为主面板。",
			Node:    "备用面板 " + s.publicAddress, Time: timestamppb.New(now)})
	}
}

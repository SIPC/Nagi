package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
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
	// followMu 让"改为跟随别的主面板"和"提升为主面板"不同时进行；promoting 之后不再跟随别人。
	followMu  sync.Mutex
	promoting bool
	// bumpedAt：上次因为同时提升了两个面板而调高任期的时间。
	bumpedAt time.Time
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
// 不直接相信节点的说法（节点可能被攻破）：只考虑本组登记过的面板，钉住它的公钥指纹，
// 去问它自己是不是主面板、任期多少，确认了才改为跟随它：
//   - 主面板：对方的任期比本面板大，降为备用面板；任期一样大（同时提升了两个）时公钥指纹小的一方留下，
//     留下的一方把任期调高一轮，让认了对方的节点改认它；
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
		if s.standby == nil && slices.Contains(standby.Removed(ctx, s.q), primary) {
			s.alertRemovedPrimary(ctx, req, primary)
		}
		return true
	}
	if term == own && s.standby == nil && s.fingerprint < primary {
		s.bumpTerm(ctx, own)
		return true
	}
	if at, ok := s.role.checking.Load(primary); ok && time.Since(at.(time.Time)) < 30*time.Second {
		return true
	}
	s.role.checking.Store(primary, time.Now())
	// 地址先用数据库里记的；连不上时再试节点报的（那个面板可能改过地址）。都钉住它的公钥指纹，地址不对也连不到别的面板。
	addrs := []string{sp.Address}
	if a := req.GetPrimaryAddress(); a != "" && a != sp.Address {
		addrs = append(addrs, a)
	}
	go s.verifyAndFollow(addrs, primary, own)
	return true
}

// bumpTerm：同时提升了两个面板、本面板留下时，把任期调高一轮（最多 5 分钟一次），节点收到后都改认本面板。
func (s *Server) bumpTerm(ctx context.Context, own uint64) {
	s.role.followMu.Lock()
	defer s.role.followMu.Unlock()
	if time.Since(s.role.bumpedAt) < 5*time.Minute {
		return
	}
	s.role.bumpedAt = time.Now()
	if err := s.q.SetSetting(ctx, store.SetSettingParams{Key: standby.SettingTerm, Value: strconv.FormatUint(own+1, 10)}); err != nil {
		s.log.Warn("调高任期失败", "err", err)
		return
	}
	s.log.Warn("另一个面板和本面板同一任期都是主面板，本面板留下，任期调高一轮", "term", own+1)
}

// verifyAndFollow 向面板（依次试 addrs，钉住 fingerprint）核实它是主面板、任期不比本面板小，然后改为跟随它。
func (s *Server) verifyAndFollow(addrs []string, fingerprint string, own uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var st standby.PanelStatus
	var address string
	var err error
	for _, address = range addrs {
		if st, err = standby.FetchStatus(ctx, address, fingerprint); err == nil {
			break
		}
	}
	if err != nil {
		s.log.Warn("无法向节点认的主面板核实，暂不跟随", "primary", address, "err", err)
		return
	}
	// 记下核实过的任期（节点上报的任期不算：节点可能被攻破，随便报一个很大的数）。以后提升时任期比它大。
	if err := standby.SeeTerm(ctx, s.q, st.Term); err != nil {
		s.log.Warn("保存见过的任期失败", "err", err)
	}
	if !st.IsPrimary || st.Primary != fingerprint || st.Term < own ||
		(st.Term == own && s.standby == nil && s.fingerprint < fingerprint) {
		s.log.Warn("节点认的面板自己说不是主面板（或任期落后），不跟随", "primary", address, "term", st.Term)
		return
	}
	s.role.followMu.Lock()
	defer s.role.followMu.Unlock()
	if s.role.promoting {
		return // 本面板正在提升为主面板
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
		return 0, i18n.New("本面板已经是主面板")
	}
	// 和"改为跟随别的主面板"互斥：已经决定跟随别人（马上按备用面板重启）时不能再提升。
	s.role.followMu.Lock()
	s.role.mu.Lock()
	changing := s.role.changed
	s.role.mu.Unlock()
	if changing {
		s.role.followMu.Unlock()
		return 0, i18n.New("面板正在切换角色，请稍后再试")
	}
	s.role.promoting = true
	s.role.followMu.Unlock()
	s.standby.Stop()
	info := &standby.Info{PrimaryAddress: s.standby.PrimaryAddress(), PrimaryFingerprint: s.standby.Info.PrimaryFingerprint}
	term, err := standby.Promote(ctx, s.db, s.q, s.cfg.DataDir, s.fingerprint, info)
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
		msg := "主面板 " + s.standby.PrimaryAddress()
		if st.LastError != nil {
			msg += "：" + st.LastError.Error()
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

// alertRemovedPrimary：节点认了一个已经移除的备用面板为主面板（它可能自己提升了，节点当时离线没收到移除）。
// 发告警，每个节点每小时最多一次。处理：在节点上移除那个面板（nagi panel remove）。
func (s *Server) alertRemovedPrimary(ctx context.Context, req *nagiv1.ReportRequest, primary string) {
	key := "removed-primary:" + req.GetNode().GetHostname()
	if at, ok := s.role.checking.Load(key); ok && time.Since(at.(time.Time)) < time.Hour {
		return
	}
	s.role.checking.Store(key, time.Now())
	s.nodes.SendPanelAlert(ctx, &nagiv1.Alert{Key: "removed_primary", Status: "firing",
		Title:   "节点认了已移除的面板为主面板",
		Message: "节点 " + req.GetNode().GetHostname() + " 现在只听已经移除的面板 " + req.GetPrimaryAddress() + "（" + primary + "）的。那个面板可能被人拿去自己提升了。请在节点上执行 nagi panel remove 移除它。",
		Node:    req.GetNode().GetHostname(), Time: timestamppb.Now()})
}

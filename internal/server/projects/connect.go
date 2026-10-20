package projects

import (
	"context"
	"slices"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/pkg/spec"
	"github.com/SIPC/nagi/internal/server/store"
)

// 项目连接（见 docs/agent.md#项目连接）：两个项目连上以后，双方的容器用"容器.网络名"访问对方，不传递。
// 实现和跨节点一样用代理，见 links.go。

func linkPair(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}

// groupNames 返回项目里的容器组（最新版本或草稿是 compose 的容器）。
func (s *Service) groupNames(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.Q.ListContainers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		group := false
		if _, cfg, _, err := s.latest(ctx, r); err == nil && !spec.IsSingle(cfg) {
			group = true
		}
		if d, err := s.draft(r); err == nil && d != nil && !spec.IsSingle(d) {
			group = true
		}
		if group {
			out = append(out, r.Name)
		}
	}
	return out, nil
}

// checkLinkNames 检查项目 projectID 里的容器组和已连接项目（或将要连接的 extra）的网络名不撞：
// 已连接项目的容器在项目内网里叫"容器.网络名"，容器组里的容器叫"容器.容器组"。
func (s *Service) checkLinkNames(ctx context.Context, projectID string, groups []string, networks map[string]string) error {
	for name, network := range networks {
		if slices.Contains(groups, network) {
			return invalid("容器组 %s 和已连接项目 %s 的网络名相同：项目内网里"+`"容器.%s"`+"会撞名，请换个名字或者先断开连接", network, name, network)
		}
	}
	return nil
}

// linkedNetworks 返回项目连接的项目：项目名 → 网络名。
func (s *Service) linkedNetworks(ctx context.Context, projectID string) (map[string]string, error) {
	linked, err := s.linkedMap(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, id := range linked[projectID] {
		if p, err := s.Q.GetProject(ctx, id); err == nil {
			out[p.Name] = p.NetworkName
		}
	}
	return out, nil
}

// checkGroupName 在添加容器组或把容器改成 compose 时调用：容器组的名字不能等于已连接项目的网络名。
func (s *Service) checkGroupName(ctx context.Context, projectID, group string) error {
	nets, err := s.linkedNetworks(ctx, projectID)
	if err != nil {
		return err
	}
	return s.checkLinkNames(ctx, projectID, []string{group}, nets)
}

// checkRenameNetwork 在改网络名时调用：已连接项目里不能有和新网络名同名的容器组。
func (s *Service) checkRenameNetwork(ctx context.Context, projectID, network string) error {
	linked, err := s.linkedMap(ctx)
	if err != nil {
		return err
	}
	for _, id := range linked[projectID] {
		groups, err := s.groupNames(ctx, id)
		if err != nil {
			return err
		}
		if slices.Contains(groups, network) {
			p, _ := s.Q.GetProject(ctx, id)
			return invalid("已连接的项目 %s 里有容器组 %s，和新网络名相同，会在它的项目内网里撞名，请换个网络名", p.Name, network)
		}
	}
	return nil
}

// linkedProjects 返回项目连接的项目（给界面显示）。
func (s *Service) linkedProjects(ctx context.Context, projectID string) ([]*nagiv1.LinkedProject, error) {
	linked, err := s.linkedMap(ctx)
	if err != nil {
		return nil, err
	}
	var out []*nagiv1.LinkedProject
	for _, id := range linked[projectID] {
		if p, err := s.Q.GetProject(ctx, id); err == nil {
			out = append(out, &nagiv1.LinkedProject{ProjectId: p.ID, Name: p.Name, NetworkName: p.NetworkName})
		}
	}
	return out, nil
}

// Connect 连接两个项目。
func (s *Service) Connect(ctx context.Context, id, otherID string) (*nagiv1.Project, error) {
	if id == otherID {
		return nil, invalid("不能连接项目自己")
	}
	a, b := linkPair(id, otherID)
	la, lb := s.lock(a), s.lock(b)
	la.Lock()
	defer la.Unlock()
	lb.Lock()
	defer lb.Unlock()
	p, err := s.project(ctx, id)
	if err != nil {
		return nil, err
	}
	other, err := s.project(ctx, otherID)
	if err != nil {
		return nil, err
	}
	linked, err := s.linkedMap(ctx)
	if err != nil {
		return nil, err
	}
	if slices.Contains(linked[id], otherID) {
		return nil, invalid("已经连接了项目 %s", other.Name)
	}
	for _, side := range []struct{ self, peer string }{{id, otherID}, {otherID, id}} {
		groups, err := s.groupNames(ctx, side.self)
		if err != nil {
			return nil, err
		}
		peer := p
		if side.peer == otherID {
			peer = other
		}
		if err := s.checkLinkNames(ctx, side.self, groups, map[string]string{peer.Name: peer.NetworkName}); err != nil {
			return nil, err
		}
	}
	if err := s.Q.InsertProjectLink(ctx, store.InsertProjectLinkParams{A: a, B: b, CreatedAt: time.Now().Unix()}); err != nil {
		return nil, err
	}
	s.refreshLinks()
	return s.reload(ctx, id)
}

// Disconnect 断开两个项目。
func (s *Service) Disconnect(ctx context.Context, id, otherID string) (*nagiv1.Project, error) {
	a, b := linkPair(id, otherID)
	n, err := s.Q.DeleteProjectLink(ctx, store.DeleteProjectLinkParams{A: a, B: b})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, notFoundError{"没有连接这个项目"}
	}
	s.refreshLinks()
	return s.reload(ctx, id)
}

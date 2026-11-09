package projects

import (
	"context"

	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 容器的告警规则（见 docs/monitoring.md#告警）：项目设置里一组，容器可以单独设置一组整个替换掉项目的。
// 面板把生效的那组随容器配置推给节点（ContainerSpec.alert_rules），节点按它判断这个容器的告警。

// containerAlertRules 返回容器单独设置的规则，没有设置时为 nil。
func containerAlertRules(c store.Container) *nagiv1.AlertRules {
	if len(c.AlertRules) == 0 {
		return nil
	}
	r := &nagiv1.AlertRules{}
	if proto.Unmarshal(c.AlertRules, r) != nil {
		return nil
	}
	return r
}

// effectiveAlertRules 返回容器生效的规则：容器单独设置了就用它的，否则用项目的。都没有时返回空规则（全用默认值），
// 不返回 nil：节点据此知道面板已经按容器下发规则，不再用告警配置里的。
func effectiveAlertRules(settings *nagiv1.ProjectSettings, c store.Container) *nagiv1.AlertRules {
	if r := containerAlertRules(c); r != nil {
		return r
	}
	if r := settings.GetAlertRules(); r != nil {
		return proto.CloneOf(r)
	}
	return &nagiv1.AlertRules{}
}

// SetContainerAlertRules 让容器单独设置告警规则，rules 为 nil 时改回用项目的。推给节点缓存，节点离线时由 Sync 补推。
func (s *Service) SetContainerAlertRules(ctx context.Context, projectID, name string, rules *nagiv1.AlertRules) (*nagiv1.Project, error) {
	var b []byte
	if rules != nil {
		if err := alert.CheckRules(rules); err != nil {
			return nil, err
		}
		// 开关全部写上：全用默认值时也存得下内容，和"没有单独设置"（NULL）区分开。
		r := alert.ContainerPart(rules)
		if r.Enabled == nil {
			r.Enabled = map[string]bool{}
		}
		for _, k := range alert.ContainerRules {
			if _, ok := r.Enabled[k]; !ok {
				r.Enabled[k] = true
			}
		}
		var err error
		if b, err = proto.Marshal(r); err != nil {
			return nil, err
		}
	}
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	if _, err := s.container(ctx, projectID, name); err != nil {
		return nil, err
	}
	if err := s.Q.SetContainerAlertRules(ctx, store.SetContainerAlertRulesParams{AlertRules: b, ProjectID: projectID, Name: name}); err != nil {
		return nil, err
	}
	_ = s.push(context.WithoutCancel(ctx), projectID, name)
	s.refreshLinks() // 跨节点入口带着目标容器的规则
	return s.reload(ctx, projectID)
}

// MigrateAlertRules 把旧版本的全局告警规则里容器的那部分写进还没有设置告警规则的项目（见 nodes.Registry.MigrateAlertRules）。
func (s *Service) MigrateAlertRules(ctx context.Context, rules *nagiv1.AlertRules) error {
	list, err := s.Q.ListProjects(ctx)
	if err != nil {
		return err
	}
	for _, p := range list {
		l := s.lock(p.ID)
		l.Lock()
		err := s.migrateProjectAlertRules(ctx, p.ID, rules)
		l.Unlock()
		if err != nil {
			return err
		}
		s.pushProject(ctx, p.ID)
	}
	return nil
}

func (s *Service) migrateProjectAlertRules(ctx context.Context, id string, rules *nagiv1.AlertRules) error {
	p, err := s.project(ctx, id)
	if err != nil {
		return err
	}
	settings, err := s.settings(p)
	if err != nil || settings.GetAlertRules() != nil {
		return err
	}
	settings.AlertRules = proto.CloneOf(rules)
	sealed, err := s.seal(settings, settingsCtx(id))
	if err != nil {
		return err
	}
	return s.Q.UpdateProject(ctx, store.UpdateProjectParams{Name: p.Name, NetworkName: p.NetworkName, Settings: sealed, ID: id})
}

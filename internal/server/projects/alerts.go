package projects

import (
	"context"

	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 容器的告警规则（见 docs/monitoring.md#告警）：在 设置 → 告警 里按字段覆盖，内置默认 → 全局默认 → 项目 → 容器。
// 面板把容器生效的规则随容器配置推给节点（ContainerSpec.effective_alert_rules，旧版本节点用换算后的 alert_rules）。
// 以前的项目规则（ProjectSettings.alert_rules）和容器单独设置的规则（containers.alert_rules）升级时迁移成覆盖，之后不再使用。

// containerAlertRules 返回容器以前单独设置的规则，没有设置时为 nil（只用于迁移）。
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

// containerRules 返回容器生效的规则（新版本节点用 eff），以及换算成旧版本节点认识的规则（legacy，不为 nil：
// 节点据此知道面板已经按容器下发规则，不再用告警配置里的）。读不到新规则时 legacy 按以前的项目、容器规则。
func (s *Service) containerRules(ctx context.Context, settings *nagiv1.ProjectSettings, c store.Container) (*nagiv1.EffectiveAlertRules, *nagiv1.AlertRules) {
	if s.Nodes != nil {
		if eff := s.Nodes.ContainerAlertRules(ctx, c.ProjectID, c.Name); eff != nil {
			return eff, alert.Legacy(eff)
		}
	}
	if r := containerAlertRules(c); r != nil {
		return nil, r
	}
	if r := settings.GetAlertRules(); r != nil {
		return nil, proto.CloneOf(r)
	}
	return nil, &nagiv1.AlertRules{}
}

// PushAlertRules 在告警规则改了以后把容器的配置（带着生效规则）重新推给节点，并刷新入口计划（跨节点映射也带规则）。
// projectID 为空时推全部项目。不在线的节点上线后由 Sync 补推。
func (s *Service) PushAlertRules(ctx context.Context, projectID string) {
	ids := []string{projectID}
	if projectID == "" {
		list, err := s.Q.ListProjects(ctx)
		if err != nil {
			return
		}
		ids = ids[:0]
		for _, p := range list {
			ids = append(ids, p.ID)
		}
	}
	for _, id := range ids {
		s.pushProject(ctx, id)
	}
	s.refreshLinks()
}

// SetContainerAlertRules 是旧版本界面和 MCP 的接口：把旧格式的规则换成容器的覆盖（rules 为 nil 时去掉容器的覆盖，
// 跟随项目）。只保留和项目生效值不一样的字段。
func (s *Service) SetContainerAlertRules(ctx context.Context, projectID, name string, rules *nagiv1.AlertRules) (*nagiv1.Project, error) {
	if rules != nil {
		if err := alert.CheckRules(rules); err != nil {
			return nil, err
		}
	}
	if _, err := s.container(ctx, projectID, name); err != nil {
		return nil, err
	}
	var layer map[string]*nagiv1.AlertRule
	if rules != nil {
		policy, layers, err := s.Nodes.AlertLayers(ctx, projectID, "")
		if err != nil {
			return nil, err
		}
		layer = nodes.DiffRules(alert.Merge(append([]map[string]*nagiv1.AlertRule{policy.GetRules()}, layers...)...), alert.FromLegacy(rules, alert.ContainerRules), alert.ContainerRules)
	}
	if err := s.Nodes.ReplaceAlertOverride(ctx, &nagiv1.AlertOverride{Scope: "container", ProjectId: projectID, Container: name, Rules: layer}); err != nil {
		return nil, err
	}
	s.PushAlertRules(context.WithoutCancel(ctx), projectID)
	return s.reload(ctx, projectID)
}

// LegacyAlertRules 返回旧版本的项目规则（按项目）和容器单独设置的规则，迁移到覆盖时用（见 nodes.Registry.MigrateAlertPolicy）。
func (s *Service) LegacyAlertRules(ctx context.Context) (map[string]*nagiv1.AlertRules, []nodes.LegacyContainerRules, error) {
	list, err := s.Q.ListProjects(ctx)
	if err != nil {
		return nil, nil, err
	}
	projects := map[string]*nagiv1.AlertRules{}
	var containers []nodes.LegacyContainerRules
	for _, p := range list {
		settings, err := s.settings(p)
		if err != nil {
			return nil, nil, err
		}
		if r := settings.GetAlertRules(); r != nil {
			projects[p.ID] = r
		}
		rows, err := s.Q.ListContainers(ctx, p.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, c := range rows {
			if own := containerAlertRules(c); own != nil {
				containers = append(containers, nodes.LegacyContainerRules{ProjectID: p.ID, Container: c.Name, Project: settings.GetAlertRules(), Own: own})
			}
		}
	}
	return projects, containers, nil
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

// legacyProjectRules 是项目生效的规则换成旧格式（旧版本界面、MCP 显示用）。
func (s *Service) legacyProjectRules(ctx context.Context, projectID string) *nagiv1.AlertRules {
	if s.Nodes == nil {
		return nil
	}
	policy, layers, err := s.Nodes.AlertLayers(ctx, projectID, "")
	if err != nil {
		return nil
	}
	return alert.Legacy(alert.Effective(policy, false, layers...))
}

// legacyContainerRules 是容器有自己的覆盖时，它生效的规则换成旧格式；没有覆盖时为 nil（跟随项目）。
func (s *Service) legacyContainerRules(ctx context.Context, c store.Container) *nagiv1.AlertRules {
	if s.Nodes == nil {
		return nil
	}
	policy, layers, err := s.Nodes.AlertLayers(ctx, c.ProjectID, c.Name)
	if err != nil || len(layers) < 2 || len(layers[1]) == 0 {
		return nil
	}
	return alert.Legacy(alert.Effective(policy, false, layers...))
}

package nodes

import (
	"context"
	"errors"
	"strconv"

	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// 告警规则的全局默认值和覆盖（见 docs/monitoring.md#告警）。规则不含密钥，存成明文：全局的在 settings 的 alert_policy，
// 节点、项目、容器的在 alert_*_overrides 表。主备照常随 panel.db 同步。

const (
	settingAlertPolicy = "alert_policy"
	// settingAlertPolicyMigrated：旧版本的节点、项目、容器规则已经换成覆盖（只做一次）。
	settingAlertPolicyMigrated = "alert_policy_migrated"
)

// ErrAlertConflict 是规则被别人改过时的错误。
var ErrAlertConflict = i18n.New("告警规则已被修改，请刷新后重试")

// AlertPolicy 读取全局设置，没有时为空（全用内置默认值）。
func (r *Registry) AlertPolicy(ctx context.Context) (*nagiv1.AlertPolicy, error) {
	p := &nagiv1.AlertPolicy{}
	v, err := r.q.GetSetting(ctx, settingAlertPolicy)
	if err != nil || v == "" {
		return p, nil
	}
	return p, protojson.Unmarshal([]byte(v), p)
}

// SetAlertPolicy 保存全局设置并下发给所有节点；base 是读到的修订号。项目和容器的生效规则由 onChange 重新下发。
func (r *Registry) SetAlertPolicy(ctx context.Context, p *nagiv1.AlertPolicy, base int64) (*nagiv1.AlertPolicy, error) {
	if err := alert.CheckLayer(p.GetRules(), true, true); err != nil {
		return nil, err
	}
	if n := p.GetFlapCount(); n != 0 && (n < 2 || n > 20) {
		return nil, i18n.New("抖动保护的次数须在 2–20 之间")
	}
	if n := p.GetFlapWindowMinutes(); n != 0 && (n < 10 || n > 1440) {
		return nil, i18n.New("抖动保护的时间窗口须在 10–1440 分钟之间")
	}
	if l := p.GetLanguage(); l != "" && l != "zh-CN" && l != "en" {
		return nil, i18n.New("通知语言只能是中文或英文")
	}
	r.policyMu.Lock()
	defer r.policyMu.Unlock()
	cur, err := r.AlertPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if cur.GetRevision() != base {
		return nil, ErrAlertConflict
	}
	next := &nagiv1.AlertPolicy{Rules: alert.Clean(p.GetRules()), Language: p.GetLanguage(), FlapWindowMinutes: p.GetFlapWindowMinutes(),
		FlapCount: p.GetFlapCount(), Revision: cur.GetRevision() + 1}
	b, err := protojson.Marshal(next)
	if err != nil {
		return nil, err
	}
	if err := r.q.SetSetting(ctx, store.SetSettingParams{Key: settingAlertPolicy, Value: string(b)}); err != nil {
		return nil, err
	}
	r.pushAllAlertConfigs()
	return next, nil
}

// AlertOverrides 返回全部覆盖。
func (r *Registry) AlertOverrides(ctx context.Context) ([]*nagiv1.AlertOverride, error) {
	rows, err := r.q.ListAlertOverrides(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*nagiv1.AlertOverride, 0, len(rows))
	for _, row := range rows {
		out = append(out, overrideProto(row))
	}
	return out, nil
}

func overrideProto(row store.AlertOverride) *nagiv1.AlertOverride {
	o := &nagiv1.AlertOverride{}
	if len(row.Rules) > 0 {
		_ = proto.Unmarshal(row.Rules, o) // 存的就是 AlertOverride，只用它的 rules
	}
	return &nagiv1.AlertOverride{Scope: row.Scope, NodeId: row.NodeID, ProjectId: row.ProjectID, Container: row.Container, Rules: o.GetRules(), Revision: row.Revision}
}

// alertOverride 读一条覆盖，没有时返回 nil 的 rules。
func (r *Registry) alertOverride(ctx context.Context, o store.AlertOverride) (map[string]*nagiv1.AlertRule, error) {
	row, err := r.q.GetAlertOverride(ctx, o)
	if err != nil {
		return nil, err
	}
	return overrideProto(row).GetRules(), nil
}

// AlertLayers 返回一个容器（container 为空时是项目）的规则层：全局、项目、容器。
func (r *Registry) AlertLayers(ctx context.Context, projectID, container string) (*nagiv1.AlertPolicy, []map[string]*nagiv1.AlertRule, error) {
	policy, err := r.AlertPolicy(ctx)
	if err != nil {
		return nil, nil, err
	}
	project, err := r.alertOverride(ctx, store.AlertOverride{Scope: "project", ProjectID: projectID})
	if err != nil {
		return nil, nil, err
	}
	layers := []map[string]*nagiv1.AlertRule{project}
	if container != "" {
		c, err := r.alertOverride(ctx, store.AlertOverride{Scope: "container", ProjectID: projectID, Container: container})
		if err != nil {
			return nil, nil, err
		}
		layers = append(layers, c)
	}
	return policy, layers, nil
}

// ContainerAlertRules 返回容器生效的规则（随容器配置、入口计划下发给节点）。读不到时返回 nil，节点按旧字段判断。
func (r *Registry) ContainerAlertRules(ctx context.Context, projectID, container string) *nagiv1.EffectiveAlertRules {
	policy, layers, err := r.AlertLayers(ctx, projectID, container)
	if err != nil {
		r.log.Warn("读取告警规则失败", "project", projectID, "container", container, "err", err)
		return nil
	}
	return alert.Effective(policy, false, layers...)
}

// NodeAlertEffective 返回节点生效的规则。
func (r *Registry) NodeAlertEffective(ctx context.Context, nodeID string) (*nagiv1.EffectiveAlertRules, error) {
	policy, err := r.AlertPolicy(ctx)
	if err != nil {
		return nil, err
	}
	node, err := r.alertOverride(ctx, store.AlertOverride{Scope: "node", NodeID: nodeID})
	if err != nil {
		return nil, err
	}
	return alert.Effective(policy, true, node), nil
}

// SetAlertOverride 保存一条覆盖（整组替换），检查规则、对象和修订号。节点的覆盖立即下发；项目、容器的由调用方重新下发容器配置。
func (r *Registry) SetAlertOverride(ctx context.Context, o *nagiv1.AlertOverride, base int64) (*nagiv1.AlertOverride, error) {
	row := store.AlertOverride{Scope: o.GetScope(), NodeID: o.GetNodeId(), ProjectID: o.GetProjectId(), Container: o.GetContainer()}
	policy, err := r.AlertPolicy(ctx)
	if err != nil {
		return nil, err
	}
	below := []map[string]*nagiv1.AlertRule{policy.GetRules()}
	switch row.Scope {
	case "node":
		if _, err := r.q.GetNode(ctx, row.NodeID); err != nil {
			return nil, i18n.New("节点不存在")
		}
		row.ProjectID, row.Container = "", ""
		err = alert.CheckLayer(o.GetRules(), true, false, below...)
	case "project":
		if _, err := r.q.GetProject(ctx, row.ProjectID); err != nil {
			return nil, i18n.New("项目不存在")
		}
		row.NodeID, row.Container = "", ""
		err = alert.CheckLayer(o.GetRules(), false, true, below...)
	case "container":
		if _, err := r.q.GetContainer(ctx, store.GetContainerParams{ProjectID: row.ProjectID, Name: row.Container}); err != nil {
			return nil, i18n.New("容器不存在")
		}
		row.NodeID = ""
		project, perr := r.alertOverride(ctx, store.AlertOverride{Scope: "project", ProjectID: row.ProjectID})
		if perr != nil {
			return nil, perr
		}
		err = alert.CheckLayer(o.GetRules(), false, true, append(below, project)...)
	default:
		return nil, i18n.New("覆盖只能设置在节点、项目或容器上")
	}
	if err != nil {
		return nil, err
	}
	rules := alert.Clean(o.GetRules())
	if len(rules) > 0 {
		if row.Rules, err = proto.Marshal(&nagiv1.AlertOverride{Rules: rules}); err != nil {
			return nil, err
		}
	}
	rev, err := r.q.SetAlertOverride(ctx, row, base)
	if errors.Is(err, store.ErrAlertOverrideConflict) {
		return nil, ErrAlertConflict
	}
	if err != nil {
		return nil, err
	}
	if row.Scope == "node" {
		if c, err := r.Get(row.NodeID); err == nil {
			go r.pushAlertConfig(context.Background(), c, nil)
		}
	}
	return &nagiv1.AlertOverride{Scope: row.Scope, NodeId: row.NodeID, ProjectId: row.ProjectID, Container: row.Container, Rules: rules, Revision: rev}, nil
}

// ForgetNodeAlertRules 删除被移除的节点的覆盖。
func (r *Registry) ForgetNodeAlertRules(ctx context.Context, nodeID string) error {
	return r.q.DeleteNodeAlertOverride(ctx, nodeID)
}

func (r *Registry) pushAllAlertConfigs() {
	for _, c := range r.Conns() {
		go r.pushAlertConfig(context.Background(), c, nil)
	}
}

// ---- 旧规则的迁移 ----

// LegacyContainerRules 是旧版本里一个容器的规则：项目的和容器单独设置的。
type LegacyContainerRules struct {
	ProjectID string
	Container string
	Project   *nagiv1.AlertRules
	Own       *nagiv1.AlertRules
}

// MigrateAlertPolicy 把旧版本的节点、项目、容器规则换成覆盖，只在主面板上做一次（见 docs/monitoring.md#告警）：
//   - 节点、项目：写了的阈值、关掉的开关各成为覆盖的字段；
//   - 容器单独设置的：只留和项目当时的生效值不一样的字段，其余以后跟随项目。
//
// 已经有覆盖的对象不动。projects 返回旧版本的项目规则（按项目）和容器单独设置的规则。
func (r *Registry) MigrateAlertPolicy(ctx context.Context, projects func(context.Context) (map[string]*nagiv1.AlertRules, []LegacyContainerRules, error)) error {
	if r.ReadOnly {
		return nil
	}
	if v, err := r.q.GetSetting(ctx, settingAlertPolicyMigrated); err == nil && v != "" {
		return nil
	}
	nodes, err := r.q.ListNodes(ctx)
	if err != nil {
		return err
	}
	projectRules, containers, err := projects(ctx)
	if err != nil {
		return err
	}
	set := func(o store.AlertOverride, rules map[string]*nagiv1.AlertRule) error {
		if len(rules) == 0 {
			return nil
		}
		if cur, err := r.q.GetAlertOverride(ctx, o); err != nil || cur.Revision != 0 {
			return err
		}
		b, err := proto.Marshal(&nagiv1.AlertOverride{Rules: rules})
		if err != nil {
			return err
		}
		o.Rules = b
		_, err = r.q.SetAlertOverride(ctx, o, 0)
		return err
	}
	count := 0
	for _, n := range nodes {
		rules := alert.FromLegacy(NodeAlertRules(n), alert.NodeRules)
		if err := set(store.AlertOverride{Scope: "node", NodeID: n.ID}, rules); err != nil {
			return err
		}
		count += len(rules)
	}
	for id, legacy := range projectRules {
		rules := alert.FromLegacy(legacy, alert.ContainerRules)
		if err := set(store.AlertOverride{Scope: "project", ProjectID: id}, rules); err != nil {
			return err
		}
		count += len(rules)
	}
	for _, c := range containers {
		if c.Own == nil {
			continue
		}
		rules := diffLegacy(c.Project, c.Own)
		if err := set(store.AlertOverride{Scope: "container", ProjectID: c.ProjectID, Container: c.Container}, rules); err != nil {
			return err
		}
		count += len(rules)
	}
	if err := r.q.SetSetting(ctx, store.SetSettingParams{Key: settingAlertPolicyMigrated, Value: "1"}); err != nil {
		return err
	}
	r.log.Info("已把旧的告警规则换成覆盖", "rules", strconv.Itoa(count))
	r.pushAllAlertConfigs()
	return nil
}

// diffLegacy 返回容器单独设置的规则里和项目生效值不一样的字段。
// 以前容器单独设置的规则整组替换项目的，没写的字段用内置默认值（不是项目的），所以按「内置默认 + 容器自己的」比较。
func diffLegacy(project, own *nagiv1.AlertRules) map[string]*nagiv1.AlertRule {
	return diffEffective(alert.Merge(alert.FromLegacy(project, alert.ContainerRules)), alert.Merge(alert.FromLegacy(own, alert.ContainerRules)), alert.ContainerRules)
}

// DiffRules 返回 layer 盖到 base（生效值）上以后，和 base 不一样的字段（names 里的规则）。
func DiffRules(base, layer map[string]*nagiv1.AlertRule, names []string) map[string]*nagiv1.AlertRule {
	return diffEffective(base, alert.Merge(base, layer), names)
}

// diffEffective 返回 mine 和 base（都是生效值）不一样的字段。
func diffEffective(base, mine map[string]*nagiv1.AlertRule, names []string) map[string]*nagiv1.AlertRule {
	out := map[string]*nagiv1.AlertRule{}
	for _, name := range names {
		a, b := base[name], mine[name]
		if a == nil || proto.Equal(a, b) {
			continue
		}
		x := &nagiv1.AlertRule{}
		if a.GetEnabled() != b.GetEnabled() {
			x.Enabled = proto.Bool(b.GetEnabled())
		}
		for _, f := range []struct{ a, b, x **float64 }{{&a.Threshold, &b.Threshold, &x.Threshold}, {&a.RecoverThreshold, &b.RecoverThreshold, &x.RecoverThreshold}, {&a.MinRequests, &b.MinRequests, &x.MinRequests}} {
			if *f.b != nil && (*f.a == nil || **f.a != **f.b) {
				*f.x = proto.Float64(**f.b)
			}
		}
		for _, f := range []struct{ a, b, x **uint32 }{{&a.ForSeconds, &b.ForSeconds, &x.ForSeconds}, {&a.RecoverSeconds, &b.RecoverSeconds, &x.RecoverSeconds}, {&a.RepeatMinutes, &b.RepeatMinutes, &x.RepeatMinutes}, {&a.WindowMinutes, &b.WindowMinutes, &x.WindowMinutes}} {
			if *f.b != nil && (*f.a == nil || **f.a != **f.b) {
				*f.x = proto.Uint32(**f.b)
			}
		}
		out[name] = x
	}
	return alert.Clean(out)
}

// ReplaceAlertOverride 不检查修订号，直接替换一条覆盖（旧接口用：它们按整组提交，没有修订号）。
func (r *Registry) ReplaceAlertOverride(ctx context.Context, o *nagiv1.AlertOverride) error {
	for range 3 {
		cur, err := r.q.GetAlertOverride(ctx, store.AlertOverride{Scope: o.GetScope(), NodeID: o.GetNodeId(), ProjectID: o.GetProjectId(), Container: o.GetContainer()})
		if err != nil {
			return err
		}
		if _, err = r.SetAlertOverride(ctx, o, cur.Revision); !errors.Is(err, ErrAlertConflict) {
			return err
		}
	}
	return ErrAlertConflict
}

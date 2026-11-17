package mcp

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"google.golang.org/protobuf/proto"
)

// 告警规则（见 docs/monitoring.md#告警）和版本列表。容器的规则在项目上设置、容器可以单独设置；节点的规则在节点上设置。

func init() {
	allTools = append(allTools,
		def(tool{name: "list_versions", title: "List versions", idempotent: true,
			description: "All versions of a container, newest first: when created and deployed, images, git commit, whether files were kept, and which version a rollback restored. Use with rollback.",
			procedures:  []string{nagiv1connect.ProjectServiceListProjectsProcedure, nagiv1connect.ProjectServiceGetProjectProcedure, nagiv1connect.ProjectServiceListVersionsProcedure}},
			toolListVersions),
		def(tool{name: "set_alert_rules", title: "Set alert rules", write: true, idempotent: true,
			description: "Change the alert rules for a project's containers (restarts, HTTP 5xx and 4xx, health checks, snapshots, backup sync, CPU, traffic, blocked egress), or give one container its own rules; use_project makes a container follow the project's again. Only the fields passed change. Takes effect immediately.",
			procedures: []string{nagiv1connect.ProjectServiceListProjectsProcedure, nagiv1connect.ProjectServiceGetProjectProcedure,
				nagiv1connect.ProjectServiceUpdateProjectSettingsProcedure, nagiv1connect.ProjectServiceSetContainerAlertRulesProcedure}},
			toolSetAlertRules),
		def(tool{name: "set_node_alert_rules", title: "Set node alert rules", write: true, idempotent: true, adminOnly: true,
			description: "Administrators only. Change a node's alert rules: memory, disk, offline, certificate expiry and CPU. Only the fields passed change. Takes effect immediately.",
			procedures:  []string{nagiv1connect.NodeServiceListNodesProcedure, nagiv1connect.NodeServiceSetNodeAlertRulesProcedure}},
			toolSetNodeAlertRules),
	)
}

type ListVersionsArgs struct {
	ContainerArg
	Limit int `json:"limit,omitempty" jsonschema:"at most this many, default 50"`
}

func toolListVersions(c *call, in ListVersionsArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	res, err := c.h.clients.projects.ListVersions(c.ctx, connect.NewRequest(&nagiv1.ContainerRef{ProjectId: s.GetProject().GetId(), Container: m.GetName()}))
	if err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	type version struct {
		Version    int64             `json:"version"`
		CreatedAt  string            `json:"created_at"`
		DeployedAt string            `json:"deployed_at,omitempty"`
		Deployed   bool              `json:"deployed"`
		Images     map[string]string `json:"images,omitempty"`
		Commit     string            `json:"commit,omitempty"`
		HasFiles   bool              `json:"files_kept"`
		RollbackOf int64             `json:"rollback_of,omitempty"`
	}
	out := []version{}
	all := res.Msg.GetVersions()
	for _, v := range all {
		if len(out) == limit {
			break
		}
		out = append(out, version{Version: v.GetVersion(), CreatedAt: ts(v.GetCreatedAt()), DeployedAt: ts(v.GetDeployedAt()), Deployed: v.GetDeployedAt() != nil,
			Images: v.GetImages(), Commit: v.GetCommit(), HasFiles: v.GetHasFiles(), RollbackOf: v.GetRollbackOf()})
	}
	return map[string]any{"versions": out, "total": len(all), "deployed_version": m.GetDeployedVersion(), "latest_version": m.GetLatestVersion()}, nil
}

// ---- 告警规则 ----

// ContainerAlertArgs 是容器的告警规则（项目上的和容器单独的一样）。没传的不变。
type ContainerAlertArgs struct {
	RestartCount   *uint32 `json:"restart_count,omitempty" jsonschema:"alert when a container restarts this many times within restart_minutes (default 3 in 10 minutes); 0 means the default"`
	RestartMinutes *uint32 `json:"restart_minutes,omitempty" jsonschema:"window for restart_count, default 10"`
	ErrorPercent   *uint32 `json:"error_percent,omitempty" jsonschema:"alert when an HTTP port mapping's 5xx share over 5 minutes exceeds this percent (at least 20 requests), default 5; 0 means the default"`
	// 规则开关，没写的不变。
	Enabled map[string]bool `json:"enabled,omitempty" jsonschema:"turn rules on (true) or off (false): restart, errors, health, snapshot, backup, container_cpu, traffic, 4xx, egress"`
}

func (a ContainerAlertArgs) apply(base *nagiv1.AlertRules) (*nagiv1.AlertRules, []string, error) {
	r := alert.ContainerPart(base)
	if r == nil {
		r = &nagiv1.AlertRules{}
	}
	var changed []string
	set := func(name string, v *uint32, dst *uint32) {
		if v != nil && *v != *dst {
			*dst = *v
			changed = append(changed, fmt.Sprintf("%s → %d", name, *v))
		}
	}
	set("restart_count", a.RestartCount, &r.RestartCount)
	set("restart_minutes", a.RestartMinutes, &r.RestartMinutes)
	set("error_percent", a.ErrorPercent, &r.ErrorPercent)
	c, err := setEnabled(r, a.Enabled, alert.ContainerRules)
	return r, append(changed, c...), err
}

// setEnabled 改规则开关。没写的规则开着，所以关掉要写 false，打开时去掉这一项也一样。
func setEnabled(r *nagiv1.AlertRules, on map[string]bool, allowed []string) ([]string, error) {
	var changed []string
	keys := slices.Sorted(maps.Keys(on))
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			return nil, fmt.Errorf("unknown rule %q; rules are %v", k, allowed)
		}
		if alert.Enabled(r, k) == on[k] {
			continue
		}
		if r.Enabled == nil {
			r.Enabled = map[string]bool{}
		}
		r.Enabled[k] = on[k]
		changed = append(changed, fmt.Sprintf("%s → %v", k, on[k]))
	}
	return changed, nil
}

type SetAlertRulesArgs struct {
	Project string `json:"project" jsonschema:"project ID, network name or name"`
	// 不填时改项目的规则（项目里没有单独设置的容器都用它）。
	Container  string `json:"container,omitempty" jsonschema:"give this container its own rules instead of changing the project's"`
	UseProject bool   `json:"use_project,omitempty" jsonschema:"with container: drop the container's own rules and follow the project's again"`
	ContainerAlertArgs
}

// alertView 是给 agent 看的规则：阈值写上默认值，列出关着的规则。
func alertView(r *nagiv1.AlertRules, rules []string) map[string]any {
	off := []string{}
	for _, k := range rules {
		if !alert.Enabled(r, k) {
			off = append(off, k)
		}
	}
	or := func(v, d uint32) uint32 {
		if v == 0 {
			return d
		}
		return v
	}
	out := map[string]any{"rules_off": off}
	if slices.Equal(rules, alert.NodeRules) {
		out["disk_percent"] = or(r.GetDiskPercent(), alert.DefaultDiskPercent)
		out["mem_percent"] = or(r.GetMemPercent(), alert.DefaultMemPercent)
		out["mem_minutes"] = or(r.GetMemMinutes(), alert.DefaultMemMinutes)
		out["cert_days"] = or(r.GetCertDays(), alert.DefaultCertDays)
		out["offline_seconds"] = or(r.GetOfflineSeconds(), alert.DefaultOfflineSeconds)
	} else {
		out["restart_count"] = or(r.GetRestartCount(), alert.DefaultRestartCount)
		out["restart_minutes"] = or(r.GetRestartMinutes(), alert.DefaultRestartMinutes)
		out["error_percent"] = or(r.GetErrorPercent(), alert.DefaultErrorPercent)
	}
	return out
}

func toolSetAlertRules(c *call, in SetAlertRulesArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	if in.Container == "" {
		if in.UseProject {
			return nil, errors.New("use_project needs a container")
		}
		old := p.GetSettings()
		rules, changed, err := in.ContainerAlertArgs.apply(old.GetAlertRules())
		if err != nil {
			return nil, err
		}
		if len(changed) == 0 {
			return map[string]any{"changed": []string{}, "rules": alertView(rules, alert.ContainerRules), "note": "nothing changed"}, nil
		}
		// 其余设置原样带上（接口按整份设置保存，没带告警规则、出站限制时保留原来的）。
		set := &nagiv1.ProjectSettings{SnapshotIntervalMinutes: old.GetSnapshotIntervalMinutes(), SnapshotLimit: old.GetSnapshotLimit(), InternalPorts: old.GetInternalPorts(),
			SnapshotSchedule: spec.ProjectSnapshotSchedule(old), AlertRules: rules}
		if _, err := c.h.clients.projects.UpdateProjectSettings(c.ctx, connect.NewRequest(&nagiv1.UpdateProjectSettingsRequest{
			ProjectId: p.GetId(), BaseRevision: p.GetRevision(), Name: p.GetName(), NetworkName: p.GetNetworkName(), Settings: set,
		})); err != nil {
			return nil, err
		}
		return map[string]any{"changed": changed, "rules": alertView(rules, alert.ContainerRules), "note": "applies to every container of the project without its own rules"}, nil
	}
	m, err := findContainer(p, in.Container)
	if err != nil {
		return nil, err
	}
	var rules *nagiv1.AlertRules
	changed := []string{"follows the project's rules again"}
	if !in.UseProject {
		base := m.GetAlertRules()
		if base == nil {
			base = p.GetSettings().GetAlertRules()
		}
		if rules, changed, err = in.ContainerAlertArgs.apply(base); err != nil {
			return nil, err
		}
		if len(changed) == 0 && m.GetAlertRules() != nil {
			return map[string]any{"changed": []string{}, "rules": alertView(rules, alert.ContainerRules), "note": "nothing changed"}, nil
		}
		if m.GetAlertRules() == nil {
			changed = append([]string{"own rules instead of the project's"}, changed...)
		}
	} else if m.GetAlertRules() == nil {
		return map[string]any{"changed": []string{}, "note": "the container already follows the project's rules"}, nil
	}
	if _, err := c.h.clients.projects.SetContainerAlertRules(c.ctx, connect.NewRequest(&nagiv1.SetContainerAlertRulesRequest{
		ProjectId: p.GetId(), Container: m.GetName(), Rules: rules,
	})); err != nil {
		return nil, err
	}
	shown := rules
	if shown == nil {
		shown = p.GetSettings().GetAlertRules()
	}
	return map[string]any{"changed": changed, "rules": alertView(shown, alert.ContainerRules)}, nil
}

type NodeAlertArgs struct {
	Node           string          `json:"node" jsonschema:"node name or ID"`
	DiskPercent    *uint32         `json:"disk_percent,omitempty" jsonschema:"alert when disk use exceeds this percent, default 90; 0 means the default"`
	MemPercent     *uint32         `json:"mem_percent,omitempty" jsonschema:"alert when memory use stays above this percent for mem_minutes, default 90"`
	MemMinutes     *uint32         `json:"mem_minutes,omitempty" jsonschema:"default 5"`
	CertDays       *uint32         `json:"cert_days,omitempty" jsonschema:"alert when an HTTPS certificate expires within this many days, default 7"`
	OfflineSeconds *uint32         `json:"offline_seconds,omitempty" jsonschema:"alert when the node is unreachable for this many seconds, default 60"`
	Enabled        map[string]bool `json:"enabled,omitempty" jsonschema:"turn rules on (true) or off (false): mem, disk, offline, cert, cpu"`
}

func toolSetNodeAlertRules(c *call, in NodeAlertArgs) (any, error) {
	nodes, err := c.nodes()
	if err != nil {
		return nil, err
	}
	n, err := findNode(nodes, in.Node)
	if err != nil {
		return nil, err
	}
	r := alert.NodePart(n.GetAlertRules())
	if r == nil {
		r = &nagiv1.AlertRules{}
	}
	r = proto.CloneOf(r)
	var changed []string
	set := func(name string, v *uint32, dst *uint32) {
		if v != nil && *v != *dst {
			*dst = *v
			changed = append(changed, fmt.Sprintf("%s → %d", name, *v))
		}
	}
	set("disk_percent", in.DiskPercent, &r.DiskPercent)
	set("mem_percent", in.MemPercent, &r.MemPercent)
	set("mem_minutes", in.MemMinutes, &r.MemMinutes)
	set("cert_days", in.CertDays, &r.CertDays)
	set("offline_seconds", in.OfflineSeconds, &r.OfflineSeconds)
	c2, err := setEnabled(r, in.Enabled, alert.NodeRules)
	if err != nil {
		return nil, err
	}
	changed = append(changed, c2...)
	sort.Strings(changed)
	if len(changed) == 0 {
		return map[string]any{"changed": []string{}, "rules": alertView(r, alert.NodeRules), "note": "nothing changed"}, nil
	}
	if _, err := c.h.clients.nodes.SetNodeAlertRules(c.ctx, connect.NewRequest(&nagiv1.SetNodeAlertRulesRequest{NodeId: n.GetId(), Rules: r})); err != nil {
		return nil, err
	}
	return map[string]any{"node": n.GetName(), "changed": changed, "rules": alertView(r, alert.NodeRules)}, nil
}

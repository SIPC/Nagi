package mcp

import (
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 项目级的修改（见 docs/mcp.md#工具）：项目设置、共享变量、部署记录、项目连接、镜像检查和镜像仓库登录。

// projectRevision 和 baseRevision 一样：agent 给了修订号就用它，没给就用刚读到的。
func projectRevision(expected int64, p *nagiv1.Project) int64 {
	if expected > 0 {
		return expected
	}
	return p.GetRevision()
}

// ---- 项目设置 ----

type UpdateProjectArgs struct {
	ProjectArg
	ExpectedRevision        int64    `json:"expected_revision,omitempty" jsonschema:"the project's revision from get_project; the change is refused if someone changed the project since"`
	Name                    *string  `json:"name,omitempty" jsonschema:"display name, can repeat"`
	NetworkName             *string  `json:"network_name,omitempty" jsonschema:"unique network name (lowercase letters, digits and -). Linked projects reach this project's containers at container.network-name and must update those addresses"`
	SnapshotIntervalMinutes *uint32  `json:"snapshot_interval_minutes,omitempty" jsonschema:"minutes between scheduled snapshots of every container, at most 10080; 0 means the default (720)"`
	SnapshotLimit           *uint32  `json:"snapshot_limit,omitempty" jsonschema:"scheduled, manual and predeploy snapshots kept per container, 3 to 1000; 0 means the default (10). The oldest beyond it are deleted after the next snapshot"`
	InternalPorts           []string `json:"internal_ports,omitempty" jsonschema:"replaces the internal ports: ports that containers on other nodes reach although the compose file does not publish them, as service:port"`
}

func toolUpdateProject(c *call, in UpdateProjectArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	old := p.GetSettings()
	name, network := p.GetName(), p.GetNetworkName()
	set := &nagiv1.ProjectSettings{SnapshotIntervalMinutes: old.GetSnapshotIntervalMinutes(), SnapshotLimit: old.GetSnapshotLimit(), InternalPorts: old.GetInternalPorts()}
	var changed []string
	if in.Name != nil && *in.Name != name {
		name = *in.Name
		changed = append(changed, "name → "+name)
	}
	if in.NetworkName != nil && *in.NetworkName != network {
		network = *in.NetworkName
		changed = append(changed, "network_name → "+network)
	}
	if in.SnapshotIntervalMinutes != nil && *in.SnapshotIntervalMinutes != set.SnapshotIntervalMinutes {
		set.SnapshotIntervalMinutes = *in.SnapshotIntervalMinutes
		changed = append(changed, fmt.Sprintf("snapshot_interval_minutes → %d", spec.SnapshotIntervalMinutes(set.SnapshotIntervalMinutes)))
	}
	if in.SnapshotLimit != nil && *in.SnapshotLimit != set.SnapshotLimit {
		set.SnapshotLimit = *in.SnapshotLimit
		changed = append(changed, fmt.Sprintf("snapshot_limit → %d", spec.SnapshotLimit(set.SnapshotLimit)))
	}
	if in.InternalPorts != nil && !slices.Equal(in.InternalPorts, set.InternalPorts) {
		set.InternalPorts = in.InternalPorts
		changed = append(changed, "internal_ports")
	}
	if len(changed) == 0 {
		return map[string]any{"changed": []string{}, "note": "nothing changed"}, nil
	}
	res, err := c.h.clients.projects.UpdateProjectSettings(c.ctx, connect.NewRequest(&nagiv1.UpdateProjectSettingsRequest{
		ProjectId: p.GetId(), BaseRevision: projectRevision(in.ExpectedRevision, p), Name: name, NetworkName: network, Settings: set,
	}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"changed": changed, "revision": res.Msg.GetRevision(), "note": "takes effect immediately"}, nil
}

// ---- 共享变量 ----

type SharedEnvArgs struct {
	ProjectArg
	ExpectedRevision int64             `json:"expected_revision,omitempty" jsonschema:"the project's revision from get_project"`
	Set              map[string]string `json:"set,omitempty" jsonschema:"set these shared variables; every container's compose file can use them as ${NAME}, a container's own variable of the same name wins"`
	Unset            []string          `json:"unset,omitempty" jsonschema:"remove these shared variables"`
	Secret           []string          `json:"secret,omitempty" jsonschema:"mark these variables as secret (hidden by default in the web UI)"`
}

// markSecret 把列出的变量标为密钥。
func markSecret(vars []*nagiv1.EnvVar, keys []string) ([]*nagiv1.EnvVar, []string, error) {
	var changed []string
	for _, k := range keys {
		i := slices.IndexFunc(vars, func(v *nagiv1.EnvVar) bool { return v.GetKey() == k })
		if i < 0 {
			return nil, nil, fmt.Errorf("variable %s is not set", k)
		}
		if !vars[i].GetSecret() {
			vars[i] = &nagiv1.EnvVar{Key: k, Value: vars[i].GetValue(), Secret: true}
			changed = append(changed, "secret "+k)
		}
	}
	return vars, changed, nil
}

func toolUpdateSharedEnv(c *call, in SharedEnvArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	base := p.GetSettings().GetSharedEnv()
	if p.GetHasSharedEnvDraft() {
		base = p.GetSharedEnvDraft()
	}
	vars, changed := patchEnv(base, in.Set, in.Unset)
	vars, secret, err := markSecret(vars, in.Secret)
	if err != nil {
		return nil, err
	}
	changed = append(changed, secret...)
	if len(changed) == 0 {
		return map[string]any{"changed": []string{}, "note": "nothing changed"}, nil
	}
	res, err := c.h.clients.projects.SaveSharedEnv(c.ctx, connect.NewRequest(&nagiv1.SaveSharedEnvRequest{
		ProjectId: p.GetId(), BaseRevision: projectRevision(in.ExpectedRevision, p), Vars: vars,
	}))
	if err != nil {
		return nil, err
	}
	affected := res.Msg.GetAffected()
	if affected == nil {
		affected = []string{}
	}
	return map[string]any{"changed": changed, "affected_containers": affected, "revision": res.Msg.GetProject().GetRevision(),
		"note": "saved as a draft; call deploy with apply_shared_env and containers set to affected_containers to apply it"}, nil
}

type ProjectRevisionArgs struct {
	ProjectArg
	ExpectedRevision int64 `json:"expected_revision,omitempty" jsonschema:"the project's revision from get_project"`
}

func toolDiscardSharedEnv(c *call, in ProjectRevisionArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	if !p.GetHasSharedEnvDraft() {
		return map[string]string{"note": "the shared variables have no draft"}, nil
	}
	if _, err := c.h.clients.projects.DiscardSharedEnv(c.ctx, connect.NewRequest(&nagiv1.ProjectRevisionRef{
		ProjectId: p.GetId(), BaseRevision: projectRevision(in.ExpectedRevision, p),
	})); err != nil {
		return nil, err
	}
	return map[string]string{"note": "draft discarded"}, nil
}

// ---- 显示名称 ----

type DisplayNameArgs struct {
	ContainerArg
	DisplayName string `json:"display_name" jsonschema:"name shown in the web UI; empty shows the container name. The container name itself cannot change"`
}

func toolSetDisplayName(c *call, in DisplayNameArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	if _, err := c.h.clients.projects.SetContainerDisplayName(c.ctx, connect.NewRequest(&nagiv1.SetContainerDisplayNameRequest{
		ProjectId: s.GetProject().GetId(), Container: m.GetName(), DisplayName: in.DisplayName,
	})); err != nil {
		return nil, err
	}
	return map[string]string{"container": m.GetName(), "display_name": in.DisplayName}, nil
}

// ---- 部署记录 ----

type DeploymentsArgs struct {
	ProjectArg
	Limit int `json:"limit,omitempty" jsonschema:"default 20, at most 100"`
}

func toolListDeployments(c *call, in DeploymentsArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	res, err := c.h.clients.projects.ListDeployments(c.ctx, connect.NewRequest(&nagiv1.ProjectRef{ProjectId: s.GetProject().GetId()}))
	if err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	type deployment struct {
		ID         int64            `json:"id"`
		Time       string           `json:"time"`
		Source     string           `json:"source"`
		Containers []string         `json:"containers"`
		Versions   map[string]int64 `json:"versions,omitempty"`
		Success    bool             `json:"success"`
		Error      string           `json:"error,omitempty"`
		RestoreOf  int64            `json:"restore_of,omitempty"`
	}
	out := []deployment{}
	for _, d := range res.Msg.GetDeployments() {
		if len(out) >= min(limit, 100) {
			break
		}
		out = append(out, deployment{ID: d.GetId(), Time: ts(d.GetTime()), Source: d.GetSource(), Containers: d.GetContainers(),
			Versions: d.GetVersions(), Success: d.GetSuccess(), Error: d.GetError(), RestoreOf: d.GetRestoreOf()})
	}
	return map[string]any{"deployments": out,
		"legend": "source: manual, git (webhook pull), image (image update), rollback, shared_env, restore (back to an earlier deployment); versions are each container's version after the deployment"}, nil
}

// ---- 项目连接 ----

type LinkArgs struct {
	ProjectArg
	Other string `json:"other" jsonschema:"the other project: ID, network name or name"`
}

func toolConnectProject(c *call, in LinkArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	o, err := c.project(in.Other)
	if err != nil {
		return nil, err
	}
	p, other := s.GetProject(), o.GetProject()
	if p.GetId() == other.GetId() {
		return nil, errors.New("a project cannot be linked to itself")
	}
	if _, err := c.h.clients.projects.ConnectProject(c.ctx, connect.NewRequest(&nagiv1.ConnectProjectRequest{ProjectId: p.GetId(), OtherProjectId: other.GetId()})); err != nil {
		return nil, err
	}
	return map[string]string{"note": fmt.Sprintf("linked; containers of %s reach %s's containers at container.%s, and the other way at container.%s",
		p.GetName(), other.GetName(), other.GetNetworkName(), p.GetNetworkName())}, nil
}

func toolDisconnectProject(c *call, in LinkArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	// 对方可能在成员的访问范围外，先按连接里记的 ID 和网络名找。
	i := slices.IndexFunc(p.GetLinks(), func(l *nagiv1.LinkedProject) bool {
		return l.GetProjectId() == in.Other || l.GetNetworkName() == in.Other || l.GetName() != "" && l.GetName() == in.Other
	})
	if i < 0 {
		return nil, fmt.Errorf("project %s is not linked to %q", p.GetName(), in.Other)
	}
	if _, err := c.h.clients.projects.DisconnectProject(c.ctx, connect.NewRequest(&nagiv1.ConnectProjectRequest{ProjectId: p.GetId(), OtherProjectId: p.GetLinks()[i].GetProjectId()})); err != nil {
		return nil, err
	}
	return map[string]string{"note": "unlinked; the two projects' containers can no longer reach each other"}, nil
}

// ---- 镜像 ----

func toolCheckImages(c *call, in ProjectArg) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	res, err := c.h.clients.projects.CheckImages(c.ctx, connect.NewRequest(&nagiv1.ProjectRef{ProjectId: s.GetProject().GetId()}))
	if err != nil {
		return nil, err
	}
	type update struct {
		Container string `json:"container"`
		Service   string `json:"service"`
		Image     string `json:"image"`
	}
	out := []update{}
	for _, u := range res.Msg.GetUpdates() {
		out = append(out, update{Container: u.GetContainer(), Service: u.GetService(), Image: u.GetImage()})
	}
	return map[string]any{"updates": out, "note": "deploy with pull_images to update"}, nil
}

type RegistryArgs struct {
	ProjectArg
	Registry string `json:"registry" jsonschema:"registry address, e.g. ghcr.io"`
}

type LoginRegistryArgs struct {
	RegistryArgs
	Username string `json:"username"`
	Password string `json:"password" jsonschema:"password or access token; stored on the panel and never returned"`
}

func registries(list []string) map[string]any {
	if list == nil {
		list = []string{}
	}
	return map[string]any{"registries": list}
}

func toolListRegistries(c *call, in ProjectArg) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	res, err := c.h.clients.projects.ListRegistries(c.ctx, connect.NewRequest(&nagiv1.ProjectRef{ProjectId: s.GetProject().GetId()}))
	if err != nil {
		return nil, err
	}
	return registries(res.Msg.GetRegistries()), nil
}

func toolLoginRegistry(c *call, in LoginRegistryArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	res, err := c.h.clients.projects.LoginRegistry(c.ctx, connect.NewRequest(&nagiv1.LoginRegistryApiRequest{
		ProjectId: s.GetProject().GetId(), Registry: in.Registry, Username: in.Username, Password: in.Password,
	}))
	if err != nil {
		return nil, err
	}
	return registries(res.Msg.GetRegistries()), nil
}

func toolLogoutRegistry(c *call, in RegistryArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	res, err := c.h.clients.projects.LogoutRegistry(c.ctx, connect.NewRequest(&nagiv1.LogoutRegistryRequest{ProjectId: s.GetProject().GetId(), Registry: in.Registry}))
	if err != nil {
		return nil, err
	}
	return registries(res.Msg.GetRegistries()), nil
}

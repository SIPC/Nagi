package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"
)

// ---- 等待流式操作 ----

// deployWait 是部署这类操作最多等多久；超过就返回「仍在进行」，操作在面板上接着做完。
const deployWait = 10 * time.Minute

type opResult struct {
	Success      bool     `json:"success"`
	StillRunning bool     `json:"still_running,omitempty"`
	Version      int64    `json:"version,omitempty"`
	Error        string   `json:"error,omitempty"`
	Output       []string `json:"last_output,omitempty"`
}

// follow 跟着一个流式操作（部署、启停、回滚、快照），把输出作为进度通知发给客户端，等到结果。
// 操作用不跟着客户端取消的 ctx：面板上的部署开始了就做完（见 docs/agent.md#部署方式），agent 断开也不会留下半截。
func (c *call) follow(open func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error)) (opResult, error) {
	stream, err := open(context.WithoutCancel(c.ctx))
	if err != nil {
		return opResult{}, err
	}
	type item struct {
		line   string
		result *nagiv1.DeployResult
		err    error
	}
	ch := make(chan item, 64)
	go func() {
		defer stream.Close()
		defer close(ch)
		for stream.Receive() {
			switch e := stream.Msg().Event.(type) {
			case *nagiv1.DeployEvent_Output:
				ch <- item{line: e.Output}
			case *nagiv1.DeployEvent_Result:
				ch <- item{result: e.Result}
			}
		}
		if err := stream.Err(); err != nil {
			ch <- item{err: err}
		}
	}()
	var token any
	if c.req.Params != nil {
		token = c.req.Params.GetProgressToken()
	}
	var tail []string
	n := 0
	timer := time.NewTimer(deployWait)
	defer timer.Stop()
	for {
		select {
		case it, ok := <-ch:
			if !ok {
				return opResult{Error: "the operation ended without a result; check the container state", Output: tail}, nil
			}
			switch {
			case it.err != nil:
				return opResult{}, it.err
			case it.result != nil:
				r := it.result
				out := opResult{Success: r.GetSuccess(), Version: r.GetVersion(), Error: r.GetError()}
				if !r.GetSuccess() {
					out.Output = tail
				}
				return out, nil
			}
			n++
			tail = append(tail, it.line)
			if len(tail) > 50 {
				tail = tail[len(tail)-50:]
			}
			if token != nil && c.req.Session != nil {
				_ = c.req.Session.NotifyProgress(c.ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: float64(n), Message: it.line})
			}
		case <-timer.C:
			// 剩下的输出在后台读完，免得面板那边的发送被堵住。
			go func() {
				for range ch {
				}
			}()
			return opResult{StillRunning: true, Output: tail}, nil
		case <-c.ctx.Done():
			go func() {
				for range ch {
				}
			}()
			return opResult{}, c.ctx.Err()
		}
	}
}

// ---- 新建项目 ----

type CreateProjectArgs struct {
	Name        string `json:"name" jsonschema:"display name, can repeat"`
	NetworkName string `json:"network_name,omitempty" jsonschema:"unique network name (lowercase letters, digits and -); generated from the name when omitted"`
}

func toolCreateProject(c *call, in CreateProjectArgs) (any, error) {
	res, err := c.h.clients.projects.CreateProject(c.ctx, connect.NewRequest(&nagiv1.CreateProjectRequest{Name: in.Name, NetworkName: in.NetworkName}))
	if err != nil {
		return nil, err
	}
	p := res.Msg
	return map[string]string{"id": p.GetId(), "name": p.GetName(), "network_name": p.GetNetworkName()}, nil
}

// ---- 添加容器 ----

type portArg struct {
	Published uint32 `json:"published" jsonschema:"port published on the node. Containers in the project reach each other without this; for HTTP prefer save_route"`
	Target    uint32 `json:"target" jsonschema:"port inside the container"`
	Protocol  string `json:"protocol,omitempty" jsonschema:"tcp (default) or udp"`
}

type AddContainerArgs struct {
	ProjectArg
	Name    string            `json:"name,omitempty" jsonschema:"container name (lowercase letters, digits and -); taken from the image when omitted; required with compose"`
	Image   string            `json:"image,omitempty" jsonschema:"image to run, e.g. nginx:1.27; use either image or compose"`
	Compose string            `json:"compose,omitempty" jsonschema:"a whole compose file, added as a container group"`
	Command string            `json:"command,omitempty" jsonschema:"command for an image container"`
	Ports   []portArg         `json:"ports,omitempty"`
	Env     map[string]string `json:"env,omitempty" jsonschema:"environment variables of an image container, or ${VARIABLES} of a compose file"`
	Volumes []string          `json:"volumes,omitempty" jsonschema:"paths inside an image container to persist, e.g. /var/lib/postgresql/data"`
	Node    string            `json:"node,omitempty" jsonschema:"node name or ID; default is the online node running most of the project's containers"`
	Dir     string            `json:"dir,omitempty" jsonschema:"container directory on the node; default <projects root>/<network name>/<container>"`
	Deploy  bool              `json:"deploy,omitempty" jsonschema:"deploy right away; default false (call deploy separately)"`
	configArgs
}

// configArgs 是添加和修改容器共用的配置项（update_container 里省略表示不变）。
type configArgs struct {
	Workdir               *string  `json:"workdir,omitempty" jsonschema:"image-form containers only: mount the container's data/ at this path inside the container as its working directory; empty string stops mounting it"`
	MemoryMB              *uint32  `json:"memory_mb,omitempty" jsonschema:"image-form containers only; 0 removes the limit"`
	CPUs                  *float64 `json:"cpus,omitempty" jsonschema:"image-form containers only; 0 removes the limit"`
	HealthCommand         *string  `json:"health_command,omitempty" jsonschema:"image-form containers only: command run with sh -c inside the container, exit code 0 means healthy; empty string removes the check"`
	HealthIntervalSeconds *uint32  `json:"health_interval_seconds,omitempty" jsonschema:"image-form containers only; 0 means the default (30)"`
	GitURL                *string  `json:"git_url,omitempty" jsonschema:"git repository whose files go into the container's data/ (SSH with the container's deploy key, or public HTTPS); empty string removes the source"`
	GitRef                *string  `json:"git_ref,omitempty" jsonschema:"branch or tag; empty means the repository's default branch"`
	Preserve              []string `json:"preserved_paths,omitempty" jsonschema:"paths relative to data/ kept when files are updated, e.g. uploads/; replaces the list, [] clears it"`
	Stateful              string   `json:"stateful,omitempty" jsonschema:"auto (default: named volumes or preserved paths make it stateful), yes or no. Stateful containers are paused for snapshots and get a predeploy snapshot"`
	DependsOn             []string `json:"depends_on,omitempty" jsonschema:"containers deployed first when deployed together; replaces the list, [] clears it"`
}

var statefulValues = map[string]nagiv1.Stateful{"auto": nagiv1.Stateful_STATEFUL_AUTO, "yes": nagiv1.Stateful_STATEFUL_YES, "no": nagiv1.Stateful_STATEFUL_NO}

// apply 把配置项写进 cfg，返回改了什么。
func (a configArgs) apply(cfg *nagiv1.ContainerConfig) ([]string, error) {
	var changed []string
	if f := cfg.GetForm(); f != nil {
		if a.Workdir != nil && *a.Workdir != f.GetWorkdir() {
			f.Workdir = *a.Workdir
			changed = append(changed, "workdir → "+*a.Workdir)
		}
		if a.MemoryMB != nil && *a.MemoryMB != f.GetMemoryMb() {
			f.MemoryMb = *a.MemoryMB
			changed = append(changed, fmt.Sprintf("memory_mb → %d", *a.MemoryMB))
		}
		if a.CPUs != nil && *a.CPUs != f.GetCpus() {
			f.Cpus = *a.CPUs
			changed = append(changed, fmt.Sprintf("cpus → %g", *a.CPUs))
		}
		if a.HealthCommand != nil && *a.HealthCommand != f.GetHealthCommand() {
			f.HealthCommand = *a.HealthCommand
			changed = append(changed, "health_command")
		}
		if a.HealthIntervalSeconds != nil && *a.HealthIntervalSeconds != f.GetHealthIntervalSeconds() {
			f.HealthIntervalSeconds = *a.HealthIntervalSeconds
			changed = append(changed, fmt.Sprintf("health_interval_seconds → %d", *a.HealthIntervalSeconds))
		}
	} else if a.Workdir != nil || a.MemoryMB != nil || a.CPUs != nil || a.HealthCommand != nil || a.HealthIntervalSeconds != nil {
		return nil, errors.New("workdir, memory_mb, cpus and health checks are for image-form containers; write them in the compose file of a container group")
	}
	if a.GitURL != nil || a.GitRef != nil {
		src := &nagiv1.Source{GitUrl: cfg.GetSource().GetGitUrl(), GitRef: cfg.GetSource().GetGitRef()}
		if a.GitURL != nil {
			src.GitUrl = *a.GitURL
		}
		if a.GitRef != nil {
			src.GitRef = *a.GitRef
		}
		if src.GetGitUrl() == "" {
			src = nil
		}
		if sourceText(src) != sourceText(cfg.GetSource()) {
			cfg.Source = src
			if src == nil {
				changed = append(changed, "source removed")
			} else {
				changed = append(changed, "source → "+sourceText(src))
			}
		}
	}
	if a.Preserve != nil && !slices.Equal(a.Preserve, cfg.GetPreserve()) {
		cfg.Preserve = a.Preserve
		changed = append(changed, "preserved_paths")
	}
	if a.Stateful != "" {
		v, ok := statefulValues[strings.ToLower(a.Stateful)]
		if !ok {
			return nil, fmt.Errorf("stateful must be auto, yes or no, not %q", a.Stateful)
		}
		if v != cfg.GetStateful() {
			cfg.Stateful = v
			changed = append(changed, "stateful → "+strings.ToLower(a.Stateful))
		}
	}
	if a.DependsOn != nil && !slices.Equal(a.DependsOn, cfg.GetDependsOn()) {
		cfg.DependsOn = a.DependsOn
		changed = append(changed, "depends_on")
	}
	return changed, nil
}

var nameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// imageName 取镜像名的最后一段作容器名，和网页的添加容器一样。
func imageName(image string) string {
	s := image
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	s, _, _ = strings.Cut(s, "@")
	s, _, _ = strings.Cut(s, ":")
	return strings.Trim(nameChars.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func envList(m map[string]string) []*nagiv1.EnvVar {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]*nagiv1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, &nagiv1.EnvVar{Key: k, Value: m[k]})
	}
	return out
}

func portList(ps []portArg) []*nagiv1.PortMapping {
	out := make([]*nagiv1.PortMapping, 0, len(ps))
	for _, p := range ps {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		out = append(out, &nagiv1.PortMapping{Published: p.Published, Target: p.Target, Protocol: proto})
	}
	return out
}

// defaultNode：项目里容器最多的在线节点，项目还没有容器时用第一个在线节点（和网页的添加容器一样）。
func defaultNode(p *nagiv1.Project, nodes *nagiv1.ListNodesResponse) (*nagiv1.Node, error) {
	usable := map[string]*nagiv1.Node{}
	var first *nagiv1.Node
	for _, n := range nodes.GetNodes() {
		if n.GetOnline() && !n.GetPending() {
			usable[n.GetId()] = n
			if first == nil {
				first = n
			}
		}
	}
	count := map[string]int{}
	var best *nagiv1.Node
	for _, m := range p.GetContainers() {
		if n := usable[m.GetNodeId()]; n != nil {
			count[n.GetId()]++
			if best == nil || count[n.GetId()] > count[best.GetId()] {
				best = n
			}
		}
	}
	if best != nil {
		return best, nil
	}
	if first == nil {
		return nil, errors.New("no online node is available")
	}
	return first, nil
}

func toolAddContainer(c *call, in AddContainerArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	nodes, err := c.nodes()
	if err != nil {
		return nil, err
	}
	var node *nagiv1.Node
	if in.Node != "" {
		node, err = findNode(nodes, in.Node)
	} else {
		node, err = defaultNode(s.GetProject(), nodes)
	}
	if err != nil {
		return nil, err
	}
	cfg := &nagiv1.ContainerConfig{}
	name := in.Name
	switch {
	case in.Image != "" && in.Compose != "":
		return nil, errors.New("give either image or compose, not both")
	case in.Compose != "":
		if name == "" {
			return nil, errors.New("name is required for a compose container group")
		}
		cfg.Compose, cfg.Env = in.Compose, envList(in.Env)
	case in.Image != "":
		if name == "" {
			name = imageName(in.Image)
		}
		cfg.Form = &nagiv1.ImageForm{Image: in.Image, Command: in.Command, Ports: portList(in.Ports), Env: envList(in.Env), Volumes: in.Volumes}
	default:
		return nil, errors.New("give an image or a compose file")
	}
	if _, err := in.configArgs.apply(cfg); err != nil {
		return nil, err
	}
	pid := s.GetProject().GetId()
	if _, err := c.h.clients.projects.AddContainer(c.ctx, connect.NewRequest(&nagiv1.AddContainerRequest{
		ProjectId: pid, Name: name, Config: cfg, NodeId: node.GetId(), Dir: in.Dir,
	})); err != nil {
		return nil, err
	}
	out := struct {
		Container string    `json:"container"`
		Node      string    `json:"node"`
		Deploy    *opResult `json:"deploy,omitempty"`
	}{Container: name, Node: node.GetName()}
	if in.Deploy {
		r, err := c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
			return c.h.clients.projects.Deploy(ctx, connect.NewRequest(&nagiv1.DeployRequest{ProjectId: pid, Targets: []*nagiv1.DeployTarget{{Container: name}}}))
		})
		if err != nil {
			return nil, err
		}
		out.Deploy = &r
	}
	return out, nil
}

// ---- 修改配置 ----

type UpdateContainerArgs struct {
	ContainerArg
	ExpectedRevision int64             `json:"expected_revision,omitempty" jsonschema:"the container's revision from get_container; the change is refused if someone changed the container since"`
	Image            *string           `json:"image,omitempty" jsonschema:"image-form containers only"`
	Command          *string           `json:"command,omitempty" jsonschema:"image-form containers only; empty string clears it"`
	Ports            []portArg         `json:"ports,omitempty" jsonschema:"image-form containers only; replaces all ports"`
	Volumes          []string          `json:"volumes,omitempty" jsonschema:"image-form containers only: paths inside the container to persist; replaces the list. Removing a path does not delete its data"`
	Env              map[string]string `json:"env,omitempty" jsonschema:"set these variables (image form: environment variables; container group: compose ${VARIABLES})"`
	EnvUnset         []string          `json:"env_unset,omitempty" jsonschema:"remove these variables"`
	EnvSecret        []string          `json:"env_secret,omitempty" jsonschema:"mark these variables as secret (hidden by default in the web UI)"`
	// 服务级环境变量（容器组）：只注入对应服务。
	ServiceEnv      map[string]map[string]string `json:"service_env,omitempty" jsonschema:"container groups only: service name to environment variables injected only into that service"`
	ServiceEnvUnset map[string][]string          `json:"service_env_unset,omitempty" jsonschema:"container groups only: service name to service environment variables to remove"`
	Compose         string                       `json:"compose,omitempty" jsonschema:"container groups only: the whole new compose file"`
	configArgs
}

// patchEnv 设置、删除变量，保留没提到的变量（以及它们是否为密钥）。
func patchEnv(vars []*nagiv1.EnvVar, set map[string]string, unset []string) ([]*nagiv1.EnvVar, []string) {
	var changed []string
	out := make([]*nagiv1.EnvVar, 0, len(vars)+len(set))
	for _, v := range vars {
		if slices.Contains(unset, v.GetKey()) {
			changed = append(changed, "removed "+v.GetKey())
			continue
		}
		if nv, ok := set[v.GetKey()]; ok {
			if nv != v.GetValue() {
				changed = append(changed, "set "+v.GetKey())
			}
			v = &nagiv1.EnvVar{Key: v.GetKey(), Value: nv, Secret: v.GetSecret()}
		}
		out = append(out, v)
	}
	for _, v := range envList(set) {
		if !slices.ContainsFunc(vars, func(x *nagiv1.EnvVar) bool { return x.GetKey() == v.GetKey() }) {
			out = append(out, v)
			changed = append(changed, "added "+v.GetKey())
		}
	}
	return out, changed
}

func toolUpdateContainer(c *call, in UpdateContainerArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	base := m.GetConfig()
	if m.GetHasDraft() {
		base = m.GetDraft()
	}
	if base == nil {
		return nil, errors.New("the container's configuration is not available")
	}
	cfg := proto.Clone(base).(*nagiv1.ContainerConfig)
	var changed []string
	if f := cfg.GetForm(); f != nil {
		if in.Compose != "" {
			return nil, errors.New("this is an image-form container: change image, command, ports or env instead of compose")
		}
		if in.Image != nil && *in.Image != f.GetImage() {
			f.Image = *in.Image
			changed = append(changed, "image → "+*in.Image)
		}
		if in.Command != nil && *in.Command != f.GetCommand() {
			f.Command = *in.Command
			changed = append(changed, "command")
		}
		if in.Ports != nil {
			f.Ports = portList(in.Ports)
			changed = append(changed, "ports")
		}
		if in.Volumes != nil && !slices.Equal(in.Volumes, f.GetVolumes()) {
			f.Volumes = in.Volumes
			changed = append(changed, "volumes")
		}
		if in.ServiceEnv != nil || in.ServiceEnvUnset != nil {
			return nil, errors.New("this is an image-form container: use env instead of service_env")
		}
		var envChanged []string
		f.Env, envChanged = patchEnv(f.GetEnv(), in.Env, in.EnvUnset)
		changed = append(changed, envChanged...)
		if f.Env, envChanged, err = markSecret(f.Env, in.EnvSecret); err != nil {
			return nil, err
		}
		changed = append(changed, envChanged...)
	} else {
		if in.Image != nil || in.Command != nil || in.Ports != nil || in.Volumes != nil {
			return nil, errors.New("this is a container group: change its compose file instead")
		}
		if in.Compose != "" && in.Compose != cfg.GetCompose() {
			cfg.Compose = in.Compose
			changed = append(changed, "compose")
		}
		var envChanged []string
		cfg.Env, envChanged = patchEnv(cfg.GetEnv(), in.Env, in.EnvUnset)
		changed = append(changed, envChanged...)
		if cfg.Env, envChanged, err = markSecret(cfg.Env, in.EnvSecret); err != nil {
			return nil, err
		}
		changed = append(changed, envChanged...)
		services := map[string]bool{}
		for svc := range in.ServiceEnv {
			services[svc] = true
		}
		for svc := range in.ServiceEnvUnset {
			services[svc] = true
		}
		if len(services) > 0 && cfg.ServiceEnv == nil {
			cfg.ServiceEnv = map[string]*nagiv1.EnvList{}
		}
		for _, svc := range slices.Sorted(maps.Keys(services)) {
			vars, svcChanged := patchEnv(cfg.GetServiceEnv()[svc].GetVars(), in.ServiceEnv[svc], in.ServiceEnvUnset[svc])
			for _, ch := range svcChanged {
				changed = append(changed, svc+": "+ch)
			}
			if len(vars) == 0 {
				delete(cfg.ServiceEnv, svc)
			} else {
				cfg.ServiceEnv[svc] = &nagiv1.EnvList{Vars: vars}
			}
		}
	}
	more, err := in.configArgs.apply(cfg)
	if err != nil {
		return nil, err
	}
	changed = append(changed, more...)
	if len(changed) == 0 {
		return map[string]any{"changed": []string{}, "note": "nothing changed"}, nil
	}
	if _, err := c.h.clients.projects.SaveDraft(c.ctx, connect.NewRequest(&nagiv1.SaveDraftRequest{
		ProjectId: s.GetProject().GetId(), Container: m.GetName(), BaseRevision: baseRevision(in.ExpectedRevision, m), Config: cfg,
	})); err != nil {
		return nil, err
	}
	return map[string]any{"changed": changed, "note": "saved as a draft; call deploy to apply it"}, nil
}

// baseRevision 是修改基于的修订号：agent 给了就用它（面板上已经不是这个修订号时拒绝，免得覆盖别人刚做的修改），
// 没给就用刚读到的。
func baseRevision(expected int64, m *nagiv1.Container) int64 {
	if expected > 0 {
		return expected
	}
	return m.GetRevision()
}

type DiscardArgs struct {
	ContainerArg
	ExpectedRevision int64 `json:"expected_revision,omitempty" jsonschema:"the container's revision from get_container"`
}

func toolDiscardDraft(c *call, in DiscardArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	if !m.GetHasDraft() {
		return map[string]string{"note": "the container has no draft"}, nil
	}
	if _, err := c.h.clients.projects.DiscardDraft(c.ctx, connect.NewRequest(&nagiv1.ContainerRevisionRef{
		ProjectId: s.GetProject().GetId(), Container: m.GetName(), BaseRevision: baseRevision(in.ExpectedRevision, m),
	})); err != nil {
		return nil, err
	}
	return map[string]string{"note": "draft discarded"}, nil
}

// ---- 部署、启停、回滚、快照 ----

type DeployArgs struct {
	ProjectArg
	Containers []string `json:"containers,omitempty" jsonschema:"containers to deploy; default: every container with a draft"`
	PullImages bool     `json:"pull_images,omitempty" jsonschema:"pull the newest images first (update images)"`
	PullGit    bool     `json:"pull_git,omitempty" jsonschema:"pull files from the git repository first"`
	// 共享变量的草稿（update_shared_env）随这次部署生效。
	ApplySharedEnv          bool  `json:"apply_shared_env,omitempty" jsonschema:"apply the shared variables draft first (see update_shared_env); name the affected containers to redeploy them, or none to only apply it"`
	ExpectedProjectRevision int64 `json:"expected_project_revision,omitempty" jsonschema:"with apply_shared_env: the project's revision from get_project"`
	// 修订号：部署的是 agent 看过的那份草稿，期间被别人改过就拒绝。
	ExpectedRevisions  map[string]int64 `json:"expected_revisions,omitempty" jsonschema:"container name to its revision from get_container; the deploy is refused if one of them changed since"`
	WaitHealthySeconds int              `json:"wait_healthy_seconds,omitempty" jsonschema:"after a successful deploy, wait up to this many seconds (at most 300) for the containers to be running and, if they have a Docker health check, healthy"`
}

func toolDeploy(c *call, in DeployArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	var targets []*nagiv1.DeployTarget
	if len(in.Containers) == 0 && !in.ApplySharedEnv {
		for _, m := range p.GetContainers() {
			if m.GetHasDraft() {
				targets = append(targets, &nagiv1.DeployTarget{Container: m.GetName()})
			}
		}
		if len(targets) == 0 {
			return nil, errors.New("no container has a draft; name the containers to redeploy")
		}
	}
	for _, ref := range in.Containers {
		m, err := findContainer(p, ref)
		if err != nil {
			return nil, err
		}
		targets = append(targets, &nagiv1.DeployTarget{Container: m.GetName()})
	}
	for _, t := range targets {
		t.BaseRevision = in.ExpectedRevisions[t.GetContainer()]
	}
	res, err := c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.Deploy(ctx, connect.NewRequest(&nagiv1.DeployRequest{ProjectId: p.GetId(), Targets: targets, PullImages: in.PullImages, Pull: in.PullGit,
			ApplySharedEnv: in.ApplySharedEnv, SettingsRevision: projectRevision(in.ExpectedProjectRevision, p)}))
	})
	if err != nil || !res.Success || in.WaitHealthySeconds <= 0 || len(targets) == 0 {
		return res, err
	}
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.GetContainer())
	}
	health, err := c.waitHealthy(p.GetId(), names, time.Duration(min(in.WaitHealthySeconds, 300))*time.Second)
	if err != nil {
		return nil, err
	}
	return map[string]any{"deploy": res, "health": health}, nil
}

type healthResult struct {
	Passed   bool          `json:"passed"`
	TimedOut bool          `json:"timed_out,omitempty"`
	Reason   string        `json:"reason,omitempty"`
	Services []serviceView `json:"services"`
}

// waitHealthy 等部署的容器都在运行，有健康检查的变成 healthy。有服务 unhealthy 或者不在运行就不算通过。
// 只回报结果，不自动回滚：有状态的容器回滚配置不等于回滚数据，由 agent 和用户决定。
func (c *call) waitHealthy(projectID string, containers []string, wait time.Duration) (healthResult, error) {
	deadline := time.Now().Add(wait)
	for {
		s, err := c.getProject(projectID)
		if err != nil {
			return healthResult{}, err
		}
		out := healthResult{Passed: true, Services: []serviceView{}}
		pending := false
		for _, name := range containers {
			svcs := services(s, name)
			if len(svcs) == 0 {
				pending = true
				out.Reason = name + ": no running service yet"
			}
			for _, v := range svcs {
				out.Services = append(out.Services, v)
				switch {
				case v.State != "running":
					out.Passed, out.Reason = false, fmt.Sprintf("%s/%s is %s (%s)", name, v.Name, v.State, v.Status)
				case v.Health == "unhealthy":
					out.Passed, out.Reason = false, fmt.Sprintf("%s/%s health check is failing", name, v.Name)
				case v.Health == "starting":
					pending = true
					out.Reason = fmt.Sprintf("%s/%s health check is still starting", name, v.Name)
				}
			}
		}
		if !out.Passed {
			return out, nil
		}
		if !pending {
			out.Reason = ""
			return out, nil
		}
		if time.Now().After(deadline) {
			out.Passed, out.TimedOut = false, true
			return out, nil
		}
		select {
		case <-c.ctx.Done():
			return healthResult{}, c.ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

type ControlArgs struct {
	ContainerArg
	Action string `json:"action" jsonschema:"start, stop or restart"`
}

func toolControl(c *call, in ControlArgs) (any, error) {
	actions := map[string]nagiv1.ContainerAction{
		"start": nagiv1.ContainerAction_CONTAINER_ACTION_START, "stop": nagiv1.ContainerAction_CONTAINER_ACTION_STOP, "restart": nagiv1.ContainerAction_CONTAINER_ACTION_RESTART,
	}
	action, ok := actions[strings.ToLower(in.Action)]
	if !ok {
		return nil, errors.New("action must be start, stop or restart")
	}
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	return c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.ControlContainer(ctx, connect.NewRequest(&nagiv1.ControlContainerRequest{ProjectId: s.GetProject().GetId(), Container: m.GetName(), Action: action}))
	})
}

type RollbackArgs struct {
	ContainerArg
	Version int64 `json:"version" jsonschema:"version to go back to (see get_container's recent_versions)"`
	// 同时恢复数据：用这个版本被替换前拍的部署前快照，之后写入的数据会丢，所以要先预览。
	RestoreData bool   `json:"restore_data,omitempty" jsonschema:"also restore the data (volumes and preserved paths) to how it was before this version was replaced, from the predeploy snapshot; data written since is lost. Needs confirm"`
	Confirm     string `json:"confirm,omitempty" jsonschema:"with restore_data: omit to get a preview, then repeat the version here (as text) after the user agreed"`
}

func toolRollback(c *call, in RollbackArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	if v := strconv.FormatInt(in.Version, 10); in.RestoreData && in.Confirm != v {
		return preview("rollback", v, fmt.Sprintf("container %s goes back to version %d (configuration, files and image) and its data is restored to how it was before version %d was replaced. Data written since then is lost. The container is stopped during the restore.",
			m.GetName(), in.Version, in.Version), nil), nil
	}
	return c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.Rollback(ctx, connect.NewRequest(&nagiv1.RollbackRequest{ProjectId: s.GetProject().GetId(), Container: m.GetName(), Version: in.Version, RestoreData: in.RestoreData}))
	})
}

func toolSnapshot(c *call, in ContainerArg) (any, error) {
	s, m, err := c.container(in)
	if err != nil {
		return nil, err
	}
	return c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.CreateSnapshot(ctx, connect.NewRequest(&nagiv1.ContainerRef{ProjectId: s.GetProject().GetId(), Container: m.GetName()}))
	})
}

// ---- 端口映射 ----

type RouteArgs struct {
	ProjectArg
	Name      string   `json:"name,omitempty" jsonschema:"name of an existing mapping to change; omit to create one"`
	Container string   `json:"container,omitempty" jsonschema:"container the traffic goes to (required for a new mapping)"`
	Service   string   `json:"service,omitempty" jsonschema:"service inside a container group"`
	Port      uint32   `json:"port,omitempty" jsonschema:"port inside the container (required for a new mapping)"`
	Protocol  string   `json:"protocol,omitempty" jsonschema:"http (default), tcp or udp"`
	Domains   []string `json:"domains,omitempty" jsonschema:"domains for http; HTTPS certificates are issued automatically"`
	Published uint32   `json:"published_port,omitempty" jsonschema:"public port for tcp/udp"`
	HTTPOnly  *bool    `json:"http_only,omitempty" jsonschema:"serve plain HTTP without HTTPS"`
	// 跨节点入口（见 docs/exposure.md#跨节点入口）。
	EntryNodes   []string `json:"entry_nodes,omitempty" jsonschema:"also take traffic in on these nodes (names or IDs) besides the container's own node; replaces the list, [] clears it"`
	NoLocalEntry *bool    `json:"no_local_entry,omitempty" jsonschema:"take traffic in only on entry_nodes, not on the container's own node"`
}

func toolSaveRoute(c *call, in RouteArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	r := &nagiv1.Exposure{Protocol: "http"}
	if in.Name != "" {
		i := slices.IndexFunc(p.GetRoutes(), func(x *nagiv1.Exposure) bool { return x.GetName() == in.Name })
		if i < 0 {
			return nil, fmt.Errorf("port mapping %q not found", in.Name)
		}
		r = proto.Clone(p.GetRoutes()[i]).(*nagiv1.Exposure)
		r.Error = ""
	}
	if in.Container != "" {
		m, err := findContainer(p, in.Container)
		if err != nil {
			return nil, err
		}
		r.Container = m.GetName()
	}
	if in.Service != "" {
		r.Service = in.Service
	}
	if in.Port != 0 {
		r.Target = in.Port
	}
	if in.Protocol != "" {
		r.Protocol = strings.ToLower(in.Protocol)
	}
	if in.Domains != nil {
		r.Domains = in.Domains
	}
	if in.Published != 0 {
		r.Published = in.Published
	}
	if in.HTTPOnly != nil {
		r.HttpOnly = *in.HTTPOnly
	}
	if in.EntryNodes != nil {
		nodes, err := c.nodes()
		if err != nil {
			return nil, err
		}
		r.EntryNodes = []string{}
		for _, ref := range in.EntryNodes {
			n, err := findNode(nodes, ref)
			if err != nil {
				return nil, err
			}
			r.EntryNodes = append(r.EntryNodes, n.GetId())
		}
	}
	if in.NoLocalEntry != nil {
		r.NoLocalEntry = *in.NoLocalEntry
	}
	if r.GetContainer() == "" || r.GetTarget() == 0 {
		return nil, errors.New("container and port are required for a new mapping")
	}
	res, err := c.h.clients.projects.SaveRoute(c.ctx, connect.NewRequest(&nagiv1.SaveRouteRequest{ProjectId: p.GetId(), Route: r}))
	if err != nil {
		return nil, err
	}
	out := []routeView{}
	for _, x := range res.Msg.GetRoutes() {
		if x.GetContainer() == r.GetContainer() {
			out = append(out, routeOf(x))
		}
	}
	return map[string]any{"routes": out}, nil
}

type DeleteRouteArgs struct {
	ProjectArg
	Name    string `json:"name" jsonschema:"port mapping name from get_project"`
	Confirm string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat the name here to delete after the user agreed"`
}

func toolDeleteRoute(c *call, in DeleteRouteArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	i := slices.IndexFunc(p.GetRoutes(), func(x *nagiv1.Exposure) bool { return x.GetName() == in.Name })
	if i < 0 {
		return nil, fmt.Errorf("port mapping %q not found", in.Name)
	}
	r := p.GetRoutes()[i]
	if in.Confirm != in.Name {
		return map[string]any{"preview": "this port mapping will be removed immediately; its domains and ports stop reaching the container", "route": routeOf(r),
			"next": "show this to the user; after they agree, call delete_route again with confirm set to the name"}, nil
	}
	if _, err := c.h.clients.projects.DeleteRoute(c.ctx, connect.NewRequest(&nagiv1.DeleteRouteRequest{ProjectId: p.GetId(), Name: in.Name})); err != nil {
		return nil, err
	}
	return map[string]string{"note": "port mapping deleted"}, nil
}

type RetryCertArgs struct {
	ProjectArg
	Route  string `json:"route" jsonschema:"port mapping name from check_routes"`
	Domain string `json:"domain"`
	Node   string `json:"node,omitempty" jsonschema:"entry node name or ID; default: every entry node where the certificate is not obtained"`
}

func toolRetryCert(c *call, in RetryCertArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	var nodeIDs []string
	if in.Node != "" {
		nodes, err := c.nodes()
		if err != nil {
			return nil, err
		}
		n, err := findNode(nodes, in.Node)
		if err != nil {
			return nil, err
		}
		nodeIDs = []string{n.GetId()}
	} else {
		i := slices.IndexFunc(p.GetRoutes(), func(x *nagiv1.Exposure) bool { return x.GetName() == in.Route })
		if i < 0 {
			return nil, fmt.Errorf("port mapping %q not found", in.Route)
		}
		res, err := c.h.clients.projects.CheckDomains(c.ctx, connect.NewRequest(&nagiv1.CheckDomainsRequest{ProjectId: p.GetId(), Routes: []*nagiv1.Exposure{p.GetRoutes()[i]}}))
		if err != nil {
			return nil, err
		}
		for _, d := range res.Msg.GetDomains() {
			if d.GetDomain() != in.Domain {
				continue
			}
			for _, e := range d.GetEntries() {
				if e.GetCert() == "pending" || e.GetCert() == "failed" {
					nodeIDs = append(nodeIDs, e.GetNodeId())
				}
			}
		}
		if len(nodeIDs) == 0 {
			return map[string]string{"note": "no entry node is waiting for this certificate; see check_routes"}, nil
		}
	}
	for _, id := range nodeIDs {
		if _, err := c.h.clients.projects.RetryDomainCert(c.ctx, connect.NewRequest(&nagiv1.RetryDomainCertRequest{
			ProjectId: p.GetId(), Route: in.Route, Domain: in.Domain, NodeId: id,
		})); err != nil {
			return nil, err
		}
	}
	return map[string]any{"retried_on": nodeIDs, "note": "the entry nodes are requesting the certificate again; check_routes shows the result in a minute. Let's Encrypt allows 5 failed validations per domain per hour"}, nil
}

// ---- 执行命令 ----

type ExecArgs struct {
	ContainerArg
	Service        string `json:"service,omitempty" jsonschema:"service inside a container group"`
	Command        string `json:"command" jsonschema:"shell command, run with sh -c"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"default 30, at most 120"`
}

func toolExec(c *call, in ExecArgs) (any, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, errors.New("command is empty")
	}
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	timeout := 30 * time.Second
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(min(in.TimeoutSeconds, 120)) * time.Second
	}
	// RunCommand 不经过接口的拦截器，自己按当前用户检查权限。
	ctx := auth.WithUser(c.ctx, c.u)
	res, err := c.h.o.Exec(ctx, s.GetProject().GetId(), m.GetName(), in.Service, []string{"sh", "-c", in.Command}, timeout)
	if err != nil {
		return nil, err
	}
	return struct {
		ExitCode  int32  `json:"exit_code"`
		TimedOut  bool   `json:"timed_out,omitempty"`
		Truncated bool   `json:"output_truncated,omitempty"`
		Output    string `json:"output"`
	}{res.ExitCode, res.TimedOut, res.Truncated, res.Output}, nil
}

// ---- 节点命令 ----

type NodeExecArgs struct {
	Node           string `json:"node" jsonschema:"node name or ID (see list_nodes)"`
	Command        string `json:"command" jsonschema:"shell command, run as root with the login shell (sh -lc)"`
	Cwd            string `json:"cwd,omitempty" jsonschema:"working directory; default root's home"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"default 30, at most 120; the whole process group is killed at the timeout"`
}

// toolNodeExec 在节点上执行一条命令（见 docs/mcp.md#节点命令）。权限在 api.RunNodeCommand 里再查一遍。
func toolNodeExec(c *call, in NodeExecArgs) (any, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, errors.New("command is empty")
	}
	nodes, err := c.nodes()
	if err != nil {
		return nil, err
	}
	n, err := findNode(nodes, in.Node)
	if err != nil {
		return nil, err
	}
	timeout := 30 * time.Second
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(min(in.TimeoutSeconds, 120)) * time.Second
	}
	res, err := c.h.o.NodeExec(auth.WithUser(c.ctx, c.u), n.GetId(), in.Command, in.Cwd, timeout)
	if err != nil {
		return nil, err
	}
	return struct {
		Node      string `json:"node"`
		ExitCode  int32  `json:"exit_code"`
		TimedOut  bool   `json:"timed_out,omitempty"`
		Truncated bool   `json:"output_truncated,omitempty"`
		Output    string `json:"output"`
		Note      string `json:"note,omitempty"`
	}{n.GetName(), res.ExitCode, res.TimedOut, res.Truncated, res.Output, inContainerNote(n)}, nil
}

// inContainerNote：节点跑在容器里时，命令进的是这个容器，不是主机。
func inContainerNote(n *nagiv1.Node) string {
	if n.GetInfo().GetInContainer() {
		return "this node runs inside a container: the command ran in that container, not on the host"
	}
	return ""
}

// Package projects 是面板上的项目和容器：配置、草稿、版本记录、部署记录、端口映射都以面板为准
// （见 docs/panel.md#项目数据）。面板把容器的完整配置（ContainerSpec）下发给它所在的节点执行，
// 涉及多个容器的操作（一起部署、回到某次部署）由这里按顺序指挥。
package projects

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 错误类型，供接口层转换成对应的错误码。参数错误用 spec.ErrInvalid。
var (
	ErrNotFound = errors.New("项目或容器不存在")
	ErrConflict = errors.New("已被其他人修改，请刷新后重试")
)

type notFoundError struct{ msg string }

func (e notFoundError) Error() string        { return e.msg }
func (e notFoundError) Is(target error) bool { return target == ErrNotFound }

func invalid(format string, args ...any) error { return spec.Invalid(format, args...) }

const (
	versionsKept    = 20
	deploymentsKept = 100
)

// Service 是面板上的项目和容器。
type Service struct {
	Q      *store.Queries
	Sealer *sealer.Sealer
	Nodes  *nodes.Registry
	Log    *slog.Logger
	// Fingerprint 返回本面板的公钥指纹，写在下发的配置里（ContainerSpec.panel）。
	Fingerprint func() string
	// ReservedDomain 返回 true 的域名已经绑定给面板，项目不能再用。
	ReservedDomain func(domain string) bool

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	// 每个节点上次下发的代理计划（连接 + 摘要），连接换了或计划变了才重新下发。
	linkSent map[string]linkSent
	// 从节点缓存拼回项目：每条连接只做一次，同一时间只有一次。
	rebuilt   map[*nodes.Conn]bool
	rebuildMu sync.Mutex
	// 上次尝试把某个容器的见证人换成本面板的时间。
	adoptTried map[string]time.Time
	// 以节点为准重建项目时，保留本面板原来的 webhook 令牌（项目 ID/容器名 → 令牌）。
	keepTokens map[string]string
	adoptedAt  map[string]time.Time
	// restamped：上次只为在节点缓存里记上本面板而重推的时间；warned：上次记某条警告的时间。
	restamped map[string]time.Time
	// unknown：节点上有、面板不认识、没有自动加入的容器（节点 ID → "项目/容器" → 容器）。
	unknown map[string]map[string]*nagiv1.UnknownContainer
	warned  map[string]time.Time
	// 第一次上报已经处理完的连接（之后才给它推代理计划）。
	synced map[*nodes.Conn]bool
}

// lock 返回项目的互斥锁：同一个项目的修改和部署串行执行。
func (s *Service) lock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	l, ok := s.locks[id]
	if !ok {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	return l
}

// ---- 加密 ----

func (s *Service) seal(m proto.Message, context string) ([]byte, error) {
	b, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	return s.Sealer.Seal(b, []byte(context))
}

func (s *Service) open(data []byte, context string, m proto.Message) error {
	b, err := s.Sealer.Open(data, []byte(context))
	if err != nil {
		return fmt.Errorf("解密 %s: %w", context, err)
	}
	return proto.Unmarshal(b, m)
}

func settingsCtx(id string) string      { return "project/" + id + "/settings" }
func sharedDraftCtx(id string) string   { return "project/" + id + "/shared-env-draft" }
func dockerCtx(id string) string        { return "project/" + id + "/docker" }
func draftCtx(id, name string) string   { return "container/" + id + "/" + name + "/draft" }
func secretsCtx(id, name string) string { return "container/" + id + "/" + name + "/secrets" }
func versionCtx(id, name string, v int64) string {
	return fmt.Sprintf("version/%s/%s/%d", id, name, v)
}

// secrets 是容器的密钥：webhook 令牌、deploy key 私钥、快照仓库密码。
type secrets struct {
	WebhookToken   string `json:"webhook_token"`
	DeployKey      []byte `json:"deploy_key"`
	BackupPassword string `json:"backup_password"`
}

func (s *Service) secrets(c store.Container) (secrets, error) {
	var out secrets
	b, err := s.Sealer.Open(c.Secrets, []byte(secretsCtx(c.ProjectID, c.Name)))
	if err != nil {
		return out, fmt.Errorf("解密容器 %s 的密钥: %w", c.Name, err)
	}
	return out, json.Unmarshal(b, &out)
}

func (s *Service) settings(p store.Project) (*nagiv1.ProjectSettings, error) {
	out := &nagiv1.ProjectSettings{}
	return out, s.open(p.Settings, settingsCtx(p.ID), out)
}

func (s *Service) sharedEnvDraft(p store.Project) ([]*nagiv1.EnvVar, bool, error) {
	if p.SharedEnvDraft == nil {
		return nil, false, nil
	}
	l := &nagiv1.EnvList{}
	return l.GetVars(), true, s.open(p.SharedEnvDraft, sharedDraftCtx(p.ID), l)
}

func (s *Service) dockerConfig(p store.Project) ([]byte, error) {
	if p.DockerConfig == nil {
		return nil, nil
	}
	return s.Sealer.Open(p.DockerConfig, []byte(dockerCtx(p.ID)))
}

func (s *Service) draft(c store.Container) (*nagiv1.ContainerConfig, error) {
	if c.Draft == nil {
		return nil, nil
	}
	cfg := &nagiv1.ContainerConfig{}
	return cfg, s.open(c.Draft, draftCtx(c.ProjectID, c.Name), cfg)
}

// versionData 解开一个版本的配置和当时的共享变量。
func (s *Service) versionData(v store.Version) (*nagiv1.ContainerConfig, []*nagiv1.EnvVar, error) {
	cfg := &nagiv1.ContainerConfig{}
	if err := s.open(v.Config, versionCtx(v.ProjectID, v.Container, v.Version)+"/config", cfg); err != nil {
		return nil, nil, err
	}
	env := &nagiv1.EnvList{}
	if err := s.open(v.SharedEnv, versionCtx(v.ProjectID, v.Container, v.Version)+"/shared-env", env); err != nil {
		return nil, nil, err
	}
	return cfg, env.GetVars(), nil
}

func (s *Service) sealVersion(id, name string, version int64, cfg *nagiv1.ContainerConfig, shared []*nagiv1.EnvVar) ([]byte, []byte, error) {
	c, err := s.seal(cfg, versionCtx(id, name, version)+"/config")
	if err != nil {
		return nil, nil, err
	}
	e, err := s.seal(&nagiv1.EnvList{Vars: shared}, versionCtx(id, name, version)+"/shared-env")
	return c, e, err
}

// ---- 读取 ----

func (s *Service) project(ctx context.Context, id string) (store.Project, error) {
	p, err := s.Q.GetProject(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return p, notFoundError{"项目不存在"}
	}
	return p, err
}

func (s *Service) container(ctx context.Context, id, name string) (store.Container, error) {
	c, err := s.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: id, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		return c, notFoundError{fmt.Sprintf("容器 %s 不存在", name)}
	}
	return c, err
}

// latest 返回容器最新版本的记录、配置和共享变量。
func (s *Service) latest(ctx context.Context, c store.Container) (store.Version, *nagiv1.ContainerConfig, []*nagiv1.EnvVar, error) {
	v, err := s.Q.LatestVersion(ctx, store.LatestVersionParams{ProjectID: c.ProjectID, Container: c.Name})
	if err != nil {
		return v, nil, nil, err
	}
	cfg, env, err := s.versionData(v)
	return v, cfg, env, err
}

// deployed 返回容器当前部署的版本；从未部署时返回 nil。
func (s *Service) deployed(ctx context.Context, c store.Container) (*store.Version, error) {
	if c.DeployedVersion == 0 {
		return nil, nil
	}
	v, err := s.Q.GetVersion(ctx, store.GetVersionParams{ProjectID: c.ProjectID, Container: c.Name, Version: c.DeployedVersion})
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// checkData 是面板上解析 compose 时用的 data/ 目录：用容器在节点上的真实路径，判断挂载的路径是不是在 data/ 下面。
func checkData(c store.Container) string { return path.Join(c.Dir, "data") }

// Get 返回项目和各容器的运行状态。
func (s *Service) Get(ctx context.Context, id string) (*nagiv1.ProjectSummary, error) {
	p, err := s.project(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.summary(ctx, p)
}

// List 返回所有项目。
func (s *Service) List(ctx context.Context) ([]*nagiv1.ProjectSummary, error) {
	rows, err := s.Q.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*nagiv1.ProjectSummary, 0, len(rows))
	for _, p := range rows {
		sum, err := s.summary(ctx, p)
		if err != nil {
			s.Log.Warn("读取项目失败", "project", p.ID, "err", err)
			continue
		}
		out = append(out, sum)
	}
	return out, nil
}

func (s *Service) summary(ctx context.Context, p store.Project) (*nagiv1.ProjectSummary, error) {
	proj, err := s.assemble(ctx, p)
	if err != nil {
		return nil, err
	}
	return &nagiv1.ProjectSummary{Project: proj, Status: s.status(proj)}, nil
}

// assemble 组装对外的项目信息：设置、容器（最新版本的配置和草稿）、映射。
func (s *Service) assemble(ctx context.Context, p store.Project) (*nagiv1.Project, error) {
	settings, err := s.settings(p)
	if err != nil {
		return nil, err
	}
	proj := &nagiv1.Project{
		Id: p.ID, Name: p.Name, NetworkName: p.NetworkName, Revision: p.Revision,
		CreatedAt: timestamppb.New(time.Unix(p.CreatedAt, 0)), Settings: settings,
	}
	if p.RebuiltAt > 0 {
		proj.RebuiltAt = timestamppb.New(time.Unix(p.RebuiltAt, 0))
	}
	if d, ok, err := s.sharedEnvDraft(p); err != nil {
		return nil, err
	} else if ok {
		proj.HasSharedEnvDraft, proj.SharedEnvDraft = true, d
	}
	rows, err := s.Q.ListContainers(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range rows {
		mem, err := s.containerInfo(ctx, c, settings)
		if err != nil {
			return nil, err
		}
		proj.Stateful = proj.Stateful || mem.Stateful
		proj.Containers = append(proj.Containers, mem)
	}
	routes, err := s.Q.ListRoutes(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range routes {
		proj.Routes = append(proj.Routes, routeProto(r))
	}
	if proj.Links, err = s.linkedProjects(ctx, p.ID); err != nil {
		return nil, err
	}
	return proj, nil
}

func (s *Service) containerInfo(ctx context.Context, c store.Container, settings *nagiv1.ProjectSettings) (*nagiv1.Container, error) {
	v, cfg, _, err := s.latest(ctx, c)
	if err != nil {
		return nil, err
	}
	draft, err := s.draft(c)
	if err != nil {
		return nil, err
	}
	sec, err := s.secrets(c)
	if err != nil {
		return nil, err
	}
	mem := &nagiv1.Container{
		Name: c.Name, Revision: c.Revision, LatestVersion: v.Version, DeployedVersion: c.DeployedVersion,
		CreatedAt: timestamppb.New(time.Unix(c.CreatedAt, 0)), Config: cfg, HasDraft: draft != nil, Draft: draft,
		DeployKey: c.DeployKeyPub, WebhookToken: sec.WebhookToken, LastCommit: c.LastCommit, Kind: spec.Kind(cfg),
		NodeId: c.NodeID, Dir: c.Dir, BackupNodeId: c.BackupNodeID,
	}
	if cfg := failoverOf(c); cfg != nil {
		mem.Failover = cfg
		mem.FailoverHolder = s.reportedFailover(c.NodeID, c.ProjectID, c.Name)
		mem.FailoverStandby = s.reportedFailover(standbyOf(cfg, c.NodeID).GetNodeId(), c.ProjectID, c.Name)
		if m := s.Nodes.Mesh(); m != nil && cfg.GetWitnessIsPanel() {
			self := m.PublicKey()
			mem.FailoverWitnessSelf = string(cfg.GetWitness().GetKey()) == string(self[:])
		}
	}
	if c.BackupNodeID != "" {
		if conn, err := s.Nodes.Peek(c.NodeID); err == nil {
			for _, cs := range conn.LastReport().GetContainers() {
				if cs.GetProjectId() == c.ProjectID && cs.GetName() == c.Name {
					mem.Backup = cs.GetBackup()
				}
			}
		}
	}
	if cp, err := spec.Load(ctx, cfg, c.Name, settings.GetSharedEnv(), checkData(c), spec.ComposeProjectName(c.ProjectID, c.Name)); err == nil {
		mem.Stateful = spec.IsStateful(cfg, cp, spec.PreservePaths(cfg, cp, checkData(c)))
		mem.UnmanagedVolumes, mem.HostNetworkServices = spec.UnmanagedVolumes(cp), spec.HostNetworkServices(cp)
	}
	if dv, err := s.deployed(ctx, c); err == nil && dv != nil {
		if dcfg, denv, err := s.versionData(*dv); err == nil {
			mem.SharedEnvOutdated = spec.SharedEnvChanged(dcfg, c.Name, denv, settings.GetSharedEnv())
		}
	}
	return mem, nil
}

// status 汇总各节点最近一次上报的容器状态。节点离线的容器只列出 compose 里的服务，没有运行状态。
func (s *Service) status(p *nagiv1.Project) *nagiv1.ProjectStatus {
	st := &nagiv1.ProjectStatus{}
	reports := map[string]map[string]*nagiv1.ContainerStatus{} // 节点 → 容器名 → 状态
	for _, c := range p.GetContainers() {
		if _, ok := reports[c.GetNodeId()]; ok {
			continue
		}
		m := map[string]*nagiv1.ContainerStatus{}
		if conn, err := s.Nodes.Peek(c.GetNodeId()); err == nil {
			for _, cs := range conn.LastReport().GetContainers() {
				if cs.GetProjectId() == p.GetId() {
					m[cs.GetName()] = cs
				}
			}
		}
		reports[c.GetNodeId()] = m
	}
	for _, c := range p.GetContainers() {
		cs := reports[c.GetNodeId()][c.GetName()]
		if cs != nil {
			st.Services = append(st.Services, cs.GetServices()...)
			st.ImageUpdates = append(st.ImageUpdates, cs.GetImageUpdates()...)
			continue
		}
		if cp, err := spec.Load(context.Background(), c.GetConfig(), c.GetName(), p.GetSettings().GetSharedEnv(), path.Join(c.GetDir(), "data"), spec.ComposeProjectName(p.GetId(), c.GetName())); err == nil {
			for _, name := range cp.ServiceNames() {
				st.Services = append(st.Services, &nagiv1.ServiceStatus{Container: c.GetName(), Name: name, Image: cp.Services[name].Image})
			}
		}
	}
	return st
}

// ---- 项目 ----

// DefaultNetworkName 由项目名生成网络名：小写字母、数字和 -，汉字转成拼音，不超过 32 个字符。项目名里没有能用的字符时返回空。
func DefaultNetworkName(name string) string { return spec.Slug(name) }

func checkProjectName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return "", invalid("项目名不能为空，且不超过 64 个字符")
	}
	return name, nil
}

// checkNetworkName 检查网络名的格式和在面板上唯一（except 是正在修改的项目）。
func (s *Service) checkNetworkName(ctx context.Context, name, except string) error {
	if err := spec.CheckNetworkName(name); err != nil {
		return err
	}
	if id, err := s.Q.ProjectByNetworkName(ctx, name); err == nil && id != except {
		return invalid("网络名 %s 已经被其他项目使用", name)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func cleanSettings(in *nagiv1.ProjectSettings) (*nagiv1.ProjectSettings, error) {
	out := &nagiv1.ProjectSettings{SnapshotIntervalMinutes: in.GetSnapshotIntervalMinutes()}
	ports, err := spec.CleanInternalPorts(in.GetInternalPorts())
	if err != nil {
		return nil, err
	}
	out.InternalPorts = ports
	if out.SnapshotIntervalMinutes > 7*24*60 {
		return nil, invalid("定时快照间隔不能超过 7 天")
	}
	return out, nil
}

// CreateRequest 是新建项目的参数。
type CreateRequest struct {
	Name, NetworkName string
	Settings          *nagiv1.ProjectSettings
	// 可选：第一个容器。
	Container *AddRequest
}

// Create 新建项目，可以同时加入第一个容器（只保存，不部署）。
func (s *Service) Create(ctx context.Context, req CreateRequest) (*nagiv1.Project, error) {
	name, err := checkProjectName(req.Name)
	if err != nil {
		return nil, err
	}
	network := strings.TrimSpace(req.NetworkName)
	if network == "" {
		if network = DefaultNetworkName(name); network == "" {
			return nil, invalid("项目名里没有英文字母或数字，请填写网络名（小写字母、数字和 -）")
		}
	}
	if err := s.checkNetworkName(ctx, network, ""); err != nil {
		return nil, err
	}
	settings, err := cleanSettings(req.Settings)
	if err != nil {
		return nil, err
	}
	if err := spec.CheckEnv(req.Settings.GetSharedEnv()); err != nil {
		return nil, err
	}
	settings.SharedEnv = req.Settings.GetSharedEnv()
	id, err := spec.RandomHex(8)
	if err != nil {
		return nil, err
	}
	sealed, err := s.seal(settings, settingsCtx(id))
	if err != nil {
		return nil, err
	}
	if err := s.Q.InsertProject(ctx, store.InsertProjectParams{ID: id, Name: name, NetworkName: network, CreatedAt: time.Now().Unix(), Settings: sealed}); err != nil {
		return nil, err
	}
	if req.Container != nil {
		req.Container.ProjectID = id
		if err := s.addContainer(ctx, *req.Container); err != nil {
			_ = s.Q.DeleteProject(ctx, id)
			return nil, err
		}
	}
	p, err := s.project(ctx, id)
	if err != nil {
		return nil, err
	}
	s.Log.Info("项目已创建", "project", id, "name", name)
	return s.assemble(ctx, p)
}

func checkRevision(have, base int64) error {
	if base != 0 && base != have {
		return ErrConflict
	}
	return nil
}

// UpdateSettings 修改项目的名称、网络名、快照间隔、内部端口，立即生效（更新各节点上的缓存）。共享变量不在这里改。
func (s *Service) UpdateSettings(ctx context.Context, id string, base int64, name, network string, in *nagiv1.ProjectSettings) (*nagiv1.Project, error) {
	name, err := checkProjectName(name)
	if err != nil {
		return nil, err
	}
	network = strings.TrimSpace(network)
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkRevision(p.Revision, base); err != nil {
		return nil, err
	}
	if network == "" {
		network = p.NetworkName
	}
	if err := s.checkNetworkName(ctx, network, id); err != nil {
		return nil, err
	}
	if network != p.NetworkName {
		if err := s.checkRenameNetwork(ctx, id, network); err != nil {
			return nil, err
		}
	}
	next, err := cleanSettings(in)
	if err != nil {
		return nil, err
	}
	old, err := s.settings(p)
	if err != nil {
		return nil, err
	}
	next.SharedEnv = old.GetSharedEnv()
	sealed, err := s.seal(next, settingsCtx(id))
	if err != nil {
		return nil, err
	}
	if err := s.Q.UpdateProject(ctx, store.UpdateProjectParams{Name: name, NetworkName: network, Settings: sealed, ID: id}); err != nil {
		return nil, err
	}
	s.pushProject(ctx, id)
	s.refreshLinks()
	return s.reload(ctx, id)
}

func (s *Service) reload(ctx context.Context, id string) (*nagiv1.Project, error) {
	p, err := s.project(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.assemble(ctx, p)
}

// SaveSharedEnv 保存共享变量的草稿，返回引用了改动变量的容器（部署共享变量时默认一起部署）。
func (s *Service) SaveSharedEnv(ctx context.Context, id string, base int64, vars []*nagiv1.EnvVar) (*nagiv1.Project, []string, error) {
	if err := spec.CheckEnv(vars); err != nil {
		return nil, nil, err
	}
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if err := checkRevision(p.Revision, base); err != nil {
		return nil, nil, err
	}
	sealed, err := s.seal(&nagiv1.EnvList{Vars: vars}, sharedDraftCtx(id))
	if err != nil {
		return nil, nil, err
	}
	if err := s.Q.SetSharedEnvDraft(ctx, store.SetSharedEnvDraftParams{SharedEnvDraft: sealed, ID: id}); err != nil {
		return nil, nil, err
	}
	settings, err := s.settings(p)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.Q.ListContainers(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	var affected []string
	for _, c := range rows {
		cfg, err := s.currentConfig(ctx, c)
		if err != nil {
			continue
		}
		if spec.SharedEnvChanged(cfg, c.Name, settings.GetSharedEnv(), vars) {
			affected = append(affected, c.Name)
		}
	}
	proj, err := s.reload(ctx, id)
	return proj, affected, err
}

// currentConfig 返回容器要部署的配置：有草稿时是草稿，否则是最新版本。
func (s *Service) currentConfig(ctx context.Context, c store.Container) (*nagiv1.ContainerConfig, error) {
	if d, err := s.draft(c); err != nil || d != nil {
		return d, err
	}
	_, cfg, _, err := s.latest(ctx, c)
	return cfg, err
}

// DiscardSharedEnv 放弃共享变量的草稿。
func (s *Service) DiscardSharedEnv(ctx context.Context, id string, base int64) (*nagiv1.Project, error) {
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkRevision(p.Revision, base); err != nil {
		return nil, err
	}
	if err := s.Q.SetSharedEnvDraft(ctx, store.SetSharedEnvDraftParams{SharedEnvDraft: nil, ID: id}); err != nil {
		return nil, err
	}
	return s.reload(ctx, id)
}

// Delete 删除项目：删除所有容器（节点上停止并删除，目录默认保留），再删除面板上的记录。
// 有容器所在的节点不在线时拒绝，免得节点上留下没人管的容器。
func (s *Service) Delete(ctx context.Context, id string, base int64, deleteDir bool, confirmName string) error {
	ctx = context.WithoutCancel(ctx)
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, id)
	if err != nil {
		return err
	}
	if err := checkRevision(p.Revision, base); err != nil {
		return err
	}
	if deleteDir && confirmName != p.Name {
		return invalid("要同时删除容器目录，请输入项目名 %s 确认", p.Name)
	}
	rows, err := s.Q.ListContainers(ctx, id)
	if err != nil {
		return err
	}
	for _, c := range rows {
		if _, err := s.agent(ctx, c.NodeID); err != nil {
			return err
		}
	}
	for _, c := range rows {
		if err := s.deleteOnNode(ctx, c, deleteDir); err != nil {
			return err
		}
		if err := s.Q.DeleteContainer(ctx, store.DeleteContainerParams{ProjectID: id, Name: c.Name}); err != nil {
			return err
		}
	}
	if err := s.Q.DeleteProject(ctx, id); err != nil {
		return err
	}
	s.Log.Info("项目已删除", "project", id, "name", p.Name, "delete_dir", deleteDir)
	s.refreshLinks()
	return nil
}

// agent 返回节点的客户端；节点不在线时说明是哪个节点。
func (s *Service) agent(ctx context.Context, nodeID string) (nagiv1.AgentServiceClient, error) {
	conn, err := s.Nodes.Get(nodeID)
	if err != nil {
		return nil, s.nodeErr(ctx, nodeID, err)
	}
	return conn.Agent, nil
}

func (s *Service) NodeName(ctx context.Context, nodeID string) string {
	if n, err := s.Q.GetNode(ctx, nodeID); err == nil {
		return n.Name
	}
	return nodeID
}

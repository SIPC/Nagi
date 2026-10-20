package projects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 面板把容器的完整配置（ContainerSpec）下发给它所在的节点，节点缓存起来（见 docs/panel.md#项目数据）。
// 每次修改都推一次；节点离线时等它上报，发现缓存的修订号和面板上的不一样再补推。

// HashToken 计算 webhook 令牌的哈希（数据库里只存哈希）。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// offlineError 是节点不在线：errors.Is 能识别为 nodes.ErrOffline，消息里说明是哪个节点。
type offlineError struct{ name string }

func (e offlineError) Error() string        { return "节点 " + e.name + " 不在线" }
func (e offlineError) Is(target error) bool { return target == nodes.ErrOffline }

func (s *Service) nodeErr(ctx context.Context, nodeID string, err error) error {
	if errors.Is(err, nodes.ErrOffline) {
		return offlineError{s.nodeName(ctx, nodeID)}
	}
	return err
}

// specFor 组装容器的配置：version 是要运行的版本（0 表示还没部署过，配置用最新版本的），
// cfg、shared、images 为 nil 时按 version 的版本记录（或最新版本）取。
func (s *Service) specFor(ctx context.Context, p store.Project, c store.Container, version int64) (*nagiv1.ContainerSpec, error) {
	settings, err := s.settings(p)
	if err != nil {
		return nil, err
	}
	sec, err := s.secrets(c)
	if err != nil {
		return nil, err
	}
	docker, err := s.dockerConfig(p)
	if err != nil {
		return nil, err
	}
	sp := &nagiv1.ContainerSpec{
		ProjectId: p.ID, ProjectName: p.Name, NetworkName: p.NetworkName, Name: c.Name, Dir: c.Dir,
		Revision: c.Revision, ProjectRevision: p.Revision, Version: version,
		DeployKey: sec.DeployKey, DeployKeyPub: c.DeployKeyPub, DockerConfig: docker, BackupPassword: sec.BackupPassword,
		SnapshotIntervalMinutes: settings.GetSnapshotIntervalMinutes(),
	}
	var v store.Version
	if version > 0 {
		v, err = s.Q.GetVersion(ctx, store.GetVersionParams{ProjectID: p.ID, Container: c.Name, Version: version})
	} else {
		v, err = s.Q.LatestVersion(ctx, store.LatestVersionParams{ProjectID: p.ID, Container: c.Name})
	}
	if err != nil {
		return nil, err
	}
	if sp.Config, sp.SharedEnv, err = s.versionData(v); err != nil {
		return nil, err
	}
	if version > 0 {
		_ = json.Unmarshal([]byte(v.Images), &sp.Images)
	}
	routes, err := s.Q.ListRoutes(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range routes {
		if r.Container == c.Name {
			sp.Routes = append(sp.Routes, routeProto(r))
		}
	}
	sp.Stateful = s.stateful(ctx, sp.GetConfig(), c, sp.GetSharedEnv())
	return sp, nil
}

// push 把容器当前部署的配置推给它所在的节点。节点不在线时返回错误（上线后由 Sync 补推）。
func (s *Service) push(ctx context.Context, projectID, name string) error {
	p, err := s.project(ctx, projectID)
	if err != nil {
		return err
	}
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return err
	}
	sp, err := s.specFor(ctx, p, c, c.DeployedVersion)
	if err != nil {
		return err
	}
	agent, err := s.agent(ctx, c.NodeID)
	if err != nil {
		return err
	}
	_, err = agent.PutContainer(ctx, &nagiv1.PutContainerRequest{Spec: sp})
	return err
}

// pushProject 把项目里所有容器的配置推给各自的节点（项目设置改了以后）。不在线的节点等它上线后补推。
func (s *Service) pushProject(ctx context.Context, projectID string) {
	rows, err := s.Q.ListContainers(ctx, projectID)
	if err != nil {
		return
	}
	for _, c := range rows {
		if err := s.push(ctx, projectID, c.Name); err != nil && !errors.Is(err, nodes.ErrOffline) {
			s.Log.Warn("下发容器配置失败", "project", projectID, "container", c.Name, "err", err)
		}
	}
}

// Sync 在节点上报后调用：节点缓存的修订号和面板上的不一样（节点离线时改过配置），就补推。
// 节点上有、面板上没有的容器不动（面板丢失后由新面板接管时要用到这些缓存）。
func (s *Service) Sync(c *nodes.Conn, req *nagiv1.ReportRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	have := map[string]*nagiv1.ContainerStatus{}
	for _, cs := range req.GetContainers() {
		have[cs.GetProjectId()+"/"+cs.GetName()] = cs
	}
	rows, err := s.Q.ListNodeContainers(ctx, c.NodeID)
	if err != nil {
		return
	}
	projects := map[string]store.Project{}
	for _, r := range rows {
		p, ok := projects[r.ProjectID]
		if !ok {
			if p, err = s.Q.GetProject(ctx, r.ProjectID); err != nil {
				continue
			}
			projects[r.ProjectID] = p
		}
		cs := have[r.ProjectID+"/"+r.Name]
		if cs != nil && cs.GetRevision() == r.Revision && cs.GetProjectRevision() == p.Revision {
			continue
		}
		// 正在部署这个项目时跳过：部署完会推最新的配置。
		l := s.lock(r.ProjectID)
		if !l.TryLock() {
			continue
		}
		err := s.push(ctx, r.ProjectID, r.Name)
		l.Unlock()
		if err != nil {
			s.Log.Warn("补推容器配置失败", "project", r.ProjectID, "container", r.Name, "node", c.NodeID, "err", err)
		} else {
			s.Log.Info("已补推容器配置", "project", r.ProjectID, "container", r.Name, "node", c.NodeID)
		}
	}
}

// ---- 转换 ----

func routeProto(r store.Route) *nagiv1.Exposure {
	e := &nagiv1.Exposure{
		Name: r.Name, Container: r.Container, Service: r.Service, Target: uint32(r.Target), Protocol: r.Protocol,
		Published: uint32(r.Published), HttpOnly: r.HttpOnly != 0,
	}
	_ = json.Unmarshal([]byte(r.Domains), &e.Domains)
	return e
}

func versionProto(v store.Version) *nagiv1.Version {
	out := &nagiv1.Version{
		Container: v.Container, Version: v.Version, CreatedAt: timestamppb.New(time.Unix(v.CreatedAt, 0)),
		HasFiles: v.HasFiles != 0, RollbackOf: v.RollbackOf, Commit: v.GitCommit,
	}
	if v.DeployedAt.Valid {
		out.DeployedAt = timestamppb.New(time.Unix(v.DeployedAt.Int64, 0))
	}
	_ = json.Unmarshal([]byte(v.Images), &out.Images)
	return out
}

func deploymentProto(d store.Deployment) *nagiv1.Deployment {
	out := &nagiv1.Deployment{
		Id: d.ID, Time: timestamppb.New(time.Unix(d.CreatedAt, 0)), Source: d.Source,
		Success: d.Success == 1, Error: d.Error, RestoreOf: d.RestoreOf,
	}
	_ = json.Unmarshal([]byte(d.Containers), &out.Containers)
	_ = json.Unmarshal([]byte(d.Versions), &out.Versions)
	return out
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

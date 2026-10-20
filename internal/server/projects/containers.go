package projects

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 容器（见 docs/agent.md#项目与容器）：项目里独立部署的单位，每个指定一个节点运行。

// AddRequest 是往项目里加一个容器的参数。
type AddRequest struct {
	ProjectID, Name string
	Config          *nagiv1.ContainerConfig
	NodeID          string
	// Dir 为空时用 <节点的项目根目录>/<网络名>/<容器名>。
	Dir string
}

// AddContainer 往项目里加一个容器，返回加好以后的项目。
func (s *Service) AddContainer(ctx context.Context, req AddRequest) (*nagiv1.Project, error) {
	if err := s.addContainer(ctx, req); err != nil {
		return nil, err
	}
	return s.reload(ctx, req.ProjectID)
}

// addContainer 往项目里加一个容器：保存配置作为它的版本 1（还没部署），把配置下发给它所在的节点（检查目录）。
func (s *Service) addContainer(ctx context.Context, req AddRequest) error {
	if err := spec.CheckContainerName(req.Name); err != nil {
		return err
	}
	if req.Config == nil {
		return invalid("缺少容器配置")
	}
	cfg := proto.Clone(req.Config).(*nagiv1.ContainerConfig)
	l := s.lock(req.ProjectID)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, req.ProjectID)
	if err != nil {
		return err
	}
	if _, err := s.container(ctx, p.ID, req.Name); err == nil {
		return invalid("项目里已经有容器 %s", req.Name)
	}
	conn, err := s.Nodes.Get(req.NodeID)
	if err != nil {
		return s.nodeErr(ctx, req.NodeID, err)
	}
	dir := strings.TrimSpace(req.Dir)
	if dir == "" {
		dir = s.defaultDir(ctx, conn, req.NodeID, p.ID, p.Name, req.Name)
	}
	if !strings.HasPrefix(dir, "/") {
		return invalid("容器目录必须是绝对路径")
	}
	dir = path.Clean(dir)
	if owner, err := s.Q.ContainerDirOwner(ctx, store.ContainerDirOwnerParams{NodeID: req.NodeID, Dir: dir}); err == nil {
		return invalid("这个节点上的容器 %s 已经在用目录 %s", owner.Name, dir)
	}
	settings, err := s.settings(p)
	if err != nil {
		return err
	}
	probe := store.Container{ProjectID: p.ID, Name: req.Name, Dir: dir}
	cp, err := spec.Validate(ctx, cfg, req.Name, settings.GetSharedEnv(), checkData(probe), spec.ComposeProjectName(p.ID, req.Name))
	if err != nil {
		return err
	}
	if err := s.checkNames(ctx, p.ID, req.Name, cfg, cp.ServiceNames()); err != nil {
		return err
	}
	key, pub, err := spec.NewDeployKey()
	if err != nil {
		return err
	}
	token, err := spec.RandomHex(32)
	if err != nil {
		return err
	}
	password, err := spec.RandomHex(32)
	if err != nil {
		return err
	}
	secretJSON, _ := json.Marshal(secrets{WebhookToken: token, DeployKey: key, BackupPassword: password})
	sealedSecrets, err := s.Sealer.Seal(secretJSON, []byte(secretsCtx(p.ID, req.Name)))
	if err != nil {
		return err
	}
	rows, err := s.Q.ListContainers(ctx, p.ID)
	if err != nil {
		return err
	}
	position := int64(1)
	for _, r := range rows {
		position = max(position, r.Position+1)
	}
	now := time.Now().Unix()
	if err := s.Q.InsertContainer(ctx, store.InsertContainerParams{
		ProjectID: p.ID, Name: req.Name, Position: position, NodeID: req.NodeID, Dir: dir, CreatedAt: now,
		WebhookHash: HashToken(token), Secrets: sealedSecrets, DeployKeyPub: pub,
	}); err != nil {
		return err
	}
	cleanup := func() { _ = s.Q.DeleteContainer(ctx, store.DeleteContainerParams{ProjectID: p.ID, Name: req.Name}) }
	sc, se, err := s.sealVersion(p.ID, req.Name, 1, cfg, settings.GetSharedEnv())
	if err == nil {
		err = s.Q.InsertVersion(ctx, store.InsertVersionParams{ProjectID: p.ID, Container: req.Name, Version: 1, Config: sc, SharedEnv: se, CreatedAt: now})
	}
	if err != nil {
		cleanup()
		return err
	}
	// 先下发一份配置：节点检查目录能不能用，并建好目录。
	if err := s.push(ctx, p.ID, req.Name); err != nil {
		cleanup()
		return err
	}
	s.Log.Info("已添加容器", "project", p.ID, "container", req.Name, "node", req.NodeID, "dir", dir)
	return nil
}

// checkNames 检查名字会不会撞：单个容器在项目内网里的名字就是容器名，容器组内部按服务原名互相访问，
// 所以单个容器的名字不能和项目里任何容器组的服务名相同，否则容器组内部查这个名字时可能查到项目内网里的单个容器。
func (s *Service) checkNames(ctx context.Context, projectID, container string, cfg *nagiv1.ContainerConfig, services []string) error {
	if !spec.IsSingle(cfg) {
		if err := s.checkGroupName(ctx, projectID, container); err != nil {
			return err
		}
	}
	rows, err := s.Q.ListContainers(ctx, projectID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Name == container {
			continue
		}
		var cfgs []*nagiv1.ContainerConfig
		if _, c, _, err := s.latest(ctx, r); err == nil {
			cfgs = append(cfgs, c)
		}
		if d, err := s.draft(r); err == nil && d != nil {
			cfgs = append(cfgs, d)
		}
		for _, other := range cfgs {
			switch {
			case spec.IsSingle(cfg) && !spec.IsSingle(other):
				cp, err := spec.Load(ctx, other, r.Name, nil, checkData(r), "check")
				if err != nil {
					continue
				}
				if _, ok := cp.Services[container]; ok {
					return invalid("容器名 %s 和容器组 %s 里的一个容器同名：%s 内部查 %s 时可能查到这个容器，请换个名字", container, r.Name, r.Name, container)
				}
			case !spec.IsSingle(cfg) && spec.IsSingle(other):
				for _, svc := range services {
					if svc == r.Name {
						return invalid("这份 compose 里的 %s 和单个容器 %s 同名：compose 内部查 %s 时可能查到那个容器，请改 compose 里的服务名", svc, r.Name, svc)
					}
				}
			}
		}
	}
	return nil
}

// DeleteContainer 删除容器：节点上停止并删除它（目录默认保留），再删除面板上的记录（版本、映射一并删除）。
func (s *Service) DeleteContainer(ctx context.Context, projectID, name string, base int64, deleteDir bool) (*nagiv1.Project, error) {
	ctx = context.WithoutCancel(ctx)
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	if err := checkRevision(c.Revision, base); err != nil {
		return nil, err
	}
	if _, err := s.agent(ctx, c.NodeID); err != nil {
		return nil, err
	}
	if cfg := failoverOf(c); cfg != nil {
		if err := s.dropFailover(ctx, c, cfg); err != nil {
			return nil, err
		}
	}
	if err := s.deleteOnNode(ctx, c, deleteDir); err != nil {
		return nil, err
	}
	if err := s.Q.DeleteContainer(ctx, store.DeleteContainerParams{ProjectID: projectID, Name: name}); err != nil {
		return nil, err
	}
	s.Log.Info("容器已删除", "project", projectID, "container", name, "delete_dir", deleteDir)
	return s.reload(ctx, projectID)
}

func (s *Service) deleteOnNode(ctx context.Context, c store.Container, deleteDir bool) error {
	agent, err := s.agent(ctx, c.NodeID)
	if err != nil {
		return err
	}
	if _, err = agent.DeleteContainer(ctx, &nagiv1.DeleteContainerRequest{ProjectId: c.ProjectID, Container: c.Name, DeleteDir: deleteDir}); err != nil {
		return err
	}
	// 备份节点上的副本一起删掉：保留目录时，目录里的 backups/ 就是完整的快照仓库。
	if c.BackupNodeID != "" {
		if b, err := s.agent(ctx, c.BackupNodeID); err == nil {
			_, _ = b.DeleteReplica(ctx, &nagiv1.ContainerRef{ProjectId: c.ProjectID, Container: c.Name})
		}
	}
	return nil
}

// SaveDraft 保存容器的草稿。草稿要先通过校验，部署时才会成为新版本。
func (s *Service) SaveDraft(ctx context.Context, projectID, name string, base int64, cfg *nagiv1.ContainerConfig) (*nagiv1.Project, error) {
	if cfg == nil {
		return nil, invalid("缺少容器配置")
	}
	cfg = proto.Clone(cfg).(*nagiv1.ContainerConfig)
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, projectID)
	if err != nil {
		return nil, err
	}
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	if err := checkRevision(c.Revision, base); err != nil {
		return nil, err
	}
	_, latest, _, err := s.latest(ctx, c)
	if err != nil {
		return nil, err
	}
	if spec.IsSingle(latest) != spec.IsSingle(cfg) {
		return nil, invalid("容器创建后不能在单个容器和容器组之间切换，请新建一个容器")
	}
	settings, err := s.settings(p)
	if err != nil {
		return nil, err
	}
	cp, err := spec.Validate(ctx, cfg, name, settings.GetSharedEnv(), checkData(c), spec.ComposeProjectName(projectID, name))
	if err != nil {
		return nil, err
	}
	if err := s.checkNames(ctx, projectID, name, cfg, cp.ServiceNames()); err != nil {
		return nil, err
	}
	sealed, err := s.seal(cfg, draftCtx(projectID, name))
	if err != nil {
		return nil, err
	}
	if err := s.Q.SetContainerDraft(ctx, store.SetContainerDraftParams{Draft: sealed, ProjectID: projectID, Name: name}); err != nil {
		return nil, err
	}
	return s.reload(ctx, projectID)
}

// DiscardDraft 放弃容器的草稿。
func (s *Service) DiscardDraft(ctx context.Context, projectID, name string, base int64) (*nagiv1.Project, error) {
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	if err := checkRevision(c.Revision, base); err != nil {
		return nil, err
	}
	if err := s.Q.SetContainerDraft(ctx, store.SetContainerDraftParams{Draft: nil, ProjectID: projectID, Name: name}); err != nil {
		return nil, err
	}
	return s.reload(ctx, projectID)
}

// Versions 列出容器的版本，最新的在前。
func (s *Service) Versions(ctx context.Context, projectID, name string) ([]*nagiv1.Version, error) {
	if _, err := s.container(ctx, projectID, name); err != nil {
		return nil, err
	}
	rows, err := s.Q.ListVersions(ctx, store.ListVersionsParams{ProjectID: projectID, Container: name})
	if err != nil {
		return nil, err
	}
	out := make([]*nagiv1.Version, 0, len(rows))
	for _, v := range rows {
		out = append(out, versionProto(v))
	}
	return out, nil
}

// ContainerByWebhook 按 webhook 令牌找到容器。
func (s *Service) ContainerByWebhook(ctx context.Context, token string) (projectID, name string, err error) {
	r, err := s.Q.ContainerByWebhook(ctx, HashToken(token))
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return r.ProjectID, r.Name, err
}

// Locate 返回容器所在的节点（上传文件、终端用）。
func (s *Service) Locate(ctx context.Context, projectID, name string) (string, error) {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return "", err
	}
	return c.NodeID, nil
}

// BackupKey 返回容器快照仓库的密码。
func (s *Service) BackupKey(ctx context.Context, projectID, name string) (string, error) {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return "", err
	}
	sec, err := s.secrets(c)
	if err != nil {
		return "", err
	}
	s.Log.Warn("查看了容器的快照仓库密码", "project", projectID, "container", name)
	return sec.BackupPassword, nil
}

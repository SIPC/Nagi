package projects

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// 部署（见 docs/agent.md#修改与部署、#版本与回滚）：面板定好每个容器的新版本，按依赖顺序逐个让容器所在的节点部署，
// 某个失败就停下（已经部署的保留）。整次部署在项目上记一条部署记录。

// DeployOptions 是一次部署的选项。
type DeployOptions struct {
	Pull       bool // 部署前从 git 拉取文件
	Upload     bool // 用上传的文件替换 data/（只能部署一个容器）
	PullImages bool // 部署前拉取最新的镜像
	// ApplySharedEnv 先让共享变量的草稿生效，SettingsRevision 是基于的项目修订号。
	ApplySharedEnv   bool
	SettingsRevision int64
	// Source 记在部署记录里：manual、git、image、shared_env，为空表示 manual。
	Source string
}

// Target 是一起部署里的一个容器。BaseRevision 不为 0 时必须和容器的修订号一致。
type Target struct {
	Container    string
	BaseRevision int64
}

// job 是一次部署里要部署的一个容器。
type job struct {
	c      store.Container
	cfg    *nagiv1.ContainerConfig
	shared []*nagiv1.EnvVar
	// draft：部署成功创建版本后删除草稿。
	draft bool
	// 回滚：回滚到的版本，用它的文件、镜像。
	rollbackOf  int64
	pins        map[string]string
	restoreData bool
	opts        DeployOptions
}

// Output 收部署过程中的输出。
type Output func(string)

// Deploy 部署一个或多个容器：有草稿的容器用草稿，没有的重新部署最新版本。只部署一个容器时返回它的新版本号。
func (s *Service) Deploy(ctx context.Context, projectID string, targets []Target, o DeployOptions, out Output) (int64, error) {
	ctx = context.WithoutCancel(ctx) // 开始了就做完：浏览器断开也不会留下一半的状态
	if o.Pull && o.Upload {
		return 0, invalid("不能同时从 git 拉取和使用上传的文件")
	}
	if o.Upload && len(targets) != 1 {
		return 0, invalid("上传的文件只能部署到一个容器")
	}
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, projectID)
	if err != nil {
		return 0, err
	}
	settings, err := s.settings(p)
	if err != nil {
		return 0, err
	}
	shared := settings.GetSharedEnv()
	if o.ApplySharedEnv {
		if err := checkRevision(p.Revision, o.SettingsRevision); err != nil {
			return 0, err
		}
		draft, ok, err := s.sharedEnvDraft(p)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, invalid("共享变量没有要部署的修改")
		}
		settings.SharedEnv, shared = draft, draft
		sealed, err := s.seal(settings, settingsCtx(projectID))
		if err != nil {
			return 0, err
		}
		if err := s.Q.SetProjectSettings(ctx, store.SetProjectSettingsParams{Settings: sealed, ID: projectID}); err != nil {
			return 0, err
		}
		if err := s.Q.SetSharedEnvDraft(ctx, store.SetSharedEnvDraftParams{SharedEnvDraft: nil, ID: projectID}); err != nil {
			return 0, err
		}
		out(i18n.T(ctx, "共享变量已生效"))
		if o.Source == "" {
			o.Source = "shared_env"
		}
	}
	if len(targets) == 0 {
		if o.ApplySharedEnv {
			s.pushProject(ctx, projectID)
			return 0, nil
		}
		return 0, invalid("请选择要部署的容器")
	}
	if o.Source == "" && o.PullImages {
		o.Source = "image"
	}
	var jobs []job
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t.Container] {
			continue
		}
		seen[t.Container] = true
		c, err := s.container(ctx, projectID, t.Container)
		if err != nil {
			return 0, err
		}
		if err := checkRevision(c.Revision, t.BaseRevision); err != nil {
			return 0, err
		}
		j := job{c: c, shared: shared, opts: o}
		if d, err := s.draft(c); err != nil {
			return 0, err
		} else if d != nil {
			j.cfg, j.draft = d, true
		} else if _, j.cfg, _, err = s.latest(ctx, c); err != nil {
			return 0, err
		}
		jobs = append(jobs, j)
	}
	source := o.Source
	if source == "" {
		source = "manual"
	}
	return s.runJobs(ctx, p, jobs, source, 0, out)
}

// runJobs 按依赖顺序逐个部署容器，某个失败就停下，最后记一条部署记录。
func (s *Service) runJobs(ctx context.Context, p store.Project, jobs []job, source string, restoreOf int64, out Output) (int64, error) {
	ordered, err := orderJobs(jobs)
	if err != nil {
		return 0, err
	}
	var touched []string
	var version int64
	var failErr error
	for i, j := range ordered {
		name := j.c.Name
		touched = append(touched, name)
		if len(ordered) > 1 {
			out(i18n.T(ctx, "==== 部署容器 %s（%d/%d）====", name, i+1, len(ordered)))
		}
		v, err := s.deployOne(ctx, p, j, out)
		if err != nil {
			failErr = err
			if len(ordered) > 1 {
				failErr = i18n.Errorf("容器 %s 部署失败：%w", name, err)
				var rest i18n.List
				for _, r := range ordered[i+1:] {
					rest = append(rest, r.c.Name)
				}
				if len(rest) > 0 {
					out(i18n.T(ctx, "已停止，没有部署：%s", rest))
				}
			}
			break
		}
		version = v
	}
	s.recordDeployment(ctx, p.ID, source, touched, failErr, restoreOf)
	if len(ordered) > 1 {
		version = 0
	}
	return version, failErr
}

// orderJobs 按容器设置的依赖排序（被依赖的先部署），其余按容器创建顺序。只考虑这次要部署的容器之间的依赖。
func orderJobs(jobs []job) ([]job, error) {
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].c.Position < jobs[j].c.Position })
	byName := map[string]job{}
	for _, j := range jobs {
		byName[j.c.Name] = j
	}
	var out []job
	state := map[string]int{} // 1：处理中，2：已排好
	var visit func(j job) error
	visit = func(j job) error {
		switch state[j.c.Name] {
		case 1:
			return invalid("容器之间的依赖有循环（%s）", j.c.Name)
		case 2:
			return nil
		}
		state[j.c.Name] = 1
		for _, d := range j.cfg.GetDependsOn() {
			if dep, ok := byName[d]; ok {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		state[j.c.Name] = 2
		out = append(out, j)
		return nil
	}
	for _, j := range jobs {
		if err := visit(j); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// deployOne 部署一个容器：记下新版本（最新版本还没成功部署过时复用它的版本号），导入 compose 里新的端口，
// 让节点部署，成功后记下镜像摘要并把版本标为已部署。
func (s *Service) deployOne(ctx context.Context, p store.Project, j job, out Output) (int64, error) {
	c := j.c
	cfg := proto.Clone(j.cfg).(*nagiv1.ContainerConfig)
	cp, err := spec.Validate(ctx, cfg, c.Name, j.shared, checkData(c), spec.ComposeProjectName(p.ID, c.Name))
	if err != nil {
		return 0, err
	}
	agent, err := s.agent(ctx, c.NodeID)
	if err != nil {
		return 0, err
	}

	// 1. 版本记录。
	latest, err := s.Q.LatestVersion(ctx, store.LatestVersionParams{ProjectID: p.ID, Container: c.Name})
	if err != nil {
		return 0, err
	}
	version := latest.Version + 1
	if !latest.DeployedAt.Valid {
		version = latest.Version
	}
	sc, se, err := s.sealVersion(p.ID, c.Name, version, cfg, j.shared)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	if version == latest.Version {
		err = s.Q.ReplaceUndeployedVersion(ctx, store.ReplaceUndeployedVersionParams{
			Config: sc, SharedEnv: se, CreatedAt: now, RollbackOf: j.rollbackOf, ProjectID: p.ID, Container: c.Name, Version: version,
		})
	} else {
		err = s.Q.InsertVersion(ctx, store.InsertVersionParams{
			ProjectID: p.ID, Container: c.Name, Version: version, Config: sc, SharedEnv: se, CreatedAt: now, RollbackOf: j.rollbackOf,
		})
	}
	if err != nil {
		return 0, err
	}
	// 草稿已经记成了版本：部署失败时下次部署用的还是它（最新版本还没部署成功，复用版本号）。
	if j.draft {
		if err := s.Q.SetContainerDraft(ctx, store.SetContainerDraftParams{Draft: nil, ProjectID: p.ID, Name: c.Name}); err != nil {
			return 0, err
		}
	}

	// 2. compose 里新出现的端口导入成映射。
	if err := s.importPorts(ctx, p, c, cp, out); err != nil {
		return 0, err
	}

	// 3. 让节点部署。
	if c, err = s.container(ctx, p.ID, c.Name); err != nil {
		return 0, err
	}
	sp, err := s.specFor(ctx, p, c, version)
	if err != nil {
		return 0, err
	}
	sp.Images = j.pins
	req := &nagiv1.DeployContainerRequest{
		Spec: sp, Pull: j.opts.Pull, UseUpload: j.opts.Upload, PullImages: j.opts.PullImages, PreviousVersion: c.DeployedVersion,
	}
	if j.rollbackOf > 0 {
		req.FilesFrom = j.rollbackOf
		if j.restoreData {
			req.RestoreDataFrom = j.rollbackOf
		}
	}
	res, err := runDeploy(ctx, agent, req, out)
	if err != nil {
		return version, err
	}

	// 4. 记下镜像摘要和部署时间，清理过旧的版本，把最终的配置推给节点。
	images, _ := json.Marshal(res.GetImages())
	if err := s.Q.SetVersionDeployed(ctx, store.SetVersionDeployedParams{
		DeployedAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}, Images: string(images), GitCommit: res.GetCommit(),
		HasFiles: boolInt(res.GetHasFiles()), ProjectID: p.ID, Container: c.Name, Version: version,
	}); err != nil {
		return version, err
	}
	if err := s.Q.SetContainerDeployed(ctx, store.SetContainerDeployedParams{DeployedVersion: version, ProjectID: p.ID, Name: c.Name}); err != nil {
		return version, err
	}
	if res.GetCommit() != "" {
		_ = s.Q.SetContainerCommit(ctx, store.SetContainerCommitParams{LastCommit: res.GetCommit(), ProjectID: p.ID, Name: c.Name})
	}
	_ = s.Q.DeleteVersionsBefore(ctx, store.DeleteVersionsBeforeParams{ProjectID: p.ID, Container: c.Name, Version: version - versionsKept + 1})
	if err := s.push(ctx, p.ID, c.Name); err != nil {
		s.Log.Warn("部署后下发容器配置失败", "project", p.ID, "container", c.Name, "err", err)
	}
	return version, nil
}

// runDeploy 调用节点部署，转发输出，返回结果。
func runDeploy(ctx context.Context, agent nagiv1.AgentServiceClient, req *nagiv1.DeployContainerRequest, out Output) (*nagiv1.DeployResult, error) {
	stream, err := agent.DeployContainer(ctx, req)
	if err != nil {
		return nil, err
	}
	return recvResult(stream.Recv, out)
}

// recvResult 读一个输出过程的流，转发输出，返回结果；结果是失败时返回错误。
func recvResult(recv func() (*nagiv1.DeployEvent, error), out Output) (*nagiv1.DeployResult, error) {
	for {
		ev, err := recv()
		if errors.Is(err, io.EOF) {
			return nil, i18n.New("节点没有返回结果")
		}
		if err != nil {
			if status.Code(err) == codes.Unavailable {
				return nil, i18n.New("和节点的连接在操作过程中断开了，操作可能没做完，也可能已经做完。等节点重新连上后，看一下容器的状态和部署记录")
			}
			return nil, err
		}
		if line := ev.GetOutput(); line != "" {
			out(line)
		}
		if r := ev.GetResult(); r != nil {
			if !r.GetSuccess() {
				return r, errors.New(r.GetError())
			}
			return r, nil
		}
	}
}

// recordDeployment 记一条部署记录：这次动到的容器，以及部署完成后所有容器各自的版本。
func (s *Service) recordDeployment(ctx context.Context, projectID, source string, touched []string, deployErr error, restoreOf int64) {
	rows, err := s.Q.ListContainers(ctx, projectID)
	if err != nil {
		return
	}
	versions := map[string]int64{}
	for _, r := range rows {
		if r.DeployedVersion > 0 {
			versions[r.Name] = r.DeployedVersion
		}
	}
	containersJSON, _ := json.Marshal(touched)
	versionsJSON, _ := json.Marshal(versions)
	msg := ""
	if deployErr != nil {
		msg = deployErr.Error()
	}
	id, err := s.Q.InsertDeployment(ctx, store.InsertDeploymentParams{
		ProjectID: projectID, CreatedAt: time.Now().Unix(), Source: source, Containers: string(containersJSON), Versions: string(versionsJSON),
		Success: boolInt(deployErr == nil), Error: msg, RestoreOf: restoreOf,
	})
	if err == nil {
		_ = s.Q.DeleteDeploymentsBefore(ctx, store.DeleteDeploymentsBeforeParams{ProjectID: projectID, ID: id - deploymentsKept + 1})
	}
}

// Deployments 列出项目的部署记录，最新的在前。
func (s *Service) Deployments(ctx context.Context, projectID string) ([]*nagiv1.Deployment, error) {
	if _, err := s.project(ctx, projectID); err != nil {
		return nil, err
	}
	rows, err := s.Q.ListDeployments(ctx, store.ListDeploymentsParams{ProjectID: projectID, Limit: deploymentsKept})
	if err != nil {
		return nil, err
	}
	out := make([]*nagiv1.Deployment, 0, len(rows))
	for _, d := range rows {
		out = append(out, deploymentProto(d))
	}
	return out, nil
}

// ---- 回滚 ----

// rollbackJob 用容器的旧版本生成一次部署：配置、文件、镜像、共享变量都用那个版本的。
func (s *Service) rollbackJob(ctx context.Context, c store.Container, target int64) (job, error) {
	v, err := s.Q.GetVersion(ctx, store.GetVersionParams{ProjectID: c.ProjectID, Container: c.Name, Version: target})
	if errors.Is(err, sql.ErrNoRows) {
		return job{}, invalid("容器 %s 的版本 %d 不存在或已被清理", c.Name, target)
	} else if err != nil {
		return job{}, err
	}
	if !v.DeployedAt.Valid {
		return job{}, invalid("容器 %s 的版本 %d 没有部署成功过，不能回滚到它", c.Name, target)
	}
	if v.HasFiles == 0 {
		return job{}, invalid("容器 %s 的版本 %d 的文件已经不在了（可能已被清理，或者容器换过节点），不能回滚到这个版本", c.Name, target)
	}
	cfg, shared, err := s.versionData(v)
	if err != nil {
		return job{}, err
	}
	if shared == nil {
		shared = []*nagiv1.EnvVar{}
	}
	j := job{c: c, cfg: cfg, shared: shared, rollbackOf: target}
	_ = json.Unmarshal([]byte(v.Images), &j.pins)
	return j, nil
}

// Rollback 回滚容器到某个版本：用它的配置、文件和镜像生成这个容器的一个新版本并部署。
// restoreData 时，同时把运行数据恢复到这个版本被替换之前（需要有对应的部署前快照）。
func (s *Service) Rollback(ctx context.Context, projectID, name string, target int64, restoreData bool, out Output) (int64, error) {
	ctx = context.WithoutCancel(ctx)
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, projectID)
	if err != nil {
		return 0, err
	}
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return 0, err
	}
	j, err := s.rollbackJob(ctx, c, target)
	if err != nil {
		return 0, err
	}
	j.restoreData = restoreData
	out(i18n.T(ctx, "把容器 %s 回滚到版本 %d", name, target))
	return s.runJobs(ctx, p, []job{j}, "rollback", 0, out)
}

// RestoreDeployment 回到某次部署时的状态：版本和那次记录不一样的容器都回滚到记录里的版本（各自生成新版本），
// 作为一次一起部署执行。之后才加的容器不动。
func (s *Service) RestoreDeployment(ctx context.Context, projectID string, deploymentID int64, out Output) (int64, error) {
	ctx = context.WithoutCancel(ctx)
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, projectID)
	if err != nil {
		return 0, err
	}
	d, err := s.Q.GetDeployment(ctx, store.GetDeploymentParams{ProjectID: projectID, ID: deploymentID})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, invalid("部署记录 %d 不存在或已被清理", deploymentID)
	} else if err != nil {
		return 0, err
	}
	var versions map[string]int64
	_ = json.Unmarshal([]byte(d.Versions), &versions)
	rows, err := s.Q.ListContainers(ctx, projectID)
	if err != nil {
		return 0, err
	}
	var jobs []job
	for _, c := range rows {
		target, ok := versions[c.Name]
		if !ok {
			out(i18n.T(ctx, "容器 %s 是之后才加的（或那时还没部署过），不动", c.Name))
			continue
		}
		if c.DeployedVersion == target {
			continue
		}
		j, err := s.rollbackJob(ctx, c, target)
		if err != nil {
			return 0, err
		}
		out(i18n.T(ctx, "容器 %s：版本 %d → 回到版本 %d", c.Name, c.DeployedVersion, target))
		jobs = append(jobs, j)
	}
	if len(jobs) == 0 {
		return 0, invalid("所有容器的版本已经和这次部署时一样")
	}
	return s.runJobs(ctx, p, jobs, "restore", deploymentID, out)
}

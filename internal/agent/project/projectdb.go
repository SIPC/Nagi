package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/store/projectstore"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 项目元数据在 project.db 的 meta 表里的键。
const (
	metaID             = "id"
	metaName           = "name"
	metaRevision       = "revision" // 项目设置的修订号
	metaCreatedAt      = "created_at"
	metaSettings       = "settings"         // ProjectSettings（JSON），共享变量是已经部署过的那份
	metaSharedEnvDraft = "shared_env_draft" // 共享变量的草稿（EnvList，JSON），没有草稿时为空
)

// ---- 格式版本号 ----

// formatVersion 是项目目录的格式版本号，记在 config/format 里。项目目录会跟着项目在不同版本的节点之间搬，
// 节点只打开自己认识的格式（见 docs/agent.md#项目目录）。以后改目录结构时加一，并在 openProject 里就地升级。
const formatVersion = 1

func (p paths) formatFile() string { return filepath.Join(p.configDir(), "format") }

func writeFormat(p paths) error {
	return writeFileAtomic(p.formatFile(), []byte(strconv.Itoa(formatVersion)+"\n"), 0o644)
}

// readFormat 读取目录的格式版本号，没有这个文件时返回 0（容器模型之前的旧格式）。
func readFormat(dir string) (int, error) {
	b, err := os.ReadFile(paths{root: dir}.formatFile())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// CheckFormat 检查项目目录（或副本）的格式本节点能不能处理。
func CheckFormat(v int) error {
	switch {
	case v == 0:
		return invalid("这是旧版本 Nagi 的项目目录格式，不再支持")
	case v > formatVersion:
		return invalid("项目目录的格式版本是 %d，本节点只支持到 %d，请先升级节点", v, formatVersion)
	}
	return nil
}

// ---- project.db ----

// projectDB 是项目目录里的 config/project.db。
type projectDB struct {
	db *sql.DB
	q  *projectstore.Queries
}

func openProject(ctx context.Context, dir string) (*projectDB, error) {
	v, err := readFormat(dir)
	if err != nil {
		return nil, err
	}
	if err := CheckFormat(v); err != nil {
		return nil, err
	}
	d, err := db.OpenMigrated(ctx, paths{root: dir}.projectDB(), projectstore.Migrations)
	if err != nil {
		return nil, err
	}
	return &projectDB{db: d, q: projectstore.New(d)}, nil
}

func (p *projectDB) Close() error { return p.db.Close() }

// snapshotDB 用 SQLite 的 VACUUM INTO 拷出一份一致的 project.db（不会拷到写了一半的内容），同步副本时用。
func (p *projectDB) snapshotDB(ctx context.Context) ([]byte, error) {
	f, err := os.CreateTemp("", "nagi-project-*.db")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	f.Close()
	os.Remove(path) // VACUUM INTO 要求目标文件不存在
	defer os.Remove(path)
	if _, err := p.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (p *projectDB) meta(ctx context.Context, key string) string {
	v, _ := p.q.GetMeta(ctx, key)
	return v
}

func (p *projectDB) setMeta(ctx context.Context, key, value string) error {
	return p.q.SetMeta(ctx, projectstore.SetMetaParams{Key: key, Value: value})
}

// checkRevision 检查项目设置的修订号。base 为 0 表示不检查。
func (p *projectDB) checkRevision(ctx context.Context, base int64) error {
	rev, _ := strconv.ParseInt(p.meta(ctx, metaRevision), 10, 64)
	if base != 0 && base != rev {
		return ErrConflict
	}
	return nil
}

func (p *projectDB) bumpRevision(ctx context.Context) error {
	rev, _ := strconv.ParseInt(p.meta(ctx, metaRevision), 10, 64)
	return p.setMeta(ctx, metaRevision, strconv.FormatInt(rev+1, 10))
}

func (p *projectDB) settings(ctx context.Context) *nagiv1.ProjectSettings {
	s := &nagiv1.ProjectSettings{}
	if raw := p.meta(ctx, metaSettings); raw != "" {
		_ = protojson.Unmarshal([]byte(raw), s)
	}
	return s
}

func (p *projectDB) setSettings(ctx context.Context, s *nagiv1.ProjectSettings) error {
	raw, err := protojson.Marshal(s)
	if err != nil {
		return err
	}
	return p.setMeta(ctx, metaSettings, string(raw))
}

// sharedEnvDraft 返回共享变量的草稿；没有草稿时返回 nil。
func (p *projectDB) sharedEnvDraft(ctx context.Context) *nagiv1.EnvList {
	raw := p.meta(ctx, metaSharedEnvDraft)
	if raw == "" {
		return nil
	}
	l := &nagiv1.EnvList{}
	if protojson.Unmarshal([]byte(raw), l) != nil {
		return nil
	}
	return l
}

// ---- 容器 ----

// container 返回容器记录，不存在时返回 ErrNotFound。
func (p *projectDB) container(ctx context.Context, name string) (projectstore.Container, error) {
	m, err := p.q.GetContainer(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return m, notFoundError{fmt.Sprintf("容器 %s 不存在", name)}
	}
	return m, err
}

// checkContainerRevision 检查容器的修订号。base 为 0 表示不检查。
func checkContainerRevision(m projectstore.Container, base int64) error {
	if base != 0 && base != m.Revision {
		return ErrConflict
	}
	return nil
}

func (p *projectDB) bumpContainer(ctx context.Context, m projectstore.Container) error {
	return p.q.SetContainerRevision(ctx, projectstore.SetContainerRevisionParams{Revision: m.Revision + 1, Name: m.Name})
}

func unmarshalConfig(b []byte) (*nagiv1.ContainerConfig, error) {
	cfg := &nagiv1.ContainerConfig{}
	return cfg, proto.Unmarshal(b, cfg)
}

func unmarshalEnv(b []byte) []*nagiv1.EnvVar {
	l := &nagiv1.EnvList{}
	_ = proto.Unmarshal(b, l)
	return l.GetVars()
}

// latest 返回容器最新版本的配置和版本号。
func (p *projectDB) latest(ctx context.Context, container string) (*nagiv1.ContainerConfig, int64, error) {
	v, err := p.q.LatestVersion(ctx, container)
	if err != nil {
		return nil, 0, err
	}
	cfg, err := unmarshalConfig(v.Config)
	return cfg, v.Version, err
}

// deployed 返回容器最近一次部署的版本；从未部署时返回 nil。
func (p *projectDB) deployed(ctx context.Context, m projectstore.Container) (*projectstore.Version, error) {
	if m.DeployedVersion == 0 {
		return nil, nil
	}
	v, err := p.q.GetVersion(ctx, projectstore.GetVersionParams{Container: m.Name, Version: m.DeployedVersion})
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// current 返回容器当前的配置：部署过的版本，没部署过时用最新版本。
func (p *projectDB) current(ctx context.Context, m projectstore.Container) (*nagiv1.ContainerConfig, error) {
	if v, err := p.deployed(ctx, m); err == nil && v != nil {
		return unmarshalConfig(v.Config)
	}
	cfg, _, err := p.latest(ctx, m.Name)
	return cfg, err
}

// draft 返回容器的草稿；没有草稿时返回 nil。
func (p *projectDB) draft(ctx context.Context, container string) (*nagiv1.ContainerConfig, error) {
	d, err := p.q.GetDraft(ctx, container)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return unmarshalConfig(d.Config)
}

func (p *projectDB) setDraft(ctx context.Context, container string, cfg *nagiv1.ContainerConfig) error {
	data, err := proto.Marshal(cfg)
	if err != nil {
		return err
	}
	return p.q.SetDraft(ctx, projectstore.SetDraftParams{Container: container, Config: data, UpdatedAt: time.Now().Unix()})
}

// ---- 组装 ----

// project 组装对外的项目信息：元数据、设置、容器（最新版本的配置和草稿）、映射。
func (p *projectDB) project(ctx context.Context, id, dir string) (*nagiv1.Project, error) {
	rows, err := p.q.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	revision, _ := strconv.ParseInt(p.meta(ctx, metaRevision), 10, 64)
	created, _ := strconv.ParseInt(p.meta(ctx, metaCreatedAt), 10, 64)
	settings := p.settings(ctx)
	pp := paths{root: dir}
	proj := &nagiv1.Project{
		Id:        id,
		Name:      p.meta(ctx, metaName),
		Dir:       dir,
		Revision:  revision,
		CreatedAt: timestamppb.New(time.Unix(created, 0)),
		Settings:  settings,
	}
	if d := p.sharedEnvDraft(ctx); d != nil {
		proj.HasSharedEnvDraft, proj.SharedEnvDraft = true, d.GetVars()
	}
	names := map[string]bool{}
	for _, r := range rows {
		names[r.Name] = true
		cfg, version, err := p.latest(ctx, r.Name)
		if err != nil {
			return nil, err
		}
		draft, err := p.draft(ctx, r.Name)
		if err != nil {
			return nil, err
		}
		mp := pp.container(r.Name)
		mem := &nagiv1.Container{
			Name:            r.Name,
			Revision:        r.Revision,
			LatestVersion:   version,
			DeployedVersion: r.DeployedVersion,
			CreatedAt:       timestamppb.New(time.Unix(r.CreatedAt, 0)),
			Config:          cfg,
			HasDraft:        draft != nil,
			Draft:           draft,
			DeployKey:       readDeployKey(mp),
			WebhookToken:    r.WebhookToken,
			LastCommit:      r.LastCommit,
			Kind:            containerKind(cfg),
		}
		if cp, err := loadContainer(ctx, cfg, r.Name, settings.GetSharedEnv(), mp.dataDir(), composeProjectName(id, r.Name)); err == nil {
			mem.Stateful = isStateful(cfg, cp, preservePaths(cfg, cp, mp.dataDir()))
			mem.UnmanagedVolumes, mem.HostNetworkServices = unmanagedVolumes(cp), hostNetworkServices(cp)
		}
		if v, err := p.deployed(ctx, r); err == nil && v != nil {
			if dcfg, err := unmarshalConfig(v.Config); err == nil {
				mem.SharedEnvOutdated = sharedEnvChanged(dcfg, r.Name, unmarshalEnv(v.SharedEnv), settings.GetSharedEnv())
			}
		}
		proj.Stateful = proj.Stateful || mem.Stateful
		proj.Containers = append(proj.Containers, mem)
	}
	proj.Routes = readRoutes(pp, names)
	return proj, nil
}

func readDeployKey(p containerPaths) string {
	b, err := os.ReadFile(p.deployKeyPub())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// containerKind 返回容器的种类：single（单个容器，镜像表单）或 group（容器组，一份 compose）。
func containerKind(cfg *nagiv1.ContainerConfig) string {
	if isSingle(cfg) {
		return "single"
	}
	return "group"
}

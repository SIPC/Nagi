package project

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/store/projectstore"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 项目元数据在 project.db 的 meta 表里的键。
const (
	metaID              = "id"
	metaName            = "name"
	metaRevision        = "revision"
	metaDeployedVersion = "deployed_version"
	metaCreatedAt       = "created_at"
	metaWebhookToken    = "webhook_token"
	metaLastCommit      = "last_commit"
)

// projectDB 是项目目录里的 config/project.db。
type projectDB struct {
	db *sql.DB
	q  *projectstore.Queries
}

func openProject(ctx context.Context, dir string) (*projectDB, error) {
	d, err := db.OpenMigrated(ctx, paths{root: dir}.projectDB(), projectstore.Migrations)
	if err != nil {
		return nil, err
	}
	return &projectDB{db: d, q: projectstore.New(d)}, nil
}

func (p *projectDB) Close() error { return p.db.Close() }

func (p *projectDB) meta(ctx context.Context, key string) string {
	v, _ := p.q.GetMeta(ctx, key)
	return v
}

func (p *projectDB) setMeta(ctx context.Context, key, value string) error {
	return p.q.SetMeta(ctx, projectstore.SetMetaParams{Key: key, Value: value})
}

func (p *projectDB) revision(ctx context.Context) (int64, error) {
	v, err := p.q.GetMeta(ctx, metaRevision)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

func (p *projectDB) setRevision(ctx context.Context, rev int64) error {
	return p.setMeta(ctx, metaRevision, strconv.FormatInt(rev, 10))
}

func (p *projectDB) insertVersion(ctx context.Context, version int64, cfg *nagiv1.ProjectConfig) error {
	data, err := proto.Marshal(cfg)
	if err != nil {
		return err
	}
	return p.q.InsertVersion(ctx, projectstore.InsertVersionParams{
		Version: version, Config: data, CreatedAt: time.Now().Unix(),
	})
}

// latest 返回最新版本的配置和版本号。
func (p *projectDB) latest(ctx context.Context) (*nagiv1.ProjectConfig, int64, error) {
	v, err := p.q.LatestVersion(ctx)
	if err != nil {
		return nil, 0, err
	}
	cfg := &nagiv1.ProjectConfig{}
	if err := proto.Unmarshal(v.Config, cfg); err != nil {
		return nil, 0, err
	}
	return cfg, v.Version, nil
}

// deployed 返回最近一次部署的版本的配置；从未部署时返回 nil。
func (p *projectDB) deployed(ctx context.Context) (*nagiv1.ProjectConfig, error) {
	n, _ := strconv.ParseInt(p.meta(ctx, metaDeployedVersion), 10, 64)
	if n == 0 {
		return nil, nil
	}
	v, err := p.q.GetVersion(ctx, n)
	if err != nil {
		return nil, err
	}
	cfg := &nagiv1.ProjectConfig{}
	return cfg, proto.Unmarshal(v.Config, cfg)
}

// draft 返回草稿；没有草稿时返回 nil。
func (p *projectDB) draft(ctx context.Context) (*nagiv1.ProjectConfig, error) {
	d, err := p.q.GetDraft(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := &nagiv1.ProjectConfig{}
	return cfg, proto.Unmarshal(d.Config, cfg)
}

func (p *projectDB) setDraft(ctx context.Context, cfg *nagiv1.ProjectConfig) error {
	data, err := proto.Marshal(cfg)
	if err != nil {
		return err
	}
	return p.q.SetDraft(ctx, projectstore.SetDraftParams{Config: data, UpdatedAt: time.Now().Unix()})
}

// project 组装对外的项目信息：元数据、最新版本的配置、草稿。
func (p *projectDB) project(ctx context.Context, id, dir string) (*nagiv1.Project, error) {
	cfg, version, err := p.latest(ctx)
	if err != nil {
		return nil, err
	}
	draft, err := p.draft(ctx)
	if err != nil {
		return nil, err
	}
	revision, _ := strconv.ParseInt(p.meta(ctx, metaRevision), 10, 64)
	deployed, _ := strconv.ParseInt(p.meta(ctx, metaDeployedVersion), 10, 64)
	created, _ := strconv.ParseInt(p.meta(ctx, metaCreatedAt), 10, 64)
	return &nagiv1.Project{
		Id:              id,
		Name:            p.meta(ctx, metaName),
		Dir:             dir,
		Revision:        revision,
		LatestVersion:   version,
		DeployedVersion: deployed,
		CreatedAt:       timestamppb.New(time.Unix(created, 0)),
		Config:          cfg,
		HasDraft:        draft != nil,
		Draft:           draft,
		DeployKey:       readDeployKey(paths{root: dir}),
		WebhookToken:    p.meta(ctx, metaWebhookToken),
		LastCommit:      p.meta(ctx, metaLastCommit),
		Routes:          readRoutes(paths{root: dir}),
	}, nil
}

func readDeployKey(p paths) string {
	b, err := os.ReadFile(p.deployKeyPub())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

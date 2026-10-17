// Package project 管理节点上的项目：项目目录、版本、override 生成、部署和状态。
// 节点是项目数据的主人，这里的数据以节点本地为准。
package project

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/agent/store/projectstore"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 错误类型，供上层转换成对应的 gRPC 状态码。
var (
	ErrNotFound        = errors.New("项目不存在")
	ErrInvalidArgument = errors.New("参数无效")
	ErrConflict        = errors.New("项目已被其他人修改，请刷新后重试")
)

// DefaultSubnetPool 是节点本地的容器网段池，每个项目分一个 /26。
const DefaultSubnetPool = "10.200.0.0/16"

const projectSubnetBits = 26

// Manager 管理本节点的所有项目。
type Manager struct {
	nodeDB *sql.DB
	nodeQ  *nodestore.Queries
	docker *docker.Client
	pool   netip.Prefix

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewManager 创建项目管理器。
func NewManager(nodeDB *sql.DB, dc *docker.Client, pool string) (*Manager, error) {
	pfx, err := netip.ParsePrefix(pool)
	if err != nil {
		return nil, fmt.Errorf("容器网段池 %q 无效: %w", pool, err)
	}
	if !pfx.Addr().Is4() || pfx.Bits() > projectSubnetBits {
		return nil, fmt.Errorf("容器网段池 %q 必须是 IPv4 且不小于 /%d", pool, projectSubnetBits)
	}
	return &Manager{
		nodeDB: nodeDB,
		nodeQ:  nodestore.New(nodeDB),
		docker: dc,
		pool:   pfx.Masked(),
		locks:  map[string]*sync.Mutex{},
	}, nil
}

// lock 返回某个项目的互斥锁，保证同一项目的修改和部署串行执行。
func (m *Manager) lock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[id]
	if !ok {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	return l
}

// ---- 项目目录 ----

type paths struct{ root string }

func (p paths) configDir() string   { return filepath.Join(p.root, "config") }
func (p paths) dataDir() string     { return filepath.Join(p.root, "data") }
func (p paths) volumesDir() string  { return filepath.Join(p.root, "volumes") }
func (p paths) backupsDir() string  { return filepath.Join(p.root, "backups") }
func (p paths) composeFile() string { return filepath.Join(p.configDir(), "compose.yml") }
func (p paths) overrideFile() string {
	return filepath.Join(p.configDir(), "override.yml")
}
func (p paths) envFile() string   { return filepath.Join(p.configDir(), ".env") }
func (p paths) projectDB() string { return filepath.Join(p.configDir(), "project.db") }

func composeProjectName(id string) string { return "nagi-" + id }

// 项目元数据在 project.db 的 meta 表里的键。
const (
	metaID              = "id"
	metaName            = "name"
	metaRevision        = "revision"
	metaDeployedVersion = "deployed_version"
	metaCreatedAt       = "created_at"
)

// ---- 创建 ----

// Create 创建项目：建目录、校验 compose、分配网段、记录第一个版本。
func (m *Manager) Create(ctx context.Context, name, dir, composeText string) (*nagiv1.Project, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, fmt.Errorf("%w：项目名不能为空，且不超过 64 个字符", ErrInvalidArgument)
	}
	if strings.TrimSpace(composeText) == "" {
		return nil, fmt.Errorf("%w：compose 不能为空", ErrInvalidArgument)
	}
	dir, err := m.checkDir(ctx, dir)
	if err != nil {
		return nil, err
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	p := paths{root: dir}
	for _, d := range []string{p.configDir(), p.dataDir(), p.volumesDir(), p.backupsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("创建项目目录: %w", err)
		}
	}
	if err := os.Chmod(p.configDir(), 0o700); err != nil {
		return nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }

	if err := writeFileAtomic(p.composeFile(), []byte(composeText), 0o644); err != nil {
		cleanup()
		return nil, err
	}
	if err := writeFileAtomic(p.envFile(), nil, 0o600); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := load(ctx, p, composeProjectName(id)); err != nil {
		cleanup()
		return nil, fmt.Errorf("%w：compose 无法解析：%v", ErrInvalidArgument, err)
	}

	subnet, err := m.allocateSubnet(ctx)
	if err != nil {
		cleanup()
		return nil, err
	}

	now := time.Now()
	pdb, err := db.OpenMigrated(ctx, p.projectDB(), projectstore.Migrations)
	if err != nil {
		cleanup()
		return nil, err
	}
	defer pdb.Close()
	pq := projectstore.New(pdb)
	for k, v := range map[string]string{
		metaID:              id,
		metaName:            name,
		metaRevision:        "1",
		metaDeployedVersion: "0",
		metaCreatedAt:       strconv.FormatInt(now.Unix(), 10),
	} {
		if err := pq.SetMeta(ctx, projectstore.SetMetaParams{Key: k, Value: v}); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := pq.InsertVersion(ctx, projectstore.InsertVersionParams{
		Version: 1, Compose: composeText, CreatedAt: now.Unix(),
	}); err != nil {
		cleanup()
		return nil, err
	}

	if err := m.nodeQ.InsertProject(ctx, nodestore.InsertProjectParams{
		ID: id, Dir: dir, Subnet: subnet, CreatedAt: now.Unix(),
	}); err != nil {
		cleanup()
		return nil, err
	}
	return m.readProject(ctx, id, dir)
}

// checkDir 校验项目目录：必须是绝对路径；已存在时必须为空；不能和其他项目的目录互相嵌套。
func (m *Manager) checkDir(ctx context.Context, dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%w：项目目录必须是绝对路径", ErrInvalidArgument)
	}
	dir = filepath.Clean(dir)
	if dir == filepath.Dir(dir) {
		return "", fmt.Errorf("%w：不能使用根目录作为项目目录", ErrInvalidArgument)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return "", fmt.Errorf("%w：目录 %s 已存在且不为空", ErrInvalidArgument, dir)
	}
	existing, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	for _, e := range existing {
		if within(dir, e.Dir) || within(e.Dir, dir) {
			return "", fmt.Errorf("%w：目录和已有项目的目录 %s 重叠", ErrInvalidArgument, e.Dir)
		}
	}
	return dir, nil
}

// within 判断 child 是否等于 parent 或位于 parent 之内。
func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// allocateSubnet 从容器网段池里找第一个没用过的 /26。
func (m *Manager) allocateSubnet(ctx context.Context) (string, error) {
	used, err := m.nodeQ.ListSubnets(ctx)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(used))
	for _, s := range used {
		taken[s] = true
	}
	size := uint32(1) << (32 - projectSubnetBits)
	count := uint32(1) << (projectSubnetBits - m.pool.Bits())
	base := m.pool.Addr().As4()
	start := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	for i := uint32(0); i < count; i++ {
		v := start + i*size
		addr := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		s := netip.PrefixFrom(addr, projectSubnetBits).String()
		if !taken[s] {
			return s, nil
		}
	}
	return "", fmt.Errorf("容器网段池 %s 已用完", m.pool)
}

// ---- 读取 ----

// List 返回本节点所有项目及其状态。
func (m *Manager) List(ctx context.Context) ([]*nagiv1.ProjectSummary, error) {
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*nagiv1.ProjectSummary, 0, len(rows))
	for _, r := range rows {
		s, err := m.summary(ctx, r.ID, r.Dir)
		if err != nil {
			// 单个项目读取失败不影响其他项目，只返回基本信息。
			s = &nagiv1.ProjectSummary{Project: &nagiv1.Project{Id: r.ID, Name: r.ID, Dir: r.Dir}}
		}
		out = append(out, s)
	}
	return out, nil
}

// Get 返回单个项目及其状态。
func (m *Manager) Get(ctx context.Context, id string) (*nagiv1.ProjectSummary, error) {
	row, err := m.nodeQ.GetProject(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return m.summary(ctx, row.ID, row.Dir)
}

func (m *Manager) summary(ctx context.Context, id, dir string) (*nagiv1.ProjectSummary, error) {
	proj, err := m.readProject(ctx, id, dir)
	if err != nil {
		return nil, err
	}
	status, err := m.status(ctx, id, dir)
	if err != nil {
		return nil, err
	}
	return &nagiv1.ProjectSummary{Project: proj, Status: status}, nil
}

func (m *Manager) readProject(ctx context.Context, id, dir string) (*nagiv1.Project, error) {
	p := paths{root: dir}
	pdb, err := db.OpenMigrated(ctx, p.projectDB(), projectstore.Migrations)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	pq := projectstore.New(pdb)

	meta := func(k string) string {
		v, _ := pq.GetMeta(ctx, k)
		return v
	}
	latest, err := pq.LatestVersion(ctx)
	if err != nil {
		return nil, err
	}
	revision, _ := strconv.ParseInt(meta(metaRevision), 10, 64)
	deployed, _ := strconv.ParseInt(meta(metaDeployedVersion), 10, 64)
	created, _ := strconv.ParseInt(meta(metaCreatedAt), 10, 64)
	return &nagiv1.Project{
		Id:              id,
		Name:            meta(metaName),
		Dir:             dir,
		Revision:        revision,
		LatestVersion:   latest.Version,
		DeployedVersion: deployed,
		Compose:         latest.Compose,
		CreatedAt:       timestamppb.New(time.Unix(created, 0)),
	}, nil
}

// status 合并 compose 里声明的服务和实际运行的容器。
func (m *Manager) status(ctx context.Context, id, dir string) (*nagiv1.ProjectStatus, error) {
	containers, err := m.docker.ProjectContainers(ctx, composeProjectName(id))
	if err != nil {
		return nil, err
	}
	st := &nagiv1.ProjectStatus{}
	seen := map[string]bool{}
	for _, c := range containers {
		seen[c.Service] = true
		st.Services = append(st.Services, &nagiv1.ServiceStatus{
			Name: c.Service, ContainerId: c.ID, Image: c.Image,
			State: c.State, Status: c.Status, Health: c.Health,
		})
	}
	// compose 里有、但还没有容器的服务（例如从未部署）。
	if proj, err := load(ctx, paths{root: dir}, composeProjectName(id)); err == nil {
		for _, name := range proj.ServiceNames() {
			if !seen[name] {
				st.Services = append(st.Services, &nagiv1.ServiceStatus{
					Name: name, Image: proj.Services[name].Image,
				})
			}
		}
	}
	return st, nil
}

// ---- 部署 ----

// Deploy 部署项目的最新版本。baseRevision 不为 0 时，必须和节点上的修订号一致。
func (m *Manager) Deploy(ctx context.Context, id string, baseRevision int64, onLine func(string)) (int64, error) {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	row, err := m.nodeQ.GetProject(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	p := paths{root: row.Dir}
	pdb, err := db.OpenMigrated(ctx, p.projectDB(), projectstore.Migrations)
	if err != nil {
		return 0, err
	}
	defer pdb.Close()
	pq := projectstore.New(pdb)

	revStr, err := pq.GetMeta(ctx, metaRevision)
	if err != nil {
		return 0, err
	}
	revision, _ := strconv.ParseInt(revStr, 10, 64)
	if baseRevision != 0 && baseRevision != revision {
		return 0, ErrConflict
	}

	latest, err := pq.LatestVersion(ctx)
	if err != nil {
		return 0, err
	}
	if err := writeFileAtomic(p.composeFile(), []byte(latest.Compose), 0o644); err != nil {
		return 0, err
	}
	proj, err := load(ctx, p, composeProjectName(id))
	if err != nil {
		return 0, fmt.Errorf("compose 无法解析：%w", err)
	}
	override, err := buildOverride(proj, p, id, latest.Version, row.Subnet)
	if err != nil {
		return 0, err
	}
	if err := writeOverride(p.overrideFile(), override); err != nil {
		return 0, err
	}

	onLine(fmt.Sprintf("部署版本 %d", latest.Version))
	err = compose.Up(ctx, compose.Files{
		ProjectName: composeProjectName(id),
		ProjectDir:  p.dataDir(),
		ComposeFile: p.composeFile(),
		Override:    p.overrideFile(),
		EnvFile:     p.envFile(),
	}, onLine)
	if err != nil {
		return 0, err
	}

	now := time.Now().Unix()
	if err := pq.MarkDeployed(ctx, projectstore.MarkDeployedParams{
		DeployedAt: sql.NullInt64{Int64: now, Valid: true}, Version: latest.Version,
	}); err != nil {
		return 0, err
	}
	if err := pq.SetMeta(ctx, projectstore.SetMetaParams{Key: metaDeployedVersion, Value: strconv.FormatInt(latest.Version, 10)}); err != nil {
		return 0, err
	}
	if err := pq.SetMeta(ctx, projectstore.SetMetaParams{Key: metaRevision, Value: strconv.FormatInt(revision+1, 10)}); err != nil {
		return 0, err
	}
	return latest.Version, nil
}

// ---- 日志 ----

// Logs 读取项目的容器日志。service 为空时读取所有服务。follow 时持续输出直到 ctx 取消。
func (m *Manager) Logs(ctx context.Context, id, service string, tail int, follow bool, onLine func(docker.LogLine)) error {
	if _, err := m.nodeQ.GetProject(ctx, id); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	containers, err := m.docker.ProjectContainers(ctx, composeProjectName(id))
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range containers {
		if service != "" && c.Service != service {
			continue
		}
		wg.Add(1)
		go func(c docker.Container) {
			defer wg.Done()
			_ = m.docker.Logs(ctx, c, tail, follow, func(l docker.LogLine) {
				mu.Lock()
				defer mu.Unlock()
				onLine(l)
			})
		}(c)
	}
	wg.Wait()
	return nil
}

// ---- 工具 ----

// newID 生成全局唯一的项目 ID：16 个十六进制字符。
func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

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
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 错误类型，供上层转换成对应的 gRPC 状态码。
var (
	ErrNotFound        = errors.New("项目不存在")
	ErrInvalidArgument = errors.New("参数无效")
	ErrConflict        = errors.New("项目已被其他人修改，请刷新后重试")
)

// invalidError 是参数错误：errors.Is 能识别为 ErrInvalidArgument，但消息只包含具体原因。
type invalidError struct{ msg string }

func (e invalidError) Error() string        { return e.msg }
func (e invalidError) Is(target error) bool { return target == ErrInvalidArgument }

func invalid(format string, args ...any) error {
	return invalidError{msg: fmt.Sprintf(format, args...)}
}

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

// ---- 创建 ----

// Create 创建项目：校验配置、建目录、分配网段、记录第一个版本。创建后不立即部署。
func (m *Manager) Create(ctx context.Context, name, dir string, cfg *nagiv1.ProjectConfig) (*nagiv1.Project, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, invalid("项目名不能为空，且不超过 64 个字符")
	}
	if cfg == nil {
		return nil, invalid("缺少项目配置")
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
	if _, err := validate(ctx, cfg, p.dataDir(), composeProjectName(id)); err != nil {
		return nil, err
	}
	if err := m.checkConflicts(ctx, id, cfg.GetExposures()); err != nil {
		return nil, err
	}
	subnet, err := m.allocateSubnet(ctx)
	if err != nil {
		return nil, err
	}

	for _, d := range []string{p.configDir(), p.dataDir(), p.volumesDir(), p.backupsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("创建项目目录: %w", err)
		}
	}
	if err := os.Chmod(p.configDir(), 0o700); err != nil {
		return nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }

	pdb, err := openProject(ctx, dir)
	if err != nil {
		cleanup()
		return nil, err
	}
	defer pdb.Close()
	if _, err := ensureDeployKey(p); err != nil {
		cleanup()
		return nil, err
	}
	webhook, err := randomToken()
	if err != nil {
		cleanup()
		return nil, err
	}
	now := time.Now()
	for k, v := range map[string]string{
		metaID:              id,
		metaName:            name,
		metaRevision:        "1",
		metaDeployedVersion: "0",
		metaCreatedAt:       strconv.FormatInt(now.Unix(), 10),
		metaWebhookToken:    webhook,
	} {
		if err := pdb.setMeta(ctx, k, v); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := pdb.insertVersion(ctx, 1, cfg); err != nil {
		cleanup()
		return nil, err
	}
	if err := writeConfigFiles(p, cfg); err != nil {
		cleanup()
		return nil, err
	}
	if err := m.nodeQ.InsertProject(ctx, nodestore.InsertProjectParams{
		ID: id, Dir: dir, Subnet: subnet, CreatedAt: now.Unix(),
	}); err != nil {
		cleanup()
		return nil, err
	}
	return pdb.project(ctx, id, dir)
}

// checkDir 校验项目目录：必须是绝对路径；已存在时必须为空；不能和其他项目的目录互相嵌套。
func (m *Manager) checkDir(ctx context.Context, dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return "", invalid("项目目录必须是绝对路径")
	}
	dir = filepath.Clean(dir)
	if dir == filepath.Dir(dir) {
		return "", invalid("不能使用根目录作为项目目录")
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return "", invalid("目录 %s 已存在且不为空", dir)
	}
	existing, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	for _, e := range existing {
		if within(dir, e.Dir) || within(e.Dir, dir) {
			return "", invalid("目录和已有项目的目录 %s 重叠", e.Dir)
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

// allocateSubnet 从容器网段池里找第一个没用过、也不和 Docker 现有网络重叠的 /26。
func (m *Manager) allocateSubnet(ctx context.Context) (string, error) {
	used, err := m.nodeQ.ListSubnets(ctx)
	if err != nil {
		return "", err
	}
	existing, err := m.docker.NetworkSubnets(ctx)
	if err != nil {
		return "", fmt.Errorf("读取 Docker 网络: %w", err)
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
		p := netip.PrefixFrom(addr, projectSubnetBits)
		if taken[p.String()] || overlapsAny(p, existing) {
			continue
		}
		return p.String(), nil
	}
	return "", fmt.Errorf("容器网段池 %s 已用完", m.pool)
}

func overlapsAny(p netip.Prefix, list []netip.Prefix) bool {
	for _, q := range list {
		if p.Overlaps(q) {
			return true
		}
	}
	return false
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
	pdb, err := openProject(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	return pdb.project(ctx, id, dir)
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
	pdb, err := openProject(ctx, dir)
	if err != nil {
		return st, nil
	}
	defer pdb.Close()
	if cfg, _, err := pdb.latest(ctx); err == nil {
		if text, err := composeText(cfg); err == nil {
			if proj, err := loadCompose(ctx, text, cfg.GetEnv(), paths{root: dir}.dataDir(), composeProjectName(id)); err == nil {
				for _, name := range proj.ServiceNames() {
					if !seen[name] {
						st.Services = append(st.Services, &nagiv1.ServiceStatus{Name: name, Image: proj.Services[name].Image})
					}
				}
			}
		}
	}
	return st, nil
}

// ---- 草稿 ----

// projectForUpdate 加锁并打开项目，检查修订号。调用方负责 unlock 和关闭数据库。
func (m *Manager) projectForUpdate(ctx context.Context, id string, baseRevision int64) (row nodestore.Project, pdb *projectDB, revision int64, unlock func(), err error) {
	l := m.lock(id)
	l.Lock()
	unlock = l.Unlock
	defer func() {
		if err != nil {
			if pdb != nil {
				pdb.Close()
			}
			unlock()
		}
	}()
	row, err = m.nodeQ.GetProject(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return row, nil, 0, unlock, ErrNotFound
	}
	if err != nil {
		return row, nil, 0, unlock, err
	}
	pdb, err = openProject(ctx, row.Dir)
	if err != nil {
		return row, nil, 0, unlock, err
	}
	revision, err = pdb.revision(ctx)
	if err != nil {
		return row, pdb, 0, unlock, err
	}
	if baseRevision != 0 && baseRevision != revision {
		return row, pdb, 0, unlock, ErrConflict
	}
	return row, pdb, revision, unlock, nil
}

// SaveDraft 保存草稿。草稿要先通过校验，部署时才会成为新版本。
func (m *Manager) SaveDraft(ctx context.Context, id string, baseRevision int64, cfg *nagiv1.ProjectConfig) (*nagiv1.Project, error) {
	if cfg == nil {
		return nil, invalid("缺少项目配置")
	}
	row, pdb, revision, unlock, err := m.projectForUpdate(ctx, id, baseRevision)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer pdb.Close()
	if _, err := validate(ctx, cfg, paths{root: row.Dir}.dataDir(), composeProjectName(id)); err != nil {
		return nil, err
	}
	if err := m.checkConflicts(ctx, id, cfg.GetExposures()); err != nil {
		return nil, err
	}
	if err := pdb.setDraft(ctx, cfg); err != nil {
		return nil, err
	}
	if err := pdb.setRevision(ctx, revision+1); err != nil {
		return nil, err
	}
	return pdb.project(ctx, id, row.Dir)
}

// DiscardDraft 放弃草稿。
func (m *Manager) DiscardDraft(ctx context.Context, id string, baseRevision int64) (*nagiv1.Project, error) {
	row, pdb, revision, unlock, err := m.projectForUpdate(ctx, id, baseRevision)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer pdb.Close()
	if err := pdb.q.DeleteDraft(ctx); err != nil {
		return nil, err
	}
	if err := pdb.setRevision(ctx, revision+1); err != nil {
		return nil, err
	}
	return pdb.project(ctx, id, row.Dir)
}

// ---- 删除 ----

// Delete 删除项目：停止并删除容器和网络，从节点上移除记录。项目目录默认保留；
// deleteDir 时删除整个目录（含数据和快照），confirmName 必须等于项目名。
func (m *Manager) Delete(ctx context.Context, id string, baseRevision int64, deleteDir bool, confirmName string, onLine func(string)) error {
	row, pdb, _, unlock, err := m.projectForUpdate(ctx, id, baseRevision)
	if err != nil {
		return err
	}
	defer unlock()
	name := pdb.meta(ctx, metaName)
	pdb.Close()
	if deleteDir && confirmName != name {
		return invalid("要同时删除项目目录，请输入项目名 %s 确认", name)
	}
	p := paths{root: row.Dir}
	files := compose.Files{
		ProjectName: composeProjectName(id),
		ProjectDir:  p.dataDir(),
		ComposeFile: p.composeFile(),
		Override:    p.overrideFile(),
		EnvFile:     p.envFile(),
	}
	// 从未部署过的项目没有 override，也没有容器。
	if _, err := os.Stat(p.overrideFile()); err == nil {
		if err := compose.Down(ctx, files, deleteDir, onLine); err != nil {
			return err
		}
	}
	if err := m.nodeQ.DeleteProject(ctx, id); err != nil {
		return err
	}
	if deleteDir {
		if err := os.RemoveAll(row.Dir); err != nil {
			return fmt.Errorf("删除项目目录: %w", err)
		}
	}
	return nil
}

// ---- 部署 ----

// Deploy 部署项目。有草稿时先把草稿记为新版本；没有草稿时重新部署最新版本。
// baseRevision 不为 0 时，必须和节点上的修订号一致。
func (m *Manager) Deploy(ctx context.Context, id string, baseRevision int64, doPull bool, onLine func(string)) (int64, error) {
	row, pdb, revision, unlock, err := m.projectForUpdate(ctx, id, baseRevision)
	if err != nil {
		return 0, err
	}
	defer unlock()
	defer pdb.Close()
	p := paths{root: row.Dir}

	cfg, version, err := pdb.latest(ctx)
	if err != nil {
		return 0, err
	}
	if draft, err := pdb.draft(ctx); err != nil {
		return 0, err
	} else if draft != nil {
		version++
		if err := pdb.insertVersion(ctx, version, draft); err != nil {
			return 0, err
		}
		if err := pdb.q.DeleteDraft(ctx); err != nil {
			return 0, err
		}
		cfg = draft
		onLine(fmt.Sprintf("草稿已保存为版本 %d", version))
	}
	if err := pdb.setRevision(ctx, revision+1); err != nil {
		return 0, err
	}

	proj, err := validate(ctx, cfg, p.dataDir(), composeProjectName(id))
	if err != nil {
		return version, err
	}
	if doPull {
		preserve := normalizePreserve(append(append([]string(nil), cfg.GetPreserve()...), bindMountPaths(proj, p.dataDir())...))
		commit, err := pull(ctx, p, cfg.GetSource(), preserve, onLine)
		if err != nil {
			return version, err
		}
		if err := pdb.setMeta(ctx, metaLastCommit, commit); err != nil {
			return version, err
		}
	}
	if err := writeConfigFiles(p, cfg); err != nil {
		return version, err
	}
	override, err := buildOverride(proj, p, id, version, row.Subnet, serviceEnv(cfg))
	if err != nil {
		return version, err
	}
	if err := writeOverride(p.overrideFile(), override); err != nil {
		return version, err
	}

	onLine(fmt.Sprintf("部署版本 %d", version))
	files := compose.Files{
		ProjectName: composeProjectName(id),
		ProjectDir:  p.dataDir(),
		ComposeFile: p.composeFile(),
		Override:    p.overrideFile(),
		EnvFile:     p.envFile(),
	}
	if err := compose.Up(ctx, files, onLine); err != nil {
		return version, err
	}
	// 文件更新了但 compose 没变时，compose 不会重建容器，挂载了 data/ 的服务要强制重建。
	if svcs := dataServices(proj, p.dataDir()); doPull && len(svcs) > 0 {
		onLine("文件已更新，重建服务：" + strings.Join(svcs, "、"))
		if err := compose.Recreate(ctx, files, svcs, onLine); err != nil {
			return version, err
		}
	}
	if err := pdb.q.MarkDeployed(ctx, projectstore.MarkDeployedParams{
		DeployedAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}, Version: version,
	}); err != nil {
		return version, err
	}
	for _, e := range cfg.GetExposures() {
		onLine("对外开放：" + exposureLabel(e))
	}
	return version, pdb.setMeta(ctx, metaDeployedVersion, strconv.FormatInt(version, 10))
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

// ---- 终端 ----

// defaultShell 优先用 bash，没有 bash 就用 sh。
var defaultShell = []string{"/bin/sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"}

// Exec 在服务正在运行的容器里打开交互式终端。
func (m *Manager) Exec(ctx context.Context, id, service string, cmd []string, cols, rows uint) (*docker.Exec, error) {
	if _, err := m.nodeQ.GetProject(ctx, id); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	containers, err := m.docker.ProjectContainers(ctx, composeProjectName(id))
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if c.Service == service && c.State == "running" {
			if len(cmd) == 0 {
				cmd = defaultShell
			}
			return m.docker.StartExec(ctx, c.ID, cmd, cols, rows)
		}
	}
	return nil, invalid("服务 %s 没有正在运行的容器", service)
}

// ---- 工具 ----

// randomToken 生成 32 字节的随机令牌（十六进制）。
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

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

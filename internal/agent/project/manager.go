// Package project 管理节点上的项目：项目是一组容器（单容器或一份 compose），共用项目目录和内网，
// 每个容器各自部署、记版本、回滚。节点是项目数据的主人，这里的数据以节点本地为准。
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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
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

// notFoundError 是"某个东西不存在"：errors.Is 能识别为 ErrNotFound，消息是具体原因。
type notFoundError struct{ msg string }

func (e notFoundError) Error() string        { return e.msg }
func (e notFoundError) Is(target error) bool { return target == ErrNotFound }

// DefaultSubnetPool 是节点本地的容器网段池，项目内网、容器组、共享网络各分一个 /26。
const DefaultSubnetPool = "10.200.0.0/16"

const subnetBits = 26

// nameRe 是容器名、网络名的格式：小写字母、数字和 -，不以 - 开头或结尾，最长 32 个字符。
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Manager 管理本节点的所有项目。
type Manager struct {
	nodeDB *sql.DB
	nodeQ  *nodestore.Queries
	docker *docker.Client
	pool   netip.Prefix
	// stateDir 是节点目录下存放快照仓库连接配置和缓存的地方。
	stateDir string

	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	netLocks map[string]*sync.Mutex

	// FastSnapshot 返回 true 的项目每 15 分钟做一次定时快照（设置了顶替的项目）。
	FastSnapshot func(id string) bool
	// ManagedRestart 返回 true 的项目由节点代管重启，容器的重启策略写成 no（设置了顶替的项目）。
	ManagedRestart func(id string) bool
	// ReservedDomain 返回 true 的域名已经被面板使用（面板绑定的域名），项目不能再用。
	ReservedDomain func(domain string) bool
	// TrustedProject 判断面板是否允许项目在本节点从副本启动时使用特权配置（见 checkReplicaConfig）。
	TrustedProject func(id string) bool
	// RemoteProjects 返回和本节点共享网络 network 相连的节点上加入了它的项目（检查网络名是否重复）。
	// 有相连的节点联系不上时返回已经拿到的部分和错误。
	RemoteProjects func(ctx context.Context, network string) ([]*nagiv1.SharedNetworkProject, error)

	restarts managedRestarts
	images   imageUpdates
}

// NewManager 创建项目管理器。
func NewManager(nodeDB *sql.DB, dc *docker.Client, pool, stateDir string) (*Manager, error) {
	pfx, err := netip.ParsePrefix(pool)
	if err != nil {
		return nil, fmt.Errorf("容器网段池 %q 无效: %w", pool, err)
	}
	if !pfx.Addr().Is4() || pfx.Bits() > subnetBits {
		return nil, fmt.Errorf("容器网段池 %q 必须是 IPv4 且不小于 /%d", pool, subnetBits)
	}
	return &Manager{
		nodeDB:   nodeDB,
		nodeQ:    nodestore.New(nodeDB),
		docker:   dc,
		pool:     pfx.Masked(),
		stateDir: stateDir,
		locks:    map[string]*sync.Mutex{},
		netLocks: map[string]*sync.Mutex{},
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

// paths 是项目目录里的各个位置（见 docs/agent.md#项目目录）。
type paths struct{ root string }

func (p paths) configDir() string     { return filepath.Join(p.root, "config") }
func (p paths) containersDir() string { return filepath.Join(p.root, "containers") }
func (p paths) backupsDir() string    { return filepath.Join(p.root, "backups") }
func (p paths) projectDB() string     { return filepath.Join(p.configDir(), "project.db") }
func (p paths) sharedEnvFile() string { return filepath.Join(p.configDir(), ".env") }
func (p paths) container(name string) containerPaths {
	return containerPaths{root: filepath.Join(p.containersDir(), name)}
}

// containerPaths 是容器目录 containers/<容器>/ 里的各个位置。
type containerPaths struct{ root string }

func (p containerPaths) composeFile() string   { return filepath.Join(p.root, "compose.yml") }
func (p containerPaths) overrideFile() string  { return filepath.Join(p.root, "override.yml") }
func (p containerPaths) envFile() string       { return filepath.Join(p.root, ".env") }
func (p containerPaths) sharedEnvFile() string { return filepath.Join(p.root, "shared.env") }
func (p containerPaths) dataDir() string       { return filepath.Join(p.root, "data") }
func (p containerPaths) volumesDir() string    { return filepath.Join(p.root, "volumes") }
func (p containerPaths) restoreDir() string    { return filepath.Join(p.root, ".restore") }

// composeProjectName 是容器的 compose 项目名。
func composeProjectName(id, container string) string { return "nagi-" + id + "-" + container }

// projectLabel 选出项目所有容器的容器。
func projectLabel(id string) string { return LabelProject + "=" + id }

// ---- 创建 ----

// Create 创建项目：建目录、写格式版本号、保存设置。container 不为空时同时加入第一个容器。创建后不部署。
func (m *Manager) Create(ctx context.Context, name, dir string, settings *nagiv1.ProjectSettings, containerName string, container *nagiv1.ContainerConfig) (*nagiv1.Project, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, invalid("项目名不能为空，且不超过 64 个字符")
	}
	if settings == nil {
		settings = &nagiv1.ProjectSettings{}
	}
	settings = proto.Clone(settings).(*nagiv1.ProjectSettings)
	// 新项目不能直接加入共享网络（要先有网络名、网络存在），在项目设置里加。
	settings.SharedNetworks, settings.NetworkName = nil, ""
	if err := checkEnv(settings.GetSharedEnv()); err != nil {
		return nil, err
	}
	if container != nil {
		if err := checkContainerName(containerName); err != nil {
			return nil, err
		}
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	dir, err = m.checkDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	p := paths{root: dir}
	for _, d := range []string{p.configDir(), p.containersDir(), p.backupsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("创建项目目录: %w", err)
		}
	}
	if err := os.Chmod(p.configDir(), 0o700); err != nil {
		return nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	if err := writeFormat(p); err != nil {
		cleanup()
		return nil, err
	}
	pdb, err := openProject(ctx, dir)
	if err != nil {
		cleanup()
		return nil, err
	}
	now := time.Now()
	for k, v := range map[string]string{
		metaID:        id,
		metaName:      name,
		metaRevision:  "1",
		metaCreatedAt: strconv.FormatInt(now.Unix(), 10),
	} {
		if err := pdb.setMeta(ctx, k, v); err != nil {
			pdb.Close()
			cleanup()
			return nil, err
		}
	}
	if err := pdb.setSettings(ctx, settings); err == nil {
		err = writeFileAtomic(p.sharedEnvFile(), envFile(settings.GetSharedEnv()), 0o600)
	}
	if err != nil {
		pdb.Close()
		cleanup()
		return nil, err
	}
	if container != nil {
		if err := m.addContainer(ctx, pdb, p, id, containerName, container); err != nil {
			pdb.Close()
			cleanup()
			return nil, err
		}
	}
	pdb.Close()
	if err := m.nodeQ.InsertProject(ctx, nodestore.InsertProjectParams{ID: id, Dir: dir, CreatedAt: now.Unix()}); err != nil {
		cleanup()
		return nil, err
	}
	return m.getProject(ctx, id)
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
		return "", invalid("目录 %s 已存在且不为空：换一个目录；如果是迁移、顶替或删除项目后留下的旧目录，确认没用以后在节点上删掉它", dir)
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

// ---- 容器网段 ----

// subnetFor 返回某个网络的网段，没有时从容器网段池里分配第一个没用过、也不和 Docker 现有网络重叠的 /26。
// owner：p:<项目 ID>（项目内网）、m:<项目 ID>/<容器>（容器自己的网络）、s:<名字>（共享网络）。
func (m *Manager) subnetFor(ctx context.Context, owner string) (string, error) {
	if s, err := m.nodeQ.GetSubnet(ctx, owner); err == nil {
		return s, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
	size := uint32(1) << (32 - subnetBits)
	count := uint32(1) << (subnetBits - m.pool.Bits())
	base := m.pool.Addr().As4()
	start := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	for i := uint32(0); i < count; i++ {
		v := start + i*size
		addr := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		p := netip.PrefixFrom(addr, subnetBits)
		if taken[p.String()] || overlapsAny(p, existing) {
			continue
		}
		if err := m.nodeQ.InsertSubnet(ctx, nodestore.InsertSubnetParams{Subnet: p.String(), Owner: owner}); err != nil {
			return "", err
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

func projectOwner(id string) string          { return "p:" + id }
func groupOwner(id, container string) string { return "g:" + id + "/" + container }

// releaseProject 删除节点上关于项目的记录：项目行、网段、快照仓库的连接配置。
func (m *Manager) releaseProject(ctx context.Context, id string) error {
	if err := m.nodeQ.DeleteProject(ctx, id); err != nil {
		return err
	}
	_ = m.nodeQ.DeleteProjectSubnets(ctx, nodestore.DeleteProjectSubnetsParams{ProjectOwner: projectOwner(id), GroupPrefix: groupOwner(id, "%")})
	os.RemoveAll(filepath.Join(m.stateDir, id))
	return nil
}

// ---- 读取 ----

// List 返回本节点所有项目及其状态。
// Dirs 返回所有项目目录。
func (m *Manager) Dirs(ctx context.Context) []string {
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Dir)
	}
	return out
}

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
			s = &nagiv1.ProjectSummary{Project: &nagiv1.Project{Id: r.ID, Name: r.ID, Dir: r.Dir}, Error: err.Error()}
		}
		out = append(out, s)
	}
	return out, nil
}

// projectRow 返回节点数据库里的项目记录。
func (m *Manager) projectRow(ctx context.Context, id string) (nodestore.Project, error) {
	row, err := m.nodeQ.GetProject(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrNotFound
	}
	return row, err
}

// Get 返回单个项目及其状态。
func (m *Manager) Get(ctx context.Context, id string) (*nagiv1.ProjectSummary, error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.summary(ctx, row.ID, row.Dir)
}

func (m *Manager) getProject(ctx context.Context, id string) (*nagiv1.Project, error) {
	s, err := m.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.GetProject(), nil
}

// Name 返回项目名称，读不到时返回 ID。用于告警内容。
func (m *Manager) Name(ctx context.Context, id string) string {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return id
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return id
	}
	defer pdb.Close()
	if n := pdb.meta(ctx, metaName); n != "" {
		return n
	}
	return id
}

func (m *Manager) summary(ctx context.Context, id, dir string) (*nagiv1.ProjectSummary, error) {
	pdb, err := openProject(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	proj, err := pdb.project(ctx, id, dir)
	if err != nil {
		return nil, err
	}
	status, err := m.status(ctx, id, dir, proj)
	if err != nil {
		return nil, err
	}
	m.imageStatus(id, status)
	return &nagiv1.ProjectSummary{Project: proj, Status: status, ReadOnlyReason: pdb.readOnlyReason}, nil
}

// status 合并各容器 compose 里声明的服务和实际的容器。
func (m *Manager) status(ctx context.Context, id, dir string, proj *nagiv1.Project) (*nagiv1.ProjectStatus, error) {
	containers, err := m.docker.Instances(ctx, projectLabel(id))
	if err != nil {
		return nil, err
	}
	st := &nagiv1.ProjectStatus{}
	seen := map[string]bool{}
	for _, c := range containers {
		seen[c.Container+"/"+c.Service] = true
		st.Services = append(st.Services, &nagiv1.ServiceStatus{
			Container: c.Container, Name: c.Service, ContainerId: c.ID, Image: c.Image,
			State: c.State, Status: c.Status, Health: c.Health,
		})
	}
	// compose 里有、但还没有容器的服务（例如从未部署）。
	shared := proj.GetSettings().GetSharedEnv()
	for _, mem := range proj.GetContainers() {
		cfg := mem.GetConfig()
		cp, err := loadContainer(ctx, cfg, mem.GetName(), shared, paths{root: dir}.container(mem.GetName()).dataDir(), composeProjectName(id, mem.GetName()))
		if err != nil {
			continue
		}
		for _, name := range cp.ServiceNames() {
			if !seen[mem.GetName()+"/"+name] {
				st.Services = append(st.Services, &nagiv1.ServiceStatus{Container: mem.GetName(), Name: name, Image: cp.Services[name].Image})
			}
		}
	}
	return st, nil
}

// ---- 修改 ----

// projectForUpdate 加锁并打开项目，检查项目不在迁移中。调用方负责 unlock 和关闭数据库。
func (m *Manager) projectForUpdate(ctx context.Context, id string) (row nodestore.Project, pdb *projectDB, unlock func(), err error) {
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
	row, err = m.projectRow(ctx, id)
	if err != nil {
		return row, nil, unlock, err
	}
	pdb, err = openProject(ctx, row.Dir)
	if err != nil {
		return row, nil, unlock, err
	}
	if pdb.meta(ctx, metaMigrating) == "1" {
		return row, pdb, unlock, invalid("项目正在迁移到其他节点，暂时不能修改")
	}
	return row, pdb, unlock, nil
}

// ---- 删除 ----

// Delete 删除项目：停止并删除所有容器的容器和项目网络，从节点上移除记录。项目目录默认保留；
// deleteDir 时删除整个目录（含数据和快照），confirmName 必须等于项目名。
func (m *Manager) Delete(ctx context.Context, id string, baseRevision int64, deleteDir bool, confirmName string, onLine func(string)) error {
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if err := pdb.checkRevision(ctx, baseRevision); err != nil {
		pdb.Close()
		return err
	}
	name := pdb.meta(ctx, metaName)
	containers, err := pdb.q.ListContainers(ctx)
	pdb.Close()
	if err != nil {
		return err
	}
	if deleteDir && confirmName != name {
		return invalid("要同时删除项目目录，请输入项目名 %s 确认", name)
	}
	p := paths{root: row.Dir}
	for _, mem := range containers {
		if err := m.down(ctx, id, p, mem.Name, onLine); err != nil {
			return err
		}
	}
	_ = m.docker.RemoveNetwork(ctx, projectNetwork(id))
	if err := m.releaseProject(ctx, id); err != nil {
		return err
	}
	if deleteDir {
		if err := os.RemoveAll(row.Dir); err != nil {
			return fmt.Errorf("删除项目目录: %w", err)
		}
	}
	return nil
}

// down 停止并删除容器的容器（命名卷都绑定在容器目录里，删除卷对象不会删数据）。从未部署过的容器什么都不做。
func (m *Manager) down(ctx context.Context, id string, p paths, container string, onLine func(string)) error {
	mp := p.container(container)
	if _, err := os.Stat(mp.overrideFile()); err != nil {
		return nil
	}
	return compose.Down(ctx, m.composeFiles(id, p, container), true, onLine)
}

// ---- 日志 ----

// Logs 读取项目的容器日志。container、service 为空时读取全部。follow 时持续输出直到 ctx 取消。
func (m *Manager) Logs(ctx context.Context, id, container, service string, tail int, follow bool, onLine func(docker.LogLine)) error {
	if _, err := m.projectRow(ctx, id); err != nil {
		return err
	}
	containers, err := m.docker.Instances(ctx, projectLabel(id))
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range containers {
		if (container != "" && c.Container != container) || (service != "" && c.Service != service) {
			continue
		}
		wg.Add(1)
		go func(c docker.Instance) {
			defer wg.Done()
			_ = m.docker.Logs(ctx, c, tail, follow, func(l docker.LogLine) {
				mu.Lock()
				defer mu.Unlock()
				l.Container = c.Container
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

// Exec 在容器的服务正在运行的容器里打开交互式终端。单个容器的 service 可以为空。
func (m *Manager) Exec(ctx context.Context, id, container, service string, cmd []string, cols, rows uint) (*docker.Exec, error) {
	if _, err := m.projectRow(ctx, id); err != nil {
		return nil, err
	}
	containers, err := m.docker.Instances(ctx, projectLabel(id))
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if c.Container == container && (service == "" || c.Service == service) && c.State == "running" {
			if len(cmd) == 0 {
				cmd = defaultShell
			}
			return m.docker.StartExec(ctx, c.ID, cmd, cols, rows)
		}
	}
	return nil, invalid("容器组 %s 里的容器 %s 没有在运行", container, service)
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

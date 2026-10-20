// Package project 在节点上运行容器：面板把容器的完整配置（ContainerSpec）下发过来，这里照着部署、
// 删除、快照和恢复，并把配置缓存在节点数据库里（见 docs/agent.md）。项目和容器的配置以面板为准，
// 节点上的缓存只用来在面板不可达时重启、恢复和顶替。
package project

import (
	"context"
	"database/sql"
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
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 错误类型，供上层转换成对应的 gRPC 状态码。
var (
	ErrNotFound        = errors.New("容器不存在")
	ErrInvalidArgument = spec.ErrInvalid
)

// notFoundError 是"某个东西不存在"：errors.Is 能识别为 ErrNotFound，消息是具体原因。
type notFoundError struct{ msg string }

func (e notFoundError) Error() string        { return e.msg }
func (e notFoundError) Is(target error) bool { return target == ErrNotFound }

func invalid(format string, args ...any) error { return spec.Invalid(format, args...) }

// DefaultSubnetPool 是节点本地的容器网段池：项目内网各分一个 /24，容器组各分一个 /26。
const DefaultSubnetPool = "10.200.0.0/16"

const (
	projectSubnetBits = 24
	groupSubnetBits   = 26
)

// DefaultProjectsRoot 是默认的项目根目录：容器目录默认是 <项目根目录>/<网络名>/<容器名>。
const DefaultProjectsRoot = "/srv/nagi"

// Manager 管理本节点上的容器。
type Manager struct {
	nodeDB *sql.DB
	nodeQ  *nodestore.Queries
	docker *docker.Client
	pool   netip.Prefix
	// stateDir 是节点目录下存放快照仓库连接配置和缓存的地方。
	stateDir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// ManagedRestart 返回 true 的容器由节点代管重启，重启策略写成 no（设置了顶替的容器）。
	ManagedRestart func(projectID, container string) bool
	// FastSnapshot 返回 true 的容器每 15 分钟做一次定时快照（设置了顶替的容器）。
	FastSnapshot func(projectID, container string) bool

	restarts managedRestarts
	images   imageUpdates
}

// NewManager 创建容器管理器。
func NewManager(nodeDB *sql.DB, dc *docker.Client, pool, stateDir string) (*Manager, error) {
	pfx, err := netip.ParsePrefix(pool)
	if err != nil {
		return nil, fmt.Errorf("容器网段池 %q 无效: %w", pool, err)
	}
	if !pfx.Addr().Is4() || pfx.Bits() > projectSubnetBits {
		return nil, fmt.Errorf("容器网段池 %q 必须是 IPv4 且不小于 /%d", pool, projectSubnetBits)
	}
	return &Manager{
		nodeDB:   nodeDB,
		nodeQ:    nodestore.New(nodeDB),
		docker:   dc,
		pool:     pfx.Masked(),
		stateDir: stateDir,
		locks:    map[string]*sync.Mutex{},
	}, nil
}

// uid 是容器在节点上的标识：<项目 ID>-<容器名>，和它的 compose 项目名的后半部分一样。
// 项目 ID 是固定 16 位的十六进制，所以不会有歧义。
func uid(projectID, container string) string { return projectID + "-" + container }

// lock 返回某个容器的互斥锁，保证同一个容器的部署、快照、删除串行执行。
func (m *Manager) lock(projectID, container string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := uid(projectID, container)
	l, ok := m.locks[k]
	if !ok {
		l = &sync.Mutex{}
		m.locks[k] = l
	}
	return l
}

// ---- 容器目录 ----

// containerPaths 是容器目录里的各个位置（见 docs/agent.md#容器目录）。
type containerPaths struct{ root string }

func (p containerPaths) formatFile() string    { return filepath.Join(p.root, "format") }
func (p containerPaths) composeFile() string   { return filepath.Join(p.root, "compose.yml") }
func (p containerPaths) overrideFile() string  { return filepath.Join(p.root, "override.yml") }
func (p containerPaths) envFile() string       { return filepath.Join(p.root, ".env") }
func (p containerPaths) sharedEnvFile() string { return filepath.Join(p.root, "shared.env") }
func (p containerPaths) dockerConfigDir() string {
	return filepath.Join(p.root, "docker")
}
func (p containerPaths) dataDir() string    { return filepath.Join(p.root, "data") }
func (p containerPaths) volumesDir() string { return filepath.Join(p.root, "volumes") }
func (p containerPaths) backupsDir() string { return filepath.Join(p.root, "backups") }
func (p containerPaths) restoreDir() string { return filepath.Join(p.root, ".restore") }

// formatVersion 是容器目录的格式版本号，记在 format 文件里。容器目录会跟着容器在不同版本的节点之间搬，
// 节点只打开自己认识的格式。以后改目录结构时加一，并在打开时就地升级。
const formatVersion = 1

func writeFormat(p containerPaths) error {
	return writeFileAtomic(p.formatFile(), []byte(strconv.Itoa(formatVersion)+"\n"), 0o644)
}

// checkFormat 检查容器目录的格式本节点能不能处理。目录里还没有 format 文件时算新目录。
func checkFormat(p containerPaths) error {
	b, err := os.ReadFile(p.formatFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return invalid("容器目录 %s 的格式版本号读不懂", p.root)
	}
	if v > formatVersion {
		return invalid("容器目录的格式版本是 %d，本节点只支持到 %d，请先升级节点", v, formatVersion)
	}
	return nil
}

// composeFiles 返回容器调用 compose 用的文件。
func composeFiles(s *nagiv1.ContainerSpec) compose.Files {
	mp := containerPaths{root: s.GetDir()}
	f := compose.Files{
		ProjectName: spec.ComposeProjectName(s.GetProjectId(), s.GetName()),
		ProjectDir:  mp.dataDir(),
		ComposeFile: mp.composeFile(),
		Override:    mp.overrideFile(),
		EnvFiles:    []string{mp.sharedEnvFile(), mp.envFile()},
	}
	// 项目登录过私有仓库时用它自己的 docker 配置；没有时用机器全局的（节点上全局 docker login 过的照常能用）。
	if len(s.GetDockerConfig()) > 0 {
		f.DockerConfig = mp.dockerConfigDir()
	}
	return f
}

// containerLabel 选出一个容器（容器组是组里所有容器）的 Docker 容器。
func containerLabel(projectID, container string) string {
	return docker.ComposeLabel(spec.ComposeProjectName(projectID, container))
}

// projectLabel 选出本节点上一个项目所有容器的 Docker 容器。
func projectLabel(id string) string { return LabelProject + "=" + id }

// ---- 配置缓存 ----

// Cached 是节点上缓存的一个容器。
type Cached struct {
	Spec         *nagiv1.ContainerSpec
	FilesRoot    string
	LastSnapshot time.Time
	LastMaintain time.Time
}

func fromRow(r nodestore.Container) (*Cached, error) {
	s := &nagiv1.ContainerSpec{}
	if err := proto.Unmarshal(r.Spec, s); err != nil {
		return nil, fmt.Errorf("读取容器 %s 的配置缓存: %w", r.Name, err)
	}
	return &Cached{Spec: s, FilesRoot: r.FilesRoot, LastSnapshot: time.Unix(r.LastSnapshot, 0), LastMaintain: time.Unix(r.LastMaintenance, 0)}, nil
}

// Get 返回缓存的容器。
func (m *Manager) Get(ctx context.Context, projectID, container string) (*Cached, error) {
	r, err := m.nodeQ.GetContainer(ctx, nodestore.GetContainerParams{ProjectID: projectID, Name: container})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundError{fmt.Sprintf("本节点上没有容器 %s", container)}
	}
	if err != nil {
		return nil, err
	}
	return fromRow(r)
}

// List 返回本节点上缓存的所有容器。读不懂的跳过。
func (m *Manager) List(ctx context.Context) []*Cached {
	rows, err := m.nodeQ.ListContainers(ctx)
	if err != nil {
		return nil
	}
	out := make([]*Cached, 0, len(rows))
	for _, r := range rows {
		if c, err := fromRow(r); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// projectContainers 返回本节点上某个项目的容器。
func (m *Manager) projectContainers(ctx context.Context, projectID string) []*Cached {
	rows, err := m.nodeQ.ListProjectContainers(ctx, projectID)
	if err != nil {
		return nil
	}
	out := make([]*Cached, 0, len(rows))
	for _, r := range rows {
		if c, err := fromRow(r); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func (m *Manager) store(ctx context.Context, s *nagiv1.ContainerSpec) error {
	data, err := proto.Marshal(s)
	if err != nil {
		return err
	}
	return m.nodeQ.UpsertContainer(ctx, nodestore.UpsertContainerParams{
		ProjectID: s.GetProjectId(), Name: s.GetName(), Dir: s.GetDir(), Spec: data, CreatedAt: time.Now().Unix(),
	})
}

// Dirs 返回所有容器目录（磁盘告警、错误说明里查剩余空间用）。
func (m *Manager) Dirs(ctx context.Context) []string {
	var out []string
	for _, c := range m.List(ctx) {
		out = append(out, c.Spec.GetDir())
	}
	return out
}

// ProjectName 返回项目名称，本节点上没有这个项目的容器时返回 ID。用于告警内容。
func (m *Manager) ProjectName(ctx context.Context, projectID string) string {
	for _, c := range m.projectContainers(ctx, projectID) {
		if n := c.Spec.GetProjectName(); n != "" {
			return n
		}
	}
	return projectID
}

// ---- 下发配置 ----

// checkSpec 检查面板下发的配置的基本字段。
func checkSpec(s *nagiv1.ContainerSpec) error {
	if len(s.GetProjectId()) != 16 {
		return invalid("项目 ID 无效")
	}
	if err := spec.CheckContainerName(s.GetName()); err != nil {
		return err
	}
	if s.GetConfig() == nil {
		return invalid("缺少容器配置")
	}
	return nil
}

// Put 保存面板下发的配置（不部署）。容器第一次放到本节点时检查目录：必须是绝对路径，不存在或为空，
// 不和本节点其他容器的目录重叠；之后目录不能改。端口映射、快照间隔这些改了立即生效（Caddy 和定时快照都读缓存）。
func (m *Manager) Put(ctx context.Context, s *nagiv1.ContainerSpec) error {
	if err := checkSpec(s); err != nil {
		return err
	}
	l := m.lock(s.GetProjectId(), s.GetName())
	l.Lock()
	defer l.Unlock()
	old, err := m.Get(ctx, s.GetProjectId(), s.GetName())
	switch {
	case err == nil:
		if old.Spec.GetDir() != s.GetDir() {
			return invalid("容器目录不能修改（%s）", old.Spec.GetDir())
		}
	case errors.Is(err, ErrNotFound):
		dir, err := m.CheckDir(ctx, s.GetDir())
		if err != nil {
			return err
		}
		s = proto.Clone(s).(*nagiv1.ContainerSpec)
		s.Dir = dir
		if err := m.prepareDir(containerPaths{root: dir}); err != nil {
			return err
		}
	default:
		return err
	}
	if err := m.checkNewPorts(ctx, s); err != nil {
		return err
	}
	return m.store(ctx, s)
}

// prepareDir 建好容器目录。
func (m *Manager) prepareDir(p containerPaths) error {
	for _, d := range []string{p.dataDir(), p.volumesDir(), p.backupsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建容器目录: %w", err)
		}
	}
	if _, err := os.Stat(p.formatFile()); errors.Is(err, os.ErrNotExist) {
		return writeFormat(p)
	}
	return checkFormat(p)
}

// CheckDir 校验新容器的目录：必须是绝对路径；已存在时必须为空；不能和本节点其他容器的目录互相嵌套。
func (m *Manager) CheckDir(ctx context.Context, dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) || !strings.HasPrefix(dir, "/") {
		return "", invalid("容器目录必须是绝对路径")
	}
	dir = filepath.Clean(dir)
	if dir == filepath.Dir(dir) {
		return "", invalid("不能使用根目录作为容器目录")
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return "", invalid("目录 %s 已存在且不为空：换一个目录；如果是迁移、顶替或删除后留下的旧目录，确认没用以后在节点上删掉它", dir)
	}
	for _, c := range m.List(ctx) {
		if within(dir, c.Spec.GetDir()) || within(c.Spec.GetDir(), dir) {
			return "", invalid("目录和本节点上容器 %s 的目录 %s 重叠", c.Spec.GetName(), c.Spec.GetDir())
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

// subnetFor 返回某个网络的网段，没有时从容器网段池里分配第一个没用过、也不和 Docker 现有网络重叠的网段。
// owner：p:<项目 ID>（项目内网，/24）、g:<项目 ID>/<容器组>（容器组自己的网络，/26）。
func (m *Manager) subnetFor(ctx context.Context, owner string, bits int) (string, error) {
	if s, err := m.nodeQ.GetSubnet(ctx, owner); err == nil {
		return s, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	usedRows, err := m.nodeQ.ListSubnets(ctx)
	if err != nil {
		return "", err
	}
	var used []netip.Prefix
	for _, s := range usedRows {
		if p, err := netip.ParsePrefix(s); err == nil {
			used = append(used, p)
		}
	}
	existing, err := m.docker.NetworkSubnets(ctx)
	if err != nil {
		return "", fmt.Errorf("读取 Docker 网络: %w", err)
	}
	if bits < m.pool.Bits() {
		bits = m.pool.Bits()
	}
	size := uint32(1) << (32 - bits)
	count := uint32(1) << (bits - m.pool.Bits())
	base := m.pool.Addr().As4()
	start := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	for i := uint32(0); i < count; i++ {
		v := start + i*size
		addr := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		p := netip.PrefixFrom(addr, bits)
		if overlapsAny(p, used) || overlapsAny(p, existing) {
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

// ---- 状态 ----

// Statuses 返回本节点上所有容器的运行状态：实际的 Docker 容器，加上 compose 里有、但还没有容器的服务。
func (m *Manager) Statuses(ctx context.Context) []*nagiv1.ContainerStatus {
	cached := m.List(ctx)
	instances, _ := m.docker.Instances(ctx, LabelProject)
	byContainer := map[string][]docker.Instance{}
	for _, c := range instances {
		byContainer[uid(c.Project, c.Container)] = append(byContainer[uid(c.Project, c.Container)], c)
	}
	out := make([]*nagiv1.ContainerStatus, 0, len(cached))
	for _, c := range cached {
		s := c.Spec
		st := &nagiv1.ContainerStatus{ProjectId: s.GetProjectId(), Name: s.GetName(), Revision: s.GetRevision(), ProjectRevision: s.GetProjectRevision()}
		seen := map[string]bool{}
		for _, in := range byContainer[uid(s.GetProjectId(), s.GetName())] {
			seen[in.Service] = true
			st.Services = append(st.Services, &nagiv1.ServiceStatus{
				Container: s.GetName(), Name: in.Service, ContainerId: in.ID, Image: in.Image,
				State: in.State, Status: in.Status, Health: in.Health,
			})
		}
		if cp, err := spec.Load(ctx, s.GetConfig(), s.GetName(), s.GetSharedEnv(), containerPaths{root: s.GetDir()}.dataDir(), spec.ComposeProjectName(s.GetProjectId(), s.GetName())); err == nil {
			for _, name := range cp.ServiceNames() {
				if !seen[name] {
					st.Services = append(st.Services, &nagiv1.ServiceStatus{Container: s.GetName(), Name: name, Image: cp.Services[name].Image})
				}
			}
		}
		if _, err := os.Stat(s.GetDir()); err != nil {
			st.Error = "容器目录读不到：" + err.Error()
		}
		m.imageStatus(s.GetProjectId(), s.GetName(), st)
		out = append(out, st)
	}
	return out
}

// ---- 删除 ----

// Delete 删除容器：停止并删除它的 Docker 容器，删除缓存和网段。容器目录默认保留，deleteDir 时一起删除。
// 本节点上已经没有这个项目的其他容器时，删除项目内网。
func (m *Manager) Delete(ctx context.Context, projectID, container string, deleteDir bool, onLine func(string)) error {
	l := m.lock(projectID, container)
	l.Lock()
	defer l.Unlock()
	c, err := m.Get(ctx, projectID, container)
	if errors.Is(err, ErrNotFound) {
		return nil // 已经删除过
	}
	if err != nil {
		return err
	}
	if err := m.down(ctx, c.Spec, onLine); err != nil {
		return err
	}
	_ = m.docker.Kill(ctx, containerLabel(projectID, container))
	if err := m.nodeQ.DeleteContainer(ctx, nodestore.DeleteContainerParams{ProjectID: projectID, Name: container}); err != nil {
		return err
	}
	_ = m.nodeQ.DeleteSubnet(ctx, groupOwner(projectID, container))
	os.RemoveAll(filepath.Join(m.stateDir, uid(projectID, container)))
	m.clearImageUpdates(projectID, container)
	if len(m.projectContainers(ctx, projectID)) == 0 {
		_ = m.docker.RemoveNetwork(ctx, projectNetwork(projectID))
		_ = m.nodeQ.DeleteSubnet(ctx, projectOwner(projectID))
	}
	if deleteDir {
		if err := os.RemoveAll(c.Spec.GetDir()); err != nil {
			return fmt.Errorf("删除容器目录: %w", err)
		}
	}
	return nil
}

// down 停止并删除容器（命名卷都绑定在容器目录里，删除卷对象不会删数据）。从未部署过的容器什么都不做。
func (m *Manager) down(ctx context.Context, s *nagiv1.ContainerSpec, onLine func(string)) error {
	if _, err := os.Stat(containerPaths{root: s.GetDir()}.overrideFile()); err != nil {
		return nil
	}
	return compose.Down(ctx, composeFiles(s), true, onLine)
}

// ---- 日志 ----

// Logs 读取容器的日志（容器组可以按服务过滤）。follow 时持续输出直到 ctx 取消。
func (m *Manager) Logs(ctx context.Context, projectID, container, service string, tail int, follow bool, onLine func(docker.LogLine)) error {
	if _, err := m.Get(ctx, projectID, container); err != nil {
		return err
	}
	instances, err := m.docker.Instances(ctx, containerLabel(projectID, container))
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range instances {
		if service != "" && c.Service != service {
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

// Exec 在容器（容器组里的某个服务）正在运行的 Docker 容器里打开交互式终端。单个容器的 service 可以为空。
func (m *Manager) Exec(ctx context.Context, projectID, container, service string, cmd []string, cols, rows uint) (*docker.Exec, error) {
	if _, err := m.Get(ctx, projectID, container); err != nil {
		return nil, err
	}
	instances, err := m.docker.Instances(ctx, containerLabel(projectID, container))
	if err != nil {
		return nil, err
	}
	for _, c := range instances {
		if (service == "" || c.Service == service) && c.State == "running" {
			if len(cmd) == 0 {
				cmd = defaultShell
			}
			return m.docker.StartExec(ctx, c.ID, cmd, cols, rows)
		}
	}
	return nil, invalid("容器 %s 没有在运行", service)
}

// ---- 工具 ----

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

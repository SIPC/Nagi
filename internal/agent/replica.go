package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 备份节点和迁移（见 docs/snapshot.md#备份节点的实现、docs/failover.md#手动迁移），按容器进行。
//   - 源节点：每次定时快照成功后，把容器的快照仓库经 mesh 增量依次同步到各个备份节点（在面板下发的 ContainerSpec.backups 里）；
//   - 目标节点：副本放在 <存放位置>/<项目 ID>-<容器名>/files/repo（存放位置默认是 <节点目录>/replicas，见 replica_dirs.go），
//     只接受登记过的源节点同步；
//     恢复用的配置是面板登记副本时给的（spec.pb），不从源节点拿。

var projectIDRe = regexp.MustCompile(`^[a-f0-9]{16}$`)

type replicaMeta struct {
	ProjectID string `json:"project_id"`
	Container string `json:"container"`
	SourceKey []byte `json:"source_key"`
	// ExtraSourceKeys：也接受这些节点的同步（设置了顶替时，运行容器的可能是另一个参与者）。
	ExtraSourceKeys [][]byte `json:"extra_source_keys,omitempty"`
	SourceNodeID    string   `json:"source_node_id"`
	SourceName      string   `json:"source_name"`
	LastSync        int64    `json:"last_sync"`
}

type replicas struct {
	dir  string // <节点目录>/replicas：没有设置默认目录时的存放位置
	file string // <节点目录>/replica-dirs.json：默认目录和不在默认目录里的副本（见 replica_dirs.go）
	// mu 保护 idx、locks、syncing。
	mu  sync.Mutex
	idx replicaIndex
	// broken：启动时 file 读不出来（损坏或读取出错），见 markCorrupt、markUnreadable；brokenCause 是原因，写进日志和告警。
	// salvaged 是从损坏的 file 里认出来的路径，算进存放位置（见 bases）。
	broken      error
	brokenCause string
	salvaged    []string
	// moving：同时只有一次移动（修改默认目录、移动一个副本）。
	moving sync.Mutex
	// layout：save 新建副本时拿读锁，修改默认目录、记下旧目录里有哪些副本时拿写锁，两者不会交错。
	layout sync.RWMutex
	// 校验存放位置用（见 checkBase）：系统目录、Nagi 自己的目录、容器目录、项目根目录、节点文件禁止访问的目录。
	forbidden    []string
	reserved     []string
	managed      func() []string
	projectsRoot func() string
	denied       func(real string) bool
	// stateDir 是副本快照仓库的连接配置和缓存，副本删除、换了位置时一起删掉。
	stateDir func(projectID, container string) string
	// 每个副本一把锁：同步写入、查看快照、恢复不能同时进行。
	locks map[string]*sync.RWMutex
	// syncing 记着每个副本正在同步的源节点和它最近一次请求的时间（见 claim）。
	syncing map[string]replicaSync
}

type replicaSync struct {
	source []byte
	at     time.Time
}

const (
	// replicaSyncIdle：源节点这么久没有请求，就算这次同步结束了，别的源节点可以开始。
	replicaSyncIdle = 2 * time.Minute
	// replicaMassDelete：一次提交要删掉超过这个比例（并且超过 replicaMassDeleteMin 个）的文件时，只接受登记的源节点。
	replicaMassDelete    = 0.5
	replicaMassDeleteMin = 20
)

// claim 让 source 占用副本的同步。几个参与者都能同步同一个副本时（设置了顶替），接收端同时只接受一个源的会话：
// 否则两个源交替 Put 和 Commit，A 的 Commit 会删掉 B 刚写入的文件。登记的源节点（容器所在的节点，primary）优先：
// 它随时可以接手；另一方只在没有别的源在 replicaSyncIdle 内同步过时才能开始，被攻破的另一方挡不住正常同步。
func (r *replicas) claim(projectID, container string, source []byte, primary bool, now time.Time) error {
	id := projectID + "-" + container
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.syncing == nil {
		r.syncing = map[string]replicaSync{}
	}
	if cur, ok := r.syncing[id]; ok && !primary && !slices.Equal(cur.source, source) && now.Sub(cur.at) < replicaSyncIdle {
		return status.Error(codes.Aborted, "另一个节点正在同步这个副本，稍后重试")
	}
	r.syncing[id] = replicaSync{source: slices.Clone(source), at: now}
	return nil
}

// release 在 source 提交以后结束它的同步。
func (r *replicas) release(projectID, container string, source []byte) {
	id := projectID + "-" + container
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.syncing[id]; ok && slices.Equal(cur.source, source) {
		delete(r.syncing, id)
	}
}

// massDelete 判断一次提交要不要删掉副本里的大部分文件。
func massDelete(existing, removed int) bool {
	return removed > replicaMassDeleteMin && float64(removed) > float64(existing)*replicaMassDelete
}

func replicaID(projectID, container string) (string, error) {
	if !projectIDRe.MatchString(projectID) {
		return "", status.Error(codes.InvalidArgument, "项目 ID 格式不正确")
	}
	if err := spec.CheckContainerName(container); err != nil {
		return "", status.Error(codes.InvalidArgument, err.Error())
	}
	return projectID + "-" + container, nil
}

// root 是副本的目录：<存放位置>/<项目 ID>-<容器名>。副本可能被移动（见 replica_dirs.go），
// 要用这个目录读写时先拿到副本的锁再调用，免得拿到的是移走以前的位置。
func (r *replicas) root(projectID, container string) (string, error) {
	id, err := replicaID(projectID, container)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	base, _ := r.baseLocked(id)
	return filepath.Join(base, id), nil
}

func (r *replicas) lock(projectID, container string) func() {
	l := r.lockOf(projectID, container)
	l.Lock()
	return l.Unlock
}

// rlock 是接收文件时加的锁：同一次同步的几条流（见 syncStreams）各写各的文件，可以同时拿；
// 提交、恢复等要改整个副本的操作用 lock，等它们都结束。
func (r *replicas) rlock(projectID, container string) func() {
	l := r.lockOf(projectID, container)
	l.RLock()
	return l.RUnlock
}

func (r *replicas) lockOf(projectID, container string) *sync.RWMutex {
	id := projectID + "-" + container
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locks == nil {
		r.locks = map[string]*sync.RWMutex{}
	}
	l := r.locks[id]
	if l == nil {
		l = &sync.RWMutex{}
		r.locks[id] = l
	}
	return l
}

// load 读取副本的登记信息和恢复用的配置。
func (r *replicas) load(projectID, container string) (*replicaMeta, *nagiv1.ContainerSpec, string, error) {
	root, err := r.root(projectID, container)
	if err != nil {
		return nil, nil, "", err
	}
	b, err := os.ReadFile(filepath.Join(root, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, root, status.Error(codes.NotFound, "此节点上没有该容器的副本")
	}
	if err != nil {
		return nil, nil, root, err
	}
	m := &replicaMeta{}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, nil, root, err
	}
	s := &nagiv1.ContainerSpec{}
	raw, err := os.ReadFile(filepath.Join(root, "spec.pb"))
	if err != nil {
		return nil, nil, root, err
	}
	if err := proto.Unmarshal(raw, s); err != nil {
		return nil, nil, root, err
	}
	return m, s, root, nil
}

// save 写入登记信息（副本还没有时新建）。调用时持有这个副本的锁。
// 副本目录里的写入经过 os.Root：副本目录或者里面的东西被换成符号链接时，不会写到别处。
func (r *replicas) save(m *replicaMeta, s *nagiv1.ContainerSpec) error {
	r.layout.RLock()
	defer r.layout.RUnlock()
	dir, err := r.root(m.ProjectID, m.Container)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := checkPlainDirs(dir); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Join("files", "repo"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := writeAtomicIn(root, "meta.json", b); err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	raw, err := proto.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomicIn(root, "spec.pb", raw)
}

// writeAtomic 写到临时文件、落盘后改名，再让所在目录落盘：断电时要么是旧内容，要么是新内容，不会是截断的文件。
func writeAtomic(path string, data []byte) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	return writeAtomicIn(root, filepath.Base(path), data)
}

// writeAtomicIn 和 writeAtomic 一样，name 是 root 里的路径。
// 改名成功就算写入成功：之后目录落盘失败只记日志（见 syncRenamed），调用方不能当成失败去回滚内存，
// 否则内存和磁盘上的内容不一致（审查 N3）。
func writeAtomicIn(root *os.Root, name string, data []byte) error {
	tmp := name + ".tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		root.Remove(tmp)
		return err
	}
	syncRenamed(filepath.Join(root.Name(), filepath.Dir(name)))
	return nil
}

// syncDir 让目录项（改名的结果）落盘。Windows 上不能这样打开目录，跳过。测试里替换。
var syncDir = func(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// syncRenamed 在改名以后让目录落盘。失败时内容已经换成新的了，只记日志；文件系统不支持对目录 fsync
// （EINVAL、ENOTSUP）时不记，免得每次写入都报一条。
func syncRenamed(dir string) {
	err := syncDir(dir)
	if err == nil || errors.Is(err, syscall.EINVAL) || errors.Is(err, errors.ErrUnsupported) {
		return
	}
	slog.Warn("写入后目录落盘失败，内容已经写入", "dir", dir, "err", err)
}

// replicaStateDir 是副本快照仓库的连接配置和缓存，和容器自己的分开。
func (a *agent) replicaStateDir(projectID, container string) string {
	return filepath.Join(a.cfg.DataDir, "snapshots", "replica-"+projectID+"-"+container)
}

// ---- 源节点上的同步状态 ----

// backupTargets 返回容器的备份节点。旧版本的面板只下发 backup 一个。
func backupTargets(s *nagiv1.ContainerSpec) []*nagiv1.BackupTarget {
	if len(s.GetBackups()) > 0 {
		return s.GetBackups()
	}
	if t := s.GetBackup(); t != nil {
		return []*nagiv1.BackupTarget{t}
	}
	return nil
}

type backupEntry struct {
	Target    string `json:"target"` // 备份节点 ID
	LastSync  int64  `json:"last_sync"`
	LastError string `json:"last_error"`
	// Stopped：最近一次同步是用户停止的（LastError 是停止的说明，不告警）。
	Stopped bool `json:"stopped,omitempty"`
	// Interrupted：同步进行中节点退出了，启动后接着同步（见 resumeInterruptedSyncs）。
	Interrupted bool `json:"interrupted,omitempty"`
	// run 是正在进行（或排着队）的同步。
	run *syncRun
}

// backupStates 记录每个容器同步到各个备份节点的结果，保存在 <节点目录>/backup-status.json。
// 键是"项目 ID-容器名"，第一个备份节点只用这个键（和只有一个备份节点的旧版本一样），其余的再加"/节点 ID"。
type backupStates struct {
	file string
	mu   sync.Mutex
	m    map[string]*backupEntry
}

func newBackupStates(file string) *backupStates {
	b := &backupStates{file: file, m: map[string]*backupEntry{}}
	if raw, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(raw, &b.m)
	}
	return b
}

// entry 返回容器同步到 target 的状态。调用时持有 mu。
func (b *backupStates) entry(id, target string) *backupEntry {
	if e := b.m[id]; e != nil && e.Target == target {
		return e
	}
	key := id + "/" + target
	e := b.m[key]
	if e == nil {
		e = &backupEntry{Target: target}
		// 第一个键空着（或者那个备份节点已经去掉了）就用它，升级前后第一个备份节点的状态接得上。
		if old := b.m[id]; old == nil || old.Target == "" {
			key = id
		}
		b.m[key] = e
	}
	return e
}

// prune 去掉容器已经不用的备份节点的状态。调用时持有 mu。
func (b *backupStates) prune(id string, targets []*nagiv1.BackupTarget) {
	keep := map[string]bool{}
	for _, t := range targets {
		keep[t.GetNodeId()] = true
	}
	for k, e := range b.m {
		if (k == id || strings.HasPrefix(k, id+"/")) && !keep[e.Target] {
			delete(b.m, k)
		}
	}
}

func (b *backupStates) persist() {
	raw, _ := json.Marshal(b.m)
	_ = writeAtomic(b.file, raw)
}

// statuses 返回容器同步到各个备份节点的状态，按备份节点的顺序；没有设置备份节点时返回 nil。
func (b *backupStates) statuses(s *nagiv1.ContainerSpec) []*nagiv1.BackupStatus {
	targets := backupTargets(s)
	if len(targets) == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*nagiv1.BackupStatus, 0, len(targets))
	for _, t := range targets {
		e := b.entry(s.GetProjectId()+"-"+s.GetName(), t.GetNodeId())
		st := &nagiv1.BackupStatus{TargetNodeId: t.GetNodeId(), TargetName: t.GetName(), LastError: e.LastError, Syncing: e.run != nil, Stopped: e.Stopped}
		if e.LastSync > 0 {
			st.LastSync = timestamppb.New(time.Unix(e.LastSync, 0))
		}
		if e.run != nil {
			e.run.progress.fill(st)
		}
		out = append(out, st)
	}
	return out
}

// failures 返回同步失败的备份节点和原因，用于告警。调用时持有 mu。
func (b *backupStates) failures(id string, targets []*nagiv1.BackupTarget) []string {
	var out []string
	for _, t := range targets {
		if e := b.entry(id, t.GetNodeId()); e.LastError != "" && !e.Stopped {
			out = append(out, t.GetName()+"："+e.LastError)
		}
	}
	return out
}

// ---- 目标节点：MeshService ----

// authorizeReplica 校验调用方就是这个副本登记的源节点，返回副本的文件目录。write 表示要写入（Put、Commit），
// 写入要占用这次同步（见 replicas.claim）；只读的请求（Manifest）不占用，也不续上占用。
// 调用方先拿着副本的锁（rlock 或 lock）：副本可能被移到别的存放位置，拿锁以后返回的目录才不会变。
func (s *meshService) authorizeReplica(ctx context.Context, projectID, container string, write bool) (string, error) {
	caller, ok := mesh.Caller(ctx, s.M)
	if !ok {
		return "", status.Error(codes.PermissionDenied, "无法确认调用方")
	}
	m, _, root, err := s.a.replicas.load(projectID, container)
	if err != nil {
		return "", err
	}
	if !slices.Equal(m.SourceKey, caller[:]) && !slices.ContainsFunc(m.ExtraSourceKeys, func(k []byte) bool { return slices.Equal(k, caller[:]) }) {
		s.a.violations.addCheck(caller, "replica:sync", projectID, container, 0, "not the replica's source node", func() bool {
			// 容器迁移、换了顶替节点以后，副本的源节点要等新配置到了才改过来。
			m, _, _, err := s.a.replicas.load(projectID, container)
			return err == nil && !slices.Equal(m.SourceKey, caller[:]) && !slices.ContainsFunc(m.ExtraSourceKeys, func(k []byte) bool { return slices.Equal(k, caller[:]) })
		})
		return "", status.Error(codes.PermissionDenied, "该容器的副本不接受来自此节点的同步")
	}
	if write {
		if err := s.a.replicas.claim(projectID, container, caller[:], slices.Equal(m.SourceKey, caller[:]), time.Now()); err != nil {
			return "", err
		}
	}
	return filepath.Join(root, "files"), nil
}

func (s *meshService) ReplicaManifest(ctx context.Context, req *nagiv1.ReplicaManifestRequest) (*nagiv1.ReplicaManifestResponse, error) {
	if _, err := replicaID(req.GetProjectId(), req.GetContainer()); err != nil {
		return nil, err
	}
	defer s.a.replicas.rlock(req.GetProjectId(), req.GetContainer())()
	files, err := s.authorizeReplica(ctx, req.GetProjectId(), req.GetContainer(), false)
	if err != nil {
		return nil, err
	}
	return replicaManifest(files), nil
}

// replicaManifest 列出副本 files 目录里的文件和传了一半的文件（见 replicaPartSuffix）。
func replicaManifest(files string) *nagiv1.ReplicaManifestResponse {
	out := &nagiv1.ReplicaManifestResponse{}
	_ = filepath.WalkDir(files, func(full string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(files, full)
		rel = filepath.ToSlash(rel)
		if dest, ok := strings.CutSuffix(rel, replicaPartSuffix); ok {
			out.Partials = append(out.Partials, &nagiv1.ReplicaFile{Path: dest, Size: info.Size()})
			return nil
		}
		if isReplicaTemp(d.Name()) {
			return nil
		}
		out.Files = append(out.Files, &nagiv1.ReplicaFile{Path: rel, Size: info.Size()})
		return nil
	})
	return out
}

// replicaTempMark 是接收中的临时文件名里的标记：<文件名>.nagi-tmp-<随机>。随机后缀让同时写同一个文件的两条流
// 各写各的临时文件，不会互相覆盖；清单不列它们，提交时作为多余的文件删掉。
const replicaTempMark = ".nagi-tmp"

// replicaPartSuffix 是传了一半的文件：<文件名>.nagi-tmp-part。流断开时（连接断了、超时、源节点停止），
// 收到的部分不少于 replicaPartMin 就把临时文件改成这个名字留着，清单里报给源节点（partials），下次从这里接着传；
// 接着传时先改名成新的临时文件（只有一条流能拿到它），核对源节点给的前一半的 SHA-256，对不上就丢掉从头传。
// 提交时它和其他多余的文件一起删掉。
const replicaPartSuffix = replicaTempMark + "-part"

// replicaPartMin：收到的部分太少时不留，从头传也不费事。
const replicaPartMin = 1 << 20

func isReplicaTemp(name string) bool { return strings.Contains(name, replicaTempMark) }

// replicaIdle 是同步副本时多久收不到数据就放弃：源节点挂了、网络断了时不一直占着锁和临时文件。
const replicaIdle = 2 * time.Minute

// replicaPutMax 是一条 ReplicaPut 流最长的时间（文案里写的是 6 小时，改的话一起改）。
const replicaPutMax = 6 * time.Hour

// ReplicaPut 接收源节点同步来的文件。不限大小（快照仓库可以很大），但磁盘剩余空间不够时停下（见 metrics.CheckDisk）。
func (s *meshService) ReplicaPut(stream nagiv1.MeshService_ReplicaPutServer) error {
	return receiveReplica(stream, func(projectID, container string) (string, func(), error) {
		if _, err := replicaID(projectID, container); err != nil {
			return "", nil, err
		}
		unlock := s.a.replicas.rlock(projectID, container)
		files, err := s.authorizeReplica(stream.Context(), projectID, container, true)
		if err != nil {
			unlock()
			return "", nil, err
		}
		return files, unlock, nil
	})
}

// receiveReplica 是 ReplicaPut 的实现。authorize 在收到第一块时核对调用方、拿副本的锁，返回副本的 files 目录和释放锁的函数。
func receiveReplica(stream nagiv1.MeshService_ReplicaPutServer, authorize func(projectID, container string) (string, func(), error)) error {
	var (
		files, path string
		// root 是副本的 files 目录：写入都经过它，副本里被换成指向外面的符号链接时拒绝，不会写到别处（见 openReplicaFiles）。
		root      *os.Root
		f         *os.File
		tmp, dest string // 相对 root 的路径
		size      int64  // 当前文件已经写了多少（包括接着传时原有的部分）
		count     uint32
		unlock    func()
		guard     *metrics.DiskGuard
	)
	finish := func() error {
		if f == nil {
			return nil
		}
		err := f.Close()
		f = nil
		if err == nil {
			err = root.Rename(tmp, dest)
		}
		if err != nil {
			root.Remove(tmp)
			return err
		}
		count++
		return nil
	}
	defer func() {
		if f != nil {
			// 流断开了：收到的部分够多就留着，下次接着传（见 replicaPartSuffix）。
			f.Close()
			if size < replicaPartMin || root.Rename(tmp, dest+replicaPartSuffix) != nil {
				root.Remove(tmp)
			}
		}
		if root != nil {
			root.Close()
		}
		if unlock != nil {
			unlock()
		}
	}()
	// 单独的 goroutine 收数据，这里等的时候能超时。返回以后流结束，Recv 随之出错退出。
	type recv struct {
		c   *nagiv1.ReplicaChunk
		err error
	}
	// 带缓冲：这边写盘的时候那边接着收，写盘慢一点也不会马上让发送方停下来。
	ch := make(chan recv, 16)
	go func() {
		for {
			c, err := stream.Recv()
			select {
			case ch <- recv{c, err}:
			case <-stream.Context().Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	idle := time.NewTimer(replicaIdle)
	defer idle.Stop()
	// 一条流拿着副本的读锁，总时长也有上限：被攻破的源节点不能靠一直慢慢发数据占着锁，挡住提交和恢复。
	// 正常的源节点断开以后会换一条流接着传（见 resumeSync）。
	total := time.NewTimer(replicaPutMax)
	defer total.Stop()
	for {
		var r recv
		select {
		case r = <-ch:
		case <-stream.Context().Done():
			// 源节点断开、取消了：马上收尾（留下传了一半的文件、放开锁），下一次连接不用等 replicaIdle。
			return status.FromContextError(stream.Context().Err()).Err()
		case <-idle.C:
			return status.Error(codes.DeadlineExceeded, "同步副本时长时间没有收到数据")
		case <-total.C:
			return status.Error(codes.DeadlineExceeded, "同步副本的单次传输超过 6 小时，已断开")
		}
		idle.Reset(replicaIdle)
		c, err := r.c, r.err
		if err == io.EOF {
			if err := finish(); err != nil {
				return err
			}
			return stream.SendAndClose(&nagiv1.ReplicaPutResponse{Files: count})
		}
		if err != nil {
			return err
		}
		if files == "" {
			if files, unlock, err = authorize(c.GetProjectId(), c.GetContainer()); err != nil {
				return err
			}
			if root, err = openReplicaFiles(files); err != nil {
				return err
			}
			guard = &metrics.DiskGuard{Dir: files}
		}
		if c.GetPath() != path {
			if err := finish(); err != nil {
				return err
			}
			path = c.GetPath()
			if _, err := project.ReplicaPath(files, path); err != nil {
				return status.Error(codes.InvalidArgument, err.Error())
			}
			dest = filepath.FromSlash(path)
			if err := root.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				return err
			}
			tmp = dest + replicaTempMark + "-" + randomSuffix()
			if f, size, err = openReplicaTemp(root, dest, tmp, c.GetOffset(), c.GetPrefixSha256()); err != nil {
				return err
			}
		}
		if err := guard.Check(len(c.GetData())); err != nil {
			return status.Error(codes.ResourceExhausted, err.Error())
		}
		if _, err := f.Write(c.GetData()); err != nil {
			return err
		}
		size += int64(len(c.GetData()))
	}
}

// openReplicaTemp 打开接收 dest 用的临时文件 tmp（都是 root 里的路径）。offset 为 0 时新建；
// 否则接着上次传了一半的文件：先把它改名成 tmp（同时只有一条流能拿到），核对前 offset 字节的 SHA-256 等于 prefix，
// 多出来的截掉，返回的文件位置在 offset。传了一半的文件不在了、比 offset 短或者对不上时丢掉它，返回 DataLoss，源节点下次从头传。
func openReplicaTemp(root *os.Root, dest, tmp string, offset int64, prefix []byte) (*os.File, int64, error) {
	if offset < 0 {
		return nil, 0, status.Error(codes.InvalidArgument, "接着传的位置无效")
	}
	if offset == 0 {
		f, err := root.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		return f, 0, err
	}
	if len(prefix) != sha256.Size {
		return nil, 0, status.Error(codes.InvalidArgument, "接着传的文件缺少校验值")
	}
	if err := root.Rename(dest+replicaPartSuffix, tmp); err != nil {
		return nil, 0, status.Errorf(codes.DataLoss, "%s 传了一半的部分已经不在了，下次从头传", dest)
	}
	f, err := root.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		root.Remove(tmp)
		return nil, 0, err
	}
	fail := func(err error) (*os.File, int64, error) {
		f.Close()
		root.Remove(tmp)
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() || info.Size() < offset {
		return fail(status.Errorf(codes.DataLoss, "%s 传了一半的部分比源节点记的短，下次从头传", dest))
	}
	sum, err := prefixHash(f, offset)
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(sum, prefix) {
		return fail(status.Errorf(codes.DataLoss, "%s 传了一半的部分和源节点的对不上，下次从头传", dest))
	}
	if err := f.Truncate(offset); err != nil {
		return fail(err)
	}
	return f, offset, nil
}

// openReplicaFiles 打开副本的 files 目录作为 os.Root，之后的写入都在它里面解析路径：里面的符号链接指向外面时拒绝。
// 副本目录和 files 本身是符号链接时也拒绝（存放位置被容器碰到时，可能被换成链接）。
func openReplicaFiles(files string) (*os.Root, error) {
	if err := checkPlainDirs(filepath.Dir(files), files); err != nil {
		return nil, err
	}
	return os.OpenRoot(files)
}

// checkPlainDirs 确认这些路径都是目录，不是符号链接。
func checkPlainDirs(paths ...string) error {
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return status.Errorf(codes.FailedPrecondition, "副本目录 %s 不是普通目录", p)
		}
	}
	return nil
}

func (s *meshService) ReplicaCommit(ctx context.Context, req *nagiv1.ReplicaCommitRequest) (*nagiv1.ReplicaCommitResponse, error) {
	if _, err := replicaID(req.GetProjectId(), req.GetContainer()); err != nil {
		return nil, err
	}
	defer s.a.replicas.lock(req.GetProjectId(), req.GetContainer())()
	files, err := s.authorizeReplica(ctx, req.GetProjectId(), req.GetContainer(), true)
	if err != nil {
		return nil, err
	}
	m, _, _, err := s.a.replicas.load(req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, p := range req.GetPaths() {
		keep[p] = true
	}
	// 源节点上已经删掉的文件（例如快照仓库维护回收的数据包），这里也删掉。经过 os.Root：
	// 副本里被换成指向外面的符号链接时，只删链接本身，不会删到别处。
	root, err := openReplicaFiles(files)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	existing, remove := replicaExtras(root, keep)
	// 要删掉大部分文件（例如提交一个空列表）时只接受登记的源节点：顶替的另一个参与者平时不运行容器，
	// 被攻破以后不能借此清空异地备份（见 docs/snapshot.md#备份节点的实现）。
	if caller, ok := mesh.Caller(ctx, s.M); ok && !slices.Equal(m.SourceKey, caller[:]) && massDelete(existing, len(remove)) {
		return nil, status.Errorf(codes.FailedPrecondition, "这次同步要删掉副本里 %d 个文件中的 %d 个，只接受容器所在节点的这种同步", existing, len(remove))
	}
	for _, rel := range remove {
		root.Remove(rel)
	}
	if caller, ok := mesh.Caller(ctx, s.M); ok {
		s.a.replicas.release(req.GetProjectId(), req.GetContainer(), caller[:])
	}
	m.LastSync = time.Now().Unix()
	if err := s.a.replicas.save(m, nil); err != nil {
		return nil, err
	}
	// 热备：提前拉好镜像，顶替时只需要启动容器。
	if s.a.managed(req.GetProjectId(), req.GetContainer()) {
		go s.a.prePull(context.WithoutCancel(ctx), req.GetProjectId(), req.GetContainer())
		s.a.changed()
	}
	return &nagiv1.ReplicaCommitResponse{}, nil
}

// replicaExtras 返回副本 files 目录（root）里文件的个数和不在 keep 里的文件（root 里的路径）。
// 不进入符号链接：指向外面的链接本身算一个文件，删除时只删链接。
func replicaExtras(root *os.Root, keep map[string]bool) (existing int, remove []string) {
	_ = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		existing++
		if !keep[rel] {
			remove = append(remove, filepath.FromSlash(rel))
		}
		return nil
	})
	return existing, remove
}

// ---- 源节点：同步 ----

// errAlreadySyncing：这个容器到这个备份节点已经有一次同步在进行（同一个容器到同一个备份节点同时只有一次同步）。
var errAlreadySyncing = errors.New("已经在同步")

// startBackupSync 开始把容器同步到它的备份节点（targetID 不为空时只同步到这一个），不等同步结束。
// 同步在节点自己的上下文（parent）里依次进行，结果记在状态里；已经在同步的备份节点不再开一次，让它结束后再同步一次。
// 没有设置备份节点时什么都不做。
func (a *agent) startBackupSync(parent context.Context, projectID, container, targetID string) error {
	c, err := a.projects.Get(parent, projectID, container)
	if err != nil {
		return err
	}
	targets := backupTargets(c.Spec)
	id := projectID + "-" + container
	b := a.backups
	b.mu.Lock()
	b.prune(id, targets)
	b.mu.Unlock()
	var list []*nagiv1.BackupTarget
	var runs []*syncRun
	for _, t := range targets {
		if targetID != "" && t.GetNodeId() != targetID {
			continue
		}
		if run := b.claim(parent, id, t.GetNodeId()); run != nil {
			list, runs = append(list, t), append(runs, run)
		}
	}
	if len(runs) == 0 {
		return nil
	}
	a.changed()
	go a.runBackupSyncs(projectID, container, targets, list, runs)
	return nil
}

// runBackupSyncs 依次同步到 list 里的备份节点（runs 是各自已经登记好的同步），一个失败了接着同步下一个，上行带宽不叠加。
// 都结束后，有失败的就告警（一个容器一条，写明哪些节点），都成功了恢复。targets 是容器所有的备份节点。
func (a *agent) runBackupSyncs(projectID, container string, targets, list []*nagiv1.BackupTarget, runs []*syncRun) {
	for i, t := range list {
		a.syncBackupTo(projectID, container, t, runs[i])
	}
	if a.ctx != nil && a.ctx.Err() != nil {
		return // 节点退出：结果留到下次启动接着同步
	}
	id := projectID + "-" + container
	b := a.backups
	b.mu.Lock()
	failed := b.failures(id, targets)
	b.mu.Unlock()
	ctx := context.Background()
	key := "backup:" + id
	if len(failed) > 0 {
		if a.alerts.ContainerEnabled(projectID, container, alert.RuleBackup) {
			a.alerts.Fire(ctx, key, "容器 "+container+" 同步到备份节点失败", strings.Join(failed, "\n"), projectID)
		}
	} else {
		a.alerts.Resolve(ctx, key)
	}
}

// syncBackupTo 用登记好的 run 同步到一个备份节点并记录结果。同步期间又有人请求（run.again）就再同步一次。
func (a *agent) syncBackupTo(projectID, container string, t *nagiv1.BackupTarget, run *syncRun) {
	id := projectID + "-" + container
	b := a.backups
	defer func() {
		run.cancel(nil)
		close(run.done)
	}()
	for {
		b.mu.Lock()
		e := b.entry(id, t.GetNodeId())
		// 上次是用户停止的：这次开始了就不再显示「已停止」，失败的原因留到这次有结果再换。
		if e.Stopped {
			e.Stopped, e.LastError = false, ""
		}
		e.Interrupted = true
		b.persist()
		b.mu.Unlock()
		a.changed()

		// 同一个容器同时只同步到一个备份节点（几次请求各自开始的也一样），上行带宽不叠加；排队时界面显示「排队中」。
		unlock, err := lockSync(run.ctx, "backup/"+id)
		if err == nil {
			run.progress.begin(time.Now())
			a.changed()
			_, err = a.syncTo(run.ctx, projectID, container, t, run.progress)
			unlock()
		}
		if status.Code(err) == codes.NotFound {
			err = fmt.Errorf("%s 上已经没有这个容器的副本登记（可能已经在那里恢复成了容器）。要继续备份，请重新添加这个备份节点（%w）", t.GetName(), err)
		}
		cause := context.Cause(run.ctx)
		shutdown := err != nil && run.ctx.Err() != nil && !errors.Is(cause, errSyncStopped)
		b.mu.Lock()
		again := run.again && err == nil
		run.again = false
		switch {
		case shutdown:
			// 节点退出（或者备份节点被去掉了）：不改上次的结果，Interrupted 留着，启动后接着同步。
		case errors.Is(cause, errSyncStopped):
			e.LastError, e.Stopped, e.Interrupted = errSyncStopped.Error(), true, false
		case err != nil:
			e.LastError, e.Stopped, e.Interrupted = err.Error(), false, false
		default:
			e.LastError, e.Stopped, e.Interrupted, e.LastSync = "", false, false, time.Now().Unix()
		}
		if !again {
			e.run = nil
		} else {
			// 再同步一次：进度从头算。
			run.progress = &syncProgress{}
		}
		b.persist()
		b.mu.Unlock()
		a.changed()
		if err != nil && !shutdown && !errors.Is(cause, errSyncStopped) {
			a.log.Warn("同步到备份节点失败", "project", projectID, "container", container, "target", t.GetName(), "err", err)
		}
		if !again {
			return
		}
	}
}

// syncTo 把容器的快照仓库增量同步到目标节点，返回传输的字节数。备份和迁移共用；p 不为空时记下进度。
// 连接中途断了（大文件走慢线路时连接超时，或者对方节点刚重启，旧的 QUIC 连接收到无状态重置）时换一条新连接接着传：
// 对方每收完一个文件就落盘，传了一半的文件也留着（见 ReplicaPut），下次核对清单时跳过已有的、从一半处接着传，
// 所以每次重试都从断开的地方往后。规则见 resumeSync。
func (a *agent) syncTo(ctx context.Context, projectID, container string, target *nagiv1.BackupTarget, p *syncProgress) (int64, error) {
	// 同一个容器到同一个目标同时只有一次同步：定时快照后的同步、每 15 分钟一次的热备同步、迁移、切回可能重叠，
	// 两次同步交错时，一次提交会删掉另一次刚传的文件。后来的等前一次结束。
	unlock, err := lockSync(ctx, projectID+"/"+container+"/"+string(target.GetKey()))
	if err != nil {
		return 0, err
	}
	defer unlock()
	if p == nil {
		p = &syncProgress{}
	}
	return resumeSync(ctx, func() (int64, int64, error) { return a.syncOnce(ctx, projectID, container, target, p) }, func(attempt int, n int64, err error) {
		p.retried(err)
		a.changed()
		a.log.Info("同步时连接断开，接着传", "project", projectID, "container", container, "target", target.GetName(),
			"attempt", attempt, "bytes", n, "err", err)
	})
}

// resumeSync 反复调用 once（返回这次传了多少字节、开始时还要传多少字节，核对清单之前就失败时是 -1），
// 返回一共传了多少字节和最后一次的结果。成功、对方拒绝（不是连接问题）、ctx 取消时停下。
// 连接断了就等一会儿再连：还要传的比以前少了（上一次有进展）就接着来，等待从 syncRetryDelay 重新算；
// 连续 syncStallAttempts 次没有进展才放弃，等待每次翻倍、最多 syncRetryMax（一共约 3 分钟），家用宽带重新拨号、换地址也等得过去。
func resumeSync(ctx context.Context, once func() (int64, int64, error), onRetry func(attempt int, n int64, err error)) (int64, error) {
	var total int64
	best := int64(-1)
	stalled := 0
	delay := syncRetryDelay
	for attempt := 1; ; attempt++ {
		n, need, err := once()
		total += n
		if err == nil || ctx.Err() != nil || !connectionLost(err) {
			return total, err
		}
		if need >= 0 && (best < 0 || need < best) {
			best, stalled, delay = need, 0, syncRetryDelay
		} else {
			stalled++
		}
		if stalled >= syncStallAttempts {
			return total, err
		}
		onRetry(attempt, n, err)
		select {
		case <-ctx.Done():
			return total, err
		case <-time.After(delay):
		}
		if stalled > 0 {
			delay = min(2*delay, syncRetryMax)
		}
	}
}

// syncStallAttempts 是连续多少次重试没有进展就放弃；syncRetryDelay 是断开以后等多久再连，没有进展时翻倍，最多 syncRetryMax。
const syncStallAttempts = 8

var (
	syncRetryDelay = 2 * time.Second
	syncRetryMax   = time.Minute
)

// connectionLost 判断同步失败是不是因为传输断了，而不是对方拒绝：连接断开（Unavailable），
// 对方太久没收到数据（DeadlineExceeded，例如旧版本的对方一次只收一条流，其余的流等它时超时），
// 或者对方传了一半的文件对不上、已经丢掉（DataLoss，下次从头传）。
func connectionLost(err error) bool {
	var se interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &se) {
		return false
	}
	c := se.GRPCStatus().Code()
	return c == codes.Unavailable || c == codes.DeadlineExceeded || c == codes.DataLoss
}

// syncLocks 是正在进行的同步，键是容器和目标，值是只能放一个的通道（能被 ctx 取消的锁）。
var syncLocks sync.Map

// lockSync 等到 key 没有别的同步在进行，返回释放的函数。
func lockSync(ctx context.Context, key string) (func(), error) {
	v, _ := syncLocks.LoadOrStore(key, make(chan struct{}, 1))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// randomSuffix 是临时文件名的随机后缀。
func randomSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sendFile 是要传的一个文件；offset 大于 0 时对方已经有前 offset 字节（上次传了一半），从那里接着传。
type sendFile struct {
	project.ReplicaFile
	offset int64
}

// plan 按对方已有的文件（remote：路径 → 大小）和传了一半的文件（partial）分出这次要传的文件。
// 对方已经有、而且不会再变的文件不用传，直接算进提交的清单（keep）。数据包先传，都到了再传引用它们的索引等文件：
// 中途断开时，备份节点上不会出现指向还没收到的数据包的索引。need 是要传的字节数。
func planSync(files []project.ReplicaFile, remote, partial map[string]int64) (keep []string, packs, others []sendFile, need int64) {
	for _, f := range files {
		if size, ok := remote[f.Path]; ok && f.Immutable && size == f.Size {
			keep = append(keep, f.Path)
			continue
		}
		s := sendFile{ReplicaFile: f}
		// 只接着传不会再变的文件：kopia.* 会被改写，前一半可能已经不一样了。
		if n := partial[f.Path]; f.Immutable && n > 0 && n <= f.Size {
			s.offset = n
		}
		need += f.Size - s.offset
		if f.Pack {
			packs = append(packs, s)
		} else {
			others = append(others, s)
		}
	}
	return keep, packs, others, need
}

// syncOnce 同步一次：核对清单、传缺少的文件、提交。返回传了多少字节，和核对清单后要传多少字节（核对清单之前就失败时是 -1）。
func (a *agent) syncOnce(ctx context.Context, projectID, container string, target *nagiv1.BackupTarget, p *syncProgress) (int64, int64, error) {
	// 同步期间仓库的文件不能被立即清理删掉，否则备份节点会收到不完整的一套文件。
	defer a.projects.ReadRepo(projectID, container)()
	if a.mesh == nil {
		return 0, -1, errors.New("节点网络没有启动")
	}
	key, err := mesh.ParseKey(target.GetKey())
	if err != nil {
		return 0, -1, err
	}
	dial := func() (*grpc.ClientConn, error) { return mesh.Client(a.mesh.m, key) }
	n, need, err := transferReplica(ctx, dial, projectID, container, target.GetName(), func() ([]project.ReplicaFile, error) {
		return a.projects.ReplicaFiles(ctx, projectID, container)
	}, p)
	if err == nil {
		a.log.Info("已同步副本", "project", projectID, "container", container, "target", target.GetName(), "bytes", n)
	}
	return n, need, err
}

// transferReplica 是 syncOnce 的传输部分：经 dial 连上对方，核对清单、传缺少的文件、提交。
// files 在拿到对方的清单以后调用，返回源节点上现在的文件。
func transferReplica(ctx context.Context, dial func() (*grpc.ClientConn, error), projectID, container, targetName string,
	files func() ([]project.ReplicaFile, error), p *syncProgress) (int64, int64, error) {
	cc, err := dial()
	if err != nil {
		return 0, -1, err
	}
	defer cc.Close()
	c := nagiv1.NewMeshServiceClient(cc)
	man, err := c.ReplicaManifest(ctx, &nagiv1.ReplicaManifestRequest{ProjectId: projectID, Container: container})
	if err != nil {
		return 0, -1, fmt.Errorf("连接节点 %s: %w", targetName, err)
	}
	remote, partial := map[string]int64{}, map[string]int64{}
	for _, f := range man.GetFiles() {
		remote[f.GetPath()] = f.GetSize()
	}
	for _, f := range man.GetPartials() {
		partial[f.GetPath()] = f.GetSize()
	}
	list, err := files()
	if err != nil {
		return 0, -1, err
	}
	paths, packs, others, need := planSync(list, remote, partial)
	p.plan(need)
	var sent atomic.Int64
	count := func(n int64) {
		sent.Add(n)
		p.add(n)
	}
	for _, batch := range [][]sendFile{packs, others} {
		done, err := sendReplicaFiles(ctx, dial, cc, projectID, container, batch, count)
		paths = append(paths, done...)
		if err != nil {
			return sent.Load(), need, err
		}
	}
	if _, err := c.ReplicaCommit(ctx, &nagiv1.ReplicaCommitRequest{ProjectId: projectID, Container: container, Paths: paths}); err != nil {
		return sent.Load(), need, fmt.Errorf("提交: %w", err)
	}
	return sent.Load(), need, nil
}

// syncStreams 是同步副本时最多同时开几条流（每条是单独的一个 mesh 连接，走 QUIC 时是同一条连接上的不同流）。
// 文件多、线路延迟高时，几个文件一起传比一个接一个快；对方写盘也能并行。测试里替换。
var syncStreams = 4

// sendReplicaFiles 用最多 syncStreams 条 ReplicaPut 流把 files 传给对方，各条流从同一个队列里取文件。
// 返回传完了的文件（对方收到并确认了）。first 是已经建好的连接，第一条流用它，其余的各用 dial 建一个。
// count 记下发出的字节数。
func sendReplicaFiles(ctx context.Context, dial func() (*grpc.ClientConn, error), first *grpc.ClientConn, projectID, container string,
	files []sendFile, count func(int64)) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	queue := make(chan sendFile, len(files))
	for _, f := range files {
		queue <- f
	}
	close(queue)
	var mu sync.Mutex
	var done []string
	g, gctx := errgroup.WithContext(ctx)
	for i := range min(syncStreams, len(files)) {
		g.Go(func() error {
			cc := first
			if i > 0 {
				var err error
				if cc, err = dial(); err != nil {
					return err
				}
				defer cc.Close()
			}
			ok, err := sendReplicaStream(gctx, nagiv1.NewMeshServiceClient(cc), projectID, container, queue, count)
			if err != nil {
				return err
			}
			mu.Lock()
			done = append(done, ok...)
			mu.Unlock()
			return nil
		})
	}
	err := g.Wait()
	return done, err
}

// prefixHash 返回文件前 n 字节的 SHA-256，读完后文件的位置正好在 n。
func prefixHash(f *os.File, n int64) ([]byte, error) {
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// sendReplicaStream 在一条 ReplicaPut 流上传队列里的文件，直到队列空了，返回这条流上传完的文件。
func sendReplicaStream(ctx context.Context, c nagiv1.MeshServiceClient, projectID, container string,
	queue <-chan sendFile, count func(int64)) ([]string, error) {
	var put nagiv1.MeshService_ReplicaPutClient
	var paths []string
	buf := make([]byte, 256<<10)
	for f := range queue {
		in, err := os.Open(f.Abs)
		if err != nil {
			continue // 维护时被删掉了，不再同步
		}
		var prefix []byte
		if f.offset > 0 {
			if prefix, err = prefixHash(in, f.offset); err != nil {
				in.Close()
				continue // 文件变短了：维护时被删掉、又不是原来的文件了，不再同步
			}
		}
		if put == nil {
			if put, err = c.ReplicaPut(ctx); err != nil {
				in.Close()
				return nil, err
			}
		}
		start := true
		var sendErr error
		for {
			n, rerr := in.Read(buf)
			if n > 0 || start {
				chunk := &nagiv1.ReplicaChunk{Path: f.Path, Data: buf[:n]}
				// 每条流的第一块带上容器，对方据此核对权限、加锁。
				if start && len(paths) == 0 {
					chunk.ProjectId, chunk.Container = projectID, container
				}
				if start && f.offset > 0 {
					chunk.Offset, chunk.PrefixSha256 = f.offset, prefix
				}
				if sendErr = put.Send(chunk); sendErr != nil {
					break
				}
				count(int64(n))
				start = false
			}
			if rerr != nil {
				break
			}
		}
		in.Close()
		if sendErr != nil {
			break // 真正的原因在 CloseAndRecv 里
		}
		paths = append(paths, f.Path)
	}
	if put == nil {
		return nil, nil
	}
	if _, err := put.CloseAndRecv(); err != nil {
		return nil, fmt.Errorf("传输文件: %w", err)
	}
	return paths, nil
}

// ---- AgentService ----

// SyncBackup 立即同步到一个备份节点（为空时第一个），返回这个备份节点的状态。已经有一次同步在进行时
// （例如快照之后自动开始的），让它结束后再做一次：那一次可能是在最新的快照之前开始的。
// 同步在节点自己的上下文里进行，面板断开、等不及也不会中断：async 时开始了就返回（进度随容器状态上报），
// 否则（旧版本的面板）等同步结束再返回。cancel 时停止正在进行的同步。
func (s *agentService) SyncBackup(ctx context.Context, req *nagiv1.SyncBackupRequest) (*nagiv1.BackupStatus, error) {
	c, err := s.projects.Get(ctx, req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, toStatus(err)
	}
	targets := backupTargets(c.Spec)
	if len(targets) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "该容器没有设置备份节点")
	}
	target := req.GetTargetNodeId()
	if target == "" {
		target = targets[0].GetNodeId()
	}
	i := slices.IndexFunc(targets, func(t *nagiv1.BackupTarget) bool { return t.GetNodeId() == target })
	if i < 0 {
		return nil, status.Error(codes.FailedPrecondition, "该节点不是这个容器的备份节点")
	}
	b := s.node.backups
	id := req.GetProjectId() + "-" + req.GetContainer()
	if req.GetCancel() {
		if b.stop(id, target) {
			s.node.log.Info("已停止同步到备份节点", "project", req.GetProjectId(), "container", req.GetContainer(), "target", targets[i].GetName())
		}
		return b.statuses(c.Spec)[i], nil
	}
	parent := s.node.ctx
	if parent == nil {
		parent = context.Background()
	}
	if err := s.node.startBackupSync(parent, req.GetProjectId(), req.GetContainer(), target); err != nil {
		return nil, toStatus(err)
	}
	if !req.GetAsync() {
		// 等这个备份节点的同步（包括排着的下一次）结束。
		for run := b.running(id, target); run != nil; run = b.running(id, target) {
			select {
			case <-ctx.Done():
				return nil, toStatus(ctx.Err())
			case <-run.done:
			}
		}
	}
	// 失败的原因记在状态里（last_error）。
	return b.statuses(c.Spec)[i], nil
}

// AcceptReplica 登记（或更新）副本。replica_dir 为空时：新登记的放在默认目录，已有的留在原处。
// 指定了 replica_dir 而本节点已有这个容器的副本（例如之前作为备份节点留下的）、又不在那里时，先把它移过去
// （和 MoveReplica 同样的移动：拿着副本的锁，移完核对无误才切换），再更新登记；移动失败时不改登记。
func (s *agentService) AcceptReplica(ctx context.Context, req *nagiv1.AcceptReplicaRequest) (*nagiv1.AcceptReplicaResponse, error) {
	if _, err := mesh.ParseKey(req.GetSourceKey()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	for _, k := range req.GetExtraSourceKeys() {
		if _, err := mesh.ParseKey(k); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	sp := req.GetSpec()
	r := s.node.replicas
	id, err := replicaID(sp.GetProjectId(), sp.GetName())
	if err != nil {
		return nil, err
	}
	var base string
	if strings.TrimSpace(req.GetReplicaDir()) != "" {
		// 校验和移动互斥（checkBase 会建目录），拿副本的锁之前做，顺序和 moveTo 一致。
		r.moving.Lock()
		defer r.moving.Unlock()
		if err := r.usable(); err != nil {
			return nil, err
		}
		if base, err = r.checkBase(req.GetReplicaDir()); err != nil {
			return nil, err
		}
		if err := s.node.moveExisting(ctx, sp.GetProjectId(), sp.GetName(), base); err != nil {
			return nil, err
		}
	}
	// 拿着 moving 时不能一直等副本的锁（见 replicaLockWait）。
	unlock := func() {}
	if base != "" {
		if unlock, err = r.lockWithin(ctx, sp.GetProjectId(), sp.GetName(), replicaLockWait); err != nil {
			return nil, err
		}
	} else {
		unlock = r.lock(sp.GetProjectId(), sp.GetName())
	}
	defer unlock()
	m, _, root, err := r.load(sp.GetProjectId(), sp.GetName())
	created := false
	switch {
	case status.Code(err) == codes.NotFound:
		// 位置记录损坏时，这个副本可能就在某个单独指定的位置，新建一份会从头同步、原来的成了孤儿。
		if err := r.usable(); err != nil {
			return nil, err
		}
		m = &replicaMeta{ProjectID: sp.GetProjectId(), Container: sp.GetName()}
		if base != "" {
			def, _ := r.defaultDir()
			if err := r.setLocation(id, base, base != def); err != nil {
				return nil, err
			}
			created = true
		}
	case err != nil:
		return nil, err
	case base != "" && filepath.Dir(root) != base:
		// 上面刚移过去，不应该出现（另一次移动不会插进来：拿着 moving）。
		return nil, status.Errorf(codes.FailedPrecondition, "此节点上已有该容器的副本（%s），未能移到指定的位置", root)
	}
	m.SourceKey, m.SourceNodeID, m.SourceName = req.GetSourceKey(), req.GetSourceNodeId(), req.GetSourceName()
	m.ExtraSourceKeys = req.GetExtraSourceKeys()
	if err := r.save(m, sp); err != nil {
		if created {
			r.forget(id)
		}
		return nil, err
	}
	return &nagiv1.AcceptReplicaResponse{}, nil
}

func (s *agentService) ListReplicas(context.Context, *nagiv1.ListReplicasRequest) (*nagiv1.ListReplicasResponse, error) {
	r := s.node.replicas
	out := &nagiv1.ListReplicasResponse{}
	for _, ref := range r.list() {
		m, sp, root, err := r.load(ref.projectID, ref.container)
		if err != nil {
			continue
		}
		rp := &nagiv1.Replica{ProjectId: m.ProjectID, Container: m.Container, ProjectName: sp.GetProjectName(), SourceNodeId: m.SourceNodeID, SourceName: m.SourceName,
			Dir: root, CustomDir: ref.custom}
		if m.LastSync > 0 {
			rp.LastSync = timestamppb.New(time.Unix(m.LastSync, 0))
		}
		_ = filepath.WalkDir(filepath.Join(root, "files"), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, err := d.Info(); err == nil {
					rp.Bytes += info.Size()
				}
			}
			return nil
		})
		out.Replicas = append(out.Replicas, rp)
	}
	return out, nil
}

func (s *agentService) ListReplicaSnapshots(ctx context.Context, req *nagiv1.ContainerRef) (*nagiv1.ListSnapshotsResponse, error) {
	if _, err := replicaID(req.GetProjectId(), req.GetContainer()); err != nil {
		return nil, err
	}
	defer s.node.replicas.lock(req.GetProjectId(), req.GetContainer())()
	_, sp, root, err := s.node.replicas.load(req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, err
	}
	list, err := project.ReplicaSnapshots(ctx, sp, filepath.Join(root, "files", "repo"), s.node.replicaStateDir(req.GetProjectId(), req.GetContainer()))
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.ListSnapshotsResponse{Snapshots: list}, nil
}

func (s *agentService) CheckContainer(ctx context.Context, req *nagiv1.PutContainerRequest) (*nagiv1.CheckContainerResponse, error) {
	if err := s.projects.CheckPlacement(ctx, req.GetSpec()); err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.CheckContainerResponse{}, nil
}

func (s *agentService) RestoreReplica(req *nagiv1.RestoreReplicaRequest, stream nagiv1.AgentService_RestoreReplicaServer) error {
	sp := req.GetSpec()
	if _, err := replicaID(sp.GetProjectId(), sp.GetName()); err != nil {
		return err
	}
	unlock := s.node.replicas.lock(sp.GetProjectId(), sp.GetName())
	_, _, root, err := s.node.replicas.load(sp.GetProjectId(), sp.GetName())
	if err != nil {
		unlock()
		return err
	}
	defer s.changed()
	ctx := stream.Context()
	err = s.projects.RestoreReplica(ctx, sp, filepath.Join(root, "files", "repo"), req.GetSnapshotId(), func(line string) {
		_ = stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: line}})
	})
	_, gerr := s.projects.Get(ctx, sp.GetProjectId(), sp.GetName())
	placed := gerr == nil
	if placed {
		// 容器已经用自己的快照仓库了，副本没用了。
		_ = s.node.replicas.remove(sp.GetProjectId(), sp.GetName())
	}
	unlock()
	if errors.Is(err, project.ErrNotFound) || errors.Is(err, project.ErrInvalidArgument) {
		return toStatus(err)
	}
	result := &nagiv1.DeployResult{Success: err == nil, Version: sp.GetVersion(), Placed: placed}
	if err != nil {
		result.Error = explain(err.Error())
	}
	return stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: result}})
}

func (s *agentService) DeleteReplica(_ context.Context, req *nagiv1.ContainerRef) (*nagiv1.DeleteReplicaResponse, error) {
	if _, err := replicaID(req.GetProjectId(), req.GetContainer()); err != nil {
		return nil, err
	}
	defer s.node.replicas.lock(req.GetProjectId(), req.GetContainer())()
	if err := s.node.replicas.remove(req.GetProjectId(), req.GetContainer()); err != nil {
		return nil, err
	}
	return &nagiv1.DeleteReplicaResponse{}, nil
}

// MigrateOut 是迁移的前半段：容器运行时先同步一次，停止并移除容器、保存最后一次快照，再增量同步一次。
func (s *agentService) MigrateOut(req *nagiv1.MigrateOutRequest, stream nagiv1.AgentService_MigrateOutServer) error {
	defer s.changed()
	t := req.GetTarget()
	if _, err := mesh.ParseKey(t.GetKey()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	ctx := stream.Context()
	pid, name := req.GetProjectId(), req.GetContainer()
	return runStream(stream.Send, func(onLine func(string)) error {
		onLine("第一次同步到 " + t.GetName() + "（容器照常运行）")
		n, err := s.node.syncTo(ctx, pid, name, t, nil)
		if err != nil {
			return err
		}
		onLine("已同步 " + humanBytes(n))
		if err := s.projects.MigrateStop(ctx, pid, name, onLine); err != nil {
			return err
		}
		onLine("增量同步最后的数据")
		if n, err = s.node.syncTo(ctx, pid, name, t, nil); err != nil {
			return err
		}
		onLine("已同步 " + humanBytes(n))
		return nil
	})
}

func (s *agentService) MigrateResume(req *nagiv1.ContainerRef, stream nagiv1.AgentService_MigrateResumeServer) error {
	defer s.changed()
	return runStream(stream.Send, func(onLine func(string)) error {
		return s.projects.MigrateResume(stream.Context(), req.GetProjectId(), req.GetContainer(), onLine)
	})
}

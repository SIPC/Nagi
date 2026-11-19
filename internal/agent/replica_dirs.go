package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 副本的存放位置（见 docs/snapshot.md#副本的存放位置）。
//   - 节点的默认目录：作为备份节点、热备收到的副本默认放在这里，没有设置时是 <节点目录>/replicas；
//   - 单个副本的存放位置：设置备份节点时可以给这个容器的副本单独指定，不随默认目录移动。
//
// 副本总是 <存放位置>/<项目 ID>-<容器名>，不同容器不会撞在一起。不在默认目录里的副本记在
// <节点目录>/replica-dirs.json 里；路径由节点自己校验（面板可能被攻破或者有错），修改时把副本移过去，
// 移动完、核对无误才切换，失败时留在原处。

// replicaIndex 是 replica-dirs.json 的内容。
type replicaIndex struct {
	// Dir 是节点的默认目录，为空表示 <节点目录>/replicas。
	Dir string `json:"dir,omitempty"`
	// Locations 是不在默认目录里的副本：键是"项目 ID-容器名"。
	Locations map[string]replicaLocation `json:"locations,omitempty"`
	// Moving 是正在往里移动副本的存放位置：节点中途退出时，启动后到这里清掉复制了一半的临时目录
	// （新指定的位置还不在 Locations 里）。
	Moving []string `json:"moving,omitempty"`
	// Moves 是正在进行的移动：改名或复制之前记下，节点中途退出时启动后据此收尾（见 recoverMoves）。v0.31.0 起。
	Moves []replicaMove `json:"moves,omitempty"`
}

// replicaMove 是一次移动：副本 ID 从 From 移到 To（都是存放位置），Custom 是移过去以后是不是单独指定的位置。
type replicaMove struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Custom bool   `json:"custom,omitempty"`
}

type replicaLocation struct {
	Base string `json:"base"`
	// Custom：单独指定的存放位置。为 false 的是换了默认目录以后还没移过去的（移动失败），下次修改默认目录时再移。
	Custom bool `json:"custom,omitempty"`
}

// replicaStagingMark 是跨文件系统移动时目标位置的临时目录名里的标记：.<项目 ID>-<容器名>.nagi-moving-<随机>。
// 复制、核对完才改名成正式的名字；节点启动时清掉中途退出留下的。
const replicaStagingMark = ".nagi-moving-"

// replicaForbidden 是不能放副本的系统目录（包括下面的子目录）。
var replicaForbidden = []string{
	"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/boot", "/proc", "/sys", "/dev", "/run",
	"/var/run", "/var/lock", "/tmp", "/var/tmp", "/var/log", "/var/cache", "/var/spool", "/var/mail",
	"/var/lib/docker", "/var/lib/containerd", "/snap",
	// 和 /tmp 一样会被系统定时清理、或者谁都能写的粘滞位目录。
	"/var/crash", "/var/lib/php", "/var/lib/systemd/coredump", "/var/metrics",
}

// replicaShallow 是不能直接用作存放位置的上级目录（下面的子目录可以），免得副本和其他目录混在一起。
var replicaShallow = []string{"/home", "/var", "/var/lib", "/opt", "/srv", "/root"}

func newReplicas(dataDir string) *replicas {
	r := &replicas{
		dir:       filepath.Join(dataDir, "replicas"),
		file:      filepath.Join(dataDir, "replica-dirs.json"),
		forbidden: replicaForbidden,
	}
	raw, err := os.ReadFile(r.file)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &r.idx); err != nil {
			r.idx = replicaIndex{}
			r.markCorrupt(raw, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		r.markUnreadable(err)
	}
	if r.broken == nil {
		r.recoverMoves()
		r.cleanStaging()
	}
	return r
}

// recoverMoves 在启动时收尾中途退出的移动（idx.Moves），调用时还没有别的 goroutine：
//   - 原位置没有副本、目标位置有完整的副本（改名或复制已经完成，记录没写进去）：采用目标位置，不用从头同步；
//   - 记录已经指向目标位置：移动完成了，只是原位置还没删（复制的情况），删掉原位置；
//   - 原位置的副本还在、记录指向原位置：移动没有完成，删掉目标位置复制好的副本（移动开始前确认过那里没有东西）。
//
// 副本完整以副本目录是普通目录（不是符号链接）、里面有 meta.json 为准：改名是原子的，复制是核对完才改名成正式的名字。
// 原位置和目标位置都要是已知的存放位置（见 knownBasesLocked；markMove 把两者都记进了 Moving），对不上的记录不处理。
func (r *replicas) recoverMoves() {
	if len(r.idx.Moves) == 0 {
		return
	}
	has := func(root string) bool {
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() {
			return false
		}
		info, err = os.Lstat(filepath.Join(root, "meta.json"))
		return err == nil && info.Mode().IsRegular()
	}
	known := r.knownBasesLocked()
	for _, m := range r.idx.Moves {
		if _, _, ok := splitReplicaID(m.ID); !ok || m.From == m.To || !slices.Contains(known, m.From) || !slices.Contains(known, m.To) {
			continue
		}
		src, dst := filepath.Join(m.From, m.ID), filepath.Join(m.To, m.ID)
		cur, _ := r.baseLocked(m.ID)
		switch {
		case cur == m.To && has(dst):
			if has(src) {
				os.RemoveAll(src)
			}
		case cur == m.From && !has(src) && has(dst):
			r.placeLocked(m.ID, m.To, m.Custom)
		case cur == m.From && has(src) && has(dst):
			os.RemoveAll(dst)
		}
	}
	r.idx.Moves = nil
	_ = r.persistLocked()
}

// knownBasesLocked 是记录里的所有存放位置：<节点目录>/replicas、默认目录、各个副本所在的位置、Moving。调用时持有 mu。
func (r *replicas) knownBasesLocked() []string {
	out := []string{r.dir}
	add := func(b string) {
		if b != "" && !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	add(r.idx.Dir)
	for _, l := range r.idx.Locations {
		add(l.Base)
	}
	for _, m := range r.idx.Moving {
		add(m)
	}
	return out
}

// placeLocked 在内存里记下副本 id 在 base 下面，不写盘。不是单独指定、又正好在默认目录里的不用记。调用时持有 mu。
func (r *replicas) placeLocked(id, base string, custom bool) {
	if !custom && base == r.defaultLocked() {
		delete(r.idx.Locations, id)
		return
	}
	if r.idx.Locations == nil {
		r.idx.Locations = map[string]replicaLocation{}
	}
	r.idx.Locations[id] = replicaLocation{Base: base, Custom: custom}
}

// markCorrupt 在 replica-dirs.json 解析不了（断电截断、磁盘故障）时调用。原文件留在原处，另存一份
// .corrupt-<时间>（内容相同的已经有了就不再存）；之后的写入都拒绝（见 persistLocked），所以每次重启都会再读到
// 这个坏文件、保持不可用，直到管理员修好它（审查 N1）。在此之前拒绝修改存放位置、登记新的副本：不知道副本原来在哪里，
// 当成空的会在默认目录从头同步一份，原来的副本成了孤儿（见 docs/snapshot.md#副本的存放位置）。
//
// 从坏文件里尽量找出还认得出的路径（见 salvagePaths），算进存放位置（bases），容器目录照样不能和它们重叠。
// 找不全也没关系：不可用期间新的容器目录只能放在项目根目录下面（见 project.Manager.ReplicaIndexBroken）。
func (r *replicas) markCorrupt(raw []byte, cause error) {
	r.salvaged = salvagePaths(raw)
	r.brokenCause = cause.Error()
	if kept := keepCorruptCopy(r.file, raw); kept != "" {
		r.broken = status.Errorf(codes.FailedPrecondition,
			"副本位置记录 replica-dirs.json 无法解析，已另存一份为 %s，原文件留在原处。修复原文件后重启节点，在此之前不能修改存放位置或登记新的副本", kept)
		return
	}
	r.broken = status.Error(codes.FailedPrecondition,
		"副本位置记录 replica-dirs.json 无法解析，原文件留在原处。修复原文件后重启节点，在此之前不能修改存放位置或登记新的副本")
}

// markUnreadable 在读 replica-dirs.json 出错（EIO、EACCES 等，不是不存在）时调用：不另存、不改名，也不当成空的，
// 这次运行期间和记录损坏一样拒绝修改。排除故障后重启节点就会重新读取。
func (r *replicas) markUnreadable(cause error) {
	r.brokenCause = cause.Error()
	r.broken = status.Error(codes.FailedPrecondition,
		"副本位置记录 replica-dirs.json 读取出错。排除故障后重启节点，在此之前不能修改存放位置或登记新的副本")
}

// keepCorruptCopy 把坏掉的记录另存为 <file>.corrupt-<时间>，返回另存的文件名；已经有内容相同的就返回它，
// 不在每次重启时多存一份。另存失败时返回空。
func keepCorruptCopy(file string, raw []byte) string {
	matches, _ := filepath.Glob(file + ".corrupt-*")
	sort.Strings(matches)
	for _, p := range matches {
		if b, err := os.ReadFile(p); err == nil && bytes.Equal(b, raw) {
			return p
		}
	}
	name := file + ".corrupt-" + time.Now().Format("20060102-150405")
	for i := 1; slices.Contains(matches, name); i++ {
		name = fmt.Sprintf("%s.corrupt-%s-%d", file, time.Now().Format("20060102-150405"), i)
	}
	if err := writeAtomic(name, raw); err != nil {
		return ""
	}
	return name
}

// jsonString 匹配 JSON 里的字符串（含引号）。
var jsonString = regexp.MustCompile(`"(?:[^"\\\x00-\x1f]|\\.)*"`)

// salvagePaths 从解析不了的记录里找出完整的、是绝对路径的字符串（存放位置都是绝对路径，键是副本 ID，不是路径）。
// 文件被截断时，断点以前的路径都还在。只用来多拒绝一些目录，认错了也不会放过什么。
func salvagePaths(raw []byte) []string {
	var out []string
	for _, q := range jsonString.FindAll(raw, -1) {
		var s string
		if json.Unmarshal(q, &s) != nil || len(s) > 4096 || strings.ContainsRune(s, 0) || !filepath.IsAbs(s) {
			continue
		}
		s = filepath.Clean(s)
		if filepath.Dir(s) != s && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// usable 返回副本位置记录不可用时的错误（见 markCorrupt、markUnreadable）。
func (r *replicas) usable() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.broken
}

// defaultLocked 是节点的默认目录。调用时持有 mu。
func (r *replicas) defaultLocked() string {
	if r.idx.Dir != "" {
		return r.idx.Dir
	}
	return r.dir
}

// defaultDir 是节点的默认目录；setting 是设置的值（没有设置时为空）。
func (r *replicas) defaultDir() (dir, setting string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.defaultLocked(), r.idx.Dir
}

// baseLocked 是副本 id 所在的存放位置。调用时持有 mu。
func (r *replicas) baseLocked(id string) (string, bool) {
	if l, ok := r.idx.Locations[id]; ok {
		return l.Base, l.Custom
	}
	return r.defaultLocked(), false
}

func (r *replicas) persistLocked() error {
	if r.broken != nil {
		return r.broken
	}
	raw, _ := json.MarshalIndent(r.idx, "", "  ")
	return writeAtomic(r.file, raw)
}

// setLocation 记下副本 id 现在在 base 下面并保存。不是单独指定、又正好在默认目录里的不用记。
func (r *replicas) setLocation(id, base string, custom bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, had := r.idx.Locations[id]
	r.placeLocked(id, base, custom)
	if err := r.persistLocked(); err != nil {
		if had {
			r.idx.Locations[id] = prev
		} else {
			delete(r.idx.Locations, id)
		}
		return err
	}
	return nil
}

// forget 删掉副本 id 的位置记录（副本已经删掉了）。
func (r *replicas) forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.idx.Locations[id]; ok {
		delete(r.idx.Locations, id)
		_ = r.persistLocked()
	}
}

// remove 删除副本和它的位置记录。调用时持有这个副本的锁。
func (r *replicas) remove(projectID, container string) error {
	root, err := r.root(projectID, container)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if r.stateDir != nil {
		os.RemoveAll(r.stateDir(projectID, container))
	}
	r.forget(projectID + "-" + container)
	return nil
}

type replicaRef struct {
	projectID, container string
	root                 string
	custom               bool
}

// splitReplicaID 把"项目 ID-容器名"拆开。
func splitReplicaID(id string) (string, string, bool) {
	if len(id) < 18 || id[16] != '-' {
		return "", "", false
	}
	if _, err := replicaID(id[:16], id[17:]); err != nil {
		return "", "", false
	}
	return id[:16], id[17:], true
}

// list 返回本节点上所有的副本（有登记信息的），按 ID 排序。
func (r *replicas) list() []replicaRef {
	r.mu.Lock()
	def := r.defaultLocked()
	locs := make(map[string]replicaLocation, len(r.idx.Locations))
	for id, l := range r.idx.Locations {
		locs[id] = l
	}
	r.mu.Unlock()
	var out []replicaRef
	add := func(id, base string, custom bool) {
		pid, name, ok := splitReplicaID(id)
		if !ok {
			return
		}
		root := filepath.Join(base, id)
		if _, err := os.Stat(filepath.Join(root, "meta.json")); err != nil {
			return
		}
		out = append(out, replicaRef{projectID: pid, container: name, root: root, custom: custom})
	}
	entries, _ := os.ReadDir(def)
	for _, e := range entries {
		if _, moved := locs[e.Name()]; e.IsDir() && !moved {
			add(e.Name(), def, false)
		}
	}
	for id, l := range locs {
		add(id, l.Base, l.Custom)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].projectID+out[i].container < out[j].projectID+out[j].container })
	return out
}

// roots 返回所有副本的目录（不能放容器目录、新的存放位置，节点文件也不能访问）。
func (r *replicas) roots() []string {
	var out []string
	for _, ref := range r.list() {
		out = append(out, ref.root)
	}
	return out
}

// bases 返回所有存放位置：<节点目录>/replicas、默认目录、各个副本所在的位置、正在往里移动副本的位置。
// 容器目录、现有目录不能和它们重叠（见 project.Manager.ReplicaBases）：即使里面还没有副本，之后收到的副本也会放进去。
// 记录损坏时加上从坏文件里认出来的路径（见 markCorrupt）。
func (r *replicas) bases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.knownBasesLocked()
	for _, b := range r.salvaged {
		if !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

// usageDirs 是统计副本占用时要算的目录：默认目录、<节点目录>/replicas，加上不在它们下面的副本。
func (r *replicas) usageDirs() []string {
	def, _ := r.defaultDir()
	out := []string{def}
	if r.dir != def {
		out = append(out, r.dir)
	}
	for _, ref := range r.list() {
		if !slices.ContainsFunc(out, func(d string) bool { return pathUnder(ref.root, d) }) {
			out = append(out, ref.root)
		}
	}
	return out
}

// cleanStaging 删掉移动到一半（节点中途退出）留下的临时目录。
func (r *replicas) cleanStaging() {
	bases := []string{r.dir}
	if r.idx.Dir != "" {
		bases = append(bases, r.idx.Dir)
	}
	for _, l := range r.idx.Locations {
		bases = append(bases, l.Base)
	}
	bases = append(bases, r.idx.Moving...)
	for _, b := range bases {
		entries, _ := os.ReadDir(b)
		for _, e := range entries {
			// ReadDir 不跟随符号链接：指向别处的链接不是目录，不会删到链接指向的地方。
			if e.IsDir() && isStagingName(e.Name()) {
				os.RemoveAll(filepath.Join(b, e.Name()))
			}
		}
	}
	if len(r.idx.Moving) > 0 {
		r.idx.Moving = nil
		_ = r.persistLocked()
	}
}

// isStagingName 判断目录名是不是移动用的临时目录，只认完整的格式：.<项目 ID>-<容器名>.nagi-moving-<12 位十六进制>。
// 存放位置里别人放的、名字碰巧带着标记的目录不删。
func isStagingName(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	i := strings.LastIndex(name, replicaStagingMark)
	if i < 0 {
		return false
	}
	suffix := name[i+len(replicaStagingMark):]
	if len(suffix) != 12 || strings.Trim(suffix, "0123456789abcdef") != "" {
		return false
	}
	_, _, ok := splitReplicaID(name[1:i])
	return ok
}

// markMoving 记下（on 为 false 时去掉）正在往 base 里移动副本，见 replicaIndex.Moving。
func (r *replicas) markMoving(base string, on bool) error {
	return r.markMove(replicaMove{To: base}, on)
}

// markMove 记下（on 为 false 时去掉）一次移动：目标位置和原位置记进 Moving（清理临时目录用；
// 启动时 recoverMoves 只处理两头都是已知存放位置的记录，原位置是单独指定的、切换以后就不在 Locations 里了），
// m.ID 不为空时整条记进 Moves（启动时收尾用，见 recoverMoves）。改名、复制之前写盘；写不进去时内存也不变。
func (r *replicas) markMove(m replicaMove, on bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	prevMoving, prevMoves := slices.Clone(r.idx.Moving), slices.Clone(r.idx.Moves)
	ends := []string{m.To}
	if m.From != "" && m.From != m.To {
		ends = append(ends, m.From)
	}
	r.idx.Moving = slices.DeleteFunc(r.idx.Moving, func(b string) bool { return slices.Contains(ends, b) })
	if m.ID != "" {
		r.idx.Moves = slices.DeleteFunc(r.idx.Moves, func(x replicaMove) bool { return x.ID == m.ID })
	}
	if on {
		r.idx.Moving = append(r.idx.Moving, ends...)
		if m.ID != "" {
			r.idx.Moves = append(r.idx.Moves, m)
		}
	}
	if err := r.persistLocked(); err != nil {
		r.idx.Moving, r.idx.Moves = prevMoving, prevMoves
		return err
	}
	return nil
}

// ---- 校验 ----

// pathUnder 判断 p 是 base 或者在 base 下面（都是整理过的绝对路径）。
func pathUnder(p, base string) bool {
	if base == "/" {
		return true
	}
	return p == base || strings.HasPrefix(p, base+"/")
}

// realPath 解开符号链接。路径后面几级还不存在时，解开已经存在的部分，再接上不存在的部分。
func realPath(p string) (string, error) {
	var rest []string
	cur := p
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{real}, rest...)...), nil
}

func realOrSelf(p string) string {
	if r, err := realPath(p); err == nil {
		return r
	}
	return p
}

// invalid 是参数错误的 gRPC 状态（文案要在 i18n/en.go 里有译文）。
func invalid(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

// checkBase 校验存放位置并建好目录，返回真实路径。dir 和默认的 <节点目录>/replicas 是同一个目录时返回 r.dir。
// 规则：绝对路径、不含 ..；解开符号链接以后按真实路径检查：不能是 / 和系统目录，不能在 Nagi 的数据目录里或者包含它，
// 不能和容器目录、现有目录重叠，不能在项目根目录里或者包含它，不能在节点文件禁止访问的目录里、别的副本的目录里；
// 已经存在的各级目录都要属于 root（或节点进程的用户），而且其他用户不可写（上级目录有粘滞位时除外）。
func (r *replicas) checkBase(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if !strings.HasPrefix(dir, "/") || strings.ContainsRune(dir, 0) {
		return "", invalid("存放位置须为绝对路径")
	}
	if slices.Contains(strings.Split(dir, "/"), "..") {
		return "", invalid("存放位置不能包含 ..")
	}
	real, err := realPath(filepath.Clean(dir))
	if err != nil {
		return "", invalid("无法解析存放位置 %s", dir)
	}
	real = filepath.ToSlash(real)
	if real == "/" {
		return "", invalid("不能使用根目录作为存放位置")
	}
	if real == realOrSelf(r.dir) {
		if err := os.MkdirAll(r.dir, 0o700); err != nil {
			return "", err
		}
		return r.dir, nil
	}
	for _, d := range r.forbidden {
		if pathUnder(real, d) || pathUnder(real, realOrSelf(d)) {
			return "", invalid("不能放在系统目录 %s 里", d)
		}
	}
	for _, d := range replicaShallow {
		if real == d {
			return "", invalid("不能直接使用 %s，请使用它下面的目录", d)
		}
	}
	for _, d := range r.reserved {
		d = realOrSelf(d)
		if pathUnder(real, d) || pathUnder(d, real) {
			return "", invalid("存放位置不能在 Nagi 的数据目录 %s 里，也不能包含它", d)
		}
	}
	if r.managed != nil {
		for _, d := range r.managed() {
			if d == "" {
				continue
			}
			d = realOrSelf(d)
			if pathUnder(real, d) || pathUnder(d, real) {
				return "", invalid("存放位置和容器目录 %s 重叠", d)
			}
		}
	}
	// 项目根目录下面是操作员可以选的容器目录，存放位置放在里面时，操作员能抢先占用它（见 docs/snapshot.md#副本的存放位置）。
	if r.projectsRoot != nil {
		if root := realOrSelf(r.projectsRoot()); root != "" && (pathUnder(root, real) || pathUnder(real, root)) {
			return "", invalid("存放位置不能在项目根目录 %s 里，也不能包含它", root)
		}
	}
	if r.denied != nil && r.denied(real) {
		return "", invalid("存放位置 %s 在禁止访问的目录里", real)
	}
	for _, root := range r.roots() {
		if pathUnder(real, realOrSelf(root)) {
			return "", invalid("存放位置不能在另一个副本的目录 %s 里", root)
		}
	}
	// 已经存在的各级目录。最后一级（存放位置本身）不允许粘滞位的例外。
	parts := strings.Split(strings.TrimPrefix(real, "/"), "/")
	cur := "/"
	for i := 0; i <= len(parts); i++ {
		if i > 0 {
			cur = filepath.Join(cur, parts[i-1])
		}
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", invalid("无法检查目录 %s", cur)
		}
		if err := checkReplicaDirOwner(cur, info, i == len(parts)); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(real, 0o700); err != nil {
		return "", invalid("无法创建目录 %s：%s", real, err.Error())
	}
	// 建好以后再核对一次：中途被换成符号链接、或者别人抢先建了目录时拒绝。
	info, err := os.Lstat(real)
	if err != nil {
		return "", err
	}
	if again, err := filepath.EvalSymlinks(real); err != nil || filepath.ToSlash(again) != real {
		return "", invalid("存放位置 %s 经过符号链接，请更换目录", real)
	}
	if err := checkReplicaDirOwner(real, info, true); err != nil {
		return "", err
	}
	return real, nil
}

// checkReplicaDirOwner 检查存放位置和它的上级目录：是目录，属于 root 或节点进程的用户，其他用户不可写。
// last 是存放位置本身；上级目录有粘滞位并且属于 root 时（例如 /tmp）别人删不掉、改不了下面的目录，可以接受。
func checkReplicaDirOwner(p string, info fs.FileInfo, last bool) error {
	if !info.IsDir() {
		return invalid("%s 不是目录", p)
	}
	uid, known := fileUID(info)
	if known && uid != 0 && uid != os.Getuid() {
		return invalid("目录 %s 不属于 root", p)
	}
	if info.Mode().Perm()&0o022 != 0 && (last || info.Mode()&fs.ModeSticky == 0 || (known && uid != 0)) {
		return invalid("目录 %s 其他用户可写", p)
	}
	return nil
}

// ---- 移动 ----

// renameDir 是同一个文件系统内的改名，测试里换成返回 EXDEV 来走复制的路径。
var renameDir = os.Rename

// errMoveCanceled：移动副本时取消了（节点退出）。
var errMoveCanceled = errors.New("已取消")

// relocate 把目录 src 移到 dst（dst 不存在）。同一个文件系统时直接改名；否则复制到 dst 旁边的临时目录，
// 逐个核对大小和 SHA-256，再改名成 dst。成功时 renamed 表示是改名（src 已经没有了）还是复制（src 还在，由调用方删除）。
// 失败时 dst 不存在，src 原样保留。
func relocate(ctx context.Context, src, dst string, onLine func(string)) (renamed bool, err error) {
	err = renameDir(src, dst)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return false, err
	}
	staging := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+replicaStagingMark+randomSuffix())
	defer func() {
		if err != nil {
			os.RemoveAll(staging)
		}
	}()
	sums, total, err := copyReplicaTree(ctx, src, staging, onLine)
	if err != nil {
		return false, err
	}
	onLine("正在核对")
	if err := verifyReplicaTree(ctx, staging, sums); err != nil {
		return false, err
	}
	if err := os.Rename(staging, dst); err != nil {
		return false, err
	}
	onLine("已复制并核对 " + humanBytes(total))
	return false, nil
}

type fileSum struct {
	size int64
	sum  [sha256.Size]byte
}

// copyReplicaTree 把 src 复制到 dst（dst 不存在），保留权限，返回每个文件的大小和 SHA-256（相对路径为键）。
// 副本里只有目录和普通文件，遇到符号链接等其他东西就失败。
func copyReplicaTree(ctx context.Context, src, dst string, onLine func(string)) (map[string]fileSum, int64, error) {
	var total int64
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	onLine("需要复制 " + humanBytes(total))
	guard := &metrics.DiskGuard{Dir: filepath.Dir(dst)}
	sums := map[string]fileSum{}
	var done, reported int64
	buf := make([]byte, 1<<20)
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errMoveCanceled
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.Mkdir(target, info.Mode().Perm()|0o700)
		case !d.Type().IsRegular():
			return status.Errorf(codes.FailedPrecondition, "副本里有不是普通文件的 %s，无法移动", rel)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.CopyBuffer(guard.Writer(io.MultiWriter(out, h)), in, buf)
		if err == nil {
			err = out.Sync()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			var low *metrics.LowDiskError
			if errors.As(err, &low) {
				return status.Error(codes.ResourceExhausted, low.Error())
			}
			return err
		}
		var s fileSum
		s.size = n
		copy(s.sum[:], h.Sum(nil))
		sums[filepath.ToSlash(rel)] = s
		done += n
		// 每复制 1 GiB 报一次进度。
		if done-reported >= 1<<30 {
			reported = done
			onLine(fmt.Sprintf("已复制 %s / %s", humanBytes(done), humanBytes(total)))
		}
		return nil
	})
	if errors.Is(err, errMoveCanceled) {
		return nil, 0, status.Error(codes.Canceled, "移动副本时节点正在退出，已取消")
	}
	return sums, total, err
}

// verifyReplicaTree 重新读一遍 dir，核对文件和 sums 一一对应、大小和 SHA-256 一致。
func verifyReplicaTree(ctx context.Context, dir string, sums map[string]fileSum) error {
	seen := 0
	buf := make([]byte, 1<<20)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errMoveCanceled
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		want, ok := sums[filepath.ToSlash(rel)]
		if !ok || !d.Type().IsRegular() {
			return status.Errorf(codes.DataLoss, "核对失败：多出了 %s", rel)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.CopyBuffer(h, f, buf)
		if err != nil {
			return err
		}
		if n != want.size || string(h.Sum(nil)) != string(want.sum[:]) {
			return status.Errorf(codes.DataLoss, "核对失败：%s 的内容和原文件不一致", rel)
		}
		seen++
		return nil
	})
	if errors.Is(err, errMoveCanceled) {
		return status.Error(codes.Canceled, "移动副本时节点正在退出，已取消")
	}
	if err == nil && seen != len(sums) {
		return status.Error(codes.DataLoss, "核对失败：复制过去的文件不全")
	}
	return err
}

// replicaLockWait 是移动副本时最多等多久正在进行的同步、恢复结束。移动时拿着节点级的 moving，
// 一直等下去的话，被攻破的源节点不停地发数据就能卡住这个节点所有的移动（审查 L3）。
var replicaLockWait = time.Minute

// lockWithin 在 wait 之内拿到副本的锁，拿不到时返回「同步进行中」。不排队等写锁：排队的写锁会挡住后来的读锁，
// 一条不结束的流就能让这个副本所有的同步都停下。
func (r *replicas) lockWithin(ctx context.Context, projectID, container string, wait time.Duration) (func(), error) {
	l, done := r.lockOf(projectID, container)
	deadline := time.Now().Add(wait)
	for {
		if l.TryLock() {
			return func() { l.Unlock(); done() }, nil
		}
		if time.Now().After(deadline) {
			done()
			return nil, status.Error(codes.Aborted, "副本同步进行中，请稍后重试")
		}
		select {
		case <-ctx.Done():
			done()
			return nil, status.Error(codes.Canceled, "移动副本时节点正在退出，已取消")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// moveTo 把一个副本移到 base 下面，custom 表示是单独指定的存放位置。调用时持有 moving。
// 拿着这个副本的锁：同步写入、提交、查看快照、恢复都要等它结束；等不到（replicaLockWait）就放弃。移动完、核对无误才记下新位置；
// 记不下来时撤销移动。复制过去的才删除原位置，删除失败只提示，数据已经在新位置了。
func (r *replicas) moveTo(ctx context.Context, projectID, container, base string, custom bool, onLine func(string)) error {
	id, err := replicaID(projectID, container)
	if err != nil {
		return err
	}
	unlock, err := r.lockWithin(ctx, projectID, container, replicaLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	src, err := r.root(projectID, container)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(src, "meta.json")); err != nil {
		return status.Error(codes.NotFound, "此节点上没有该容器的副本")
	}
	dst := filepath.Join(base, id)
	if src == dst {
		return r.setLocation(id, base, custom)
	}
	if _, err := os.Lstat(dst); err == nil {
		return status.Errorf(codes.FailedPrecondition, "目标位置已有同名目录 %s，请先处理", dst)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	mv := replicaMove{ID: id, From: filepath.Dir(src), To: base, Custom: custom}
	if err := r.markMove(mv, true); err != nil {
		return err
	}
	keepMove := false
	defer func() {
		if !keepMove {
			_ = r.markMove(mv, false)
		}
	}()
	onLine("移动 " + src + " → " + dst)
	renamed, err := relocate(ctx, src, dst, onLine)
	if err != nil {
		return err
	}
	if err := r.setLocation(id, base, custom); err != nil {
		if renamed {
			if rerr := renameDir(dst, src); rerr != nil {
				// 移回去也失败了：数据在 dst，磁盘上的记录还指着 src。记录写不进去多半是节点目录所在的盘出了问题。
				// 内存里改用 dst（之后的同步、恢复找得到它，不会在 src 新建一份），磁盘上留着这次移动的记录：
				// 下次启动时 recoverMoves 看到原位置没有、目标位置有，自动采用 dst。不删任何东西。
				keepMove = true
				r.mu.Lock()
				r.placeLocked(id, base, custom)
				_ = r.persistLocked()
				r.mu.Unlock()
				if r.stateDir != nil {
					os.RemoveAll(r.stateDir(projectID, container))
				}
				return status.Errorf(codes.Internal, "保存副本位置失败（%s）。副本已在 %s，照常使用，节点重启时自动采用这个位置", err.Error(), dst)
			}
		} else {
			os.RemoveAll(dst)
		}
		return err
	}
	// 快照仓库的连接配置里记着旧路径，删掉，下次打开时按新路径重新连接。
	if r.stateDir != nil {
		os.RemoveAll(r.stateDir(projectID, container))
	}
	if !renamed {
		if err := os.RemoveAll(src); err != nil {
			onLine("已移动，但删除原位置失败（" + err.Error() + "），请手动删除 " + src)
		}
	}
	onLine("已移动到 " + dst)
	return nil
}

// move 把一个副本移到 dir 下面（为空时移回节点的默认目录）。和节点的默认目录相同时按默认处理。
func (r *replicas) move(ctx context.Context, projectID, container, dir string, onLine func(string)) error {
	r.moving.Lock()
	defer r.moving.Unlock()
	if _, err := replicaID(projectID, container); err != nil {
		return err
	}
	if err := r.usable(); err != nil {
		return err
	}
	def, _ := r.defaultDir()
	base, custom := def, false
	if strings.TrimSpace(dir) != "" {
		b, err := r.checkBase(dir)
		if err != nil {
			return err
		}
		base, custom = b, b != def
	}
	return r.moveTo(ctx, projectID, container, base, custom, onLine)
}

// setDefault 修改节点的默认目录（为空表示 <节点目录>/replicas），把在旧的默认目录里的副本依次移过去。
// 先把它们记成"还在旧目录里"、换上新的默认目录，再一个一个移：移动失败的留在原处照常使用，下次修改时再移。
func (r *replicas) setDefault(ctx context.Context, dir string, onLine func(string)) error {
	r.moving.Lock()
	defer r.moving.Unlock()
	if err := r.usable(); err != nil {
		return err
	}
	base := r.dir
	if strings.TrimSpace(dir) != "" {
		b, err := r.checkBase(dir)
		if err != nil {
			return err
		}
		base = b
	} else if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	// 在旧的默认目录里的副本记下位置，换了默认目录也找得到。拿着 layout：新登记的副本（save）要么在这之前
	// 建好、被记下来，要么在这之后建在新的默认目录里。
	r.layout.Lock()
	pending := r.list()
	r.mu.Lock()
	old := r.defaultLocked()
	prev := r.idx
	prev.Locations = make(map[string]replicaLocation, len(r.idx.Locations))
	for id, l := range r.idx.Locations {
		prev.Locations[id] = l
	}
	for _, ref := range pending {
		id := ref.projectID + "-" + ref.container
		if _, ok := r.idx.Locations[id]; !ok && old != base {
			if r.idx.Locations == nil {
				r.idx.Locations = map[string]replicaLocation{}
			}
			r.idx.Locations[id] = replicaLocation{Base: old}
		}
	}
	r.idx.Dir = base
	if base == r.dir {
		r.idx.Dir = ""
	}
	if err := r.persistLocked(); err != nil {
		r.idx = prev
		r.mu.Unlock()
		r.layout.Unlock()
		return err
	}
	var todo []string
	for id, l := range r.idx.Locations {
		if !l.Custom && l.Base != base {
			todo = append(todo, id)
		}
	}
	r.mu.Unlock()
	r.layout.Unlock()
	sort.Strings(todo)
	if len(todo) == 0 {
		onLine("默认目录已改为 " + base + "，没有需要移动的副本")
		return nil
	}
	var failed []string
	for i, id := range todo {
		pid, name, ok := splitReplicaID(id)
		if !ok {
			continue
		}
		onLine(fmt.Sprintf("(%d/%d) %s", i+1, len(todo), id))
		if err := r.moveTo(ctx, pid, name, base, false, onLine); err != nil {
			if status.Code(err) == codes.NotFound {
				r.forget(id) // 副本已经不在了，记录没用了
				continue
			}
			onLine("移动失败，留在原处：" + err.Error())
			failed = append(failed, id)
		}
	}
	onLine("默认目录已改为 " + base)
	if len(failed) > 0 {
		return status.Errorf(codes.Aborted, "%d 个副本没有移动，仍在原位置照常使用：%s。新收到的副本存到新目录，再次保存会重试", len(failed), strings.Join(failed, "、"))
	}
	return nil
}

// moveExisting 在登记副本时指定了存放位置、本节点却已有这个容器的副本在别处时，把它移到 base 下面。
// 没有副本或者已经在那里时什么都不做。调用时持有 moving。移动不随面板断开取消（面板等不及时节点照样做完），
// 过程写进节点日志。
func (a *agent) moveExisting(ctx context.Context, projectID, container, base string) error {
	r := a.replicas
	root, err := r.root(projectID, container)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "meta.json")); err != nil || filepath.Dir(root) == base {
		return nil
	}
	ctx, cancel := a.stopWithNode(ctx)
	defer cancel()
	def, _ := r.defaultDir()
	log := func(line string) {
		if a.log != nil {
			a.log.Info("移动已有的副本", "project", projectID, "container", container, "msg", line)
		}
	}
	if err := r.moveTo(ctx, projectID, container, base, base != def, log); err != nil {
		return status.Errorf(status.Code(err), "此节点上已有该容器的副本，移到 %s 失败，仍在原处：%s", base, status.Convert(err).Message())
	}
	return nil
}

// ---- AgentService ----

// SetReplicaDir 修改节点保存副本的默认目录，并把副本移过去。开始了就做完，面板断开也不中断。
func (s *agentService) SetReplicaDir(req *nagiv1.SetReplicaDirRequest, stream nagiv1.AgentService_SetReplicaDirServer) error {
	ctx, cancel := s.node.stopWithNode(stream.Context())
	defer cancel()
	defer s.changed()
	return runStream(stream.Send, func(onLine func(string)) error {
		return plainErr(s.node.replicas.setDefault(ctx, req.GetDir(), onLine))
	})
}

// MoveReplica 把一个副本移到指定的存放位置（为空时移回默认目录）。开始了就做完，面板断开也不中断。
func (s *agentService) MoveReplica(req *nagiv1.MoveReplicaRequest, stream nagiv1.AgentService_MoveReplicaServer) error {
	ctx, cancel := s.node.stopWithNode(stream.Context())
	defer cancel()
	defer s.changed()
	return runStream(stream.Send, func(onLine func(string)) error {
		return plainErr(s.node.replicas.move(ctx, req.GetProjectId(), req.GetContainer(), req.GetDir(), onLine))
	})
}

// plainErr 把 gRPC 状态换成只有文字的错误：放进流的结果里，面板按文字翻译。
func plainErr(err error) error {
	if st, ok := status.FromError(err); ok && err != nil {
		return errors.New(st.Message())
	}
	return err
}

// stopWithNode 返回一个不随面板断开取消、但节点退出时取消的 ctx。
func (a *agent) stopWithNode(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	if a.ctx != nil {
		stop := context.AfterFunc(a.ctx, cancel)
		return ctx, func() { stop(); cancel() }
	}
	return ctx, cancel
}

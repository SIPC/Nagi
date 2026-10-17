package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/snapshot"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 备份节点：项目的快照仓库和恢复项目所需的配置，同步到另一个节点（见 docs/snapshot.md）。
// 这里负责源节点上的设置和文件清单，以及在备份节点上把副本恢复成项目；传输由节点经 mesh 完成。

const (
	metaBackupTarget    = "backup_target"
	metaBackupLastSync  = "backup_last_sync"
	metaBackupLastError = "backup_last_error"
)

// BackupTarget 是项目的备份节点。
type BackupTarget struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Key    []byte `json:"key"` // 备份节点的 mesh 公钥
}

// BackupState 是源节点上的同步状态。
type BackupState struct {
	Target    *BackupTarget
	LastSync  time.Time
	LastError string
}

// BackupState 返回项目的备份设置和最近一次同步的结果。
func (m *Manager) BackupState(ctx context.Context, id string) (BackupState, error) {
	var st BackupState
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return st, err
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return st, err
	}
	defer pdb.Close()
	if raw := pdb.meta(ctx, metaBackupTarget); raw != "" {
		t := &BackupTarget{}
		if json.Unmarshal([]byte(raw), t) == nil {
			st.Target = t
		}
	}
	if sec, _ := strconv.ParseInt(pdb.meta(ctx, metaBackupLastSync), 10, 64); sec > 0 {
		st.LastSync = time.Unix(sec, 0)
	}
	st.LastError = pdb.meta(ctx, metaBackupLastError)
	return st, nil
}

// SetBackupTarget 设置备份节点，t 为 nil 表示取消。
func (m *Manager) SetBackupTarget(ctx context.Context, id string, t *BackupTarget) error {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return err
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return err
	}
	defer pdb.Close()
	value := ""
	if t != nil {
		b, _ := json.Marshal(t)
		value = string(b)
	}
	for k, v := range map[string]string{metaBackupTarget: value, metaBackupLastSync: "", metaBackupLastError: ""} {
		if err := pdb.setMeta(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

// RecordBackupSync 记录一次同步的结果。
func (m *Manager) RecordBackupSync(ctx context.Context, id string, syncErr error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return
	}
	defer pdb.Close()
	if syncErr != nil {
		_ = pdb.setMeta(ctx, metaBackupLastError, syncErr.Error())
		return
	}
	_ = pdb.setMeta(ctx, metaBackupLastError, "")
	_ = pdb.setMeta(ctx, metaBackupLastSync, strconv.FormatInt(time.Now().Unix(), 10))
}

// BackupKeys 返回所有项目快照仓库的密码（项目 ID → 密码），上报给面板备份。
func (m *Manager) BackupKeys(ctx context.Context) map[string]string {
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, r := range rows {
		if b, err := os.ReadFile(paths{root: r.Dir}.passwordFile()); err == nil {
			out[r.ID] = strings.TrimSpace(string(b))
		}
	}
	return out
}

// ReplicaFile 是要同步到备份节点的一个文件。
type ReplicaFile struct {
	Path string // 副本里的相对路径：repo/...、config/...、members/<成员>/... 或 project.json
	Abs  string // 本地文件；Data 不为空时不用
	Data []byte
	Size int64
	// Immutable 表示内容写入后不会再变，大小相同就不用再传。kopia 仓库里的文件名都由内容决定、写入后不变，
	// 只有 kopia.* 开头的几个（例如维护进度 kopia.maintenance）会被改写。
	Immutable bool
}

// replicaConfig 是副本里要带上的项目配置（相对 config/）。project.db 单独处理（拷一份一致的副本）。
var replicaConfig = []string{"format", ".env", "routes", "docker", "backup.key"}

// replicaMemberSkip 是成员目录里不同步的东西：运行文件和数据从快照恢复，临时目录不要。
var replicaMemberSkip = []string{"data", "volumes", ".checkout", ".restore", ".upload"}

// replicaInfo 是副本里的 project.json。
type replicaInfo struct {
	// Format 是项目目录的格式版本号，接收方不认识就拒绝。
	Format int    `json:"format"`
	Name   string `json:"name"`
	// 是否有状态：顶替时有状态项目默认要用户确认。
	Stateful bool `json:"stateful,omitempty"`
}

// ReplicaFiles 返回要同步的文件：快照仓库、项目配置、成员目录里的配置文件、project.db 的一致副本和 project.json。
// 顺序是先数据包、再其他仓库文件：仓库的索引写在数据包之后，按这个顺序传，备份节点上的仓库始终能打开。
func (m *Manager) ReplicaFiles(ctx context.Context, id string) ([]ReplicaFile, string, error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, "", err
	}
	p := paths{root: row.Dir}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return nil, "", err
	}
	name := pdb.meta(ctx, metaName)
	stateful := m.projectStateful(ctx, pdb, id, p)
	dbCopy, err := pdb.snapshotDB(ctx)
	pdb.Close()
	if err != nil {
		return nil, "", fmt.Errorf("复制 project.db: %w", err)
	}

	var packs, others, config []ReplicaFile
	err = filepath.WalkDir(p.backupsDir(), func(full string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil // 维护时可能正在删除
		}
		rel, _ := filepath.Rel(p.backupsDir(), full)
		b := d.Name()
		f := ReplicaFile{Path: "repo/" + filepath.ToSlash(rel), Abs: full, Size: info.Size(), Immutable: !strings.HasPrefix(b, "kopia.")}
		if strings.HasPrefix(b, "p") || strings.HasPrefix(b, "q") {
			packs = append(packs, f)
		} else {
			others = append(others, f)
		}
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("读取快照仓库: %w", err)
	}
	// addTree 把 root 下的普通文件加进清单，路径前缀是 prefix。skip 返回 true 的顶层条目跳过。
	addTree := func(root, prefix string, skip func(top string) bool) {
		_ = filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, full)
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return nil
			}
			if top, _, _ := strings.Cut(rel, "/"); skip != nil && skip(top) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() || strings.HasSuffix(rel, ".tmp") {
				return nil
			}
			config = append(config, ReplicaFile{Path: prefix + rel, Abs: full, Size: info.Size()})
			return nil
		})
	}
	for _, c := range replicaConfig {
		full := filepath.Join(p.configDir(), c)
		info, err := os.Stat(full)
		switch {
		case err != nil:
		case info.Mode().IsRegular():
			config = append(config, ReplicaFile{Path: "config/" + c, Abs: full, Size: info.Size()})
		case info.IsDir():
			addTree(full, "config/"+c+"/", nil)
		}
	}
	config = append(config, ReplicaFile{Path: "config/project.db", Data: dbCopy, Size: int64(len(dbCopy))})
	if entries, err := os.ReadDir(p.membersDir()); err == nil {
		for _, e := range entries {
			if !e.IsDir() || !nameRe.MatchString(e.Name()) {
				continue
			}
			addTree(filepath.Join(p.membersDir(), e.Name()), "members/"+e.Name()+"/", func(top string) bool {
				return slices.Contains(replicaMemberSkip, top) || strings.HasPrefix(top, ".upload-")
			})
		}
	}
	info, _ := json.Marshal(replicaInfo{Format: formatVersion, Name: name, Stateful: stateful})
	out := append(append(append(packs, others...), config...), ReplicaFile{Path: "project.json", Data: info, Size: int64(len(info))})
	return out, name, nil
}

// ReplicaPath 校验副本里的相对路径，返回 root 下的绝对路径。
func ReplicaPath(root, rel string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean != rel || strings.HasPrefix(clean, "../") || filepath.IsAbs(rel) ||
		!(clean == "project.json" || strings.HasPrefix(clean, "repo/") || strings.HasPrefix(clean, "config/") || strings.HasPrefix(clean, "members/")) {
		return "", fmt.Errorf("副本路径无效：%q", rel)
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

// readReplicaInfo 读取副本里的 project.json，检查格式版本本节点认识。
func readReplicaInfo(root string) (replicaInfo, error) {
	var info replicaInfo
	raw, err := os.ReadFile(filepath.Join(root, "project.json"))
	if err != nil {
		return info, errors.New("副本还没有同步完成")
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, err
	}
	return info, CheckFormat(info.Format)
}

// CheckReplicaFormat 检查副本清单里的 project.json 的格式版本本节点认识（同步提交时调用）。
func CheckReplicaFormat(projectJSON []byte) error {
	var info replicaInfo
	if err := json.Unmarshal(projectJSON, &info); err != nil {
		return err
	}
	return CheckFormat(info.Format)
}

// ReplicaRepo 打开副本里的快照仓库（只用于查看和恢复）。stateDir 是这个副本的连接配置和缓存目录。
func ReplicaRepo(ctx context.Context, id, root, stateDir string) (*snapshot.Repo, error) {
	pw := filepath.Join(root, "config", "backup.key")
	if _, err := os.Stat(pw); err != nil {
		return nil, errors.New("副本还没有同步完成")
	}
	return snapshot.Open(ctx, id, filepath.Join(root, "repo"), pw, stateDir)
}

// ReplicaSnapshots 列出副本里的快照。
func ReplicaSnapshots(ctx context.Context, id, root, stateDir string) ([]*nagiv1.Snapshot, error) {
	rep, err := ReplicaRepo(ctx, id, root, stateDir)
	if err != nil {
		return nil, err
	}
	defer rep.Close(ctx)
	list, err := rep.List(ctx)
	if err != nil {
		return nil, err
	}
	return snapshotsProto(list), nil
}

// CheckRestore 检查能不能把项目恢复到 dir：本节点还没有这个项目，目录可用，
// 项目的对外端口和域名在本节点上没有被其他项目或程序占用。
func (m *Manager) CheckRestore(ctx context.Context, id, dir string, routes []*nagiv1.Exposure) error {
	if _, err := m.projectRow(ctx, id); err == nil {
		return invalid("目标节点上已经有这个项目")
	}
	if _, err := m.checkDir(ctx, dir); err != nil {
		return err
	}
	for _, r := range routes {
		if r.GetError() != "" {
			continue
		}
		if err := m.checkConflicts(ctx, id, r.GetName(), r); err != nil {
			return err
		}
	}
	return nil
}

// RestoreReplica 把副本恢复成本节点上的项目：项目 ID 不变，复制快照仓库、项目配置（含 project.db，
// 版本记录和部署记录都在）和成员的配置文件，恢复快照里各成员的 data/ 和 volumes/，
// 然后按各成员当前部署的版本启动（不生成新版本）。snapshotID 为空时用最新的定时或手动快照。
func (m *Manager) RestoreReplica(ctx context.Context, id, root, dir, snapshotID string, onLine func(string)) (*nagiv1.Project, error) {
	if _, err := m.projectRow(ctx, id); err == nil {
		return nil, invalid("本节点上已经有这个项目")
	}
	if _, err := readReplicaInfo(root); err != nil {
		return nil, err
	}
	dir, err := m.checkDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	p := paths{root: dir}
	fail := func(err error) (*nagiv1.Project, error) {
		_ = m.releaseProject(ctx, id)
		os.RemoveAll(dir)
		return nil, err
	}
	onLine("复制快照仓库和配置")
	for _, c := range []struct{ src, dst string }{
		{filepath.Join(root, "repo"), p.backupsDir()},
		{filepath.Join(root, "config"), p.configDir()},
		{filepath.Join(root, "members"), p.membersDir()},
	} {
		if err := copyTree(c.src, c.dst); err != nil {
			return fail(fmt.Errorf("复制副本: %w", err))
		}
	}
	_ = os.Chmod(p.configDir(), 0o700)
	pdb, err := openProject(ctx, dir)
	if err != nil {
		return fail(err)
	}
	defer pdb.Close()
	if err := m.nodeQ.InsertProject(ctx, nodestore.InsertProjectParams{ID: id, Dir: dir, CreatedAt: time.Now().Unix()}); err != nil {
		return fail(err)
	}
	members, err := pdb.q.ListMembers(ctx)
	if err != nil {
		return fail(err)
	}
	for _, mem := range members {
		mp := p.member(mem.Name)
		for _, d := range []string{mp.dataDir(), mp.volumesDir()} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return fail(err)
			}
		}
	}

	rep, err := m.openRepo(ctx, id, p)
	if err != nil {
		return fail(fmt.Errorf("打开快照仓库: %w", err))
	}
	if snapshotID == "" {
		list, err := rep.List(ctx, snapshot.Scheduled, snapshot.Manual)
		if err == nil && len(list) > 0 {
			snapshotID = list[0].ID
			onLine(fmt.Sprintf("使用最新的快照（%s）", list[0].Time.Local().Format("2006-01-02 15:04:05")))
		} else if len(members) > 0 {
			rep.Close(ctx)
			return fail(errors.New("副本里没有含数据的快照"))
		}
	}
	for _, mem := range members {
		if snapshotID == "" {
			break
		}
		mp := p.member(mem.Name)
		onLine("恢复成员 " + mem.Name + " 的 data/ 和 volumes/")
		if err := rep.Restore(ctx, snapshotID, "member."+mem.Name+"/data", mp.dataDir()); err != nil {
			onLine("快照里没有成员 " + mem.Name + " 的数据，跳过：" + err.Error())
			continue
		}
		if err := rep.Restore(ctx, snapshotID, "member."+mem.Name+"/volumes", mp.volumesDir()); err != nil {
			onLine("快照里没有成员 " + mem.Name + " 的 volumes/，跳过：" + err.Error())
		}
	}
	rep.Close(ctx)

	// 文件已经恢复好，启动失败时项目保留下来，用户可以修改配置后重新部署。
	var startErr error
	for _, mem := range members {
		if mem.DeployedVersion == 0 {
			continue
		}
		onLine(fmt.Sprintf("启动成员 %s（版本 %d）", mem.Name, mem.DeployedVersion))
		if err := m.startDeployed(ctx, pdb, p, id, mem, onLine); err != nil && startErr == nil {
			startErr = fmt.Errorf("项目已恢复，但成员 %s 启动失败：%w", mem.Name, err)
		}
	}
	proj, err := pdb.project(ctx, id, dir)
	if err != nil {
		return nil, err
	}
	return proj, startErr
}

// copyTree 把 src 下的普通文件复制到 dst（同名覆盖）。src 不存在时什么都不做。
func copyTree(src, dst string) error {
	var files []string
	err := filepath.WalkDir(src, func(full string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.SkipAll
		}
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, full)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		rel, _ := filepath.Rel(src, f)
		info, err := os.Stat(f)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := copyFile(f, target, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

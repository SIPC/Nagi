//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testPID = "0123456789abcdef"

// testReplicas 建一个在临时目录里的 replicas：<tmp>/data 是节点目录。测试的临时目录在 /tmp 下面，
// 所以系统目录里去掉 /tmp。
func testReplicas(t *testing.T) (*replicas, string) {
	t.Helper()
	tmp := t.TempDir()
	if err := os.Chmod(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(tmp, "data")
	if err := os.MkdirAll(filepath.Join(data, "replicas"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := newReplicas(data)
	r.forbidden = slices.DeleteFunc(slices.Clone(replicaForbidden), func(d string) bool { return d == "/tmp" })
	r.reserved = []string{data}
	r.stateDir = func(pid, name string) string { return filepath.Join(data, "snapshots", "replica-"+pid+"-"+name) }
	return r, tmp
}

// addReplica 在副本当前的位置登记一个副本，并放几个文件进去。
func addReplica(t *testing.T, r *replicas, name string) string {
	t.Helper()
	unlock := r.lock(testPID, name)
	defer unlock()
	if err := r.save(&replicaMeta{ProjectID: testPID, Container: name, SourceKey: make([]byte, 32)}, &nagiv1.ContainerSpec{ProjectId: testPID, Name: name}); err != nil {
		t.Fatal(err)
	}
	root, _ := r.root(testPID, name)
	repo := filepath.Join(root, "files", "repo")
	for _, f := range []string{"p0001", "sub/q0002", "kopia.repository"} {
		p := filepath.Join(repo, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name+"/"+f+strings.Repeat("x", 4096)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func checkReplicaFiles(t *testing.T, root, name string) {
	t.Helper()
	for _, f := range []string{"p0001", "sub/q0002", "kopia.repository"} {
		b, err := os.ReadFile(filepath.Join(root, "files", "repo", f))
		if err != nil || string(b) != name+"/"+f+strings.Repeat("x", 4096) {
			t.Fatalf("%s 的内容不对：%v", f, err)
		}
		info, _ := os.Stat(filepath.Join(root, "files", "repo", f))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s 的权限是 %v，应该保持 0600", f, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "meta.json")); err != nil {
		t.Fatal("登记信息不在了")
	}
}

func discard(string) {}

// testManager 建一个只有空数据库的容器管理器（不连 Docker），用来测目录的检查。
func testManager(t *testing.T) *project.Manager {
	t.Helper()
	d, err := db.OpenMigrated(context.Background(), filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	m, err := project.NewManager(d, nil, project.DefaultSubnetPool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestReplicaBaseRules(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("需要 root：要改目录属主")
	}
	r, tmp := testReplicas(t)
	data := filepath.Join(tmp, "data")
	container := filepath.Join(tmp, "srv", "nagi", "web", "app")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	r.managed = func() []string { return []string{container} }
	r.projectsRoot = func() string { return filepath.Join(tmp, "srv", "nagi") }
	denied := filepath.Join(tmp, "denied")
	r.denied = func(real string) bool { return pathUnder(real, denied) }
	existing := addReplica(t, r, "web")

	file := filepath.Join(tmp, "file")
	os.WriteFile(file, nil, 0o600)
	open := filepath.Join(tmp, "open")
	os.Mkdir(open, 0o755)
	os.Chmod(open, 0o777)
	other := filepath.Join(tmp, "other")
	os.Mkdir(other, 0o755)
	os.Chown(other, 1000, 1000)
	toEtc := filepath.Join(tmp, "to-etc")
	os.Symlink("/etc", toEtc)
	sticky := filepath.Join(tmp, "sticky")
	os.Mkdir(sticky, 0o755)
	os.Chmod(sticky, 0o777|os.ModeSticky)

	bad := map[string]string{
		"相对路径":            "replicas",
		"空":               " ",
		"包含 ..":           tmp + "/x/../y",
		"根目录":             "/",
		"/etc":            "/etc/nagi",
		"/usr":            "/usr/local/nagi",
		"/proc":           "/proc/1",
		"/var/lib/docker": "/var/lib/docker/replicas",
		"/home 本身":        "/home",
		"/srv 本身":         "/srv",
		"数据目录里":           filepath.Join(data, "x"),
		"包含数据目录":          tmp,
		"容器目录里":           filepath.Join(container, "replicas"),
		"包含容器目录":          filepath.Join(tmp, "srv", "nagi", "web"),
		"包含项目根目录":         filepath.Join(tmp, "srv"),
		"项目根目录里":          filepath.Join(tmp, "srv", "nagi", "replicas"),
		"禁止访问的目录":         filepath.Join(denied, "x"),
		"别的副本的目录里":        filepath.Join(existing, "files"),
		"不是目录":            file,
		"其他用户可写":          open,
		"属主不是 root":       other,
		"有粘滞位的存放位置":       sticky,
		"符号链接到 /etc":      filepath.Join(toEtc, "nagi"),
		"上级是文件":           filepath.Join(file, "x"),
	}
	for name, dir := range bad {
		t.Run(name, func(t *testing.T) {
			got, err := r.checkBase(dir)
			if err == nil {
				t.Fatalf("%s 应该被拒绝，实际接受为 %s", dir, got)
			}
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("应该是参数错误：%v", err)
			}
		})
	}
	// /tmp 会被定时清理（测试的临时目录在它下面，上面的 r 去掉了它，这里用默认的规则）。
	if _, err := newReplicas(data).checkBase("/tmp/nagi-replicas"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("/tmp 下面应该被拒绝：%v", err)
	}
	// 拒绝的路径不会留下新建的目录。
	if _, err := os.Stat(filepath.Join(denied, "x")); err == nil {
		t.Fatal("拒绝的路径不应该创建目录")
	}

	t.Run("新目录", func(t *testing.T) {
		dir := filepath.Join(tmp, "mnt", "big", "replicas")
		got, err := r.checkBase(dir + "/")
		if err != nil || got != dir {
			t.Fatalf("应该接受 %s：%v %s", dir, err, got)
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("应该建成 0700 的目录：%v %v", err, info.Mode())
		}
	})
	t.Run("经过符号链接的上级", func(t *testing.T) {
		link := filepath.Join(tmp, "disk")
		os.Symlink(filepath.Join(tmp, "mnt"), link)
		got, err := r.checkBase(filepath.Join(link, "big", "r2"))
		if err != nil || got != filepath.Join(tmp, "mnt", "big", "r2") {
			t.Fatalf("应该按真实路径接受：%v %s", err, got)
		}
	})
	t.Run("上级有粘滞位", func(t *testing.T) {
		if _, err := r.checkBase(filepath.Join(sticky, "r")); err != nil {
			t.Fatalf("上级目录有粘滞位、属于 root 时应该接受：%v", err)
		}
	})
	t.Run("默认目录", func(t *testing.T) {
		got, err := r.checkBase(filepath.Join(data, "replicas"))
		if err != nil || got != r.dir {
			t.Fatalf("默认目录应该接受：%v %s", err, got)
		}
	})
}

// 审查 M1 的场景：管理员以前把默认目录设在项目根目录下面（现在 checkBase 拒绝，这里直接写进记录），目录还是空的。
// 操作员新建的容器目录等于这个存放位置、或者是之后副本会落到的位置时，节点都要拒绝。
func TestAuditContainerDirEqualsEmptyReplicaBase(t *testing.T) {
	r, tmp := testReplicas(t)
	projectsRoot := filepath.Join(tmp, "srv", "nagi")
	base := filepath.Join(projectsRoot, "replicas")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	r.projectsRoot = func() string { return projectsRoot }
	// 现在不能再这样设置。
	if _, err := r.checkBase(base); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("项目根目录下面的存放位置应该拒绝：%v", err)
	}
	if _, err := r.checkBase(filepath.Join(tmp, "srv")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("包含项目根目录的存放位置应该拒绝：%v", err)
	}
	r.idx.Dir = base // 升级以前留下的设置
	m := testManager(t)
	m.ProjectsRoot = r.projectsRoot
	m.ReplicaRoots = r.roots
	m.ReplicaBases = r.bases
	for _, dir := range []string{base, filepath.Join(base, testPID+"-db"), filepath.Join(base, "x", "y")} {
		if _, err := m.CheckDir(context.Background(), dir); err == nil {
			t.Errorf("容器目录 %s 和空的存放位置重叠，应该拒绝", dir)
		}
	}
	// 正在往里移动副本的位置也算。
	moving := filepath.Join(tmp, "mnt", "moving")
	if err := r.markMoving(moving, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CheckDir(context.Background(), filepath.Join(moving, "x")); err == nil {
		t.Error("正在移入的位置应该拒绝")
	}
	// 经过符号链接指到存放位置的目录也拒绝。
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CheckDir(context.Background(), filepath.Join(link, "x")); err == nil {
		t.Error("经过符号链接指到存放位置的目录应该拒绝")
	}
	if _, err := m.CheckDir(context.Background(), filepath.Join(projectsRoot, "web", "app")); err != nil {
		t.Errorf("别的目录应该接受：%v", err)
	}
}

// 存放位置不能放在现有目录（compose 的 project_dir）里面（审查 L1）。
func TestAuditBaseInsideProjectDir(t *testing.T) {
	r, tmp := testReplicas(t)
	immich := filepath.Join(tmp, "opt", "immich")
	if err := os.MkdirAll(immich, 0o755); err != nil {
		t.Fatal(err)
	}
	r.managed = func() []string { return []string{filepath.Join(tmp, "srv", "nagi", "p", "app"), immich} }
	if _, err := r.checkBase(filepath.Join(immich, "replicas")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("现有目录里面的存放位置应该拒绝：%v", err)
	}
}

// 接收同步时写入经过 os.Root：副本里被换成指向外面的符号链接时拒绝，不会写到别处（审查 L1）。
func TestReplicaFilesRoot(t *testing.T) {
	tmp := t.TempDir()
	files := filepath.Join(tmp, "base", testPID+"-web", "files")
	outside := filepath.Join(tmp, "outside")
	for _, d := range []string{filepath.Join(files, "repo"), outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(files, "repo", "p")); err != nil {
		t.Fatal(err)
	}
	root, err := openReplicaFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Join("repo", "p", "x"), 0o700); err == nil {
		t.Error("经过指向外面的链接建目录应该失败")
	}
	if f, err := root.OpenFile(filepath.Join("repo", "p", "evil"), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
		t.Error("经过指向外面的链接写文件应该失败")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("外面的目录不能被写入：%v", entries)
	}
	// 副本的 files 目录本身被换成链接时拒绝。
	other := filepath.Join(tmp, "base", testPID+"-db")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(other, "files")); err != nil {
		t.Fatal(err)
	}
	if _, err := openReplicaFiles(filepath.Join(other, "files")); err == nil {
		t.Error("files 是符号链接时应该拒绝")
	}
}

func TestMoveReplica(t *testing.T) {
	for _, crossFS := range []bool{false, true} {
		name := "同一文件系统"
		if crossFS {
			name = "跨文件系统"
		}
		t.Run(name, func(t *testing.T) {
			if crossFS {
				defer forceCopy(t)()
			}
			r, tmp := testReplicas(t)
			old := addReplica(t, r, "web")
			state := r.stateDir(testPID, "web")
			os.MkdirAll(state, 0o700)
			os.WriteFile(filepath.Join(state, "repository.config"), []byte("old path"), 0o600)
			dir := filepath.Join(tmp, "big")
			if err := r.move(context.Background(), testPID, "web", dir, discard); err != nil {
				t.Fatal(err)
			}
			root, _ := r.root(testPID, "web")
			if root != filepath.Join(dir, testPID+"-web") {
				t.Fatalf("副本应该在 %s，实际 %s", dir, root)
			}
			checkReplicaFiles(t, root, "web")
			if _, err := os.Stat(old); !os.IsNotExist(err) {
				t.Fatal("原位置应该删掉")
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("快照仓库的连接配置记着旧路径，应该删掉")
			}
			list := r.list()
			if len(list) != 1 || !list[0].custom || list[0].root != root {
				t.Fatalf("列表不对：%+v", list)
			}
			// 记录写进了文件：重新打开也找得到。
			again := newReplicas(filepath.Dir(r.dir))
			if got, _ := again.root(testPID, "web"); got != root {
				t.Fatalf("重新打开后副本应该在 %s，实际 %s", root, got)
			}
			// 不留临时目录。
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("存放位置里应该只有这个副本：%v", entries)
			}
			// 移回默认目录。
			if err := r.move(context.Background(), testPID, "web", "", discard); err != nil {
				t.Fatal(err)
			}
			root, _ = r.root(testPID, "web")
			if root != filepath.Join(r.dir, testPID+"-web") {
				t.Fatalf("应该回到默认目录，实际 %s", root)
			}
			checkReplicaFiles(t, root, "web")
			if len(r.idx.Locations) != 0 {
				t.Fatalf("回到默认目录后不用记录位置：%v", r.idx.Locations)
			}
		})
	}
}

// forceCopy 让改名一律返回 EXDEV，走跨文件系统的复制。
func forceCopy(t *testing.T) func() {
	t.Helper()
	prev := renameDir
	renameDir = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EXDEV} }
	return func() { renameDir = prev }
}

func TestMoveReplicaFailureKeepsOld(t *testing.T) {
	defer forceCopy(t)()
	r, tmp := testReplicas(t)
	old := addReplica(t, r, "web")
	// 副本里有符号链接：复制会失败。
	os.Symlink("/etc/passwd", filepath.Join(old, "files", "repo", "link"))
	dir := filepath.Join(tmp, "big")
	err := r.move(context.Background(), testPID, "web", dir, discard)
	if err == nil {
		t.Fatal("应该失败")
	}
	root, _ := r.root(testPID, "web")
	if root != old {
		t.Fatalf("失败时副本应该留在原处，实际 %s", root)
	}
	checkReplicaFiles(t, old, "web")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("失败时不能留下复制了一半的目录：%v", entries)
	}
	if len(r.idx.Locations) != 0 {
		t.Fatalf("失败时不应该记下新位置：%v", r.idx.Locations)
	}
}

func TestMoveReplicaTargetExists(t *testing.T) {
	r, tmp := testReplicas(t)
	old := addReplica(t, r, "web")
	dir := filepath.Join(tmp, "big")
	os.MkdirAll(filepath.Join(dir, testPID+"-web"), 0o700)
	err := r.move(context.Background(), testPID, "web", dir, discard)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("目标位置已有同名目录时应该拒绝：%v", err)
	}
	checkReplicaFiles(t, old, "web")
}

func TestMoveReplicaWaitsForSync(t *testing.T) {
	r, tmp := testReplicas(t)
	addReplica(t, r, "web")
	// 正在同步（ReplicaPut 拿着读锁）。
	unlock := r.rlock(testPID, "web")
	done := make(chan error, 1)
	go func() { done <- r.move(context.Background(), testPID, "web", filepath.Join(tmp, "big"), discard) }()
	select {
	case err := <-done:
		t.Fatalf("同步进行中不应该开始移动：%v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if root, _ := r.root(testPID, "web"); !strings.HasPrefix(root, r.dir) {
		t.Fatal("同步进行中位置不应该变")
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if root, _ := r.root(testPID, "web"); !strings.HasPrefix(root, filepath.Join(tmp, "big")) {
		t.Fatalf("同步结束后应该移过去，实际 %s", root)
	}
}

// 一条同步流一直不结束时（审查 L3），移动等一段时间就放弃，报「同步进行中」，不一直占着节点级的 moving：
// 别的副本照样能移动，被卡住的副本的同步也不受影响。
func TestMoveReplicaGivesUpOnStuckSync(t *testing.T) {
	prev := replicaLockWait
	replicaLockWait = 300 * time.Millisecond
	defer func() { replicaLockWait = prev }()
	r, tmp := testReplicas(t)
	addReplica(t, r, "web")
	addReplica(t, r, "db")
	unlock := r.rlock(testPID, "web") // 被攻破的源节点占着
	defer unlock()
	start := time.Now()
	err := r.move(context.Background(), testPID, "web", filepath.Join(tmp, "big"), discard)
	if status.Code(err) != codes.Aborted || !strings.Contains(err.Error(), "同步进行中") {
		t.Fatalf("应该报同步进行中：%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("等太久了")
	}
	if root, _ := r.root(testPID, "web"); !strings.HasPrefix(root, r.dir) {
		t.Fatal("位置不应该变")
	}
	// 别的副本照样能移动。
	if err := r.move(context.Background(), testPID, "db", filepath.Join(tmp, "big"), discard); err != nil {
		t.Fatal(err)
	}
	// 等的时候没有排队的写锁：同一个副本的同步照样能开始。
	done := make(chan struct{})
	go func() { r.rlock(testPID, "web")(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("同步被挡住了")
	}
	// 添加备份节点时指定位置也一样，不会一直等。
	s := &agentService{node: &agent{replicas: r}}
	_, err = s.AcceptReplica(context.Background(), &nagiv1.AcceptReplicaRequest{
		Spec: &nagiv1.ContainerSpec{ProjectId: testPID, Name: "web"}, SourceKey: make([]byte, 32), ReplicaDir: filepath.Join(tmp, "big"),
	})
	if err == nil || !strings.Contains(err.Error(), "同步进行中") {
		t.Fatalf("应该报同步进行中：%v", err)
	}
}

func TestSetReplicaDefault(t *testing.T) {
	r, tmp := testReplicas(t)
	addReplica(t, r, "a")
	addReplica(t, r, "b")
	custom := filepath.Join(tmp, "custom")
	addReplica(t, r, "c")
	if err := r.move(context.Background(), testPID, "c", custom, discard); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "big")
	if err := r.setDefault(context.Background(), dir, discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		root, _ := r.root(testPID, name)
		if root != filepath.Join(dir, testPID+"-"+name) {
			t.Fatalf("%s 应该移到新的默认目录，实际 %s", name, root)
		}
		checkReplicaFiles(t, root, name)
	}
	if root, _ := r.root(testPID, "c"); root != filepath.Join(custom, testPID+"-c") {
		t.Fatalf("单独指定了位置的副本不应该移动，实际 %s", root)
	}
	if def, setting := r.defaultDir(); def != dir || setting != dir {
		t.Fatalf("默认目录应该是 %s：%s %s", dir, def, setting)
	}
	// 新登记的副本放在新的默认目录。
	addReplica(t, r, "d")
	if root, _ := r.root(testPID, "d"); root != filepath.Join(dir, testPID+"-d") {
		t.Fatalf("新副本应该在新的默认目录，实际 %s", root)
	}
	if got := len(r.list()); got != 4 {
		t.Fatalf("应该有 4 个副本，实际 %d", got)
	}
	// 改回默认的 <节点目录>/replicas。
	if err := r.setDefault(context.Background(), "", discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "d"} {
		root, _ := r.root(testPID, name)
		if root != filepath.Join(r.dir, testPID+"-"+name) {
			t.Fatalf("%s 应该回到 <节点目录>/replicas，实际 %s", name, root)
		}
	}
	if _, setting := r.defaultDir(); setting != "" {
		t.Fatalf("改回默认后设置应该为空：%s", setting)
	}
}

func TestSetReplicaDefaultPartialFailure(t *testing.T) {
	defer forceCopy(t)()
	r, tmp := testReplicas(t)
	addReplica(t, r, "a")
	broken := addReplica(t, r, "b")
	os.Symlink("/etc/passwd", filepath.Join(broken, "files", "link"))
	dir := filepath.Join(tmp, "big")
	err := r.setDefault(context.Background(), dir, discard)
	if err == nil || !strings.Contains(err.Error(), testPID+"-b") {
		t.Fatalf("应该报告 b 没有移动：%v", err)
	}
	if root, _ := r.root(testPID, "a"); root != filepath.Join(dir, testPID+"-a") {
		t.Fatalf("a 应该移过去，实际 %s", root)
	}
	if root, _ := r.root(testPID, "b"); root != broken {
		t.Fatalf("b 移动失败，应该留在原处照常使用，实际 %s", root)
	}
	checkReplicaFiles(t, broken, "b")
	// 修好以后再保存一次，接着移动。
	os.Remove(filepath.Join(broken, "files", "link"))
	if err := r.setDefault(context.Background(), dir, discard); err != nil {
		t.Fatal(err)
	}
	if root, _ := r.root(testPID, "b"); root != filepath.Join(dir, testPID+"-b") {
		t.Fatalf("再次保存后 b 应该移过去，实际 %s", root)
	}
	if len(r.idx.Locations) != 0 {
		t.Fatalf("都在默认目录里了，不用记录位置：%v", r.idx.Locations)
	}
}

func TestCleanStaging(t *testing.T) {
	r, _ := testReplicas(t)
	staging := filepath.Join(r.dir, "."+testPID+"-web"+replicaStagingMark+"abc")
	os.MkdirAll(filepath.Join(staging, "files"), 0o700)
	keep := filepath.Join(r.dir, ".prepull-x")
	os.MkdirAll(keep, 0o700)
	// 往新指定的位置移动时退出：位置还不在 Locations 里，靠 Moving 找到。
	other := filepath.Join(t.TempDir(), "big")
	otherStaging := filepath.Join(other, "."+testPID+"-db"+replicaStagingMark+"def")
	os.MkdirAll(otherStaging, 0o700)
	if err := r.markMoving(other, true); err != nil {
		t.Fatal(err)
	}
	again := newReplicas(filepath.Dir(r.dir))
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatal("中途退出留下的临时目录应该删掉")
	}
	if _, err := os.Stat(otherStaging); !os.IsNotExist(err) {
		t.Fatal("新位置里复制了一半的临时目录应该删掉")
	}
	if len(again.idx.Moving) != 0 {
		t.Fatal("清理以后不用再记")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("其他目录不能删")
	}
}

// replica-dirs.json 损坏时（审查 L2）：原文件改名留着，不当成空的覆盖；修改存放位置、登记新的副本都拒绝。
func TestCorruptReplicaIndex(t *testing.T) {
	r, tmp := testReplicas(t)
	addReplica(t, r, "web")
	custom := filepath.Join(tmp, "custom")
	if err := r.move(context.Background(), testPID, "web", custom, discard); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(r.file)
	if err != nil {
		t.Fatal(err)
	}
	// 断电截断了文件。
	if err := os.WriteFile(r.file, good[:len(good)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	broken := newReplicas(filepath.Dir(r.dir))
	broken.forbidden, broken.reserved, broken.stateDir = r.forbidden, r.reserved, r.stateDir
	if broken.usable() == nil {
		t.Fatal("损坏的记录应该被发现")
	}
	matches, _ := filepath.Glob(r.file + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("原文件应该改名留着：%v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != string(good[:len(good)/2]) {
		t.Fatal("留下的应该是原来的内容")
	}
	if err := broken.move(context.Background(), testPID, "web", "", discard); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("记录损坏时不能移动副本：%v", err)
	}
	if err := broken.setDefault(context.Background(), filepath.Join(tmp, "big"), discard); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("记录损坏时不能修改默认目录：%v", err)
	}
	s := &agentService{node: &agent{replicas: broken}}
	if _, err := s.AcceptReplica(context.Background(), &nagiv1.AcceptReplicaRequest{
		Spec: &nagiv1.ContainerSpec{ProjectId: testPID, Name: "web"}, SourceKey: make([]byte, 32),
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("记录损坏时不能在默认目录新建副本：%v", err)
	}
	broken.forget(testPID + "-web")
	if _, err := os.Stat(r.file); !os.IsNotExist(err) {
		t.Fatal("记录损坏时不能写入新的记录")
	}
	// 修好放回去，重启后照常。
	if err := os.Rename(matches[0], r.file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.file, good, 0o600); err != nil {
		t.Fatal(err)
	}
	fixed := newReplicas(filepath.Dir(r.dir))
	if fixed.usable() != nil {
		t.Fatal("修好以后应该可以用")
	}
	if root, _ := fixed.root(testPID, "web"); root != filepath.Join(custom, testPID+"-web") {
		t.Fatalf("应该找到原来的位置，实际 %s", root)
	}
}

func TestAcceptReplicaDir(t *testing.T) {
	r, tmp := testReplicas(t)
	s := &agentService{node: &agent{replicas: r}}
	key := make([]byte, 32)
	accept := func(name, dir string) error {
		_, err := s.AcceptReplica(context.Background(), &nagiv1.AcceptReplicaRequest{
			Spec: &nagiv1.ContainerSpec{ProjectId: testPID, Name: name}, SourceKey: key, ReplicaDir: dir,
		})
		return err
	}
	dir := filepath.Join(tmp, "big")
	if err := accept("web", dir); err != nil {
		t.Fatal(err)
	}
	if err := accept("db", dir); err != nil {
		t.Fatal(err)
	}
	// 每个容器一个子目录，不会撞在一起。
	for _, name := range []string{"web", "db"} {
		root, _ := r.root(testPID, name)
		if root != filepath.Join(dir, testPID+"-"+name) {
			t.Fatalf("%s 应该在 %s 下面，实际 %s", name, dir, root)
		}
		if _, err := os.Stat(filepath.Join(root, "meta.json")); err != nil {
			t.Fatal(err)
		}
	}
	// 再登记一次（例如更新源节点），不带位置：留在原处。
	if err := accept("web", ""); err != nil {
		t.Fatal(err)
	}
	if root, _ := r.root(testPID, "web"); !strings.HasPrefix(root, dir) {
		t.Fatalf("不带位置的再次登记不应该移动副本，实际 %s", root)
	}
	// 不能放在另一个副本的目录里。
	webRoot, _ := r.root(testPID, "web")
	if err := accept("api", filepath.Join(webRoot, "files")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("应该拒绝放在另一个副本的目录里：%v", err)
	}
	// 删除副本时位置记录一起删掉，目录里的其他副本不受影响。
	if _, err := s.DeleteReplica(context.Background(), &nagiv1.ContainerRef{ProjectId: testPID, Container: "web"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.idx.Locations[testPID+"-web"]; ok {
		t.Fatal("删除副本后位置记录应该删掉")
	}
	if _, err := os.Stat(webRoot); !os.IsNotExist(err) {
		t.Fatal("副本目录应该删掉")
	}
	if dbRoot, _ := r.root(testPID, "db"); dbRoot != filepath.Join(dir, testPID+"-db") {
		t.Fatal("别的副本不受影响")
	} else if _, err := os.Stat(filepath.Join(dbRoot, "meta.json")); err != nil {
		t.Fatal("别的副本不受影响")
	}
	// 统计和列表找得到不在默认目录里的副本。
	if !slices.ContainsFunc(r.usageDirs(), func(d string) bool { return strings.HasPrefix(d, dir) }) {
		t.Fatalf("统计占用时要算上 %s：%v", dir, r.usageDirs())
	}
}

// 添加备份节点时指定了存放位置，节点上却已有这个容器的旧副本：先把它移过去（同样的移动和核对），再更新登记；
// 移动失败时副本和登记都不变。移动等正在进行的同步结束。
func TestAcceptReplicaMovesExisting(t *testing.T) {
	for _, crossFS := range []bool{false, true} {
		t.Run(map[bool]string{false: "同一文件系统", true: "跨文件系统"}[crossFS], func(t *testing.T) {
			if crossFS {
				defer forceCopy(t)()
			}
			r, tmp := testReplicas(t)
			s := &agentService{node: &agent{replicas: r}}
			old := addReplica(t, r, "web")
			unlock := r.rlock(testPID, "web") // 正在同步
			dir := filepath.Join(tmp, "big")
			done := make(chan error, 1)
			go func() {
				_, err := s.AcceptReplica(context.Background(), &nagiv1.AcceptReplicaRequest{
					Spec: &nagiv1.ContainerSpec{ProjectId: testPID, Name: "web"}, SourceKey: make([]byte, 32), SourceName: "new-source", ReplicaDir: dir,
				})
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("同步进行中不应该开始移动：%v", err)
			case <-time.After(300 * time.Millisecond):
			}
			unlock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			root, _ := r.root(testPID, "web")
			if root != filepath.Join(dir, testPID+"-web") {
				t.Fatalf("旧副本应该移到 %s，实际 %s", dir, root)
			}
			checkReplicaFiles(t, root, "web")
			if _, err := os.Stat(old); !os.IsNotExist(err) {
				t.Fatal("原位置应该删掉")
			}
			m, _, _, err := r.load(testPID, "web")
			if err != nil || m.SourceName != "new-source" {
				t.Fatalf("登记应该更新：%v %+v", err, m)
			}
			if l := r.idx.Locations[testPID+"-web"]; !l.Custom || l.Base != dir {
				t.Fatalf("应该记成单独指定的位置：%+v", l)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("不能留下临时目录：%v", entries)
			}
		})
	}
	t.Run("移动失败", func(t *testing.T) {
		defer forceCopy(t)()
		r, tmp := testReplicas(t)
		s := &agentService{node: &agent{replicas: r}}
		old := addReplica(t, r, "web")
		os.Symlink("/etc/passwd", filepath.Join(old, "files", "link"))
		dir := filepath.Join(tmp, "big")
		_, err := s.AcceptReplica(context.Background(), &nagiv1.AcceptReplicaRequest{
			Spec: &nagiv1.ContainerSpec{ProjectId: testPID, Name: "web"}, SourceKey: make([]byte, 32), SourceName: "new-source", ReplicaDir: dir,
		})
		if err == nil || !strings.Contains(err.Error(), "仍在原处") {
			t.Fatalf("应该报告移动失败：%v", err)
		}
		if root, _ := r.root(testPID, "web"); root != old {
			t.Fatalf("失败时副本留在原处，实际 %s", root)
		}
		checkReplicaFiles(t, old, "web")
		if m, _, _, _ := r.load(testPID, "web"); m.SourceName == "new-source" {
			t.Fatal("移动失败时不应该更新登记")
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("不能留下复制了一半的目录：%v", entries)
		}
	})
}

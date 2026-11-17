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
	// 已有副本、指定了另一个位置：拒绝，要移动用 MoveReplica。
	if err := accept("web", filepath.Join(tmp, "other")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("应该拒绝：%v", err)
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

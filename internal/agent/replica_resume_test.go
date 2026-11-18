package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/project"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// recvTestServer 用真正的接收逻辑（receiveReplica）把文件写进 files 目录，不核对调用方。
type recvTestServer struct {
	nagiv1.UnimplementedMeshServiceServer
	files string
}

func (s *recvTestServer) ReplicaPut(stream nagiv1.MeshService_ReplicaPutServer) error {
	return receiveReplica(stream, func(string, string) (string, func(), error) { return s.files, func() {}, nil })
}

func startRecvServer(t *testing.T) (string, func() (*grpc.ClientConn, error)) {
	t.Helper()
	files := filepath.Join(t.TempDir(), "files")
	if err := os.MkdirAll(filepath.Join(files, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(4 << 20)
	srv := grpc.NewServer()
	nagiv1.RegisterMeshServiceServer(srv, &recvTestServer{files: files})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return files, func() (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	}
}

// waitFor 等到 cond 成立（接收端在流断开后异步收尾）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("等不到：" + what)
}

// 流断开时传了一半的文件留着、清单里报出来；下次从一半处接着传，内容完整，只传剩下的部分。
func TestReplicaResumePartial(t *testing.T) {
	files, dial := startRecvServer(t)
	data := make([]byte, 5<<20)
	_, _ = rand.Read(data)
	src := filepath.Join(t.TempDir(), "pack")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	const path = "repo/p/012/3abc.f"

	// 第一次只发前 3 MiB 就断开。
	cc, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	put, err := nagiv1.NewMeshServiceClient(cc).ReplicaPut(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < 3<<20; off += 256 << 10 {
		chunk := &nagiv1.ReplicaChunk{Path: path, Data: data[off : off+256<<10]}
		if off == 0 {
			chunk.ProjectId, chunk.Container = "p", "c"
		}
		if err := put.Send(chunk); err != nil {
			t.Fatal(err)
		}
	}
	// 等对方收完再断开，断开时收到多少都留着。
	waitFor(t, "收到 3 MiB", func() bool {
		m, _ := filepath.Glob(filepath.Join(files, "repo", "p", "012", "3abc.f"+replicaTempMark+"-*"))
		if len(m) != 1 {
			return false
		}
		info, err := os.Stat(m[0])
		return err == nil && info.Size() == 3<<20
	})
	cancel()
	cc.Close()
	var partial int64
	waitFor(t, "留下传了一半的文件", func() bool {
		man := replicaManifest(files)
		if len(man.GetPartials()) != 1 || len(man.GetFiles()) != 0 {
			return false
		}
		partial = man.GetPartials()[0].GetSize()
		return man.GetPartials()[0].GetPath() == path
	})
	if partial != 3<<20 {
		t.Fatalf("传了一半的大小 %d", partial)
	}

	// 第二次按清单接着传。
	_, packs, others, need := planSync([]project.ReplicaFile{{Path: path, Abs: src, Size: int64(len(data)), Immutable: true, Pack: true}},
		map[string]int64{}, map[string]int64{path: partial})
	if len(packs) != 1 || len(others) != 0 || packs[0].offset != partial || need != int64(len(data))-partial {
		t.Fatalf("计划：%+v need=%d", packs, need)
	}
	first, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	var sent int64
	done, err := sendReplicaFiles(context.Background(), dial, first, "p", "c", packs, func(n int64) { sent += n })
	if err != nil || len(done) != 1 {
		t.Fatalf("接着传：%v %v", done, err)
	}
	if sent != need {
		t.Errorf("只应传剩下的 %d 字节，传了 %d", need, sent)
	}
	got, err := os.ReadFile(filepath.Join(files, filepath.FromSlash(path)))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("内容不对：%d 字节，%v", len(got), err)
	}
	if man := replicaManifest(files); len(man.GetPartials()) != 0 || len(man.GetFiles()) != 1 {
		t.Fatalf("传完后清单：%v", man)
	}
}

// 接着传时前一半对不上（例如断电后内容坏了）：对方丢掉它、返回 DataLoss，下次从头传。传了一半的文件不在了也一样。
func TestReplicaResumeMismatch(t *testing.T) {
	files, dial := startRecvServer(t)
	const path = "repo/p/012/3abc.f"
	dest := filepath.Join(files, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+replicaPartSuffix, bytes.Repeat([]byte{1}, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	send := func(prefix []byte) error {
		cc, err := dial()
		if err != nil {
			t.Fatal(err)
		}
		defer cc.Close()
		put, err := nagiv1.NewMeshServiceClient(cc).ReplicaPut(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := put.Send(&nagiv1.ReplicaChunk{ProjectId: "p", Container: "c", Path: path, Offset: 1 << 20, PrefixSha256: prefix, Data: []byte("rest")}); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		_, err = put.CloseAndRecv()
		return err
	}
	wrong := sha256.Sum256(bytes.Repeat([]byte{2}, 1<<20))
	if err := send(wrong[:]); status.Code(err) != codes.DataLoss {
		t.Fatalf("对不上应返回 DataLoss：%v", err)
	}
	if man := replicaManifest(files); len(man.GetPartials()) != 0 || len(man.GetFiles()) != 0 {
		t.Fatalf("对不上的部分应删掉：%v", man)
	}
	right := sha256.Sum256(bytes.Repeat([]byte{1}, 1<<20))
	if err := send(right[:]); status.Code(err) != codes.DataLoss {
		t.Fatalf("不在了应返回 DataLoss：%v", err)
	}
	// 没有校验值的接着传不接受。
	if err := os.WriteFile(dest+replicaPartSuffix, bytes.Repeat([]byte{1}, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := send(nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("缺少校验值应拒绝：%v", err)
	}
	// 对得上：多出来的截掉，接上新的内容。
	if err := send(right[:]); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if want := append(bytes.Repeat([]byte{1}, 1<<20), "rest"...); !bytes.Equal(got, want) {
		t.Fatalf("接上以后 %d 字节", len(got))
	}
}

// 计划：对方有的不变文件跳过；kopia.* 每次都传、不接着传；传了一半的不变文件从一半处接着传。
func TestPlanSync(t *testing.T) {
	files := []project.ReplicaFile{
		{Path: "repo/p/1", Size: 100, Immutable: true, Pack: true},
		{Path: "repo/p/2", Size: 100, Immutable: true, Pack: true},
		{Path: "repo/x/1", Size: 10, Immutable: true},
		{Path: "repo/kopia.repository.f", Size: 5},
		{Path: "repo/p/3", Size: 100, Immutable: true, Pack: true},
	}
	remote := map[string]int64{"repo/p/1": 100, "repo/kopia.repository.f": 5, "repo/p/3": 50}
	partial := map[string]int64{"repo/p/2": 40, "repo/kopia.repository.f": 3}
	keep, packs, others, need := planSync(files, remote, partial)
	if len(keep) != 1 || keep[0] != "repo/p/1" {
		t.Errorf("跳过的：%v", keep)
	}
	if len(packs) != 2 || packs[0].Path != "repo/p/2" || packs[0].offset != 40 || packs[1].offset != 0 {
		t.Errorf("数据包：%+v", packs)
	}
	if len(others) != 2 || others[1].offset != 0 {
		t.Errorf("其他：%+v", others)
	}
	if need != 60+10+5+100 {
		t.Errorf("need = %d", need)
	}
}

// 进度：重试时按还要传的重新算已传的；速度按最近的窗口算，已传的变少时从头算。
func TestSyncProgress(t *testing.T) {
	p := &syncProgress{}
	now := time.Unix(1000, 0)
	p.begin(now)
	p.plan(1000)
	p.sample(now)
	p.add(300)
	p.sample(now.Add(10 * time.Second))
	st := &nagiv1.BackupStatus{}
	p.fill(st)
	if st.BytesTotal != 1000 || st.BytesDone != 300 || st.BytesPerSecond != 30 {
		t.Fatalf("进度：%v", st)
	}
	// 断开重连：还要传 800（断开时那个文件没传完的 100 不算），已传按 200 算。
	p.retried(status.Error(codes.Unavailable, "lost"))
	p.plan(800)
	p.sample(now.Add(11 * time.Second))
	st = &nagiv1.BackupStatus{}
	p.fill(st)
	if st.BytesDone != 200 || st.BytesPerSecond != 0 || st.Retries != 1 || st.RetryError == "" {
		t.Fatalf("重试后：%v", st)
	}
	// 窗口外的样本丢掉，速度只看最近的。
	p.add(100)
	p.sample(now.Add(12 * time.Second))
	p.add(2000)
	p.sample(now.Add(40 * time.Second))
	st = &nagiv1.BackupStatus{}
	p.fill(st)
	if st.BytesPerSecond != 2000/28 || st.BytesDone != st.BytesTotal {
		t.Fatalf("速度只看最近的窗口，已传不超过总量：%v", st)
	}
	p.plan(2300) // 新的一次连接要传的比总量多，总量跟着变
	if p.total.Load() != 2300 || p.done.Load() != 0 {
		t.Fatalf("plan：%d %d", p.total.Load(), p.done.Load())
	}
	// 还没开始（排队）时不报进度。
	q := &syncProgress{}
	q.plan(10)
	st = &nagiv1.BackupStatus{}
	q.fill(st)
	if st.BytesTotal != 0 || st.StartedAt != nil {
		t.Fatalf("排队时：%v", st)
	}
}

// 同一个容器到同一个备份节点只有一次同步：已经在同步时标记再来一次；停止时取消上下文、不再来一次。
func TestBackupClaimStop(t *testing.T) {
	b := newBackupStates(filepath.Join(t.TempDir(), "backup-status.json"))
	run := b.claim(context.Background(), "p-c", "n1")
	if run == nil {
		t.Fatal("应该登记")
	}
	if again := b.claim(context.Background(), "p-c", "n1"); again != nil || !run.again {
		t.Fatal("在同步时应标记再来一次")
	}
	if other := b.claim(context.Background(), "p-c", "n2"); other == nil {
		t.Fatal("别的备份节点互不影响")
	}
	if b.running("p-c", "n1") != run {
		t.Fatal("running")
	}
	if !b.stop("p-c", "n1") || run.again || run.ctx.Err() == nil || context.Cause(run.ctx) != errSyncStopped {
		t.Fatal("停止应取消上下文、不再来一次")
	}
	if b.stop("p-c", "n3") {
		t.Fatal("没有同步时停止什么都不做")
	}
	if !b.tick(time.Now()) {
		t.Fatal("有同步在进行")
	}
}

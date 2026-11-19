//go:build replicabench

package agent

// 副本同步的实测（不在平时的测试里跑）：同一个进程里两个 mesh 成员经 127.0.0.1 直连，源节点用真正的发送逻辑
// （resumeSync、transferReplica、sendReplicaFiles），备份节点用真正的接收逻辑（receiveReplica、replicaManifest），
// 线路的延迟、丢包、限速用 tc netem 加在容器的 lo 上。例如：
//
//	tc qdisc add dev lo root netem delay 20ms loss 1% rate 100mbit
//	REPLICA_BENCH_MB=300 REPLICA_BENCH_STREAMS=4 REPLICA_BENCH_QUIC=0 go test -tags replicabench -run ReplicaBench -v ./internal/agent/
//
// REPLICA_BENCH_CUT 为秒数时，每隔这么久断开一次所有连接（模拟线路断开），看自动重试、从一半接着传。

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type benchServer struct {
	nagiv1.UnimplementedMeshServiceServer
	files string
}

// REPLICA_BENCH_NORESUME=1 时不报传了一半的文件，和旧版本的备份节点一样从头传。
func (s *benchServer) ReplicaManifest(context.Context, *nagiv1.ReplicaManifestRequest) (*nagiv1.ReplicaManifestResponse, error) {
	m := replicaManifest(s.files)
	if os.Getenv("REPLICA_BENCH_NORESUME") == "1" {
		m.Partials = nil
	}
	return m, nil
}

func (s *benchServer) ReplicaPut(stream nagiv1.MeshService_ReplicaPutServer) error {
	return receiveReplica(stream, func(string, string) (string, *metrics.DiskGuard, func(), error) { return s.files, nil, func() {}, nil })
}

func (s *benchServer) ReplicaCommit(context.Context, *nagiv1.ReplicaCommitRequest) (*nagiv1.ReplicaCommitResponse, error) {
	return &nagiv1.ReplicaCommitResponse{}, nil
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

// cutConn 记下所有连接，cut 时全部断开。
type cutConns struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (c *cutConns) add(conn net.Conn) net.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conns = append(c.conns, conn)
	return conn
}

func (c *cutConns) cut() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.conns {
		conn.Close()
	}
	c.conns = nil
}

func TestReplicaBench(t *testing.T) {
	sizeMB := envInt("REPLICA_BENCH_MB", 200)
	syncStreams = envInt("REPLICA_BENCH_STREAMS", 4)
	quic := envInt("REPLICA_BENCH_QUIC", 0) == 1
	cutEvery := time.Duration(envInt("REPLICA_BENCH_CUT", 0)) * time.Second
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var pa, pb mesh.Key
	_, _ = rand.Read(pa[:])
	_, _ = rand.Read(pb[:])
	ma, err := mesh.New(mesh.Config{PrivateKey: pa, NoQUIC: !quic, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	defer ma.Close()
	mb, err := mesh.New(mesh.Config{PrivateKey: pb, NoQUIC: !quic, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	local := func(port uint16) []netip.AddrPort {
		return []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)}
	}
	_ = ma.SetPeers([]mesh.Peer{{Key: mb.PublicKey(), Endpoints: local(mb.Port())}})
	_ = mb.SetPeers([]mesh.Peer{{Key: ma.PublicKey(), Endpoints: local(ma.Port())}})

	files := filepath.Join(t.TempDir(), "files")
	_ = os.MkdirAll(filepath.Join(files, "repo"), 0o700)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mesh.Serve(ctx, mb, &benchServer{files: files})

	// 等直连（和 QUIC）建好。
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		ok := false
		for _, s := range ma.Status() {
			if s.Key == mb.PublicKey() && s.Path == "direct" && (!quic || s.QUIC) {
				ok = true
			}
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("没有建好直连")
		}
	}

	// 源文件：像 kopia 的数据包，每个约 20 MiB，随机内容（加密过的数据包压缩不了）。
	src := t.TempDir()
	var list []project.ReplicaFile
	buf := make([]byte, 20<<20)
	for i, left := 0, sizeMB<<20; left > 0; i++ {
		n := min(left, len(buf))
		_, _ = rand.Read(buf[:n])
		abs := filepath.Join(src, fmt.Sprint(i))
		if err := os.WriteFile(abs, buf[:n], 0o600); err != nil {
			t.Fatal(err)
		}
		list = append(list, project.ReplicaFile{Path: fmt.Sprintf("repo/p/%03d/%04d.f", i%7, i), Abs: abs, Size: int64(n), Immutable: true, Pack: true})
		left -= n
	}

	var conns cutConns
	dial := func() (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bench",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithInitialWindowSize(16<<20), grpc.WithInitialConnWindowSize(16<<20),
			grpc.WithReadBufferSize(256<<10), grpc.WithWriteBufferSize(256<<10),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				c, err := ma.Dial(ctx, mb.PublicKey(), mesh.ServicePort)
				if err != nil {
					return nil, err
				}
				return conns.add(c), nil
			}))
	}
	syncRetryDelay = time.Second
	p := &syncProgress{}
	p.begin(time.Now())
	var cuts, retries atomic.Int32
	if cutEvery > 0 {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(cutEvery):
					cuts.Add(1)
					conns.cut()
				}
			}
		}()
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-time.After(5 * time.Second):
				p.sample(now)
				st := &nagiv1.BackupStatus{}
				p.fill(st)
				t.Logf("进度 %d / %d MiB，%.1f MiB/s，重试 %d", st.BytesDone>>20, st.BytesTotal>>20, float64(st.BytesPerSecond)/(1<<20), st.Retries)
			}
		}
	}()
	start := time.Now()
	sent, err := resumeSync(ctx, func() (int64, int64, error) {
		return transferReplica(ctx, dial, "p", "c", "b", func() ([]project.ReplicaFile, error) { return list, nil }, p)
	}, func(attempt int, n int64, err error) {
		retries.Add(1)
		p.retried(err)
		t.Logf("第 %d 次断开（这次传了 %d MiB）：%v", attempt, n>>20, err)
	})
	elapsed := time.Since(start)
	cancel()
	if err != nil {
		t.Fatalf("同步失败：%v", err)
	}
	// 核对内容。
	for _, f := range list {
		a, _ := os.ReadFile(f.Abs)
		b, err := os.ReadFile(filepath.Join(files, filepath.FromSlash(f.Path)))
		if err != nil || string(a) != string(b) {
			t.Fatalf("%s 内容不对", f.Path)
		}
	}
	total := int64(sizeMB) << 20
	t.Logf("结果：%d MiB，%d 条流，QUIC=%v，用时 %.1f 秒，%.2f MiB/s，发送 %d MiB（多发 %.1f%%），断开 %d 次，重试 %d 次",
		sizeMB, syncStreams, quic, elapsed.Seconds(), float64(total)/(1<<20)/elapsed.Seconds(), sent>>20, 100*float64(sent-total)/float64(total), cuts.Load(), retries.Load())
}

package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SIPC/nagi/internal/agent/project"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// recvServer 记下每个 ReplicaPut 流收到的文件，和同时有几个流在收。
type recvServer struct {
	nagiv1.UnimplementedMeshServiceServer
	mu       sync.Mutex
	got      map[string][]byte
	active   atomic.Int32
	peak     atomic.Int32
	firstOK  atomic.Int32
	failNext atomic.Bool
}

func (s *recvServer) ReplicaPut(stream nagiv1.MeshService_ReplicaPutServer) error {
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for p := s.peak.Load(); n > p && !s.peak.CompareAndSwap(p, n); p = s.peak.Load() {
	}
	first := true
	for {
		c, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&nagiv1.ReplicaPutResponse{})
		}
		if err != nil {
			return err
		}
		// 每条流的第一块要带上容器，对方据此核对权限。
		if first {
			if c.GetProjectId() == "p" && c.GetContainer() == "c" {
				s.firstOK.Add(1)
			}
			first = false
		}
		s.mu.Lock()
		s.got[c.GetPath()] = append(s.got[c.GetPath()], c.GetData()...)
		s.mu.Unlock()
	}
}

func TestSendReplicaFilesParallel(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	rs := &recvServer{got: map[string][]byte{}}
	nagiv1.RegisterMeshServiceServer(srv, rs)
	go srv.Serve(lis)
	defer srv.Stop()
	dial := func() (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	}
	first, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	dir := t.TempDir()
	var files []project.ReplicaFile
	want := map[string][]byte{}
	for i := range 12 {
		data := make([]byte, 100<<10*(i%4)) // 有空文件，也有超过一块（256 KiB）的
		_, _ = rand.Read(data)
		abs := filepath.Join(dir, fmt.Sprint(i))
		if err := os.WriteFile(abs, data, 0o600); err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("repo/p%02d", i)
		files = append(files, project.ReplicaFile{Path: path, Abs: abs, Size: int64(len(data))})
		want[path] = data
	}
	// 维护时被删掉的文件跳过，不算传完。
	files = append(files, project.ReplicaFile{Path: "repo/gone", Abs: filepath.Join(dir, "gone")})

	var sent atomic.Int64
	done, err := sendReplicaFiles(context.Background(), dial, first, "p", "c", files, &sent)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(done)
	if len(done) != 12 || slices.Contains(done, "repo/gone") {
		t.Fatalf("传完的文件：%v", done)
	}
	var total int64
	for path, data := range want {
		if string(rs.got[path]) != string(data) {
			t.Errorf("%s 内容不对：收到 %d 字节，应为 %d", path, len(rs.got[path]), len(data))
		}
		total += int64(len(data))
	}
	if sent.Load() != total {
		t.Errorf("sent = %d，应为 %d", sent.Load(), total)
	}
	if rs.peak.Load() < 2 {
		t.Errorf("应该并行传，最多同时 %d 条流", rs.peak.Load())
	}
	if rs.firstOK.Load() != rs.peak.Load() && rs.firstOK.Load() < 2 {
		t.Errorf("每条流的第一块都要带上容器：%d", rs.firstOK.Load())
	}

	// 没有文件时不开流。
	if done, err := sendReplicaFiles(context.Background(), dial, first, "p", "c", nil, &sent); err != nil || done != nil {
		t.Fatalf("空列表：%v %v", done, err)
	}
}

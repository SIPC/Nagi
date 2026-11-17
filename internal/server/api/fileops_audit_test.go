package api

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
)

// fakeFileOps 模拟节点：没有指定同名处理方式、目标又已经存在时只返回同名的名字。
type fakeFileOps struct {
	nagiv1connect.UnimplementedProjectServiceHandler
}

func probeResult(conflict string) *nagiv1.FileOpResult {
	if conflict == "" {
		return &nagiv1.FileOpResult{Conflicts: []string{"a.txt"}}
	}
	return &nagiv1.FileOpResult{Paths: []string{"dir/a.txt"}}
}

func (fakeFileOps) CopyVolumeFiles(_ context.Context, req *connect.Request[nagiv1.VolumeTransferRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	if err := out.Send(&nagiv1.FileOpEvent{Bytes: 1}); err != nil {
		return err
	}
	return out.Send(&nagiv1.FileOpEvent{Result: probeResult(req.Msg.GetConflict())})
}

func (fakeFileOps) RenameVolumeFile(_ context.Context, req *connect.Request[nagiv1.VolumeTransferRequest]) (*connect.Response[nagiv1.FileOpResult], error) {
	return connect.NewResponse(probeResult(req.Msg.GetConflict())), nil
}

// 同名探测（只返回同名的名字、什么都没改）不记审计，带着处理方式真正做的那一次照常记。
func TestConflictProbeNotAudited(t *testing.T) {
	ctx := context.Background()
	l, err := audit.Open(ctx, filepath.Join(t.TempDir(), "audit.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a := &API{Audit: l}
	_, h := nagiv1connect.NewProjectServiceHandler(fakeFileOps{}, connect.WithInterceptors(activityLog{a}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := nagiv1connect.NewProjectServiceClient(srv.Client(), srv.URL)
	count := func(action string) int {
		var n int
		_ = l.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE action = ?`, action).Scan(&n)
		return n
	}
	drain := func(conflict string) {
		stream, err := client.CopyVolumeFiles(ctx, connect.NewRequest(&nagiv1.VolumeTransferRequest{ProjectId: "p1", Container: "web", Paths: []string{"a.txt"}, DestDir: "dir", Conflict: conflict}))
		if err != nil {
			t.Fatal(err)
		}
		for stream.Receive() {
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
	}
	drain("")
	if n := count("CopyVolumeFiles"); n != 0 {
		t.Fatalf("同名探测记了 %d 条", n)
	}
	drain("rename")
	if n := count("CopyVolumeFiles"); n != 1 {
		t.Fatalf("真正的复制应该记 1 条，记了 %d 条", n)
	}
	rename := func(name, conflict string) {
		if _, err := client.RenameVolumeFile(ctx, connect.NewRequest(&nagiv1.VolumeTransferRequest{ProjectId: "p1", Container: "web", Paths: []string{"b.txt"}, NewName: name, Conflict: conflict})); err != nil {
			t.Fatal(err)
		}
	}
	rename("a.txt", "") // 同名：不记
	if n := count("RenameVolumeFile"); n != 0 {
		t.Fatalf("同名的改名记了 %d 条", n)
	}
	rename("c.txt", "skip")
	if n := count("RenameVolumeFile"); n != 1 {
		t.Fatalf("改名应该记 1 条，记了 %d 条", n)
	}
}

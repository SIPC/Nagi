package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
)

func TestReplicaReceptionIsNotCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	newMesh := func() *mesh.Mesh {
		m, err := mesh.New(mesh.Config{PrivateKey: randKey(), Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return m
	}
	src, dst := newMesh(), newMesh()
	endpoint := func(m *mesh.Mesh) []netip.AddrPort {
		return []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), m.Port())}
	}
	if err := src.SetPeers([]mesh.Peer{{Key: dst.PublicKey(), Endpoints: endpoint(dst)}}); err != nil {
		t.Fatal(err)
	}
	if err := dst.SetPeers([]mesh.Peer{{Key: src.PublicKey(), Endpoints: endpoint(src)}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "logs.db")
	logs, err := logstore.Open(ctx, path, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	a := &agent{cfg: Config{Logs: logs}, replicas: newReplicas(t.TempDir()), failovers: newFailovers(t.TempDir())}
	key := src.PublicKey()
	if err := a.replicas.save(&replicaMeta{ProjectID: lockTestPID, Container: "web", SourceKey: key[:]}, &nagiv1.ContainerSpec{}); err != nil {
		t.Fatal(err)
	}
	svc := &meshService{Service: mesh.Service{M: dst}, a: a}
	done := make(chan error, 1)
	go func() { done <- mesh.Serve(ctx, dst, svc) }()
	defer func() { cancel(); <-done }()
	cc, err := mesh.Client(src, dst.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	client := nagiv1.NewMeshServiceClient(cc)
	put, err := client.ReplicaPut(ctx, grpc.WaitForReady(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := put.Send(&nagiv1.ReplicaChunk{ProjectId: lockTestPID, Container: "web", Path: "repo/pack", Data: []byte("sensitive file body")}); err != nil {
		t.Fatal(err)
	}
	if r, err := put.CloseAndRecv(); err != nil || r.GetFiles() != 1 {
		t.Fatalf("receive: %v %v", r, err)
	}
	waitFor(t, "接收结果", func() bool {
		rows, _ := logs.Query(ctx, logstore.Filter{Category: logstore.Task})
		return len(rows) == 2
	})
	rows, err := logs.Query(ctx, logstore.Filter{Category: logstore.Task})
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Phase, Result string
		Received      int64 `json:"received_bytes"`
		Files         int
	}
	if err := json.Unmarshal(rows[1].Data.(json.RawMessage), &data); err != nil || rows[1].Kind != "replica.receive" || data.Result != "files_received" || data.Received != 19 || data.Files != 1 {
		t.Fatalf("收文件不是完成提交: %+v %v", rows, err)
	}
	_, _, root, err := a.replicas.load(lockTestPID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "files", "repo", "pack")); err != nil || string(b) != "sensitive file body" {
		t.Fatalf("body: %s %v", b, err)
	}
	if _, err := client.ReplicaCommit(ctx, &nagiv1.ReplicaCommitRequest{ProjectId: lockTestPID, Container: "web", Paths: []string{"repo/pack"}}); err != nil {
		t.Fatal(err)
	}
	rows, err = logs.Query(ctx, logstore.Filter{Category: logstore.Task})
	if err != nil || len(rows) != 3 || rows[2].Kind != "replica.commit" {
		t.Fatalf("提交结果: %+v %v", rows, err)
	}
	if err := json.Unmarshal(rows[2].Data.(json.RawMessage), &data); err != nil || data.Result != "committed" {
		t.Fatalf("commit: %s %v", rows[2].Data, err)
	}
}

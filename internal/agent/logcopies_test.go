package agent

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type lostLogAck struct {
	nagiv1.LogCopyServiceClient
	lost bool
}

func (c *lostLogAck) PushLogs(ctx context.Context, b *nagiv1.LogCopyBatch, opts ...grpc.CallOption) (*nagiv1.LogReceipt, error) {
	r, err := c.LogCopyServiceClient.PushLogs(ctx, b, opts...)
	if err == nil && !c.lost {
		c.lost = true
		return nil, status.Error(codes.DeadlineExceeded, "lost after commit")
	}
	return r, err
}

func TestRecentLogCopiesOverAuthenticatedMesh(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
	node, panel := newMesh(), newMesh()
	ep := func(m *mesh.Mesh) []netip.AddrPort {
		return []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), m.Port())}
	}
	if err := node.SetPeers([]mesh.Peer{{Key: panel.PublicKey(), Endpoints: ep(panel)}}); err != nil {
		t.Fatal(err)
	}
	if err := panel.SetPeers([]mesh.Peer{{Key: node.PublicKey(), Endpoints: ep(node)}}); err != nil {
		t.Fatal(err)
	}
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q := store.New(d)
	key := node.PublicKey()
	if err := q.CreateNode(ctx, store.CreateNodeParams{ID: "actual-node", Name: "n", MeshKey: key[:]}); err != nil {
		t.Fatal(err)
	}
	r := nodes.NewRegistry(q, logger, nil, nil)
	panelLogs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer panelLogs.Close()
	copies, err := panelLogs.OpenCopies(ctx, logstore.Config{RecentCapacityMiB: 16, RecentHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer copies.Close()
	r.LogCopies = copies
	r.SetMesh(panel, "panel")
	done := make(chan struct{})
	go func() { defer close(done); r.ServeMesh(ctx) }()
	defer func() { cancel(); <-done }()
	cc, err := mesh.Client(node, panel.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	client := nagiv1.NewLogCopyServiceClient(cc)
	// First call waits for service startup; no TLS/control fallback exists.
	if _, err := client.GetLogReceipt(ctx, &nagiv1.LogReceiptQuery{Source: "s", Category: logstore.Audit}, grpc.WaitForReady(true)); err != nil {
		t.Fatal(err)
	}
	local, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	for _, category := range []string{logstore.Container, logstore.Audit, logstore.Container, logstore.Audit} {
		if err := local.Record(ctx, logstore.Event{Category: category, Kind: "event", Data: strings.Repeat("x", 2000)}); err != nil {
			t.Fatal(err)
		}
	}
	lost := &lostLogAck{LogCopyServiceClient: client}
	if err := copyLogCategory(ctx, local, lost, logstore.Container, 1000); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("lost response not exercised: %v", err)
	}
	for range 3 {
		if err := copyLogCategory(ctx, local, client, logstore.Audit, 1000); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if err := copyLogCategory(ctx, local, client, logstore.Container, 1000); err != nil {
			t.Fatal(err)
		}
	}
	for _, category := range []string{logstore.Audit, logstore.Container} {
		p, _, err := copies.Read(ctx, "actual-node", "", logstore.Filter{Category: category})
		if err != nil || len(p.Events) != 2 || p.Next != 4 {
			t.Fatalf("bounded batch/lost ack/catchup: %+v %v", p, err)
		}
	}
	// No user-supplied node ID can select a different account's receipt.
	if _, _, err := copies.Read(ctx, "invented-node", "", logstore.Filter{Category: logstore.Audit}); err == nil {
		t.Fatal("wrong node obtained copies")
	}
	for _, change := range []string{`UPDATE nodes SET pending=1`, `UPDATE nodes SET pending=0,revoked=1`, `UPDATE nodes SET revoked=0,isolated=1`} {
		if _, err := d.ExecContext(ctx, change); err != nil {
			t.Fatal(err)
		}
		if _, err := client.GetLogReceipt(ctx, &nagiv1.LogReceiptQuery{Source: "s", Category: logstore.Audit}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("revoked/pending/isolated caller accepted: %s %v", change, err)
		}
	}
}

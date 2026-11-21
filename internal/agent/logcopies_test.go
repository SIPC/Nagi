package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	for _, standby := range []bool{false, true} {
		name := "primary"
		if standby {
			name = "standby"
		}
		t.Run(name, func(t *testing.T) { testRecentLogCopiesOverAuthenticatedMesh(t, standby) })
	}
}

func testRecentLogCopiesOverAuthenticatedMesh(t *testing.T, standby bool) {
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
		if standby {
			return nil // No UDP path: standby protection must use its own mesh link.
		}
		return []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), m.Port())}
	}
	if err := node.SetPeers([]mesh.Peer{{Key: panel.PublicKey(), Endpoints: ep(panel)}}); err != nil {
		t.Fatal(err)
	}
	if err := panel.SetPeers([]mesh.Peer{{Key: node.PublicKey(), Endpoints: ep(node)}}); err != nil {
		t.Fatal(err)
	}
	if standby {
		// Exercise actual WireGuard over the same authenticated-link API used by
		// control relays. Pin packet sources instead of trusting supplied headers.
		detachNode := node.AttachLink(panel.PublicKey(), func(_, dst mesh.Key, payload []byte) error {
			panel.HandleLink(node.PublicKey(), dst, payload)
			return nil
		})
		defer detachNode()
		detachPanel := panel.AttachLink(node.PublicKey(), func(_, dst mesh.Key, payload []byte) error {
			node.HandleLink(panel.PublicKey(), dst, payload)
			return nil
		})
		defer detachPanel()
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
	r.ReadOnly = standby
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
	panelClient := nagiv1.NewPanelLogServiceClient(cc)
	if _, err := panelClient.ListLogCopies(ctx, &nagiv1.PanelLogPosition{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("node upload permission granted panel inventory: %v", err)
	}
	if _, err := panelClient.ReadLogCopies(ctx, &nagiv1.PanelLogRead{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("node upload permission granted other origins' bodies: %v", err)
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
	if _, err := copyLogCategory(ctx, local, lost, logstore.Container, 128, 1000, false); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("lost response not exercised: %v", err)
	}
	for range 3 {
		if _, err := copyLogCategory(ctx, local, client, logstore.Audit, 128, 1000, false); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if _, err := copyLogCategory(ctx, local, client, logstore.Container, 128, 1000, false); err != nil {
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
	if standby {
		// Exercise the actual sender, not just its single-batch helper: a trusted
		// standby receives new facts while the primary has no reachable mesh peer.
		id, err := loadIdentity(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, fp := range []string{"offline-primary", "standby"} {
			if err := id.addPanel(trustedPanel{Fingerprint: fp, Address: fp + ":1"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := id.observe("", 1, "offline-primary"); err != nil {
			t.Fatal(err)
		}
		a := &agent{id: id, cfg: Config{Logs: local, LogConfig: logstore.Config{CopyImportantSeconds: 1, CopyOrdinarySeconds: 1}}, log: logger,
			mesh: &meshNode{m: node, panelKeys: map[string]mesh.Key{"offline-primary": mesh.PublicKey(randKey()), "standby": panel.PublicKey()}}}
		sp, _ := id.panel("standby")
		if a.role(sp) != "standby" {
			t.Fatal("fixture did not exercise standby role")
		}
		if err := local.Record(ctx, logstore.Event{Category: logstore.Security, Kind: "primary.offline", Data: "standby must receive"}); err != nil {
			t.Fatal(err)
		}
		work, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); a.logCopiesLoop(work) }()
		defer func() { stop(); <-done }()
		st, err := local.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			receipt, err := copies.Receipt(ctx, "actual-node", st.Source, logstore.Security)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.After == 5 {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("standby upload depended on primary availability")
			case <-tick.C:
			}
		}
		if _, err := id.removePanel("standby", true, nil); err != nil {
			t.Fatal(err)
		}
		if err := local.Record(ctx, logstore.Event{Category: logstore.Security, Kind: "after.revocation", Data: "must stay local"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(2200 * time.Millisecond):
		}
		stop()
		<-done
		page, _, err := copies.Read(ctx, "actual-node", st.Source, logstore.Filter{Category: logstore.Security})
		if err != nil || len(page.Events) != 1 || page.Events[0].Seq != 5 || page.Events[0].Kind != "primary.offline" {
			t.Fatalf("standby sender lost identity or ignored revocation: %+v %v", page, err)
		}
	} else {
		testCopyCatchup(ctx, t, local, copies, r, node, panel)
	}
	for _, change := range []string{`UPDATE nodes SET pending=1`, `UPDATE nodes SET pending=0,revoked=1`, `UPDATE nodes SET revoked=0,isolated=1`} {
		if _, err := d.ExecContext(ctx, change); err != nil {
			t.Fatal(err)
		}
		if _, err := client.GetLogReceipt(ctx, &nagiv1.LogReceiptQuery{Source: "s", Category: logstore.Audit}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("revoked/pending/isolated caller accepted: %s %v", change, err)
		}
		if _, err := client.PushLogs(ctx, &nagiv1.LogCopyBatch{Source: "s", Category: logstore.Audit}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("revoked/pending/isolated caller could push: %s %v", change, err)
		}
	}
}

// A backlog far beyond one default batch drains through the real sender at
// the catch-up rate while the panel accepts it, and only at the normal
// schedule while the panel refuses catch-up under pressure.
func testCopyCatchup(ctx context.Context, t *testing.T, local *logstore.Store, copies *logstore.Copies, r *nodes.Registry, node, panel *mesh.Mesh) {
	const backlog = 1500
	for i := range backlog {
		if err := local.Record(ctx, logstore.Event{Category: logstore.Access, Kind: "request", Data: fmt.Sprintf("GET /item/%d %s", i, strings.Repeat("p", 150))}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := local.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pressure := atomic.Uint64{}
	pressure.Store(100)
	r.CopyIntake = nodes.NewCopyIntake(50, 40, t.TempDir())
	r.CopyIntake.PSI = func() float64 { return float64(pressure.Load()) }
	id, err := loadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := id.addPanel(trustedPanel{Fingerprint: "primary", Address: "primary:1"}); err != nil {
		t.Fatal(err)
	}
	// Default schedule: ordinary categories get one 128-entry batch per 10 s.
	a := &agent{id: id, cfg: Config{Logs: local, LogConfig: logstore.DefaultConfig()}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		mesh: &meshNode{m: node, panelKeys: map[string]mesh.Key{"primary": panel.PublicKey()}}}
	a.copies.psi = func() float64 { return 0 }
	work, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); a.logCopiesLoop(work) }()
	defer func() { stop(); <-done }()
	acked := func() int64 {
		receipt, err := copies.Receipt(ctx, "actual-node", st.Source, logstore.Access)
		if err != nil {
			t.Fatal(err)
		}
		return receipt.After
	}
	// Panel under pressure: catch-up refused, the first normal batch still lands.
	time.Sleep(3 * time.Second)
	if got := acked(); got == 0 || got >= st.Last-backlog+256 {
		t.Fatalf("refused catch-up: acknowledged through %d of %d, want only the first normal batch", got, st.Last)
	}
	pressure.Store(0)
	start := time.Now()
	for acked() < st.Last {
		// Without catch-up the remaining ~1370 entries need about 110 s.
		if time.Since(start) > 12*time.Second {
			t.Fatalf("backlog not drained by catch-up: %d of %d after %v", acked(), st.Last, time.Since(start))
		}
		time.Sleep(50 * time.Millisecond)
	}
	page, _, err := copies.Read(ctx, "actual-node", st.Source, logstore.Filter{Category: logstore.Access, Limit: 1000})
	if err != nil || len(page.Events) != 1000 || !page.More {
		t.Fatalf("copied page: %d events more=%v %v", len(page.Events), page.More, err)
	}
	t.Logf("drained %d entries %v after pressure cleared", backlog, time.Since(start))
}

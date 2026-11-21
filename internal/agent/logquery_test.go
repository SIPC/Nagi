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

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestQueryLocalLogsOverMesh(t *testing.T) {
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
	node, panel := newMesh(), newMesh()
	endpoint := func(m *mesh.Mesh) []netip.AddrPort {
		return []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), m.Port())}
	}
	if err := node.SetPeers([]mesh.Peer{{Key: panel.PublicKey(), Endpoints: endpoint(panel)}}); err != nil {
		t.Fatal(err)
	}
	if err := panel.SetPeers([]mesh.Peer{{Key: node.PublicKey(), Endpoints: endpoint(node)}}); err != nil {
		t.Fatal(err)
	}
	id, err := loadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := id.addPanel(trustedPanel{Address: "panel:1", Fingerprint: "panel-fp", NodeID: "n1"}); err != nil {
		t.Fatal(err)
	}
	store, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	at := time.Now().UTC().Truncate(time.Second)
	for i, p := range []string{"p1", "p2", "p1"} {
		if err := store.Record(ctx, logstore.Event{Time: at.Add(time.Duration(i) * time.Minute), Category: logstore.Access, Kind: "request", Project: p, Data: map[string]any{"token": "never-show-this", "status": 200 + i}}); err != nil {
			t.Fatal(err)
		}
	}
	a := &agent{id: id, cfg: Config{Logs: store}, mesh: &meshNode{m: node, panelKeys: map[string]mesh.Key{"panel-fp": panel.PublicKey()}}}
	svc := &meshService{Service: mesh.Service{M: node}, a: a}
	done := make(chan error, 1)
	go func() { done <- mesh.Serve(ctx, node, svc) }()
	defer func() { cancel(); <-done }()
	cc, err := mesh.Client(panel, node.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	client := nagiv1.NewLogServiceClient(cc)
	first, err := client.QueryLogs(ctx, &nagiv1.LogQuery{Project: "p1", Limit: 1}, grpc.WaitForReady(true))
	if err != nil || len(first.GetEntries()) != 1 || first.GetEntries()[0].Seq != 1 || !first.GetMore() || first.GetNextAfter() != 1 || first.GetLatestSeq() != 3 {
		t.Fatalf("首屏 %+v %v", first, err)
	}
	second, err := client.QueryLogs(ctx, &nagiv1.LogQuery{Source: first.Source, After: first.NextAfter, Project: "p1"})
	if err != nil || len(second.GetEntries()) != 1 || second.GetEntries()[0].Seq != 3 || second.GetMore() || second.GetNextAfter() != 3 {
		t.Fatalf("筛选翻页漏读/串项目 %+v %v", second, err)
	}
	if strings.Contains(string(second.Entries[0].DataJson), "never-show-this") {
		t.Fatal("查询泄露凭据")
	}
	rangePage, err := client.QueryLogs(ctx, &nagiv1.LogQuery{From: timestamppb.New(at), To: timestamppb.New(at.Add(time.Minute))})
	if err != nil || len(rangePage.GetEntries()) != 1 || rangePage.GetEntries()[0].Seq != 1 {
		t.Fatalf("时间边界不是闭开区间 %+v %v", rangePage, err)
	}
	for _, q := range []*nagiv1.LogQuery{{After: 1}, {Limit: 1001}, {From: timestamppb.New(at.Add(time.Minute)), To: timestamppb.New(at)}} {
		if _, err := client.QueryLogs(ctx, q); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("错误查询应拒绝 %+v %v", q, err)
		}
	}
	if _, err := client.QueryLogs(ctx, &nagiv1.LogQuery{Source: "old-database", After: 999}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("重建后沿用旧游标未拒绝: %v", err)
	}
	if _, err := id.removePanel("panel-fp", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryLogs(ctx, &nagiv1.LogQuery{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("现存 mesh 连接上的撤权未生效: %v", err)
	}
}

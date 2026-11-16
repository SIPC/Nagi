package nodes

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc"
)

// sinkAgent 记下面板下发给节点的告警配置和外送目标。
type sinkAgent struct {
	nagiv1.AgentServiceClient
	mu     sync.Mutex
	sinks  [][]*nagiv1.NodeAuditSink
	alerts int
}

func (a *sinkAgent) SetAuditSink(_ context.Context, in *nagiv1.SetAuditSinkRequest, _ ...grpc.CallOption) (*nagiv1.SetAuditSinkResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sinks = append(a.sinks, in.GetSinks())
	return &nagiv1.SetAuditSinkResponse{}, nil
}

func (a *sinkAgent) SetAlertConfig(context.Context, *nagiv1.SetAlertConfigRequest, ...grpc.CallOption) (*nagiv1.SetAlertConfigResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts++
	return &nagiv1.SetAlertConfigResponse{}, nil
}

func (a *sinkAgent) got() ([][]*nagiv1.NodeAuditSink, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]*nagiv1.NodeAuditSink(nil), a.sinks...), a.alerts
}

// H1：拿泄露的接入码加入、还在等批准的节点，管理员改外送设置或告警配置时不能收到凭据；
// 被隔离的节点收到空列表；批准并认了本面板的节点照常收到。
func TestAuditSinksOnlyForTrustedNodes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	key, _ := sealer.Load(filepath.Join(dir, "master.key"))
	q := store.New(d)
	r := NewRegistry(q, slog.New(slog.DiscardHandler), key, nil)
	r.AuditSinks = func(context.Context) []*nagiv1.NodeAuditSink {
		return []*nagiv1.NodeAuditSink{{Id: "s3", Type: "s3", AccessKey: "ak", SecretKey: "sk"}}
	}
	k := func(b byte) []byte { m := make([]byte, 32); m[0] = b; return m }
	for _, n := range []store.CreateNodeParams{
		{ID: "pending", Name: "pending", MeshKey: k(1), Pending: 1},
		{ID: "ok", Name: "ok", MeshKey: k(2)},
		{ID: "iso", Name: "iso", MeshKey: k(3)},
		{ID: "other", Name: "other", MeshKey: k(4)},
	} {
		if err := q.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.SetNodeIsolated(ctx, "iso", true); err != nil {
		t.Fatal(err)
	}
	agents := map[string]*sinkAgent{}
	r.mu.Lock()
	for id, c := range map[string]*Conn{
		"pending": {pending: true, accepted: true},
		"ok":      {accepted: true},
		"iso":     {accepted: true},
		// 认的主面板不是本面板（例如本面板刚被取代）：下发的东西会被拒绝，也不该发。
		"other": {},
	} {
		a := &sinkAgent{}
		agents[id] = a
		c.NodeID, c.name, c.Agent = id, id, a
		r.conns[id] = c
	}
	r.mu.Unlock()

	r.PushAuditSinks()
	if err := r.SetAlertConfig(ctx, &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "c", Url: "https://hook/secret"}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	for id, a := range agents {
		sinks, alerts := a.got()
		switch id {
		case "pending", "other":
			if len(sinks) != 0 || alerts != 0 {
				t.Errorf("%s 收到了 %d 次外送目标、%d 次告警配置，应该一次都没有", id, len(sinks), alerts)
			}
		case "iso":
			for _, s := range sinks {
				if len(s) != 0 {
					t.Errorf("被隔离的节点收到了外送目标 %v", s)
				}
			}
		case "ok":
			if len(sinks) == 0 || len(sinks[len(sinks)-1]) != 1 || alerts == 0 {
				t.Errorf("正常节点应该收到外送目标和告警配置，实际 %v、%d", sinks, alerts)
			}
		}
	}

	// 解除隔离后重新下发；隔离后下发空列表。
	if err := r.SetIsolated(ctx, "iso", false); err != nil {
		t.Fatal(err)
	}
	if err := r.SetIsolated(ctx, "ok", true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if s, _ := agents["iso"].got(); len(s) == 0 || len(s[len(s)-1]) != 1 {
		t.Errorf("解除隔离后应该重新收到外送目标，实际 %v", s)
	}
	if s, _ := agents["ok"].got(); len(s) == 0 || len(s[len(s)-1]) != 0 {
		t.Errorf("隔离后应该收到空列表，实际 %v", s)
	}
}

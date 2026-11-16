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

// H1：拿泄露的接入码加入、还在等批准的节点，管理员改告警配置时收不到渠道地址。
// H2：节点的本地审计由面板代发，任何节点都收不到外送凭据：批准并认了本面板的节点只收到空列表（让旧版本的节点删掉以前保存的）。
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
		case "iso", "ok":
			if len(sinks) == 0 || alerts == 0 {
				t.Errorf("%s 应该收到告警配置和空的外送目标列表，实际 %v、%d", id, sinks, alerts)
			}
			for _, s := range sinks {
				if len(s) != 0 {
					t.Errorf("%s 收到了外送目标 %v", id, s)
				}
			}
		}
	}
}

package nodes

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
)

// 节点上报越权：发起方记为操作者，上报的节点记在详情里；同一个发起方 10 分钟内只告警一次；
// 被隔离的节点的上报只记录、不告警。
func TestReportViolations(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	key, _ := sealer.Load(filepath.Join(dir, "master.key"))
	q := store.New(d)
	log, err := audit.Open(ctx, filepath.Join(dir, "audit.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	r := NewRegistry(q, slog.Default(), key, nil)
	r.Audit = log
	evil := make([]byte, 32)
	evil[0] = 7
	for _, n := range []store.CreateNodeParams{{ID: "good", Name: "good", MeshKey: make([]byte, 32)}, {ID: "evil", Name: "evil", MeshKey: evil}} {
		if err := q.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	reporter := &Conn{NodeID: "good", name: "good"}
	p := &panelService{r: r, conn: reporter}
	report := func() {
		if _, err := p.ReportViolations(ctx, &nagiv1.ReportViolationsRequest{Violations: []*nagiv1.Violation{
			{Caller: evil, Method: "intranet:connect", ProjectId: "p1", Container: "db", Port: 5432},
			{Caller: evil, Method: "meshservice:VolumeDownload"},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	report()
	report()
	rows, _ := log.List(ctx, audit.Filter{Limit: 50})
	violations, alerts := 0, 0
	for _, e := range rows {
		switch {
		case e.Category == audit.Security && e.Action == "violation":
			violations++
			if e.ActorID != "evil" || e.TargetName != "good" || !strings.Contains(e.Params, `"reporter":"good"`) {
				t.Fatalf("记录不对：%+v", e)
			}
		case e.Category == audit.Alert && strings.Contains(e.Params, "疑似被攻破"):
			alerts++
		}
	}
	if violations != 4 || alerts != 1 {
		t.Fatalf("记录 %d 条越权、%d 条告警", violations, alerts)
	}
	// 被隔离的节点上报：只记录。
	if err := r.SetIsolated(ctx, "good", true); err != nil {
		t.Fatal(err)
	}
	r.iso.windows = nil
	report()
	rows, _ = log.List(ctx, audit.Filter{Limit: 50})
	alerts = 0
	for _, e := range rows {
		if e.Category == audit.Alert && strings.Contains(e.Params, "疑似被攻破") {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("被隔离的节点的上报不应告警：%d", alerts)
	}
}

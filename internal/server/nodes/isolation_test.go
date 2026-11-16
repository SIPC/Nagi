package nodes

import (
	"context"
	"crypto/rand"
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

// 节点上报越权：上报的节点记为操作者，被指控的一方记为对象；同一个发起方 10 分钟内只告警一次；
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
			if e.ActorID != "good" || e.TargetID != "evil" || e.TargetName != "evil" || !strings.Contains(e.Params, `"reporter":"good"`) {
				t.Fatalf("记录不对：%+v", e)
			}
		case e.Category == audit.Alert && strings.Contains(e.Params, "报告 evil 越权"):
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
		if e.Category == audit.Alert && strings.Contains(e.Params, "报告 evil 越权") {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("被隔离的节点的上报不应告警：%d", alerts)
	}
}

// M8：被攻破的节点 X 诬陷 Y、用随机公钥刷屏。审计的操作者是 X，Y 只是对象；告警写明是 X 报告的；
// 随机公钥合并成一条告警；每个节点 10 分钟最多记 200 条，字段截短，告警窗口不会无限增长。
func TestReportViolationsForgedAndFlood(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	key, _ := sealer.Load(filepath.Join(dir, "master.key"))
	q := store.New(d)
	log, err := audit.Open(ctx, filepath.Join(dir, "audit.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	r := NewRegistry(q, slog.New(slog.DiscardHandler), key, nil)
	r.Audit = log
	victim := make([]byte, 32)
	victim[0] = 9
	for _, n := range []store.CreateNodeParams{{ID: "x", Name: "attacker", MeshKey: make([]byte, 32)}, {ID: "y", Name: "victim", MeshKey: victim}} {
		if err := q.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	p := &panelService{r: r, conn: &Conn{NodeID: "x", name: "attacker"}}
	_, _ = p.ReportViolations(ctx, &nagiv1.ReportViolationsRequest{Violations: []*nagiv1.Violation{{Caller: victim, Method: "meshservice:VolumeDownload"}}})
	long := strings.Repeat("A", 100000)
	for range 5 {
		var list []*nagiv1.Violation
		for range 200 {
			k := make([]byte, 32)
			_, _ = rand.Read(k)
			list = append(list, &nagiv1.Violation{Caller: k, Method: long, ProjectId: long, Container: long})
		}
		_, _ = p.ReportViolations(ctx, &nagiv1.ReportViolationsRequest{Violations: list})
	}
	rows, _ := log.List(ctx, audit.Filter{Limit: 5000})
	viol, alerts, maxLen := 0, 0, 0
	for _, e := range rows {
		switch {
		case e.Category == audit.Security && e.Action == "violation":
			viol++
			if e.ActorID != "x" {
				t.Fatalf("操作者应该是上报的节点 x：%+v", e)
			}
			maxLen = max(maxLen, len(e.Params))
		case e.Category == audit.Alert:
			alerts++
			if strings.Contains(e.Params, "victim 疑似被攻破") {
				t.Errorf("上报的越权不应写成「victim 疑似被攻破」：%s", e.Params)
			}
		}
	}
	if viol != violationRecordLimit {
		t.Errorf("记了 %d 条越权，上限是 %d", viol, violationRecordLimit)
	}
	if maxLen > 1000 {
		t.Errorf("参数没截短：%d 字节", maxLen)
	}
	// 一条「attacker 报告 victim 越权」，一条未登记地址的合并告警。
	if alerts != 2 {
		t.Errorf("告警 %d 条，应该是 2 条", alerts)
	}
	if n := len(r.iso.windows); n > 2 {
		t.Errorf("告警窗口 %d 个，随机公钥不应各开一个", n)
	}
}

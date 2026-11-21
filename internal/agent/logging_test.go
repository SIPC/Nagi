package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/alerts"
	"github.com/SIPC/nagi/internal/agent/logging"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

func TestBackupRequestPersistsTransferFailureWithoutViewer(t *testing.T) {
	ctx := t.Context()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	projects, err := project.NewManager(d, nil, project.DefaultSubnetPool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := &nagiv1.ContainerSpec{ProjectId: "0123456789abcdef", Name: "web", Dir: t.TempDir(),
		Backups: []*nagiv1.BackupTarget{{NodeId: "target-a", Name: "backup-a"}}}
	b, err := proto.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodestore.New(d).UpsertContainer(ctx, nodestore.UpsertContainerParams{ProjectID: spec.ProjectId, Name: spec.Name, Dir: spec.Dir, Spec: b}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "logs.db")
	logs, err := logstore.Open(ctx, path, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &agent{ctx: ctx, cfg: Config{Logs: logs, LogConfig: logstore.DefaultConfig()}, projects: projects,
		backups: newBackupStates(filepath.Join(t.TempDir(), "backup-status.json")), log: logger,
		alerts: alerts.New(logger, "test", t.TempDir(), "", nil, nil, nil, nil)}
	a.alerts.ContainerRules = func(string, string) (*nagiv1.AlertRules, bool) {
		return &nagiv1.AlertRules{Enabled: map[string]bool{"backup": false}}, true
	}
	st, err := (&agentService{node: a, projects: projects}).SyncBackup(ctx, &nagiv1.SyncBackupRequest{ProjectId: spec.ProjectId, Container: spec.Name})
	if err != nil || st.GetLastError() != "节点网络没有启动" {
		t.Fatalf("existing result changed: %+v %v", st, err)
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err = logstore.Open(ctx, path, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	rows, err := logs.Query(ctx, logstore.Filter{Category: logstore.Task, Project: spec.ProjectId})
	if err != nil || len(rows) != 5 {
		t.Fatalf("transfer history: %+v %v", rows, err)
	}
	taskIDs := map[string]string{}
	for i, row := range rows {
		var data struct {
			TaskID               string `json:"task_id"`
			Target               string `json:"target_node"`
			Phase, Result, Error string
		}
		if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
			t.Fatal(err)
		}
		if taskIDs[row.Kind] == "" {
			taskIDs[row.Kind] = data.TaskID
		}
		if data.TaskID == "" || taskIDs[row.Kind] != data.TaskID || data.Target != "target-a" || row.Object != "web" || data.Phase != []string{"queued", "active", "started", "finished", "finished"}[i] {
			t.Fatalf("association: %+v %s", row, row.Data)
		}
		if data.Phase == "finished" && (data.Result != "failed" || data.Error != st.LastError) {
			t.Fatalf("failure lost: %s", row.Data)
		}
	}
}

func TestRawMetricHistoryKeepsFullChunksAndDoesNotInventMissingValues(t *testing.T) {
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	cfg := logstore.DefaultConfig()
	cfg.MetricsEverySeconds = 20
	collect := logging.CaptureMetrics(logs, cfg)
	start := time.Now().Add(-48 * time.Hour) // 新库不套用旧图表的 24 小时降采样规则。
	values := map[string]float64{"node:cpu": 37.5, "bad": math.NaN(), "c:project/secret-service:mem": 12345}
	for i := range 67 {
		values[fmt.Sprintf("c:project/container/service-%02d:mem", i)] = float64(1000 + i)
	}
	collect(start, values)
	collect(start.Add(10*time.Second), map[string]float64{"node:cpu": 99}) // 入库频率可配，不篡改采样频率。
	collect(start.Add(20*time.Second), map[string]float64{"node:cpu": 42})
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Metrics})
	if err != nil || len(rows) != 6 {
		t.Fatalf("原始采样没有分块完整入库: rows=%d err=%v", len(rows), err)
	}
	seen := map[string]float64{}
	for _, row := range rows[:3] {
		if !row.Time.Equal(start) {
			t.Fatalf("旧时间被改成了当前时间: %+v", row)
		}
		var data struct {
			Values []struct {
				Series string
				Value  float64
			}
		}
		if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
			t.Fatal(err)
		}
		for _, p := range data.Values {
			seen[p.Series] = p.Value
		}
	}
	if len(seen) != 69 || seen["node:cpu"] != 37.5 || seen["c:project/container/service-66:mem"] != 1066 || seen["c:project/secret-service:mem"] != 12345 {
		t.Fatalf("分块边界丢失/错误补零/把非有限数当正常采样: %+v", seen)
	}
	if _, exists := seen["bad"]; exists {
		t.Fatal("缺失值被补造")
	}
	invalid, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.System})
	if err != nil || len(invalid) != 1 || invalid[0].Kind != "sample.invalid" {
		t.Fatalf("无效数值没有留痕: %+v err=%v", invalid, err)
	}
}

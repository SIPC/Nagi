package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
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
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type diagnosticHistoryServer struct {
	nagiv1.UnimplementedAgentServiceServer
}

func (diagnosticHistoryServer) MeshPing(context.Context, *nagiv1.MeshPingRequest) (*nagiv1.MeshPingResponse, error) {
	return &nagiv1.MeshPingResponse{RttMs: 12.75}, nil
}

func (diagnosticHistoryServer) MeshSpeedTest(_ context.Context, in *nagiv1.MeshPingRequest) (*nagiv1.MeshSpeedResponse, error) {
	if string(in.GetPublicKey()) == "unreachable" {
		return nil, status.Error(codes.Unavailable, "peer unavailable")
	}
	if in.GetSpeedVia() == "" {
		return &nagiv1.MeshSpeedResponse{Upload: 4321, Download: 8765, Quic: true, WireguardTested: true, WireguardUpload: 2468, WireguardDownload: 1357}, nil
	}
	return &nagiv1.MeshSpeedResponse{Upload: 1234, Download: 5678, Quic: true, WireguardTested: false}, nil
}

func TestMeshDiagnosticHistoryRetainsActualResultsAndUnknowns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	logs, err := logstore.Open(t.Context(), path, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { logs.Close() }()
	a := &agent{cfg: Config{Logs: logs}}
	u, _ := a.auditInterceptors("authenticated-panel")
	ln := bufconn.Listen(1 << 20)
	defer ln.Close()
	srv := grpc.NewServer(grpc.UnaryInterceptor(u))
	nagiv1.RegisterAgentServiceServer(srv, diagnosticHistoryServer{})
	go srv.Serve(ln)
	defer srv.Stop()
	cc, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return ln.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	c := nagiv1.NewAgentServiceClient(cc)
	if out, err := c.MeshPing(t.Context(), &nagiv1.MeshPingRequest{PublicKey: []byte("peer")}); err != nil || out.GetRttMs() != 12.75 {
		t.Fatalf("ping=%v err=%v", out, err)
	}
	if out, err := c.MeshSpeedTest(t.Context(), &nagiv1.MeshPingRequest{PublicKey: []byte("peer"), SpeedVia: "path"}); err != nil || out.GetDownload() != 5678 {
		t.Fatalf("speed=%v err=%v", out, err)
	}
	if _, err := c.MeshSpeedTest(t.Context(), &nagiv1.MeshPingRequest{PublicKey: []byte("unreachable")}); status.Code(err) != codes.Unavailable {
		t.Fatalf("failure changed: %v", err)
	}
	if _, err := c.MeshSpeedTest(t.Context(), &nagiv1.MeshPingRequest{PublicKey: []byte("peer")}); err != nil {
		t.Fatal(err)
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err = logstore.Open(t.Context(), path, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Task})
	if err != nil || len(rows) != 4 {
		t.Fatalf("history=%+v err=%v", rows, err)
	}
	for i, row := range rows {
		var d map[string]any
		if err := json.Unmarshal(row.Data.(json.RawMessage), &d); err != nil {
			t.Fatal(err)
		}
		if row.Kind != "mesh.diagnostic" || d["panel"] != "authenticated-panel" {
			t.Fatalf("source=%s", row.Data)
		}
		switch i {
		case 0:
			if d["rtt_ms"] != 12.75 || d["result"] != "ok" {
				t.Fatalf("ping=%v", d)
			}
		case 1:
			if d["upload_bytes_per_second"] != float64(1234) || d["download_bytes_per_second"] != float64(5678) || d["mode"] != "path" || d["wireguard_tested"] != false {
				t.Fatalf("speed=%v", d)
			}
			if _, ok := d["wireguard_upload_bytes_per_second"]; ok {
				t.Fatalf("untested was called zero: %v", d)
			}
		case 2:
			if d["result"] != "failed" || d["error"] != "peer unavailable" {
				t.Fatalf("failure=%v", d)
			}
			if _, ok := d["upload_bytes_per_second"]; ok {
				t.Fatalf("failure became zero throughput: %v", d)
			}
		case 3:
			if d["wireguard_tested"] != true || d["wireguard_upload_bytes_per_second"] != float64(2468) || d["wireguard_download_bytes_per_second"] != float64(1357) {
				t.Fatalf("comparison=%v", d)
			}
		}
	}
}

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
		alerts: alerts.New(logger, "test", t.TempDir(), "", nil, nil, nil, nil, logs)}
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

func TestNodeStartupFailureIsNotRecordedAsReadyOrHealthyShutdown(t *testing.T) {
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), Config{Embedded: true, DataDir: dir, Logs: logs, Version: "test-version", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil {
		t.Fatal("invalid identity directory must fail startup before contacting Docker")
	}
	rows, readErr := logs.Query(t.Context(), logstore.Filter{Category: logstore.Task})
	if readErr != nil || len(rows) != 2 || rows[0].Kind != "node.starting" || rows[1].Kind != "node.stopped" {
		t.Fatalf("startup boundary: %+v %v", rows, readErr)
	}
	var end struct{ Result, Error string }
	if readErr := json.Unmarshal(rows[1].Data.(json.RawMessage), &end); readErr != nil {
		t.Fatal(readErr)
	}
	if end.Result != "failed" || end.Error == "" {
		t.Fatalf("startup failure hidden: %s", rows[1].Data)
	}
}

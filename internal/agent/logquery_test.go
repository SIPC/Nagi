package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/agent/nodefiles"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
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
	dir := t.TempDir()
	legacy, err := db.OpenMigrated(ctx, filepath.Join(dir, "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	for _, sample := range []struct {
		table, series string
		at            time.Time
		value         float64
	}{{"metrics_raw", "c:p1/web:cpu", at, 7.25}, {"metrics_raw", "c:p2/web:cpu", at, 97.5},
		{"metrics_m5", "c:p1/web:cpu", at.Add(-48 * time.Hour), 19.75}} {
		if _, err := legacy.ExecContext(ctx, "INSERT INTO "+sample.table+" VALUES (?,?,?)", sample.series, sample.at.Unix(), sample.value); err != nil {
			t.Fatal(err)
		}
	}
	history, err := metrics.OpenHistory(ctx, filepath.Join(dir, "logs", "metrics.db"), legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	collector := metrics.New(history, nil, "", logger)
	defer collector.CloseReader()
	a := &agent{id: id, cfg: Config{Logs: store}, metrics: collector,
		mesh: &meshNode{m: node, panelKeys: map[string]mesh.Key{"panel-fp": panel.PublicKey()}}}
	a.cfg.DataDir, a.log = dir, logger
	closeRecordings := a.openRecordings(ctx)
	defer closeRecordings()
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
	for _, sample := range []struct {
		at    time.Time
		value float64
	}{{at, 7.25}, {at.Add(-48 * time.Hour), 19.75}} {
		result, err := client.QueryMetrics(ctx, &nagiv1.QueryMetricsRequest{Prefix: "c:p1/",
			From: timestamppb.New(sample.at.Add(-time.Second)), To: timestamppb.New(sample.at.Add(time.Second))})
		if err != nil || len(result.GetSeries()) != 1 || result.Series[0].Name != "c:p1/web:cpu" || len(result.Series[0].Values) != 1 || result.Series[0].Values[0] != sample.value || result.Series[0].Times[0] != sample.at.Unix() {
			t.Fatalf("迁移的统计历史没有经 mesh 原样读出: %+v %v", result, err)
		}
	}
	for _, q := range []*nagiv1.QueryMetricsRequest{{Step: -1}, {Step: 1 << 62}, {From: timestamppb.New(at), To: timestamppb.New(at.Add(-time.Second))}, {From: &timestamppb.Timestamp{Seconds: 1 << 62}}} {
		if _, err := client.QueryMetrics(ctx, q); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("错误统计查询应拒绝: %+v %v", q, err)
		}
	}
	recordingID := exerciseRecordingCapture(t, ctx, a, legacy, client)
	if nodeAccessSupported() {
		t.Run("chunk history over mesh", func(t *testing.T) {
			a.files = &nodefiles.Service{StagingDir: filepath.Join(t.TempDir(), "staging"), BackupDir: t.TempDir()}
			path := filepath.Join(t.TempDir(), "uploaded")
			ref := &nagiv1.NodeFileRef{Path: path}
			mc := nagiv1.NewMeshServiceClient(cc)
			begin := &nagiv1.UploadBeginRequest{Node: ref, Size: 7, Key: "do-not-log-this-fingerprint"}
			session, err := mc.NodeFileUploadBegin(ctx, begin)
			if err != nil {
				t.Fatal(err)
			}
			part := func(hash string) error {
				stream, err := mc.NodeFileUploadPart(ctx)
				if err != nil {
					return err
				}
				if err := stream.Send(&nagiv1.UploadPartChunk{Node: ref, Id: session.Id, Sha256: hash, Data: []byte("payload")}); err != nil {
					return err
				}
				_, err = stream.CloseAndRecv()
				return err
			}
			if err := part(strings.Repeat("0", 64)); status.Code(err) != codes.DataLoss {
				t.Fatalf("bad hash: %v", err)
			}
			hash := sha256.Sum256([]byte("payload"))
			if err := part(hex.EncodeToString(hash[:])); err != nil {
				t.Fatal(err)
			}
			again, err := mc.NodeFileUploadBegin(ctx, begin)
			if err != nil || again.GetOffset() != 7 {
				t.Fatalf("resume: %+v %v", again, err)
			}
			if _, err := mc.NodeFileUploadCommit(ctx, &nagiv1.UploadCommitRequest{Node: ref, Id: session.Id, Sha256: hex.EncodeToString(hash[:])}); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != "payload" {
				t.Fatalf("file: %q %v", b, err)
			}
			// Reports are queued; wait for the writer before reading them back.
			if err := store.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			history, err := client.QueryLogs(ctx, &nagiv1.LogQuery{Category: logstore.Task})
			if err != nil {
				t.Fatal(err)
			}
			var seen []string
			for _, row := range history.GetEntries() {
				if row.GetKind() != "files.upload" {
					continue
				}
				body := string(row.DataJson)
				if strings.Contains(body, "do-not-log-this-fingerprint") || strings.Contains(body, `"payload"`) {
					t.Fatalf("leaked upload: %s", body)
				}
				var data struct {
					Action, Result string
					Stored         *int64 `json:"stored_offset"`
				}
				if err := json.Unmarshal(row.DataJson, &data); err != nil {
					t.Fatal(err)
				}
				seen = append(seen, data.Action+":"+data.Result)
				if data.Result == "failed" && data.Stored != nil {
					t.Fatalf("failed chunk confirmed: %s", body)
				}
				if data.Action == "node.part" && data.Result == "ok" && (data.Stored == nil || *data.Stored != 7) {
					t.Fatalf("offset: %s", body)
				}
			}
			if strings.Join(seen, ",") != "node.begin:ok,node.part:failed,node.part:ok,node.begin:ok,node.commit:ok" {
				t.Fatalf("history: %v", seen)
			}
		})
	}
	// The same authenticated panel may become a standby without losing log
	// reads. This must not turn standby membership into write permission.
	if err := id.addPanel(trustedPanel{Address: "primary:1", Fingerprint: "other-primary", NodeID: "n1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := id.observe("", 1, "other-primary"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryLogs(ctx, &nagiv1.LogQuery{Category: logstore.Access}); err != nil {
		t.Fatalf("standby log query rejected: %v", err)
	}
	if _, err := client.QueryMetrics(ctx, &nagiv1.QueryMetricsRequest{}); err != nil {
		t.Fatalf("standby metric query rejected: %v", err)
	}
	if _, err := nagiv1.NewMeshServiceClient(cc).NodeFileUploadBegin(ctx, &nagiv1.UploadBeginRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("standby obtained write permission: %v", err)
	}
	if _, err := id.removePanel("panel-fp", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryLogs(ctx, &nagiv1.LogQuery{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("现存 mesh 连接上的撤权未生效: %v", err)
	}
	if _, err := client.QueryMetrics(ctx, &nagiv1.QueryMetricsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("现存统计 mesh 连接上的撤权未生效: %v", err)
	}
	replay, err := client.ReadRecording(ctx, &nagiv1.LocalRecordingQuery{Id: recordingID})
	if err == nil {
		_, err = replay.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("撤权后仍可读取录制正文: %v", err)
	}
}

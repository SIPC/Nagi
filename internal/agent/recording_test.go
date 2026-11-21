package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/recording"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// Used by the real-mesh LogService integration test: the real container Exec
// handler reads a local fake Docker engine, not a mocked recorder or RPC stream.
func exerciseRecordingCapture(t *testing.T, ctx context.Context, a *agent, nodeDB *sql.DB, logs nagiv1.LogServiceClient) string {
	t.Helper()
	const output = "private-terminal-body-not-in-ordinary-log\r\n"
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			io.WriteString(w, `[{"Id":"test","State":"running","Labels":{"com.docker.compose.service":"app"}}]`)
		case strings.HasSuffix(r.URL.Path, "/containers/test/exec"):
			io.WriteString(w, `{"Id":"terminal"}`)
		case strings.HasSuffix(r.URL.Path, "/exec/terminal/start"):
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			fmt.Fprint(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"+output)
			rw.Flush()
		case strings.HasSuffix(r.URL.Path, "/exec/terminal/json"):
			io.WriteString(w, `{"ExitCode":7}`)
		default:
			t.Errorf("unexpected Docker request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(engine.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.56")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	dc, err := docker.New()
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	manager, err := project.NewManager(nodeDB, dc, project.DefaultSubnetPool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const pid = "0123456789abcdef"
	spec := &nagiv1.ContainerSpec{ProjectId: pid, Name: "web", Dir: t.TempDir(), Config: &nagiv1.ContainerConfig{}}
	body, _ := proto.Marshal(spec)
	if err := nodestore.New(nodeDB).UpsertContainer(ctx, nodestore.UpsertContainerParams{ProjectID: pid, Name: "web", Dir: spec.Dir, Spec: body}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u, st := a.primaryGuard("panel-fp")
	server := grpc.NewServer(grpc.UnaryInterceptor(u), grpc.StreamInterceptor(st))
	nagiv1.RegisterAgentServiceServer(server, &agentService{node: a, projects: manager, panel: "panel-fp"})
	go server.Serve(ln)
	defer server.Stop()
	cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	var savedID string
	for _, policy := range []string{"enabled", "disabled", "legacy"} {
		callCtx := ctx
		if policy != "legacy" {
			callCtx = recording.WithCapture(ctx, policy == "enabled")
		}
		stream, err := nagiv1.NewAgentServiceClient(cc).Exec(callCtx)
		if err != nil {
			t.Fatal(err)
		}
		if err = stream.Send(&nagiv1.ExecInput{Input: &nagiv1.ExecInput_Start{Start: &nagiv1.ExecStart{ProjectId: pid, Container: "web", Size: &nagiv1.TerminalSize{Cols: 91, Rows: 33}}}}); err != nil {
			t.Fatal(err)
		}
		header, err := stream.Header()
		if err != nil {
			t.Fatal(err)
		}
		var got strings.Builder
		var exit int32 = -99
		for {
			msg, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if out, ok := msg.Output.(*nagiv1.ExecOutput_Stdout); ok {
				got.Write(out.Stdout)
			}
			if code, ok := msg.Output.(*nagiv1.ExecOutput_ExitCode); ok {
				exit = code.ExitCode
			}
		}
		stream.CloseSend()
		if got.String() != output || exit != 7 {
			t.Fatalf("%s changed execution: output=%q exit=%d", policy, got.String(), exit)
		}
		ids := header.Get(recording.IDHeader)
		if policy == "enabled" {
			if len(ids) != 1 || !recording.ValidID(ids[0]) {
				t.Fatalf("missing execution-node recording: %v", header)
			}
			savedID = ids[0]
			var summary recording.Summary
			trailers := stream.Trailer().Get(recording.SummaryTrailer)
			if len(trailers) != 1 || json.Unmarshal([]byte(trailers[0]), &summary) != nil || summary.ID != savedID || summary.Truncated || summary.Bytes == 0 {
				t.Fatalf("missing durable summary: %v", trailers)
			}
		} else if len(ids) != 1 || ids[0] != "" {
			t.Fatalf("%s captured despite absent/disabled policy: %v", policy, header)
		}
	}
	replay, err := logs.ReadRecording(ctx, &nagiv1.LocalRecordingQuery{Id: savedID})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for {
		part, err := replay.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got.Write(part.Data)
	}
	if !strings.Contains(got.String(), `"width":91`) || !strings.Contains(got.String(), strings.TrimSpace(output)) {
		t.Fatalf("mesh replay lost execution body: %q", got.String())
	}
	page, err := logs.QueryLogs(ctx, &nagiv1.LogQuery{Category: logstore.Audit})
	if err != nil || len(page.GetEntries()) != 1 || page.Entries[0].Kind != "terminal.recording" || strings.Contains(string(page.Entries[0].DataJson), strings.TrimSpace(output)) {
		t.Fatalf("audit must keep only manifest, not plaintext or disabled captures: %v %v", page, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(a.cfg.DataDir, "logs", "recordings.db")); len(matches) != 1 {
		t.Fatal("execution body did not enter local database")
	}
	return savedID
}

func TestLostRecordingKeyDoesNotReplaceExistingHistory(t *testing.T) {
	dir := t.TempDir()
	journal, err := logstore.Open(t.Context(), filepath.Join(dir, "logs", "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	path := filepath.Join(dir, "logs", "recordings.db")
	const history = "encrypted-history-must-not-change"
	if err := os.WriteFile(path, []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: Config{DataDir: dir, Logs: journal}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	closeRecordings := a.openRecordings(t.Context())
	closeRecordings()
	if _, err := os.Stat(filepath.Join(dir, "identity", "recording.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replaced missing key: %v", err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != history {
		t.Fatalf("changed preserved history: %q %v", body, err)
	}
}

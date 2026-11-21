//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/agent/nodefiles"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
)

type fileHistoryStream struct {
	grpc.ServerStream
	ctx  context.Context
	fail error
}

func (s *fileHistoryStream) Context() context.Context       { return s.ctx }
func (s *fileHistoryStream) Send(*nagiv1.FileOpEvent) error { return s.fail }

func TestFileHistoryKeepsLocalResultWhenResponseCannotBeDelivered(t *testing.T) {
	logsPath := filepath.Join(t.TempDir(), "logs.db")
	logs, err := logstore.Open(t.Context(), logsPath, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := &agentService{node: &agent{cfg: Config{DataDir: t.TempDir(), Logs: logs, LogConfig: logstore.DefaultConfig()}, files: &nodefiles.Service{}}}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "original"), []byte("asymmetric payload"), 0600); err != nil {
		t.Fatal(err)
	}
	deliveryErr := errors.New("response lost")
	stream := &fileHistoryStream{ctx: t.Context(), fail: deliveryErr}
	req := &nagiv1.NodeTransferRequest{Paths: []string{filepath.Join(work, "original")}, DestDir: work, NewName: "renamed"}
	if err := s.TransferNodeFiles(req, stream); !errors.Is(err, deliveryErr) {
		t.Fatalf("response error changed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(work, "renamed")); err != nil || string(b) != "asymmetric payload" {
		t.Fatalf("rename didn't complete: %q %v", b, err)
	}
	stream.fail = nil
	if err := s.TransferNodeFiles(req, stream); err == nil {
		t.Fatal("missing source should fail")
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err = logstore.Open(t.Context(), logsPath, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Task})
	if err != nil || len(rows) != 4 {
		t.Fatalf("history: %+v %v", rows, err)
	}
	ids := map[string]bool{}
	for i, row := range rows {
		var data struct {
			Phase, Result string
			TaskID        string `json:"task_id"`
			Completed     int
		}
		if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
			t.Fatal(err)
		}
		if data.TaskID == "" {
			t.Fatal("no task identity")
		}
		if i%2 == 0 {
			ids[data.TaskID] = true
		}
		if !ids[data.TaskID] || row.Kind != "files.task" {
			t.Fatalf("lost association: %s", row.Data)
		}
		if i == 1 && (data.Phase != "finished" || data.Result != "ok" || data.Completed != 1) {
			t.Fatalf("delivery failure overwrote local success: %s", row.Data)
		}
		if i == 3 && data.Result != "failed" {
			t.Fatalf("failure not retained: %s", row.Data)
		}
	}
	if len(ids) != 2 {
		t.Fatal("unrelated calls share a task id")
	}
}

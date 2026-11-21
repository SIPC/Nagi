package release

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestDownloadJournalPersistsIndependentOfLiveProgress(t *testing.T) {
	setup(t)
	body := bytes.Repeat([]byte("nagi"), 8192)
	srv := slowServer(t, body, 4, time.Millisecond, true)
	path := filepath.Join(t.TempDir(), "logs.db")
	logs, err := logstore.Open(t.Context(), path, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logs.Close() })
	var live progressLog
	ctx := WithProgress(t.Context(), live.add)
	ctx, finish := Journal(ctx, logs, time.Hour, "v0.1.0", "v0.2.0")
	_, err = fetchFirst(ctx, []Source{{Base: srv.URL + "/nagi", Mirror: true}}, filepath.Join(t.TempDir(), "binary"), nil, slog.New(slog.DiscardHandler))
	finish("downloaded", err)
	finish("wrong duplicate", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.all()) == 0 {
		t.Fatal("journal replaced live progress callback")
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err = logstore.Open(t.Context(), path, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Task})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 4 {
		t.Fatalf("missing start/progress/end: %+v", rows)
	}
	var id string
	var bytesSeen bool
	for i, row := range rows {
		var data struct {
			TaskID                  string `json:"task_id"`
			Phase, Result, From, To string
			Downloaded              int64 `json:"downloaded_bytes"`
		}
		if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			id = data.TaskID
		}
		if id == "" || data.TaskID != id || data.From != "v0.1.0" || data.To != "v0.2.0" {
			t.Fatalf("bad task evidence %+v", data)
		}
		if data.Phase == "progress" && data.Downloaded == int64(len(body)) {
			bytesSeen = true
		}
		if i == 0 && data.Phase != "started" {
			t.Fatal("missing start")
		}
		if i == len(rows)-1 && (data.Phase != "finished" || data.Result != "downloaded") {
			t.Fatalf("bad final result %+v", data)
		}
	}
	if !bytesSeen {
		t.Fatal("final download byte count was throttled away")
	}
}

func TestCanceledDownloadJournalStillCommitsOutcome(t *testing.T) {
	setup(t)
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	srv := slowServer(t, []byte("nagi"), 1, 0, true)
	ctx, cancel := context.WithCancel(t.Context())
	ctx, finish := Journal(ctx, logs, time.Second, "old", "new")
	cancel()
	_, err = fetchFirst(ctx, []Source{{Base: srv.URL + "/nagi", Mirror: true}}, filepath.Join(t.TempDir(), "binary"), nil, slog.New(slog.DiscardHandler))
	finish("failed", err)
	if err == nil {
		t.Fatal("canceled download unexpectedly succeeded")
	}
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Task})
	if err != nil || len(rows) < 2 {
		t.Fatalf("missing outcome %+v %v", rows, err)
	}
	var data struct{ Phase, Result string }
	if err := json.Unmarshal(rows[len(rows)-1].Data.(json.RawMessage), &data); err != nil {
		t.Fatal(err)
	}
	if data.Phase != "finished" || data.Result != "canceled" {
		t.Fatalf("cancellation lost %+v", data)
	}
}

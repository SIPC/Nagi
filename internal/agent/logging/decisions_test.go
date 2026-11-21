package logging_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/logging"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestDecisionQueueDoesNotBlockOnStorageAndAccountsForOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	s, err := logstore.Open(t.Context(), path, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer other.Exec("ROLLBACK")
	queue := logging.NewDecisions(s, 2)
	defer queue.Close()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		for i := 0; i < 1000; i++ {
			queue.Report(logstore.Event{Category: logstore.Task, Kind: "vote", Data: map[string]any{"epoch": i}})
		}
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("vote logging waited for SQLite writer")
	}
	if _, err := other.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	queue.Close()
	rows, err := s.Query(t.Context(), logstore.Filter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	kept, lost := 0, 0
	for _, row := range rows {
		switch row.Kind {
		case "vote":
			kept++
		case "decisions.gap":
			var data struct {
				Lost     int  `json:"lost_records"`
				Complete bool `json:"history_complete"`
			}
			if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
				t.Fatal(err)
			}
			if data.Complete {
				t.Fatal("overflow reported complete history")
			}
			lost += data.Lost
		}
	}
	if kept == 0 || lost == 0 || kept+lost != 1000 {
		t.Fatalf("kept=%d lost=%d", kept, lost)
	}
}

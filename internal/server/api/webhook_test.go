package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/server/projects"
)

func TestWebhookHistoryDoesNotCallQueuedDeploymentSuccess(t *testing.T) {
	h := newAuditHarness(t)
	h.a.Projects = &projects.Service{Q: h.a.Q, Sealer: h.a.Sealer, Log: h.a.Log}
	s, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h.a.Logs = s
	const pid, token = "aaaaaaaaaaaaaaaa", "do-not-log-this-hook-token"
	if _, err := h.db.Exec(`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES (?, 'a', 'a', 1, 0, X'')`, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub) VALUES (?, 'web', 1, 'n1', '/srv/a', 1, 0, ?, X'', '')`, pid, projects.HashToken(token)); err != nil {
		t.Fatal(err)
	}
	w := &webhooks{api: h.a, running: map[string]bool{pid + "/web": true}, again: map[string]bool{}}
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		w.serve(rr, httptest.NewRequest(http.MethodPost, "/hooks/"+token, strings.NewReader(`{"password":"do-not-log-body"}`)))
		if rr.Code != http.StatusAccepted {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
		}
	}
	rows, err := s.Query(t.Context(), logstore.Filter{Category: logstore.Task})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for i, row := range rows {
		var data struct{ Coalesced bool }
		if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
			t.Fatal(err)
		}
		if row.Kind != "webhook.queued" || data.Coalesced != (i == 1) {
			t.Fatalf("queued is not completed: %+v %s", row, row.Data)
		}
	}
	// Clear only the test's synthetic queue and accept a real background attempt.
	delete(w.running, pid+"/web")
	delete(w.again, pid+"/web")
	rr := httptest.NewRecorder()
	w.serve(rr, httptest.NewRequest(http.MethodPost, "/hooks/"+token, nil))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d", rr.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, err = s.Query(t.Context(), logstore.Filter{Category: logstore.Task})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background result missing: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var begin, end struct {
		TaskID        string `json:"task_id"`
		Result, Error string
	}
	if err := json.Unmarshal(rows[3].Data.(json.RawMessage), &begin); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rows[4].Data.(json.RawMessage), &end); err != nil {
		t.Fatal(err)
	}
	if rows[2].Kind != "webhook.accepted" || rows[3].Kind != "webhook.deploy.started" || rows[4].Kind != "webhook.deploy.finished" || begin.TaskID == "" || begin.TaskID != end.TaskID || end.Result != "failed" || end.Error == "" {
		t.Fatalf("accepted hid backend failure: %+v", rows)
	}
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if row.Project != pid || row.Object != "web" || strings.Contains(string(b), token) || strings.Contains(string(b), "do-not-log-body") {
			t.Fatalf("wrong ownership or credential leak: %s", b)
		}
	}
}

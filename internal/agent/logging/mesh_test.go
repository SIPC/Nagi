package logging

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestMeshHistoryPreservesUnknownLossAndObservedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	logs, err := logstore.Open(t.Context(), path, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	cfg := logstore.DefaultConfig()
	cfg.MetricsEverySeconds = 10
	capture := CaptureMesh(logs, cfg)
	at := time.Now()
	peer := &nagiv1.MeshPeerStatus{PublicKey: []byte{1, 2}, Path: "direct", Endpoint: "192.0.2.3:51820", RxBytes: 9007199254740993, LocalPort: 51820}
	st := &nagiv1.MeshStatus{Peers: []*nagiv1.MeshPeerStatus{peer}}
	capture(at, st)
	peer.Path, peer.Endpoint, peer.TxLossKnown, peer.TxLossPpm = "node-relay", "", true, 17000
	peer.Via = "relay-b"
	capture(at.Add(time.Second), st) // configured frequency, not every caller
	capture(at.Add(10*time.Second), st)
	capture(at.Add(20*time.Second), nil)
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err = logstore.Open(t.Context(), path, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Metrics})
	if err != nil || len(rows) != 5 {
		t.Fatalf("history %+v %v", rows, err)
	}
	if rows[1].Object != "0102" || rows[1].Kind != "mesh.peer" {
		t.Fatalf("peer identity: %+v", rows[1])
	}
	var data map[string]any
	if err := json.Unmarshal(rows[1].Data.(json.RawMessage), &data); err != nil {
		t.Fatal(err)
	}
	if data["tx_loss_known"] != false || data["path"] != "direct" {
		t.Fatalf("unknown became absent/healthy: %s", rows[1].Data)
	}
	if err := json.Unmarshal(rows[3].Data.(json.RawMessage), &data); err != nil {
		t.Fatal(err)
	}
	if data["path"] != "node-relay" || data["via"] != "relay-b" || data["tx_loss_known"] != true || data["tx_loss_ppm"] != float64(17000) || data["rx_bytes"] != "9007199254740993" {
		t.Fatalf("path/quality/counter lost: %s", rows[3].Data)
	}
	if rows[4].Kind != "mesh.unavailable" {
		t.Fatalf("missing source must be explicit: %+v", rows[4])
	}
}

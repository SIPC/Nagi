package logging

import (
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// CaptureMesh saves the existing read-only status, one peer per bounded record.
// No probes are sent. Missing peers are not interpreted as disconnected or removed.
func CaptureMesh(logs *logstore.Store, cfg logstore.Config) func(time.Time, *nagiv1.MeshStatus) {
	var last time.Time
	return func(at time.Time, st *nagiv1.MeshStatus) {
		if !last.IsZero() && at.Sub(last) >= 0 && at.Sub(last) < time.Duration(cfg.MetricsEverySeconds)*time.Second {
			return
		}
		last = at
		if st == nil {
			logs.Report(logstore.Event{Time: at, Category: logstore.Metrics, Kind: "mesh.unavailable", Data: map[string]any{"available": false}})
			return
		}
		logs.Report(logstore.Event{Time: at, Category: logstore.Metrics, Kind: "mesh.local", Data: map[string]any{
			"public_key": hex.EncodeToString(st.PublicKey), "address": st.Address, "port": st.Port, "endpoints": st.Endpoints,
			"nat": st.Nat, "quic_off": st.QuicOff, "relay_off": st.RelayOff, "peers_observed": len(st.Peers),
		}})
		for _, peer := range st.Peers {
			if peer == nil {
				continue
			}
			// Explicit known=false is essential: an unmeasured loss must not look
			// like zero loss. StatusProto is already a public-information view.
			data, err := (protojson.MarshalOptions{EmitUnpopulated: true, UseProtoNames: true}).Marshal(peer)
			if err != nil {
				logs.Report(logstore.Event{Time: at, Category: logstore.System, Kind: "mesh.snapshot.invalid", Data: map[string]any{"error": err.Error()}})
				continue
			}
			logs.Report(logstore.Event{Time: at, Category: logstore.Metrics, Kind: "mesh.peer", Object: hex.EncodeToString(peer.PublicKey), Data: json.RawMessage(data)})
		}
	}
}

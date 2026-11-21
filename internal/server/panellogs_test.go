package server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPairedPanelsReconcileLogsAcrossPromotionAndOldPrimaryReturn(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	logger := slog.New(slog.DiscardHandler)
	type running struct {
		s    *Server
		stop func()
		done chan struct{}
	}
	start := func(dir, code string, logs *logstore.Store) running {
		t.Helper()
		s, err := New(ctx, Config{DataDir: dir, StandbyCode: code, Logger: logger, Logs: logs, LogConfig: logstore.Config{RecentCapacityMiB: 16, RecentHours: 24}})
		if err != nil {
			t.Fatal(err)
		}
		if s.mesh == nil {
			t.Fatal("mesh startup failed")
		}
		s.publicAddress = "127.0.0.1"
		if err := s.q.SetSetting(ctx, store.SetSettingParams{Key: "public_address", Value: "127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
		if s.standby == nil {
			if err := s.q.SetSetting(ctx, store.SetSettingParams{Key: standby.SettingPrimaryAddress, Value: s.LocalAddr()}); err != nil {
				t.Fatal(err)
			}
		}
		work, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer s.audit.Close()
			if s.auditReplica != nil {
				defer s.auditReplica.Close()
			}
			if err := s.Run(work); err != nil && !errors.Is(err, ErrRoleChanged) && work.Err() == nil {
				t.Errorf("panel run: %v", err)
			}
		}()
		var once sync.Once
		closePanel := func() { once.Do(func() { stop(); <-done }) }
		t.Cleanup(closePanel)
		client := http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: time.Second}
		t.Cleanup(client.CloseIdleConnections)
		for {
			res, err := client.Get("https://" + s.LocalAddr() + "/api/standby/status")
			if err == nil {
				res.Body.Close()
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("panel listener not ready", ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
		return running{s, closePanel, done}
	}
	newLogs := func(dir string) *logstore.Store {
		s, err := logstore.Open(ctx, filepath.Join(dir, "logs", "logs.db"), 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	wait := func(label string, f func() bool) {
		t.Helper()
		for !f() {
			select {
			case <-ctx.Done():
				t.Fatal(label, ctx.Err())
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	aDir, bDir := t.TempDir(), t.TempDir()
	aLogs, bLogs := newLogs(aDir), newLogs(bDir)
	a := start(aDir, "", aLogs)
	code, _, err := a.s.primary.NewCode(ctx, a.s.LocalAddr(), a.s.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	b := start(bDir, code, bLogs)
	// Pairing records the initially detected address. Keep this isolated fixture
	// on loopback without changing production address discovery or TLS pinning.
	if _, err := a.s.db.ExecContext(ctx, `UPDATE standby_panels SET address=? WHERE server_fp=?`, b.s.LocalAddr(), b.s.fingerprint); err != nil {
		t.Fatal(err)
	}
	wait("mutually authenticated mesh identities", func() bool {
		return a.s.authorizeLogPanel(ctx, b.s.mesh.PublicKey()) == nil && b.s.authorizeLogPanel(ctx, a.s.mesh.PublicKey()) == nil
	})
	if !b.s.nodes.ReadOnly {
		t.Fatal("standby became writable")
	}
	t.Run("group certificate alone is not panel membership", func(t *testing.T) {
		key, err := tlsutil.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, err := tlsutil.CreateCSR(key, "unregistered-panel")
		if err != nil {
			t.Fatal(err)
		}
		cert, err := a.s.ca.SignCSR(csr, "unregistered-panel", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		cfg := tlsutil.PinnedConfig(a.s.fingerprint)
		cfg.NextProtos = []string{panelMeshALPN}
		cfg.Certificates = []tls.Certificate{{Certificate: [][]byte{cert.Raw}, PrivateKey: key}}
		conn, err := tls.Dial("tcp", a.s.LocalAddr(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := panelMeshRead(conn, &nagiv1.MeshInfo{}); err == nil {
			t.Fatal("unregistered group certificate obtained panel mesh identity")
		}
	})
	t.Run("panel cannot claim another mesh identity", func(t *testing.T) {
		cfg := tlsutil.PinnedConfig(a.s.fingerprint)
		cfg.NextProtos = []string{panelMeshALPN}
		cfg.Certificates = []tls.Certificate{*b.s.groupClientCert()}
		conn, err := tls.Dial("tcp", a.s.LocalAddr(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var hello nagiv1.MeshInfo
		if err := panelMeshRead(conn, &hello); err != nil {
			t.Fatal(err)
		}
		if err := panelMeshWrite(conn, &hello); err != nil {
			t.Fatal(err)
		}
		if err := panelMeshRead(conn, &nagiv1.MeshPacket{}); err == nil {
			t.Fatal("panel claimed receiver's mesh identity")
		}
		if err := a.s.authorizeLogPanel(ctx, b.s.mesh.PublicKey()); err != nil {
			t.Fatal("rejected bootstrap replaced valid binding", err)
		}
	})
	if err := aLogs.Record(ctx, logstore.Event{Category: logstore.Security, Kind: "primary.before.failure", Data: "preserve original"}); err != nil {
		t.Fatal(err)
	}
	aState, err := aLogs.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read := func(s *Server, node, source, kind string) []logstore.Event {
		p, _, err := s.nodes.LogCopies.Read(ctx, node, source, logstore.Filter{Category: logstore.Security, Limit: 1000})
		if err != nil {
			return nil
		}
		var found []logstore.Event
		for _, e := range p.Events {
			if e.Kind == kind {
				found = append(found, e)
			}
		}
		return found
	}
	wait("standby mesh copy", func() bool {
		return len(read(b.s, "panel:"+a.s.fingerprint, aState.Source, "primary.before.failure")) == 1
	})
	first := read(b.s, "panel:"+a.s.fingerprint, aState.Source, "primary.before.failure")[0]
	if first.Seq != aState.Last {
		t.Fatalf("renumbered original: %d != %d", first.Seq, aState.Last)
	}
	cc, err := mesh.Client(b.s.mesh, a.s.mesh.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	client := nagiv1.NewPanelLogServiceClient(cc)
	if _, err := client.ListLogCopies(ctx, &nagiv1.PanelLogPosition{}); err != nil {
		t.Fatal(err)
	}
	a.s.nodes.PanelAlert(ctx, &nagiv1.Alert{Key: "upgrade-alert", Title: "Upgrade fixture failure"})
	if err := b.s.nodes.RefreshAlertReplica(ctx, ""); err != nil {
		t.Fatal(err)
	}
	alerts, err := b.s.nodes.AlertStates(ctx, "", false)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("standby alert snapshot: %v %v", alerts, err)
	}
	alert := alerts[0]
	beforeHandling, err := b.s.nodes.AlertReplica(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	alertClient := nagiv1.NewPanelAlertServiceClient(cc)
	handled, err := alertClient.RelayHandleAlert(ctx, &nagiv1.RelayHandleAlertRequest{Actor: b.s.fingerprint + ":42", Request: &nagiv1.HandleAlertRequest{Key: alert.Alert.Key, Occurrence: alert.Alert.Occurrence, Action: "acknowledge"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.s.nodes.ImportAlertReplica(ctx, handled); err != nil {
		t.Fatal(err)
	}
	alerts, err = b.s.nodes.AlertStates(ctx, "", false)
	if err != nil || len(alerts) != 1 || !alerts[0].Acknowledged {
		t.Fatalf("standby forwarded handling: %v %v", alerts, err)
	}
	// An out-of-order response cannot roll handling backwards.
	if err := b.s.nodes.ImportAlertReplica(ctx, beforeHandling); err != nil {
		t.Fatal(err)
	}
	alerts, err = b.s.nodes.AlertStates(ctx, "", false)
	if err != nil || len(alerts) != 1 || !alerts[0].Acknowledged {
		t.Fatalf("old replica overwrote handling: %v %v", alerts, err)
	}
	// Live connections do not outlive revocation, even while the stale mesh key
	// and link are still cached. Restore this fixture afterwards to test failover.
	if err := standby.AddRemoved(ctx, a.s.q, b.s.fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListLogCopies(ctx, &nagiv1.PanelLogPosition{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked inventory accepted: %v", err)
	}
	if _, err := client.ReadLogCopies(ctx, &nagiv1.PanelLogRead{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked bodies accepted: %v", err)
	}
	if _, err := alertClient.ReadAlertReplica(ctx, &nagiv1.AlertReplicaRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked alert read accepted: %v", err)
	}
	if _, err := alertClient.RelayHandleAlert(ctx, &nagiv1.RelayHandleAlertRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked alert handling accepted: %v", err)
	}
	if err := a.s.q.SetSetting(ctx, store.SetSettingParams{Key: standby.SettingRemoved, Value: "[]"}); err != nil {
		t.Fatal(err)
	}
	a.stop()
	// The old primary's disk retains a fact unavailable to the standby. This
	// models a partitioned old primary continuing to write before it returns.
	if err := aLogs.Record(ctx, logstore.Event{Category: logstore.Security, Kind: "old.primary.unique", Data: "recover on return"}); err != nil {
		t.Fatal(err)
	}
	unique, err := aLogs.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bLogs.Record(ctx, logstore.Event{Category: logstore.Security, Kind: "standby.while.primary.down", Data: "independent receipt"}); err != nil {
		t.Fatal(err)
	}
	bState, err := bLogs.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wait("standby records while primary is down", func() bool {
		return len(read(b.s, "panel:"+b.s.fingerprint, bState.Source, "standby.while.primary.down")) == 1
	})
	if _, err := b.s.Promote(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	promoted := start(bDir, "", bLogs)
	if promoted.s.standby != nil || promoted.s.nodes.ReadOnly {
		t.Fatal("promotion did not preserve primary role")
	}
	alerts, err = promoted.s.nodes.AlertStates(ctx, "", false)
	if err != nil || len(alerts) != 1 || !alerts[0].Acknowledged || alerts[0].HandlingRevision != 1 {
		t.Fatalf("promotion lost handling: %v %v", alerts, err)
	}
	if _, err := promoted.s.nodes.HandleAlert(ctx, &nagiv1.HandleAlertRequest{Key: alert.Alert.Key, Occurrence: alert.Alert.Occurrence, HandlingRevision: 1, Action: "conclusion", Conclusion: "checked after promotion"}, "user:43"); err != nil {
		t.Fatal(err)
	}
	if rows := read(promoted.s, "panel:"+a.s.fingerprint, aState.Source, "primary.before.failure"); len(rows) != 1 || rows[0].Seq != first.Seq || !rows[0].Received.Equal(first.Received) {
		t.Fatalf("promotion replaced retained history: %+v", rows)
	}
	if err := standby.Follow(aDir, promoted.s.LocalAddr(), promoted.s.fingerprint); err != nil {
		t.Fatal(err)
	}
	returned := start(aDir, "", aLogs)
	wait("old primary unique history after rejoin", func() bool {
		return len(read(promoted.s, "panel:"+returned.s.fingerprint, unique.Source, "old.primary.unique")) == 1
	})
	rows := read(promoted.s, "panel:"+returned.s.fingerprint, unique.Source, "old.primary.unique")
	if rows[0].Seq != unique.Last {
		t.Fatalf("old primary history renumbered: %+v", rows)
	}
	wait("new primary history on returned old primary", func() bool {
		return len(read(returned.s, "panel:"+promoted.s.fingerprint, bState.Source, "standby.while.primary.down")) == 1
	})
	wait("new primary handling on returned old primary", func() bool {
		states, err := returned.s.nodes.AlertStates(ctx, "", false)
		return err == nil && len(states) == 1 && states[0].Conclusion == "checked after promotion" && states[0].HandlingRevision == 2
	})
}

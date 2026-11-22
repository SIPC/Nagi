package standby

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
)

func TestAuditStreamStopsAfterRevocation(t *testing.T) {
	for _, revoke := range []string{"removed", "database-error"} {
		for _, payload := range []string{"recording", "sink-cursor"} {
			t.Run(revoke+"/"+payload, func(t *testing.T) {
				ctx := t.Context()
				d := openTestDB(t, filepath.Join(t.TempDir(), "panel.db"))
				q := store.New(d)
				log, err := audit.Open(ctx, filepath.Join(t.TempDir(), "audit.db"), slog.Default())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { log.Close() })
				key, err := sealer.Load(filepath.Join(t.TempDir(), "master.key"))
				if err != nil {
					t.Fatal(err)
				}
				rec := &audit.Recordings{Dir: t.TempDir(), Sealer: key}
				if log.Record(ctx, audit.Event{Category: audit.Operation, Action: "BeforeRevocation"}) == 0 {
					t.Fatal("initial audit was not persisted")
				}
				ca, err := tlsutil.NewCA("audit stream test")
				if err != nil {
					t.Fatal(err)
				}
				serverKey, err := tlsutil.GenerateKey()
				if err != nil {
					t.Fatal(err)
				}
				serverCert, err := ca.IssueServer(serverKey, []string{"127.0.0.1"})
				if err != nil {
					t.Fatal(err)
				}
				clientKey, err := tlsutil.GenerateKey()
				if err != nil {
					t.Fatal(err)
				}
				csr, err := tlsutil.CreateCSR(clientKey, "standby")
				if err != nil {
					t.Fatal(err)
				}
				clientCert, err := ca.SignCSR(csr, "standby", time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				fp := tlsutil.Fingerprint(clientCert)
				if err := q.InsertStandbyPanel(ctx, store.InsertStandbyPanelParams{ID: "standby", ServerFp: fp}); err != nil {
					t.Fatal(err)
				}
				ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
					Certificates: []tls.Certificate{{Certificate: [][]byte{serverCert.Raw}, PrivateKey: serverKey}},
					ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: ca.Pool(), NextProtos: []string{AuditALPN},
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { ln.Close() })
				done := make(chan error, 1)
				go func() {
					conn, err := ln.Accept()
					if err != nil {
						done <- err
						return
					}
					tc := conn.(*tls.Conn)
					defer tc.Close()
					if err := tc.HandshakeContext(ctx); err != nil {
						done <- err
						return
					}
					(&Primary{Q: q}).ServeAudit(ctx, tc, log, rec)
					done <- nil
				}()
				cfg := tlsutil.PinnedConfig(tlsutil.Fingerprint(serverCert))
				cfg.Certificates = []tls.Certificate{{Certificate: [][]byte{clientCert.Raw}, PrivateKey: clientKey}}
				cfg.NextProtos = []string{AuditALPN}
				conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", ln.Addr().String(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { conn.Close() })
				if err := writeFrame(conn, []byte(`{"after":0}`)); err != nil {
					t.Fatal(err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				frame, err := readFrame(conn)
				var batch audit.Batch
				if err != nil || json.Unmarshal(frame, &batch) != nil || len(batch.Events) != 1 || batch.Events[0].Action != "BeforeRevocation" {
					t.Fatalf("registered standby did not receive initial audit: %s, %v", frame, err)
				}
				// An authorized connection must keep receiving after its first batch.
				if payload == "recording" {
					summary := rec.WriteOnce(ctx, "authorized recording")
					if summary.ID == "" || log.Record(ctx, audit.Event{Category: audit.Session, Action: "StillRegistered", RecordingID: summary.ID}) == 0 {
						t.Fatal("authorized audit/recording was not persisted")
					}
				} else if err := log.SetMeta(ctx, "sink:test:cursor", "1"); err != nil {
					t.Fatal(err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				frame, err = readFrame(conn)
				if err != nil {
					t.Fatal(err)
				}
				if payload == "recording" {
					batch = audit.Batch{}
					if json.Unmarshal(frame, &batch) != nil || len(batch.Events) != 1 || batch.Events[0].Action != "StillRegistered" || len(batch.Chunks) == 0 {
						t.Fatalf("authorized recording did not arrive: %s", frame)
					}
				} else if !bytes.Equal(frame, audit.CursorFrame(map[string]int64{"sink:test:cursor": 1})) {
					t.Fatalf("authorized sink cursor did not arrive: %s", frame)
				}
				if revoke == "removed" {
					if err := AddRemoved(ctx, q, fp); err != nil {
						t.Fatal(err)
					}
					if n, err := q.DeleteStandbyPanel(ctx, "standby"); err != nil || n != 1 {
						t.Fatalf("remove standby: %d, %v", n, err)
					}
				} else if err := d.Close(); err != nil {
					t.Fatal(err)
				}
				if payload == "recording" {
					summary := rec.WriteOnce(ctx, "secret recording after revocation")
					if summary.ID == "" || log.Record(ctx, audit.Event{Category: audit.Session, Action: "AfterRevocation", RecordingID: summary.ID}) == 0 {
						t.Fatal("post-revocation audit/recording was not persisted")
					}
				} else if err := log.SetMeta(ctx, "sink:test:cursor", "2"); err != nil {
					t.Fatal(err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				frame, err = readFrame(conn)
				if !errors.Is(err, io.EOF) {
					t.Fatalf("revoked stream must close without another frame: bytes=%d secret=%v error=%v", len(frame), bytes.Contains(frame, []byte("AfterRevocation")), err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

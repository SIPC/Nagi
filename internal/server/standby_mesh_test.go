package server

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/mesh"
)

func TestStandbyOwnsPersistentIndependentMesh(t *testing.T) {
	start := func(dir string) *Server {
		t.Helper()
		s, err := New(t.Context(), Config{DataDir: dir, Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			s.listener.Close()
			if s.mesh != nil {
				s.mesh.Close()
			}
			s.recordings.Close()
			s.audit.Close()
			if s.auditReplica != nil {
				s.auditReplica.Close()
			}
			s.db.Close()
		})
		return s
	}
	primary := start(t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "standby.json"), []byte(`{"primary_address":"unreachable:1","primary_fingerprint":"offline-primary"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	standby := start(dir)
	if standby.mesh == nil || standby.nodes.Mesh() != standby.mesh || !standby.nodes.ReadOnly {
		t.Fatal("standby must join mesh without becoming writable or contacting its primary")
	}
	key := standby.mesh.PublicKey()
	if key == (mesh.Key{}) || key == primary.mesh.PublicKey() {
		t.Fatal("standby must not share the primary's mesh identity")
	}
	if standby.nodes.Cluster != nil || standby.nodes.OnReport != nil {
		t.Fatal("standby must not announce primary membership or push project configuration")
	}
	standby.listener.Close()
	standby.mesh.Close()
	standby.recordings.Close()
	standby.audit.Close()
	standby.auditReplica.Close()
	standby.db.Close()
	restarted := start(dir)
	if restarted.mesh == nil || restarted.mesh.PublicKey() != key {
		t.Fatal("standby restart replaced the independent mesh identity")
	}
}

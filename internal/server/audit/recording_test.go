package audit

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/server/sealer"
)

func testRecordings(t *testing.T) *Recordings {
	t.Helper()
	key, err := sealer.Load(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	return &Recordings{Dir: t.TempDir(), Sealer: key}
}

func readAll(t *testing.T, r *Recordings, id string) string {
	t.Helper()
	var out bytes.Buffer
	if err := r.Read(id, func(b []byte) error { out.Write(b); return nil }); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

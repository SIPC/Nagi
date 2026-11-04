package projects

import (
	"log/slog"
	"path/filepath"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
)

// 共享变量的草稿要能原样读回：读成空的话，部署时会把共享变量清空。
func TestSharedEnvDraftRoundTrip(t *testing.T) {
	key, err := sealer.Load(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Sealer: key, Log: slog.Default()}
	const project = "aaaaaaaaaaaaaaaa"
	sealed, err := s.seal(&nagiv1.EnvList{Vars: []*nagiv1.EnvVar{{Key: "A", Value: "1"}, {Key: "B", Value: "2", Secret: true}}}, sharedDraftCtx(project))
	if err != nil {
		t.Fatal(err)
	}
	vars, ok, err := s.sharedEnvDraft(store.Project{ID: project, SharedEnvDraft: sealed})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(vars) != 2 || vars[0].GetKey() != "A" || vars[1].GetValue() != "2" || !vars[1].GetSecret() {
		t.Fatalf("草稿读回的内容不对：%v", vars)
	}
}

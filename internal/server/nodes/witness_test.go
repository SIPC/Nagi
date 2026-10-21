package nodes

import (
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 以前当过见证人留下的旧票，在重新当见证人时换成给的持有者和轮次。
func TestSetWitnessFromReplacesStaleVote(t *testing.T) {
	r := &Registry{}
	r.SetWitnessDir(t.TempDir())
	cfg := &nagiv1.FailoverConfig{ProjectId: "aaaaaaaaaaaaaaaa", Container: "web",
		A: &nagiv1.FailoverParticipant{Key: []byte("node-a")}, B: &nagiv1.FailoverParticipant{Key: []byte("node-b")}, WitnessIsPanel: true}
	if err := r.SetWitness(cfg); err != nil {
		t.Fatal(err)
	}
	if err := r.SetWitnessFrom(cfg, []byte("node-b"), 3); err != nil {
		t.Fatal(err)
	}
	if h, e, ok := r.WitnessRecord("aaaaaaaaaaaaaaaa", "web"); !ok || string(h) != "node-b" || e != 3 {
		t.Fatalf("应换成 B 第 3 轮：%s %d %v", h, e, ok)
	}
	// 更旧的不覆盖（例如保存同一份设置）。
	if err := r.SetWitness(cfg); err != nil {
		t.Fatal(err)
	}
	if h, e, _ := r.WitnessRecord("aaaaaaaaaaaaaaaa", "web"); string(h) != "node-b" || e != 3 {
		t.Fatalf("不应回到 A 第 1 轮：%s %d", h, e)
	}
}

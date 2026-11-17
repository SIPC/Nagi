package projects

import (
	"context"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// R3：顶替以后容器改由 B 运行，之前备份节点上的副本仍然只认原来的 A：被换下的 A 被攻破时还能大批删除副本，
// B 大批清理时反而被拒绝。现在持有方换了节点时更新备份节点上的登记。
func TestHolderChangeRegistersBackups(t *testing.T) {
	ctx := context.Background()
	s := haTestService(t)
	cfg := &nagiv1.FailoverConfig{ProjectId: "aaaaaaaaaaaaaaaa", Container: "web",
		A: &nagiv1.FailoverParticipant{NodeId: "n1", Key: []byte{1}, Dir: "/srv/a"}, B: &nagiv1.FailoverParticipant{NodeId: "n2", Key: []byte{2}, Dir: "/srv/b"},
		Witness: &nagiv1.FailoverParticipant{NodeId: "n3", Key: []byte{3}}}
	b, _ := proto.Marshal(cfg)
	// 见证人 n3 的票是 n2、轮次 5（确认）。
	s.witnessReport = func(string) *nagiv1.ReportRequest {
		return &nagiv1.ReportRequest{Failovers: []*nagiv1.FailoverStatus{{Role: "witness", Config: cfg, HolderKey: []byte{2}, Epoch: 5}}}
	}
	if err := s.Q.SetContainerFailover(ctx, store.SetContainerFailoverParams{Failover: b, FailoverEpoch: 3, ProjectID: "aaaaaaaaaaaaaaaa", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	type call struct {
		name  string
		moved bool
	}
	var calls []call
	s.onHolderChanged = func(_, name string, moved bool) { calls = append(calls, call{name, moved}) }
	// n2 报告自己持有租约、轮次更大：容器换到 n2。
	s.syncFailover(ctx, &nodes.Conn{NodeID: "n2"}, &nagiv1.FailoverStatus{Role: "holder", Epoch: 4}, "aaaaaaaaaaaaaaaa", "web")
	if len(calls) != 1 || !calls[0].moved {
		t.Fatalf("持有方换了节点时应更新备份节点上的登记：%v", calls)
	}
	c, _ := s.container(ctx, "aaaaaaaaaaaaaaaa", "web")
	if c.NodeID != "n2" {
		t.Fatalf("容器应改为在 n2 上：%s", c.NodeID)
	}
	if source, _, err := s.backupSources(ctx, c); err != nil || source.ID != "n2" {
		t.Fatalf("备份节点应改为只认 n2：%v %v", source.ID, err)
	}
	// 同一个节点轮次变大：不用重新登记。
	s.syncFailover(ctx, &nodes.Conn{NodeID: "n2"}, &nagiv1.FailoverStatus{Role: "holder", Epoch: 5}, "aaaaaaaaaaaaaaaa", "web")
	if len(calls) != 2 || calls[1].moved {
		t.Fatalf("持有方没换节点时不用重新登记：%v", calls)
	}
}

// S3：见证人是节点时，之前参与者自报 role=holder、轮次 +1，面板就把它记成持有方，R3 以后还立即让它成为备份节点
// 登记的源节点。现在要见证人节点的上报确认了持有方和轮次，才更新副本登记。
func TestHolderChangeNeedsWitness(t *testing.T) {
	ctx := context.Background()
	s := haTestService(t)
	cfg := &nagiv1.FailoverConfig{ProjectId: "aaaaaaaaaaaaaaaa", Container: "web",
		A: &nagiv1.FailoverParticipant{NodeId: "n1", Key: []byte{1}, Dir: "/srv/a"}, B: &nagiv1.FailoverParticipant{NodeId: "n2", Key: []byte{2}, Dir: "/srv/b"},
		Witness: &nagiv1.FailoverParticipant{NodeId: "n3", Key: []byte{3}}}
	b, _ := proto.Marshal(cfg)
	if err := s.Q.SetContainerFailover(ctx, store.SetContainerFailoverParams{Failover: b, FailoverEpoch: 3, ProjectID: "aaaaaaaaaaaaaaaa", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	// 见证人 n3 的票还在 n1（轮次 3）。
	vote := &nagiv1.FailoverStatus{Role: "witness", Config: cfg, HolderKey: []byte{1}, Epoch: 3}
	s.witnessReport = func(id string) *nagiv1.ReportRequest {
		if id != "n3" {
			return nil
		}
		return &nagiv1.ReportRequest{Failovers: []*nagiv1.FailoverStatus{vote}}
	}
	oldRetry, oldGiveUp := witnessRetry, witnessGiveUp
	witnessRetry, witnessGiveUp = time.Millisecond, 0 // 不在后台等
	t.Cleanup(func() { witnessRetry, witnessGiveUp = oldRetry, oldGiveUp })
	var registered []bool
	s.onHolderChanged = func(_, _ string, register bool) { registered = append(registered, register) }
	// 被攻破的 n2 自称持有方、轮次 4：路由照旧改，副本登记不改。
	s.syncFailover(ctx, &nodes.Conn{NodeID: "n2"}, &nagiv1.FailoverStatus{Role: "holder", Epoch: 4}, "aaaaaaaaaaaaaaaa", "web")
	if len(registered) != 1 || registered[0] {
		t.Fatalf("见证人没确认时不应更新副本登记：%v", registered)
	}
	// 见证人的票改到 n2 以后确认。
	if s.witnessConfirms(cfg, "aaaaaaaaaaaaaaaa", "web", []byte{2}, 4) {
		t.Fatal("见证人的票还在 n1 时不应确认")
	}
	vote.HolderKey, vote.Epoch = []byte{2}, 4
	if !s.witnessConfirms(cfg, "aaaaaaaaaaaaaaaa", "web", []byte{2}, 4) {
		t.Fatal("见证人的票是 n2、轮次 4 时应确认")
	}
	if s.witnessConfirms(cfg, "aaaaaaaaaaaaaaaa", "web", []byte{2}, 5) {
		t.Fatal("轮次比见证人的票大时不应确认")
	}
}

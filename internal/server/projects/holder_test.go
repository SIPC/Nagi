package projects

import (
	"context"
	"testing"

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
	cfg := &nagiv1.FailoverConfig{A: &nagiv1.FailoverParticipant{NodeId: "n1", Dir: "/srv/a"}, B: &nagiv1.FailoverParticipant{NodeId: "n2", Dir: "/srv/b"}}
	b, _ := proto.Marshal(cfg)
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

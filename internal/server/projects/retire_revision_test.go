package projects

import (
	"context"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"google.golang.org/grpc"
)

// revisionAgent 记下退役请求里带的修订号。
type revisionAgent struct {
	nagiv1.AgentServiceClient
	got []int64
}

func (a *revisionAgent) DeleteContainer(_ context.Context, req *nagiv1.DeleteContainerRequest, _ ...grpc.CallOption) (*nagiv1.DeleteContainerResponse, error) {
	a.got = append(a.got, req.GetRevision())
	return &nagiv1.DeleteContainerResponse{}, nil
}

// 让原节点停掉已换到别的节点的容器时，带上核对的是节点上报的哪一份：节点上的比它新（核对以后又部署到了这个节点）时节点会拒绝。
func TestRetireMovedSendsRevision(t *testing.T) {
	s := haTestService(t) // 容器 web 在 n1 上
	const pid = "aaaaaaaaaaaaaaaa"
	agent := &revisionAgent{}
	c := &nodes.Conn{NodeID: "n2", Agent: agent}
	s.retireMoved(context.Background(), c, []*nagiv1.ContainerStatus{{ProjectId: pid, Name: "web", Revision: 3}}, nil)
	if len(agent.got) != 1 || agent.got[0] != 3 {
		t.Fatalf("退役请求应带上节点上报的修订号 3：%v", agent.got)
	}
}

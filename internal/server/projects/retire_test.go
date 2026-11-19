package projects

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc"
)

// retireAgent 记下面板让节点删除容器的调用；block 不为 nil 时每次调用等它关闭（模拟不回应的节点）。
type retireAgent struct {
	nagiv1.AgentServiceClient
	mu      sync.Mutex
	calls   int
	fail    bool
	entered chan struct{}
	block   chan struct{}
}

func (a *retireAgent) DeleteContainer(ctx context.Context, req *nagiv1.DeleteContainerRequest, _ ...grpc.CallOption) (*nagiv1.DeleteContainerResponse, error) {
	a.mu.Lock()
	a.calls++
	fail, entered, block := a.fail, a.entered, a.block
	a.mu.Unlock()
	if !req.GetRetire() {
		return nil, errors.New("应是退役")
	}
	if entered != nil {
		entered <- struct{}{}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail {
		return nil, errors.New("节点出错")
	}
	return &nagiv1.DeleteContainerResponse{}, nil
}

func (a *retireAgent) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// 让原节点停掉已换到别的节点的容器时不拿着项目锁调用节点：节点不回应也不拖住项目的部署。
// 刚停掉的容器很快又报上来不管；过一会儿还报的记越权、不再调用节点；调用失败的下次照常重试。
func TestRetireMovedOutsideLock(t *testing.T) {
	ctx := context.Background()
	s := haTestService(t) // 容器 web 在 n1 上
	const pid = "aaaaaaaaaaaaaaaa"
	var foreign []string
	s.onForeignReport = func(nodeID, container string) { foreign = append(foreign, nodeID+" "+container) }
	agent := &retireAgent{entered: make(chan struct{}, 1), block: make(chan struct{})}
	c := &nodes.Conn{NodeID: "n2", Agent: agent}
	reported := []*nagiv1.ContainerStatus{{ProjectId: pid, Name: "web"}}

	done := make(chan struct{})
	go func() {
		s.retireMoved(ctx, c, reported, nil)
		close(done)
	}()
	select {
	case <-agent.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("没有让 n2 停掉容器")
	}
	// 节点还没回应：项目锁是空的，部署不用等。
	l := s.lock(pid)
	if !l.TryLock() {
		t.Fatal("调用节点时还拿着项目锁")
	}
	l.Unlock()
	close(agent.block)
	<-done
	agent.mu.Lock()
	agent.entered, agent.block = nil, nil
	agent.mu.Unlock()

	// 刚停掉又报上来（可能是停掉之前发出的上报）：不调用、不记越权。
	s.retireMoved(ctx, c, reported, nil)
	if agent.count() != 1 || len(foreign) != 0 {
		t.Fatalf("刚停掉的不应再处理：calls=%d foreign=%v", agent.count(), foreign)
	}
	key := "n2/" + pid + "/web"
	setRetired := func(ago time.Duration) {
		s.mu.Lock()
		s.retired[key] = time.Now().Add(-ago)
		s.mu.Unlock()
	}
	// 停掉 30 秒以后还报：记越权，不再调用节点。
	setRetired(retireSettle + time.Second)
	s.retireMoved(ctx, c, reported, nil)
	if agent.count() != 1 || len(foreign) != 1 || foreign[0] != "n2 "+pid+"/web" {
		t.Fatalf("停掉以后还报应记越权：calls=%d foreign=%v", agent.count(), foreign)
	}
	// 过了 10 分钟：当作新的一次，再让它停一次。
	setRetired(retireMemory + time.Second)
	s.retireMoved(ctx, c, reported, nil)
	if agent.count() != 2 || len(foreign) != 1 {
		t.Fatalf("过了 10 分钟应再让它停一次：calls=%d foreign=%v", agent.count(), foreign)
	}

	// 调用失败的不记下，下次照常重试，也不算越权。
	s.mu.Lock()
	s.retired = nil
	s.mu.Unlock()
	agent.mu.Lock()
	agent.fail = true
	agent.mu.Unlock()
	s.retireMoved(ctx, c, reported, nil)
	s.retireMoved(ctx, c, reported, nil)
	if agent.count() != 4 || len(foreign) != 1 {
		t.Fatalf("失败的应每次重试：calls=%d foreign=%v", agent.count(), foreign)
	}

	// 容器就在上报的节点上：不处理。
	s.retireMoved(ctx, &nodes.Conn{NodeID: "n1", Agent: agent}, reported, nil)
	if agent.count() != 4 {
		t.Fatalf("容器在上报的节点上时不应让它停：%d", agent.count())
	}
}

// 逐个让原节点停掉容器时，前一个还没回应、后一个被迁回了这个节点：轮到它时要按此刻的记录核对，不能再停。
func TestRetireMovedRechecksEach(t *testing.T) {
	ctx := context.Background()
	s := haTestService(t) // 容器 web 在 n1 上
	const pid = "aaaaaaaaaaaaaaaa"
	err := s.Q.InsertContainer(ctx, store.InsertContainerParams{ProjectID: pid, Name: "api", Position: 2, NodeID: "n1", Dir: "/srv/api", WebhookHash: "h2", Secrets: []byte{}})
	if err != nil {
		t.Fatal(err)
	}
	agent := &retireAgent{entered: make(chan struct{}, 2), block: make(chan struct{})}
	c := &nodes.Conn{NodeID: "n2", Agent: agent}
	reported := []*nagiv1.ContainerStatus{{ProjectId: pid, Name: "web"}, {ProjectId: pid, Name: "api"}}

	done := make(chan struct{})
	go func() {
		s.retireMoved(ctx, c, reported, nil)
		close(done)
	}()
	select {
	case <-agent.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("没有让 n2 停掉 web")
	}
	// web 的请求还没回应，api 迁回了 n2。
	if err := s.Q.MoveContainer(ctx, store.MoveContainerParams{NodeID: "n2", Dir: "/srv/api", ProjectID: pid, Name: "api"}); err != nil {
		t.Fatal(err)
	}
	close(agent.block)
	<-done
	if n := agent.count(); n != 1 {
		t.Fatalf("迁回 n2 的 api 不应被停掉：调用了 %d 次", n)
	}
}

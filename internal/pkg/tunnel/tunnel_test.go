package tunnel

import (
	"context"
	"net"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
)

type panelSrv struct {
	nagiv1.UnimplementedPanelServiceServer
	got chan string
}

func (p *panelSrv) Report(_ context.Context, r *nagiv1.ReportRequest) (*nagiv1.ReportResponse, error) {
	p.got <- r.GetNode().GetHostname()
	return &nagiv1.ReportResponse{}, nil
}

type agentSrv struct {
	nagiv1.UnimplementedAgentServiceServer
}

func (agentSrv) GetMirrors(context.Context, *nagiv1.GetMirrorsRequest) (*nagiv1.Mirrors, error) {
	return &nagiv1.Mirrors{Mirrors: []string{"p1"}}, nil
}

// 两端在同一条连接上互相调用。
func TestBidirectionalGRPC(t *testing.T) {
	a, b := net.Pipe()
	panelSess, err := Server(a)
	if err != nil {
		t.Fatal(err)
	}
	agentSess, err := Client(b)
	if err != nil {
		t.Fatal(err)
	}
	defer panelSess.Close()
	defer agentSess.Close()

	ps := &panelSrv{got: make(chan string, 1)}
	pg := grpc.NewServer()
	nagiv1.RegisterPanelServiceServer(pg, ps)
	go Serve(panelSess, pg)

	ag := grpc.NewServer()
	nagiv1.RegisterAgentServiceServer(ag, agentSrv{})
	go Serve(agentSess, ag)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 节点调用面板
	ac, err := Dial(agentSess)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	if _, err := nagiv1.NewPanelServiceClient(ac).Report(ctx, &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{Hostname: "h1"}}); err != nil {
		t.Fatal(err)
	}
	if h := <-ps.got; h != "h1" {
		t.Fatalf("hostname = %q", h)
	}

	// 面板反向调用节点
	pc, err := Dial(panelSess)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	resp, err := nagiv1.NewAgentServiceClient(pc).GetMirrors(ctx, &nagiv1.GetMirrorsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Mirrors) != 1 || resp.Mirrors[0] != "p1" {
		t.Fatalf("unexpected response: %v", resp)
	}
}

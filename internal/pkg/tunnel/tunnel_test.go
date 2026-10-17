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

func (agentSrv) ListProjects(context.Context, *nagiv1.ListProjectsRequest) (*nagiv1.ListProjectsResponse, error) {
	return &nagiv1.ListProjectsResponse{Projects: []*nagiv1.ProjectSummary{{Project: &nagiv1.Project{Id: "p1"}}}}, nil
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
	resp, err := nagiv1.NewAgentServiceClient(pc).ListProjects(ctx, &nagiv1.ListProjectsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Projects) != 1 || resp.Projects[0].Project.Id != "p1" {
		t.Fatalf("unexpected response: %v", resp)
	}
}

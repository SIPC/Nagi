package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
)

// metricsNodes 模拟面板的监控接口：series 按请求的节点 ID 给（空的 ID 是成员看不到的节点合在一起）；
// visible 为 nil 时 ListNodes 没有权限（成员没有「查看节点」）。
type metricsNodes struct {
	nagiv1connect.NodeServiceClient
	visible []*nagiv1.Node
	series  map[string][]*nagiv1.MetricSeries
	asked   []string
}

func (f *metricsNodes) ListNodes(context.Context, *connect.Request[nagiv1.ListNodesRequest]) (*connect.Response[nagiv1.ListNodesResponse], error) {
	if f.visible == nil {
		return nil, connect.NewError(connect.CodePermissionDenied, nil)
	}
	return connect.NewResponse(&nagiv1.ListNodesResponse{Nodes: f.visible}), nil
}

func (f *metricsNodes) QueryNodeMetrics(_ context.Context, req *connect.Request[nagiv1.QueryNodeMetricsRequest]) (*connect.Response[nagiv1.QueryMetricsResponse], error) {
	f.asked = append(f.asked, req.Msg.GetNodeId()+"|"+req.Msg.GetPrefix())
	var out []*nagiv1.MetricSeries
	for _, s := range f.series[req.Msg.GetNodeId()] {
		if strings.HasPrefix(s.GetName(), req.Msg.GetPrefix()) {
			out = append(out, s)
		}
	}
	return connect.NewResponse(&nagiv1.QueryMetricsResponse{Series: out}), nil
}

// metricsProjects 给一个项目 p1，容器 web 在成员看不到的节点上（面板清掉了节点 ID），也没有映射列表（没有「网络和域名」）。
type metricsProjects struct {
	nagiv1connect.ProjectServiceClient
}

func metricsProject() *nagiv1.ProjectSummary {
	return &nagiv1.ProjectSummary{Project: &nagiv1.Project{Id: "p1", Name: "shop", NetworkName: "shop",
		Containers: []*nagiv1.Container{{Name: "web"}}}}
}

func (metricsProjects) ListProjects(context.Context, *connect.Request[nagiv1.ListProjectsRequest]) (*connect.Response[nagiv1.ListProjectsResponse], error) {
	return connect.NewResponse(&nagiv1.ListProjectsResponse{Projects: []*nagiv1.ProjectSummary{metricsProject()}}), nil
}

func (metricsProjects) GetProject(context.Context, *connect.Request[nagiv1.ProjectRef]) (*connect.Response[nagiv1.ProjectSummary], error) {
	return connect.NewResponse(metricsProject()), nil
}

func series(name string, vals ...float64) *nagiv1.MetricSeries {
	s := &nagiv1.MetricSeries{Name: name}
	for i, v := range vals {
		s.Times, s.Values = append(s.Times, int64(60*(i+1))), append(s.Values, v)
	}
	return s
}

func metricsCall(f *metricsNodes) *call {
	u := auth.User{ID: 2, Role: auth.RoleMember, Perms: auth.PermsOf(auth.PermView), TokenID: 1}
	return &call{ctx: context.Background(), u: u, h: &handler{clients: clients{nodes: f, projects: metricsProjects{}}}}
}

type metricsOut struct {
	Node   string          `json:"node"`
	Series []metricSummary `json:"series"`
}

func decodeMetrics(t *testing.T, v any) metricsOut {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out metricsOut
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func findSeries(t *testing.T, v any, name string) metricSummary {
	t.Helper()
	list := decodeMetrics(t, v).Series
	for _, s := range list {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("没有序列 %s：%#v", name, list)
	return metricSummary{}
}

// 只有项目「查看」权限（没有「查看节点」）也能用 query_metrics 查容器的占用和端口映射，查不了节点本身。
func TestQueryMetricsViewOnly(t *testing.T) {
	u := auth.User{ID: 2, Role: auth.RoleMember, Perms: auth.PermsOf(auth.PermView), TokenID: 1}
	for _, tl := range allTools {
		switch tl.name {
		case "query_metrics":
			if !tl.allowedFor(u, false) {
				t.Error("只有「查看」的令牌应该能用 query_metrics")
			}
		case "list_nodes":
			if tl.allowedFor(u, false) {
				t.Error("没有「查看节点」不应该有 list_nodes")
			}
		}
	}

	f := &metricsNodes{series: map[string][]*nagiv1.MetricSeries{
		"": {series("c:p1/web/cpu", 10, 30), series("r:p1/web:req", 4)},
	}}
	v, err := toolQueryMetrics(metricsCall(f), MetricsArgs{Project: "shop", Container: "web"})
	if err != nil {
		t.Fatalf("查容器不应该要求「查看节点」：%v", err)
	}
	if s := findSeries(t, v, "cpu"); s.Max != 30 || s.Avg != 20 {
		t.Errorf("cpu = %+v", s)
	}
	if node := decodeMetrics(t, v).Node; node != otherNode {
		t.Errorf("看不到的节点应该显示 %q，实际 %q", otherNode, node)
	}
	if want := "|c:p1/web/"; len(f.asked) != 1 || f.asked[0] != want {
		t.Errorf("应该不填节点、按容器的前缀查：%v", f.asked)
	}

	f.asked = nil
	v, err = toolQueryMetrics(metricsCall(f), MetricsArgs{Project: "shop", Route: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if s := findSeries(t, v, "web:req"); s.Last != 4 {
		t.Errorf("web:req = %+v", s)
	}
	if want := "|r:p1/"; len(f.asked) != 1 || f.asked[0] != want {
		t.Errorf("列不出节点时只查看不到的节点：%v", f.asked)
	}

	if _, err := toolQueryMetrics(metricsCall(f), MetricsArgs{Node: "n1"}); err == nil {
		t.Error("没有「查看节点」不能查节点")
	}
}

// 端口映射的统计记在各个入口节点上：请求数相加，耗时取最大；只留指定的映射。
func TestQueryRouteMetricsMerge(t *testing.T) {
	f := &metricsNodes{
		visible: []*nagiv1.Node{{Id: "n1", Name: "hk"}, {Id: "n2", Name: "sg"}},
		series: map[string][]*nagiv1.MetricSeries{
			"n1": {series("r:p1/web:req", 3, 1), series("r:p1/web:p95", 100, 20), series("r:p1/api:req", 50)},
			"n2": {series("r:p1/web:req", 5), series("r:p1/web:p95", 400)},
			"":   {series("r:p1/web:req", 7), series("r:p1/web:p95", 250)},
		},
	}
	v, err := toolQueryMetrics(metricsCall(f), MetricsArgs{Project: "shop", Route: "web"})
	if err != nil {
		t.Fatal(err)
	}
	// 第一个点三台相加 3+5+7，第二个点只有 n1 有。
	if s := findSeries(t, v, "web:req"); len(s.Points) != 2 || s.Points[0][1] != 15 || s.Points[1][1] != 1 || s.Unit != "requests/s" {
		t.Errorf("web:req = %+v", s)
	}
	if s := findSeries(t, v, "web:p95"); s.Points[0][1] != 400 || s.Unit != "ms" {
		t.Errorf("p95 应该取最大值：%+v", s)
	}
	for _, s := range decodeMetrics(t, v).Series {
		if strings.HasPrefix(s.Name, "api:") {
			t.Errorf("只查 web 时不应该有 %s", s.Name)
		}
	}

	v, err = toolQueryMetrics(metricsCall(f), MetricsArgs{Project: "shop", Route: "*"})
	if err != nil {
		t.Fatal(err)
	}
	findSeries(t, v, "api:req")

	if _, err := toolQueryMetrics(metricsCall(f), MetricsArgs{Project: "shop", Route: "nope"}); err == nil {
		t.Error("没有数据的映射应该报错")
	}
}

package mcp

import (
	"math"
	"strings"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 排查用的只读工具：端口映射的域名检查、项目事件、节点磁盘明细。

type RoutesArgs struct {
	ProjectArg
	Container string `json:"container,omitempty" jsonschema:"only the mappings of this container"`
}

// toolCheckRoutes 用面板自己的域名检查（解析是否指向入口节点、证书状态）。不按 agent 给的地址发请求，不能被拿来探测内网。
func toolCheckRoutes(c *call, in RoutesArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	var routes []*nagiv1.Exposure
	for _, r := range p.GetRoutes() {
		if in.Container == "" || r.GetContainer() == in.Container {
			routes = append(routes, r)
		}
	}
	type entry struct {
		Node      string `json:"node"`
		Address   string `json:"address,omitempty"`
		DNS       string `json:"dns"`
		Cert      string `json:"cert"`
		CertError string `json:"cert_error,omitempty"`
		CertUntil string `json:"cert_not_after,omitempty"`
	}
	type domain struct {
		Route     string   `json:"route"`
		Domain    string   `json:"domain"`
		Addresses []string `json:"resolves_to"`
		Entries   []entry  `json:"entry_nodes"`
	}
	out := struct {
		Routes  []routeView `json:"routes"`
		Domains []domain    `json:"domains"`
		Legend  string      `json:"legend"`
	}{Routes: []routeView{}, Domains: []domain{},
		Legend: "dns: ok (points to the entry node), mismatch (points elsewhere, e.g. a CDN), unresolved, unknown; cert: obtained, pending, failed, not_needed (HTTP only), unknown"}
	for _, r := range routes {
		out.Routes = append(out.Routes, routeOf(r))
	}
	if len(routes) == 0 {
		return out, nil
	}
	res, err := c.h.clients.projects.CheckDomains(c.ctx, connect.NewRequest(&nagiv1.CheckDomainsRequest{ProjectId: p.GetId(), Routes: routes}))
	if err != nil {
		return nil, err
	}
	for _, d := range res.Msg.GetDomains() {
		v := domain{Route: d.GetRoute(), Domain: d.GetDomain(), Addresses: d.GetAddresses(), Entries: []entry{}}
		for _, e := range d.GetEntries() {
			v.Entries = append(v.Entries, entry{Node: e.GetNodeName(), Address: e.GetAddress(), DNS: e.GetDns(), Cert: e.GetCert(), CertError: e.GetCertError(), CertUntil: ts(e.GetCertNotAfter())})
		}
		out.Domains = append(out.Domains, v)
	}
	return out, nil
}

type EventsArgs struct {
	ProjectArg
	Container  string `json:"container,omitempty" jsonschema:"only events of this container"`
	Limit      int    `json:"limit,omitempty" jsonschema:"default 50, at most 200"`
	Before     int64  `json:"before,omitempty" jsonschema:"page: only events with an ID below this (use next_before from the previous page)"`
	FailedOnly bool   `json:"failed_only,omitempty"`
}

func toolQueryEvents(c *call, in EventsArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	container := in.Container
	if container != "" {
		m, err := findContainer(s.GetProject(), container)
		if err != nil {
			return nil, err
		}
		container = m.GetName()
	}
	res, err := c.h.clients.projects.ListProjectEvents(c.ctx, connect.NewRequest(&nagiv1.ListProjectEventsRequest{
		ProjectId: s.GetProject().GetId(), Container: container, Limit: int32(in.Limit), Before: in.Before, FailedOnly: in.FailedOnly,
	}))
	if err != nil {
		return nil, err
	}
	type event struct {
		ID     int64  `json:"id"`
		Time   string `json:"time"`
		Kind   string `json:"kind"`
		Actor  string `json:"actor,omitempty"`
		Action string `json:"action"`
		Target string `json:"target,omitempty"`
		Detail string `json:"detail,omitempty"`
		Failed bool   `json:"failed,omitempty"`
		Via    string `json:"via,omitempty"`
	}
	out := struct {
		Events     []event `json:"events"`
		NextBefore int64   `json:"next_before,omitempty"`
	}{Events: []event{}}
	for _, e := range res.Msg.GetEntries() {
		out.Events = append(out.Events, event{ID: e.GetId(), Time: ts(e.GetTime()), Kind: e.GetKind(), Actor: e.GetActor(), Action: e.GetAction(),
			Target: e.GetTarget(), Detail: e.GetDetail(), Failed: e.GetFailed(), Via: e.GetVia()})
	}
	if n := len(out.Events); n > 0 {
		out.NextBefore = out.Events[n-1].ID
	}
	return out, nil
}

type NodeArg struct {
	Node string `json:"node" jsonschema:"node name or ID"`
}

// toolInspectNodeStorage 只给管理员：节点上所有项目的镜像和容器目录都在里面。只读，不清理。
func toolInspectNodeStorage(c *call, in NodeArg) (any, error) {
	nodes, err := c.nodes()
	if err != nil {
		return nil, err
	}
	n, err := findNode(nodes, in.Node)
	if err != nil {
		return nil, err
	}
	res, err := c.h.clients.nodes.GetNodeStorage(c.ctx, connect.NewRequest(&nagiv1.NodeRef{NodeId: n.GetId()}))
	if err != nil {
		return nil, err
	}
	st := res.Msg
	gb := func(b int64) float64 { return math.Round(float64(b)/(1<<30)*100) / 100 }
	type total struct {
		Count         int64   `json:"count"`
		Active        int64   `json:"active"`
		GB            float64 `json:"gb"`
		ReclaimableGB float64 `json:"reclaimable_gb"`
	}
	tot := func(t *nagiv1.StorageTotal) total {
		return total{Count: t.GetCount(), Active: t.GetActive(), GB: gb(t.GetBytes()), ReclaimableGB: gb(t.GetReclaimableBytes())}
	}
	type disk struct {
		Mount   string  `json:"mount"`
		UsedGB  float64 `json:"used_gb"`
		TotalGB float64 `json:"total_gb"`
		Percent float64 `json:"used_percent"`
	}
	type image struct {
		Name       string  `json:"name"`
		GB         float64 `json:"gb"`
		Containers int64   `json:"containers_using"`
	}
	type dir struct {
		Project   string  `json:"project"`
		Container string  `json:"container"`
		Dir       string  `json:"dir"`
		DataGB    float64 `json:"data_gb"`
		VolumesGB float64 `json:"volumes_gb"`
		BackupsGB float64 `json:"snapshots_gb"`
		Partial   bool    `json:"incomplete,omitempty"`
	}
	out := struct {
		Node          string  `json:"node"`
		Disks         []disk  `json:"disks"`
		Images        total   `json:"docker_images"`
		Containers    total   `json:"docker_container_layers"`
		Volumes       total   `json:"docker_volumes"`
		BuildCache    total   `json:"docker_build_cache"`
		LargestImages []image `json:"largest_images"`
		ContainerDirs []dir   `json:"container_dirs"`
		ReplicasGB    float64 `json:"replicas_gb"`
		DockerError   string  `json:"docker_error,omitempty"`
		Note          string  `json:"note"`
	}{Node: n.GetName(), Disks: []disk{}, Images: tot(st.GetImages()), Containers: tot(st.GetContainers()), Volumes: tot(st.GetVolumes()),
		BuildCache: tot(st.GetBuildCache()), LargestImages: []image{}, ContainerDirs: []dir{}, ReplicasGB: gb(st.GetReplicasBytes()), DockerError: st.GetDockerError(),
		Note: "read-only. Reclaimable image space and build cache are usually safe to clean on the node; volumes, container data and snapshots can hold business data. Nothing is cleaned through MCP."}
	for _, d := range st.GetDisks() {
		pct := 0.0
		if d.GetTotalBytes() > 0 {
			pct = math.Round(float64(d.GetUsedBytes())/float64(d.GetTotalBytes())*1000) / 10
		}
		out.Disks = append(out.Disks, disk{Mount: d.GetMount(), UsedGB: gb(int64(d.GetUsedBytes())), TotalGB: gb(int64(d.GetTotalBytes())), Percent: pct})
	}
	for _, im := range st.GetLargestImages() {
		out.LargestImages = append(out.LargestImages, image{Name: im.GetName(), GB: gb(im.GetBytes()), Containers: im.GetContainers()})
	}
	for _, d := range st.GetContainerDirs() {
		project := d.GetProjectName()
		if project == "" {
			project = d.GetProjectId()
		}
		out.ContainerDirs = append(out.ContainerDirs, dir{Project: strings.TrimSpace(project), Container: d.GetContainer(), Dir: d.GetDir(),
			DataGB: gb(d.GetDataBytes()), VolumesGB: gb(d.GetVolumesBytes()), BackupsGB: gb(d.GetBackupsBytes()), Partial: d.GetPartial()})
	}
	return out, nil
}

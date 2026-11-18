package api

import (
	"context"
	"encoding/hex"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 按权限和访问范围清理回复（见 docs/panel.md#成员和角色、#访问范围）。所有服务（ProjectService、NodeService、
// SettingsService、AppService）的回复都经过 scopeCheck 交到这里，MCP 在进程内调用同一套接口，App 也一样。
// 管理员不经过这里。
//
// 规则：
//   - 没有相应权限时去掉字段：配置和变量（查看配置）、备份、顶替、端口映射和连接（网络和域名）、告警规则、快照计划；
//   - 看不到的节点（范围外，或者没有「查看节点」）一律是「其他节点」：ID、名称、地址、目录、公钥都不返回，也不给是否在线；
//   - 节点和面板的 IP 一律不返回，例外是有「查看访问地址」时域名检查里的地址；
//   - 节点详情（主机名、系统、版本、路径、挂载点）要「查看节点详情」。
//
// clean_test.go 列出回复里所有带地址、节点引用的字段（cleanedFields），新增字段忘了登记时测试失败。

// cleanedFields 登记回复里带地址、节点引用、路径的字段和处理方式。键是 消息.字段。
var cleanedFields = map[string]string{
	// ---- 项目 ----
	"nagi.v1.Container.node_id":               "看不到的节点清空（其他节点）",
	"nagi.v1.Container.dir":                   "要查看配置，节点看不到时清空",
	"nagi.v1.Container.backup_node_id":        "要备份权限；看不到的清空",
	"nagi.v1.Container.backup_node_ids":       "要备份权限；看不到的换成空字符串",
	"nagi.v1.Container.backup_replica_dirs":   "要备份权限；看不到的节点去掉",
	"nagi.v1.Container.failover_witness_self": "要高可用权限",
	"nagi.v1.Container.deploy_key":            "要查看配置",
	"nagi.v1.Container.webhook_token":         "要查看配置",
	"nagi.v1.BackupStatus.target_node_id":     "看不到的清空",
	"nagi.v1.BackupStatus.target_name":        "看不到的清空",
	"nagi.v1.BackupStatus.last_error":         "去掉看不到的节点名",
	"nagi.v1.BackupStatus.retry_error":        "去掉看不到的节点名",
	"nagi.v1.FailoverParticipant.key":         "看不到的清空",
	"nagi.v1.FailoverParticipant.node_id":     "看不到的清空",
	"nagi.v1.FailoverParticipant.name":        "看不到的清空",
	"nagi.v1.FailoverParticipant.dir":         "看不到的清空；看得到的要查看配置",
	"nagi.v1.FailoverConfig.witness_is_panel": "要高可用权限（整个 FailoverConfig）",
	"nagi.v1.FailoverStatus.holder_name":      "看不到的清空",
	"nagi.v1.FailoverStatus.holder_key":       "看不到的清空",
	"nagi.v1.FailoverStatus.last_error":       "去掉看不到的节点名",
	"nagi.v1.PendingHA.backup_node_id":        "要备份权限；看不到的清空",
	"nagi.v1.PendingHA.backup_node_ids":       "要备份权限；看不到的换成空字符串",
	"nagi.v1.Exposure.entry_nodes":            "要网络和域名；看不到的换成空字符串",
	"nagi.v1.Exposure.domains":                "要网络和域名（整个 routes）",
	"nagi.v1.Exposure.published":              "要网络和域名（整个 routes）；节点地址不在这里",
	"nagi.v1.Exposure.target":                 "要网络和域名（整个 routes）",
	"nagi.v1.ProjectSettings.internal_ports":  "要网络和域名",
	"nagi.v1.Egress.allow":                    "要网络和域名（整个 egress）",
	"nagi.v1.LinkedProject.network_name":      "保留（配置里要用，C6）",
	"nagi.v1.LinkedProject.name":              "范围外的项目清空",
	"nagi.v1.ContainerConfig.project_dir":     "要查看配置（整个 config）",
	"nagi.v1.ServiceStatus.ports":             "保留：容器声明的端口，不是地址",
	"nagi.v1.ServiceStatus.udp_ports":         "保留",
	"nagi.v1.ServiceStatus.container_id":      "保留",
	"nagi.v1.PortMapping.published":           "要查看配置（整个 config）",
	"nagi.v1.PortMapping.target":              "要查看配置（整个 config）",
	"nagi.v1.ImageForm.workdir":               "要查看配置（整个 config）",
	"nagi.v1.DomainCheck.addresses":           "要查看访问地址",
	"nagi.v1.DomainEntryCheck.node_id":        "看不到的清空",
	"nagi.v1.DomainEntryCheck.node_name":      "看不到的清空",
	"nagi.v1.DomainEntryCheck.address":        "要查看访问地址，看不到的节点清空",
	"nagi.v1.DomainEntryCheck.cert_error":     "去掉 IP（C7）和看不到的节点名",
	"nagi.v1.DeployEvent.output":              "去掉看不到的节点名；IP 保留（B10，见文档）",
	"nagi.v1.DeployResult.error":              "去掉看不到的节点名",
	"nagi.v1.ActivityEntry.actor":             "去掉看不到的节点名（告警的发出节点）",
	"nagi.v1.ActivityEntry.target":            "去掉看不到的节点名",
	"nagi.v1.ActivityEntry.detail":            "去掉看不到的节点名",
	"nagi.v1.ListVolumesResponse.volumes":     "保留：容器自己的卷",
	"nagi.v1.VolumeInfo.host_path":            "要查看节点详情",
	"nagi.v1.ImageUpdate.current_digest":      "保留",
	"nagi.v1.ImageUpdate.latest_digest":       "保留",

	// ---- 节点 ----
	"nagi.v1.ListNodesResponse.other_nodes":       "不再返回（看不到的节点一律是「其他节点」，不给是否在线）",
	"nagi.v1.Node.id":                             "看不到的节点整条去掉",
	"nagi.v1.Node.address":                        "非管理员清空（B1）",
	"nagi.v1.Node.unknown_containers":             "非管理员清空（C5）",
	"nagi.v1.Node.stale_projects":                 "非管理员清空（C5）",
	"nagi.v1.Node.alert_rules":                    "要节点管理",
	"nagi.v1.Node.own_embedded":                   "保留",
	"nagi.v1.NodeInfo.hostname":                   "要查看节点详情（C1）",
	"nagi.v1.NodeInfo.projects_root":              "要查看节点详情",
	"nagi.v1.NodeInfo.replica_dir":                "要查看节点详情",
	"nagi.v1.NodeInfo.in_container":               "要查看节点详情",
	"nagi.v1.NodeInfo.embedded":                   "保留",
	"nagi.v1.DirectDownload.address":              "非管理员清空（B1）",
	"nagi.v1.DirectDownload.http_port":            "保留：端口号",
	"nagi.v1.DirectDownload.error":                "去掉 IP",
	"nagi.v1.DirectDownload.http_error":           "去掉 IP",
	"nagi.v1.DiskUsage.mount":                     "要查看节点详情",
	"nagi.v1.MeshStatus.address":                  "非管理员清空（B6）",
	"nagi.v1.MeshStatus.port":                     "非管理员清空",
	"nagi.v1.MeshStatus.endpoints":                "非管理员清空",
	"nagi.v1.MeshStatus.public_key":               "保留：节点自己的 mesh 公钥",
	"nagi.v1.MeshPeerStatus.public_key":           "只留看得到的节点（A4）",
	"nagi.v1.MeshPeerStatus.name":                 "只留看得到的节点",
	"nagi.v1.MeshPeerStatus.address":              "非管理员清空",
	"nagi.v1.MeshPeerStatus.endpoint":             "非管理员清空",
	"nagi.v1.MeshPeerStatus.local_port":           "非管理员清空",
	"nagi.v1.MeshPeerStatus.via":                  "看不到的节点、面板换成空",
	"nagi.v1.MetricSeries.name":                   "到看不到的节点的 m: 序列合并成 m:other（A5）",
	"nagi.v1.Replica.source_node_id":              "看不到的清空（A6）",
	"nagi.v1.Replica.source_name":                 "看不到的清空",
	"nagi.v1.Replica.dir":                         "要查看节点详情",
	"nagi.v1.Replica.project_name":                "范围外的项目整条去掉",
	"nagi.v1.TrustedPanel.address":                "接口只给管理员",
	"nagi.v1.TrustedPanel.fingerprint":            "接口只给管理员",
	"nagi.v1.TrustedPanel.last_error":             "接口只给管理员",
	"nagi.v1.ListNodePanelsResponse.panels":       "接口只给管理员",
	"nagi.v1.CreateEnrollCommandResponse.command": "接口只给管理员",
	"nagi.v1.HandoverResult.node_id":              "接口只给管理员",
	"nagi.v1.UnknownContainer.project_name":       "非管理员清空",
	"nagi.v1.ComposeDirPreview.path":              "接口要节点终端和文件",

	// ---- 面板 ----
	"nagi.v1.PanelSettings.public_address":        "非管理员清空（B3）",
	"nagi.v1.PanelSettings.port":                  "非管理员清空",
	"nagi.v1.PanelSettings.fingerprint":           "非管理员清空",
	"nagi.v1.PanelSettings.domain":                "要面板设置",
	"nagi.v1.PanelSettings.master_key_path":       "非管理员清空",
	"nagi.v1.PanelSettings.default_name":          "非管理员清空（C3，主机名）",
	"nagi.v1.GetStandbysResponse.primary_address": "非管理员只给域名（B4）",
	"nagi.v1.GetStandbysResponse.last_error":      "非管理员清空",
	"nagi.v1.StandbyPanel.address":                "非管理员只留绑了域名的",
	"nagi.v1.AppInfoResponse.public_address":      "非管理员：面板绑了域名时清空（B11）",
	"nagi.v1.AppInfoResponse.domain":              "保留",
	"nagi.v1.AuditEvent.ip":                       "非管理员清空（audit_api.go）",
	"nagi.v1.AuditEvent.actor_name":               "去掉看不到的节点名（audit_api.go）",
	"nagi.v1.AuditEvent.target_id":                "看不到的节点清空（audit_api.go）",
	"nagi.v1.AuditEvent.target_name":              "去掉看不到的节点名（audit_api.go）",
	"nagi.v1.AuditEvent.params":                   "非管理员去掉地址类参数（B9）和看不到的节点（audit_api.go）",
	"nagi.v1.AuditEvent.error":                    "去掉看不到的节点名（audit_api.go）",
	"nagi.v1.LoginSession.ip":                     "保留：本人的登录设备",
	"nagi.v1.LoginSession.last_ip":                "保留：本人的登录设备",
	"nagi.v1.AccessToken.last_ip":                 "保留：本人的令牌（看别人的只给管理员）",
	"nagi.v1.AccessToken.networks":                "保留：本人的令牌",
	"nagi.v1.AdminNetworks.networks":              "接口要安全设置",
	"nagi.v1.AlertChannel.url":                    "接口要面板设置",
	"nagi.v1.AuditSink.address":                   "接口要审计日志",
	"nagi.v1.AccessToken.allow_node_exec":         "保留：令牌的选项",
	"nagi.v1.ActivityEntry.via":                   "保留：来源（网页、MCP 令牌名）",
	"nagi.v1.AuditEvent.via":                      "保留：来源",
	"nagi.v1.AlertConfig.node_name":               "接口要面板设置；只在下发给节点时填写",
	"nagi.v1.Container.failover_holder":           "要高可用权限；持有者看不到时清空名称和公钥",
	"nagi.v1.Container.host_network_services":     "保留：服务名",
	"nagi.v1.ContainerDirUsage.dir":               "接口要节点管理（整个节点的磁盘明细）",
	"nagi.v1.NodeStorage.container_dirs":          "接口要节点管理（整个节点的磁盘明细）",
	"nagi.v1.FailoverConfig.witness":              "要高可用权限；看不到的见证节点清空",
	"nagi.v1.GetUpdateStatusResponse.nodes":       "接口要面板设置；看不到的节点去掉",
	"nagi.v1.ListNodesResponse.nodes":             "只留看得到的节点",
	"nagi.v1.NodeFileEntry.is_dir":                "接口要节点终端和文件",
	"nagi.v1.VolumeFileEntry.is_dir":              "保留",
	"nagi.v1.NodeInfo.cross_node_entry":           "保留：能力标记",
	"nagi.v1.NodeInfo.node_access":                "保留：能力标记",
	"nagi.v1.NodeInfo.node_access_disabled":       "保留：能力标记",
	"nagi.v1.NodeInfo.replica_dirs":               "保留：能力标记",
	"nagi.v1.Replica.custom_dir":                  "保留",
	"nagi.v1.UpgradeAllNodesResponse.node_ids":    "接口只给管理员（会升级范围外的节点）",
	"nagi.v1.UserScope.all_nodes":                 "保留：成员自己或管理的成员的范围",
	"nagi.v1.UserScope.node_ids":                  "接口要成员权限；非管理员设的范围不超过自己的",
	"nagi.v1.UserScope.node_tags":                 "保留",
	"nagi.v1.UserScope.nodes_follow":              "保留",
}

// otherNode 是看不到的节点在文字里的名称。
func otherNode(ctx context.Context) string { return i18n.T(ctx, "其他节点") }

// filterReply 按范围和权限清理回复。x 为 nil（管理员）时不改。
func (a *API) filterReply(ctx context.Context, x *access, msg any) {
	if x == nil {
		return
	}
	switch m := msg.(type) {
	case *nagiv1.ListProjectsResponse:
		kept := m.Projects[:0]
		for _, s := range m.GetProjects() {
			if x.project(s.GetProject().GetId()) {
				a.cleanProject(ctx, x, s.GetProject())
				kept = append(kept, s)
			}
		}
		m.Projects = kept
	case *nagiv1.ProjectSummary:
		a.cleanProject(ctx, x, m.GetProject())
	case *nagiv1.Project:
		a.cleanProject(ctx, x, m)
	case *nagiv1.SaveSharedEnvResponse:
		a.cleanProject(ctx, x, m.GetProject())
	case *nagiv1.ListNodesResponse:
		a.filterNodes(ctx, x, m)
	case *nagiv1.MeshStatus:
		a.cleanMesh(ctx, x, m)
	case *nagiv1.QueryMetricsResponse:
		a.filterMetrics(ctx, x, m)
	case *nagiv1.ListNodeReplicasResponse:
		kept := m.Replicas[:0]
		for _, r := range m.GetReplicas() {
			if !x.project(r.GetReplica().GetProjectId()) {
				continue
			}
			if rep := r.GetReplica(); rep != nil {
				if !x.node(rep.GetSourceNodeId()) {
					rep.SourceNodeId, rep.SourceName = "", ""
				}
				if !x.can(auth.PermNodeDetails) {
					rep.Dir = ""
				}
			}
			kept = append(kept, r)
		}
		m.Replicas = kept
	case *nagiv1.CheckDomainsResponse:
		a.cleanDomains(ctx, x, m)
	case *nagiv1.DeployEvent:
		if out := m.GetOutput(); out != "" {
			m.Event = &nagiv1.DeployEvent_Output{Output: a.scrubNodes(ctx, x, out)}
		}
		if r := m.GetResult(); r != nil {
			r.Error = a.scrubNodes(ctx, x, r.GetError())
		}
	case *nagiv1.ListActivityResponse:
		for _, e := range m.GetEntries() {
			e.Actor, e.Target, e.Detail = a.scrubNodes(ctx, x, e.GetActor()), a.scrubNodes(ctx, x, e.GetTarget()), a.scrubNodes(ctx, x, e.GetDetail())
		}
	case *nagiv1.ListVolumesResponse:
		if !x.can(auth.PermNodeDetails) {
			for _, v := range m.GetVolumes() {
				v.HostPath = ""
			}
		}
	case *nagiv1.GetSettingsResponse:
		cleanSettings(x, m.GetSettings())
	case *nagiv1.UpdateSettingsResponse:
		cleanSettings(x, m.GetSettings())
	case *nagiv1.GetStandbysResponse:
		cleanStandbys(m)
	case *nagiv1.GetUpdateStatusResponse:
		for id := range m.GetNodes() {
			if !x.node(id) {
				delete(m.Nodes, id)
			}
		}
	case *nagiv1.AppInfoResponse:
		if m.GetDomain() != "" {
			m.PublicAddress = ""
		}
	}
}

// cleanProject 清理一个项目。
func (a *API) cleanProject(ctx context.Context, x *access, p *nagiv1.Project) {
	if p == nil {
		return
	}
	for _, l := range p.GetLinks() {
		// 已连接的项目不在范围内时不显示名称，界面显示"另一个项目"。网络名留着，配置里要用它访问对方的容器。
		if !x.project(l.GetProjectId()) {
			l.Name = ""
		}
	}
	if !x.can(auth.PermConfig) {
		p.SharedEnvDraft = nil
		if p.Settings != nil {
			p.Settings.SharedEnv = nil
		}
	}
	if !x.can(auth.PermNetwork) {
		p.Routes, p.Links = nil, nil
		if p.Settings != nil {
			p.Settings.InternalPorts, p.Settings.Egress = nil, nil
		}
	}
	for _, r := range p.GetRoutes() {
		r.EntryNodes = hideNodeIDs(x, r.GetEntryNodes())
	}
	if !x.can(auth.PermAlerts) && p.Settings != nil {
		p.Settings.AlertRules = nil
	}
	if !x.can(auth.PermSnapshots) && p.Settings != nil {
		p.Settings.SnapshotIntervalMinutes, p.Settings.SnapshotLimit, p.Settings.SnapshotSchedule = 0, 0, nil
	}
	for _, c := range p.GetContainers() {
		a.cleanContainer(ctx, x, c)
	}
}

// hideNodeIDs 把看不到的节点换成空字符串，位置不变（界面显示「其他节点」）。
func hideNodeIDs(x *access, ids []string) []string {
	for i, id := range ids {
		if !x.node(id) {
			ids[i] = ""
		}
	}
	return ids
}

func (a *API) cleanContainer(ctx context.Context, x *access, c *nagiv1.Container) {
	if !x.can(auth.PermConfig) {
		c.Config, c.Draft, c.DeployKey, c.WebhookToken, c.Dir = nil, nil, "", "", ""
	}
	if !x.node(c.GetNodeId()) {
		c.NodeId, c.Dir = "", ""
	}
	if !x.can(auth.PermAlerts) {
		c.AlertRules = nil
	}
	if !x.can(auth.PermSnapshots) {
		c.SnapshotSchedule = nil
	}
	if !x.can(auth.PermBackup) {
		c.BackupNodeId, c.Backup, c.BackupNodeIds, c.Backups, c.BackupReplicaDirs = "", nil, nil, nil, nil
	} else {
		if !x.node(c.GetBackupNodeId()) {
			c.BackupNodeId = ""
		}
		c.BackupNodeIds = hideNodeIDs(x, c.GetBackupNodeIds())
		for _, b := range append([]*nagiv1.BackupStatus{c.GetBackup()}, c.GetBackups()...) {
			a.cleanBackupStatus(ctx, x, b)
		}
		for id := range c.GetBackupReplicaDirs() {
			if !x.node(id) {
				delete(c.BackupReplicaDirs, id)
			}
		}
	}
	if !x.can(auth.PermHA) {
		c.Failover, c.FailoverHolder, c.FailoverStandby, c.FailoverWitnessSelf = nil, nil, nil, false
	} else {
		a.cleanFailover(x, c.GetFailover())
		for _, st := range []*nagiv1.FailoverStatus{c.GetFailoverHolder(), c.GetFailoverStandby()} {
			a.cleanFailoverStatus(ctx, x, st)
		}
	}
	if h := c.GetPendingHa(); h != nil {
		if !x.can(auth.PermHA) {
			h.Failover, h.FailoverEpoch = nil, 0
		}
		a.cleanFailover(x, h.GetFailover())
		if !x.can(auth.PermBackup) {
			h.BackupNodeId, h.BackupNodeIds = "", nil
		} else {
			if !x.node(h.GetBackupNodeId()) {
				h.BackupNodeId = ""
			}
			h.BackupNodeIds = hideNodeIDs(x, h.GetBackupNodeIds())
		}
		if h.Failover == nil && h.BackupNodeId == "" && len(h.BackupNodeIds) == 0 {
			c.PendingHa = nil
		}
	}
}

func (a *API) cleanBackupStatus(ctx context.Context, x *access, b *nagiv1.BackupStatus) {
	if b == nil {
		return
	}
	if !x.node(b.GetTargetNodeId()) {
		b.TargetNodeId, b.TargetName = "", ""
	}
	b.LastError, b.RetryError = a.scrubNodes(ctx, x, b.GetLastError()), a.scrubNodes(ctx, x, b.GetRetryError())
}

func (a *API) cleanFailover(x *access, f *nagiv1.FailoverConfig) {
	if f == nil {
		return
	}
	for _, p := range []*nagiv1.FailoverParticipant{f.GetA(), f.GetB(), f.GetWitness()} {
		if p == nil {
			continue
		}
		if !x.node(p.GetNodeId()) {
			// 面板当见证人时 node_id 为空，没有地址，名称是面板名，照常给。
			if p.GetNodeId() != "" {
				p.Key, p.NodeId, p.Name, p.Dir = nil, "", "", ""
			}
		} else if !x.can(auth.PermConfig) {
			p.Dir = ""
		}
	}
}

func (a *API) cleanFailoverStatus(ctx context.Context, x *access, st *nagiv1.FailoverStatus) {
	if st == nil {
		return
	}
	a.cleanFailover(x, st.GetConfig())
	if len(st.GetHolderKey()) > 0 {
		if owner := a.meshNodes(ctx)[hex.EncodeToString(st.GetHolderKey())]; owner == "" || !x.node(owner) {
			st.HolderKey, st.HolderName = nil, ""
		}
	} else if slices.Contains(a.hiddenNames(ctx, x), st.GetHolderName()) {
		st.HolderName = ""
	}
	st.LastError = a.scrubNodes(ctx, x, st.GetLastError())
}

// filterNodes 只留看得到的节点。节点上的容器数只算范围内的项目；不认识的容器、旧项目的名称只给管理员（C5）。
// 节点的地址只给管理员（B1），详情要「查看节点详情」（C1），告警规则要「节点管理」。
func (a *API) filterNodes(ctx context.Context, x *access, m *nagiv1.ListNodesResponse) {
	count := map[string]int32{}
	if x.projects != nil {
		rows, _ := a.Q.ContainerNodes(ctx)
		for _, r := range rows {
			if x.project(r.ProjectID) {
				count[r.NodeID]++
			}
		}
	}
	kept := m.Nodes[:0]
	for _, n := range m.GetNodes() {
		if !x.node(n.GetId()) {
			continue
		}
		n.UnknownContainers, n.StaleProjects, n.Address = nil, nil, ""
		if x.projects != nil {
			n.Containers = count[n.GetId()]
		}
		if !x.can(auth.PermNodeManage) {
			n.AlertRules = nil
		}
		cleanNodeInfo(x, n)
		kept = append(kept, n)
	}
	m.Nodes, m.OtherNodes = kept, nil
}

func cleanNodeInfo(x *access, n *nagiv1.Node) {
	if info := n.GetInfo(); info != nil {
		if d := info.GetDirectDownload(); d != nil {
			d.Address, d.Error, d.HttpError = "", scrubIPs(d.GetError()), scrubIPs(d.GetHttpError())
		}
		if !x.can(auth.PermNodeDetails) {
			info.Hostname, info.Os, info.Version, info.DockerVersion, info.ComposeVersion = "", "", "", "", ""
			info.ProjectsRoot, info.ReplicaDir, info.EgressError, info.InContainer = "", "", "", false
		}
	}
	if !x.can(auth.PermNodeDetails) {
		for _, d := range n.GetUsage().GetDisks() {
			d.Mount = ""
		}
	}
}

// cleanMesh：非管理员只看得到路径、延迟、丢包这些，地址、端口、对端地址都不给（B6）；
// 只留看得到的节点，面板、备用面板、已移除的节点这些不属于节点的对端只给管理员（A4）。
func (a *API) cleanMesh(ctx context.Context, x *access, m *nagiv1.MeshStatus) {
	m.Address, m.Port, m.Endpoints = "", 0, nil
	owners := a.meshNodes(ctx)
	names := a.visibleNames(ctx, x)
	kept := m.Peers[:0]
	for _, p := range m.GetPeers() {
		if id := owners[hex.EncodeToString(p.GetPublicKey())]; id == "" || !x.node(id) {
			continue
		}
		p.Address, p.Endpoint, p.LocalPort = "", "", 0
		if !names[p.GetVia()] {
			p.Via = ""
		}
		kept = append(kept, p)
	}
	m.Peers = kept
}

// filterMetrics：节点整体的序列（node:）照常给。到看不到的节点的 mesh 流量（m:）合并成一条 m:other，
// 不显示有几个、是哪些节点，概览算对外流量时仍然能从网卡流量里减掉它（A5）。其余序列都以"<种类>:<项目 ID>/"开头
// （容器 c:、端口映射 r:、日志 l: 等），只留范围内的项目；不是这种形式的也去掉，以后加了新的种类不会漏。
func (a *API) filterMetrics(ctx context.Context, x *access, m *nagiv1.QueryMetricsResponse) {
	var owners map[string]string
	other := map[string]map[int64]float64{}
	kept := m.Series[:0]
	for _, s := range m.GetSeries() {
		kind, rest, _ := strings.Cut(s.GetName(), ":")
		switch kind {
		case "node":
		case "m":
			key, metric, _ := strings.Cut(rest, ":")
			if owners == nil {
				owners = a.meshNodes(ctx)
			}
			if id := owners[key]; id == "" || !x.node(id) {
				sum := other[metric]
				if sum == nil {
					sum = map[int64]float64{}
					other[metric] = sum
				}
				for i, t := range s.GetTimes() {
					if i < len(s.GetValues()) {
						sum[t] += s.GetValues()[i]
					}
				}
				continue
			}
		default:
			if project, ok := metricProject(rest); !ok || !x.project(project) {
				continue
			}
		}
		kept = append(kept, s)
	}
	metrics := make([]string, 0, len(other))
	for k := range other {
		metrics = append(metrics, k)
	}
	sort.Strings(metrics)
	for _, metric := range metrics {
		sum := other[metric]
		s := &nagiv1.MetricSeries{Name: "m:other:" + metric}
		for t := range sum {
			s.Times = append(s.Times, t)
		}
		slices.Sort(s.Times)
		for _, t := range s.Times {
			s.Values = append(s.Values, sum[t])
		}
		kept = append(kept, s)
	}
	m.Series = kept
}

// cleanDomains：域名检查里范围外的入口节点只给结论（解析和证书的状态），不给名称和地址（A2）；
// 解析到的地址和入口节点的地址要「查看访问地址」（B7）；证书失败的原因去掉 IP（C7）。
func (a *API) cleanDomains(ctx context.Context, x *access, m *nagiv1.CheckDomainsResponse) {
	addresses := x.can(auth.PermAddresses)
	for _, d := range m.GetDomains() {
		if !addresses {
			d.Addresses = nil
		}
		for _, e := range d.GetEntries() {
			if !x.node(e.GetNodeId()) {
				e.NodeId, e.NodeName, e.Address = "", "", ""
			}
			if !addresses {
				e.Address = ""
			}
			e.CertError = a.scrubNodes(ctx, x, scrubIPs(e.GetCertError()))
		}
	}
}

// cleanSettings：非管理员只拿到名称和版本；有「面板设置」时加上域名（B3、C3）。
func cleanSettings(x *access, s *nagiv1.PanelSettings) {
	if s == nil {
		return
	}
	*s = nagiv1.PanelSettings{Name: s.GetName(), Version: s.GetVersion(), Domain: s.GetDomain()}
	if !x.can(auth.PermSettings) {
		s.Domain = ""
	}
}

// cleanStandbys：非管理员只拿到绑了域名的备用面板（只给域名），可以自动切换；只有 IP 的不给（B4）。
func cleanStandbys(m *nagiv1.GetStandbysResponse) {
	m.LastError = ""
	if !hostAddress(m.GetPrimaryAddress()) {
		m.PrimaryAddress = ""
	}
	kept := m.Standbys[:0]
	for _, s := range m.GetStandbys() {
		if hostAddress(s.GetAddress()) {
			kept = append(kept, s)
		}
	}
	m.Standbys = kept
}

// hostAddress 判断地址（可以带端口和 https://）是不是域名，不是 IP。
func hostAddress(addr string) bool {
	addr = strings.TrimPrefix(strings.TrimPrefix(addr, "https://"), "http://")
	addr, _, _ = strings.Cut(addr, "/")
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	addr = strings.Trim(addr, "[]")
	return addr != "" && net.ParseIP(addr) == nil && strings.ContainsFunc(addr, unicode.IsLetter)
}

// ---- 文字里的节点名和 IP ----

// nodeNames 读出全部节点的 ID → 名称（每次请求只读一次）。
func (a *API) nodeNames(ctx context.Context, x *access) map[string]string {
	if x.names == nil {
		x.names = map[string]string{}
		if rows, err := a.Q.ListNodes(ctx); err == nil {
			for _, r := range rows {
				x.names[r.ID] = r.Name
			}
		}
	}
	return x.names
}

// hiddenNames 是成员看不到的节点的名称。
func (a *API) hiddenNames(ctx context.Context, x *access) []string {
	var out []string
	for id, name := range a.nodeNames(ctx, x) {
		if !x.node(id) && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// visibleNames 是成员看得到的节点的名称。
func (a *API) visibleNames(ctx context.Context, x *access) map[string]bool {
	out := map[string]bool{}
	for id, name := range a.nodeNames(ctx, x) {
		if x.node(id) {
			out[name] = true
		}
	}
	return out
}

// scrubNodes 把文字里看不到的节点名换成「其他节点」（A7）。只换完整的名称：前后不是字母、数字、-、_、.。
// 看得到的节点里有同名的不换。
func (a *API) scrubNodes(ctx context.Context, x *access, s string) string {
	if s == "" {
		return s
	}
	visible := a.visibleNames(ctx, x)
	names := a.hiddenNames(ctx, x)
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, n := range names {
		if !visible[n] {
			s = replaceWord(s, n, otherNode(ctx))
		}
	}
	return s
}

func nameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.'
}

// replaceWord 把 s 里作为完整名称出现的 old 换成 repl。
func replaceWord(s, old, repl string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, old)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, _ := utf8.DecodeRuneInString(s[i+len(old):])
		whole := (i == 0 || !nameRune(before)) && (i+len(old) == len(s) || !nameRune(after))
		b.WriteString(s[:i])
		if whole {
			b.WriteString(repl)
		} else {
			b.WriteString(old)
		}
		s = s[i+len(old):]
	}
}

var ipPattern = regexp.MustCompile(`\[?(?:\d{1,3}\.){3}\d{1,3}\]?|\[?(?:[0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}\]?`)

// scrubIPs 去掉文字里的 IP 地址（证书失败、直连下载出错的原因里可能有节点的地址）。
func scrubIPs(s string) string {
	if s == "" {
		return s
	}
	return ipPattern.ReplaceAllStringFunc(s, func(m string) string {
		if net.ParseIP(strings.Trim(m, "[]")) == nil {
			return m
		}
		return "…"
	})
}

package alert

import (
	"slices"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 规则名（AlertRules.enabled 的键）。节点的规则在节点上设置，容器的规则在项目上设置、容器可以单独设置。
const (
	RuleMem      = "mem"
	RuleDisk     = "disk"
	RuleOffline  = "offline"
	RuleCert     = "cert"
	RuleRestart  = "restart"
	RuleErrors   = "errors"
	RuleHealth   = "health"
	RuleSnapshot = "snapshot"
	RuleBackup   = "backup"
	// CPU 和流量规则（见 docs/monitoring.md#告警）。
	RuleCPU          = "cpu"
	RuleContainerCPU = "container_cpu"
	RuleTraffic      = "traffic"
	RuleClientErrors = "4xx"
	// RuleEgress 是容器出站被拦截（见 docs/agent.md#出站限制）。
	RuleEgress = "egress"

	// v0.37.1 新增（见 docs/monitoring.md#告警）。
	RuleInode         = "inode"
	RuleNodeCert      = "node_cert"
	RuleUpgrade       = "upgrade"
	RuleLogWrite      = "logstore_write"
	RuleLogFull       = "logstore_full"
	RuleMigration     = "migration"
	RuleMeshPeer      = "mesh_peer"
	RuleMeshLoss      = "mesh_loss"
	RuleMeshRTT       = "mesh_rtt"
	RuleWitness       = "witness"
	RuleKernelStorage = "kernel_storage"
	RuleKernelOOM     = "kernel_oom"
	RuleNIC           = "nic_errors"
	RuleConntrack     = "conntrack"
	RuleClockSkew     = "clock_skew"
	RuleContainerOOM  = "container_oom"
	RuleSnapshotStale = "snapshot_stale"
	RuleBackupLag     = "backup_lag"
	RuleL4Surge       = "l4_conn_surge"
	RuleL4Active      = "l4_active"
	RuleL4Fail        = "l4_fail"
	RuleL4Bandwidth   = "l4_bandwidth"
	RuleL4Listen      = "l4_listen"
	RuleAuditVerify   = "audit_verify"
	RuleTokenExpiry   = "token_expiry"
)

// NodeRules、ContainerRules 是旧版本（AlertRules）认识的规则。
var (
	NodeRules      = []string{RuleMem, RuleDisk, RuleOffline, RuleCert, RuleCPU}
	ContainerRules = []string{RuleRestart, RuleErrors, RuleHealth, RuleSnapshot, RuleBackup, RuleContainerCPU, RuleTraffic, RuleClientErrors, RuleEgress}
)

// Enabled 判断规则开着没有：没有写的规则开着。
func Enabled(r *nagiv1.AlertRules, rule string) bool {
	on, ok := r.GetEnabled()[rule]
	return !ok || on
}

// NodePart 只留节点的规则，ContainerPart 只留容器的规则。
func NodePart(r *nagiv1.AlertRules) *nagiv1.AlertRules {
	if r == nil {
		return nil
	}
	return &nagiv1.AlertRules{
		DiskPercent: r.GetDiskPercent(), MemPercent: r.GetMemPercent(), MemMinutes: r.GetMemMinutes(),
		CertDays: r.GetCertDays(), OfflineSeconds: r.GetOfflineSeconds(), Enabled: pick(r.GetEnabled(), NodeRules),
	}
}

func ContainerPart(r *nagiv1.AlertRules) *nagiv1.AlertRules {
	if r == nil {
		return nil
	}
	return &nagiv1.AlertRules{
		RestartCount: r.GetRestartCount(), RestartMinutes: r.GetRestartMinutes(), ErrorPercent: r.GetErrorPercent(),
		Enabled: pick(r.GetEnabled(), ContainerRules),
	}
}

func pick(m map[string]bool, keys []string) map[string]bool {
	var out map[string]bool
	for k, v := range m {
		if slices.Contains(keys, k) {
			if out == nil {
				out = map[string]bool{}
			}
			out[k] = v
		}
	}
	return out
}

// IsEmpty 判断规则全是默认值（阈值没填、开关都开着）。
func IsEmpty(r *nagiv1.AlertRules) bool {
	if r == nil {
		return true
	}
	x := proto.CloneOf(r)
	for k, v := range x.GetEnabled() {
		if v {
			delete(x.Enabled, k)
		}
	}
	return proto.Equal(x, &nagiv1.AlertRules{})
}

// CheckRules 检查阈值：百分比不超过 100。
func CheckRules(r *nagiv1.AlertRules) error {
	if r.GetDiskPercent() > 100 || r.GetMemPercent() > 100 || r.GetErrorPercent() > 100 {
		return i18n.New("百分比须在 0–100 之间")
	}
	for k := range r.GetEnabled() {
		if !slices.Contains(NodeRules, k) && !slices.Contains(ContainerRules, k) {
			return i18n.Errorf("没有告警规则 %s", k)
		}
	}
	return nil
}

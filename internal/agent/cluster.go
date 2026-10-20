package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 主备面板（见 docs/standby.md）：同一组面板共用一个 CA。主面板在上报的回复里告诉节点任期、主面板和所有成员，
// 节点据此自动信任（连接）备用面板，并且只执行主面板发来的修改；备用面板只能查询。
// 节点按 CA 分组记下见过的最大任期和那个任期的主面板（identity/cluster.json）：节点同时信任别的面板（移交）时互不影响。

// clusterState 是一组面板里节点见过的最大任期和那个任期的主面板。
type clusterState struct {
	Term    uint64 `json:"term"`
	Primary string `json:"primary"`
}

func (id *identity) clusterPath() string { return filepath.Join(id.dir, "cluster.json") }

// clusterID 是面板所在的那组面板的标识：CA 证书的哈希。
func (p trustedPanel) clusterID() string {
	block, _ := pem.Decode([]byte(p.CAPEM))
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:8])
}

func (id *identity) readClusters() map[string]clusterState {
	out := map[string]clusterState{}
	if b, err := os.ReadFile(id.clusterPath()); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// cluster 返回某组面板的任期和主面板；没收到过时为零值。
func (id *identity) cluster(cid string) clusterState {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.readClusters()[cid]
}

// observe 记下面板告诉的任期和主面板：任期更大时采用；同一任期以先收到的为准。
func (id *identity) observe(cid string, term uint64, primary string) (clusterState, error) {
	id.mu.Lock()
	defer id.mu.Unlock()
	all := id.readClusters()
	st := all[cid]
	if term < st.Term || (term == st.Term && st.Primary != "") {
		return st, nil
	}
	st = clusterState{Term: term, Primary: primary}
	all[cid] = st
	b, _ := json.MarshalIndent(all, "", "  ")
	tmp := id.clusterPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return st, err
	}
	return st, os.Rename(tmp, id.clusterPath())
}

// errUnchanged 让 update 不写文件。
var errUnchanged = errors.New("unchanged")

// syncCluster 按主面板 p 告诉的成员调整信任列表里这组面板的备用面板：新的加进来（证书沿用主面板签发的那张，
// 同一个 CA 签的在每个面板上都有效），不在列表里的移除。返回是否有变化。
func (id *identity) syncCluster(cid string, p trustedPanel, members []*nagiv1.ClusterPanel) (bool, error) {
	err := id.update(func(panels []trustedPanel) ([]trustedPanel, error) {
		want := map[string]string{}
		for _, m := range members {
			if m.GetFingerprint() != "" && m.GetFingerprint() != p.Fingerprint {
				want[m.GetFingerprint()] = m.GetAddress()
			}
		}
		var out []trustedPanel
		for _, x := range panels {
			addr, ok := want[x.Fingerprint]
			delete(want, x.Fingerprint)
			if x.Cluster != cid || x.Fingerprint == p.Fingerprint {
				out = append(out, x)
				continue
			}
			if ok {
				x.Address, x.NodeID, x.CertPEM, x.CAPEM = addr, p.NodeID, p.CertPEM, p.CAPEM
				out = append(out, x)
			}
		}
		for _, m := range members {
			if addr, ok := want[m.GetFingerprint()]; ok {
				out = append(out, trustedPanel{Address: addr, Fingerprint: m.GetFingerprint(), NodeID: p.NodeID, CertPEM: p.CertPEM, CAPEM: p.CAPEM, Cluster: cid})
			}
		}
		if reflect.DeepEqual(out, panels) {
			return nil, errUnchanged
		}
		return out, nil
	})
	if errors.Is(err, errUnchanged) {
		return false, nil
	}
	return err == nil, err
}

// pruneClusters 移除主面板已经不在信任列表里的那些组的备用面板（例如节点移交给了别的面板）。
func (id *identity) pruneClusters() (bool, error) {
	clusters := func() map[string]clusterState {
		id.mu.Lock()
		defer id.mu.Unlock()
		return id.readClusters()
	}()
	err := id.update(func(panels []trustedPanel) ([]trustedPanel, error) {
		trusted := map[string]bool{}
		for _, p := range panels {
			trusted[p.Fingerprint] = true
		}
		var out []trustedPanel
		for _, p := range panels {
			if p.Cluster != "" && !trusted[clusters[p.Cluster].Primary] {
				continue
			}
			out = append(out, p)
		}
		if len(out) == len(panels) {
			return nil, errUnchanged
		}
		return out, nil
	})
	if errors.Is(err, errUnchanged) {
		return false, nil
	}
	return err == nil, err
}

// applyCluster 处理面板 p 在上报回复里带的主备信息。返回 true 表示节点认的主面板变了
// （这时马上再上报一次，让新的主面板知道节点已经认它了）。
func (a *agent) applyCluster(p trustedPanel, c *nagiv1.PanelCluster) bool {
	cid := p.clusterID()
	if c == nil || c.GetTerm() == 0 || c.GetPrimary() == "" || cid == "" {
		return false
	}
	// 主面板必须是节点信任的面板（自己就是主面板，或者是同一组里已经信任的面板）。
	if c.GetPrimary() != p.Fingerprint && !a.id.trusts(c.GetPrimary()) {
		return false
	}
	before := a.id.cluster(cid)
	st, err := a.id.observe(cid, c.GetTerm(), c.GetPrimary())
	if err != nil {
		a.log.Warn("保存主备面板信息失败", "err", err)
		return false
	}
	switched := st != before
	if switched && before.Primary != "" {
		a.log.Warn("主面板已更换", "term", st.Term, "panel", p.Address)
	}
	// 成员列表只听当前主面板的。
	if st.Primary != p.Fingerprint || st.Term != c.GetTerm() {
		return switched
	}
	changed, err := a.id.syncCluster(cid, p, c.GetPanels())
	if err != nil {
		a.log.Warn("更新备用面板失败", "err", err)
		return switched
	}
	if changed {
		a.log.Info("主面板更新了备用面板列表", "panel", p.Address, "members", len(c.GetPanels()))
		a.reloadPanels()
	}
	return switched
}

// role 返回面板在主备面板里的角色："primary"、"standby"，不在主备面板里时为空。
func (a *agent) role(p trustedPanel) string {
	st := a.id.cluster(p.clusterID())
	switch {
	case st.Primary == "":
		return ""
	case st.Primary == p.Fingerprint:
		return "primary"
	default:
		return "standby"
	}
}

// readOnlyRPCs 是非主面板也能调用的查询接口。
var readOnlyRPCs = map[string]bool{
	nagiv1.AgentService_ListSnapshots_FullMethodName:        true,
	nagiv1.AgentService_StreamLogs_FullMethodName:           true,
	nagiv1.AgentService_CheckImages_FullMethodName:          true,
	nagiv1.AgentService_ListReplicas_FullMethodName:         true,
	nagiv1.AgentService_ListReplicaSnapshots_FullMethodName: true,
	nagiv1.AgentService_CheckContainer_FullMethodName:       true,
	nagiv1.AgentService_GetCache_FullMethodName:             true,
	nagiv1.AgentService_QueryMetrics_FullMethodName:         true,
	nagiv1.AgentService_ListPanels_FullMethodName:           true,
	nagiv1.AgentService_GetMeshStatus_FullMethodName:        true,
	nagiv1.AgentService_MeshPing_FullMethodName:             true,
	nagiv1.AgentService_GetMirrors_FullMethodName:           true,
}

// checkPrimary 判断面板 fp 能不能调用 method：查询接口都可以；修改只接受主面板的。
func (a *agent) checkPrimary(fp, method string) error {
	if readOnlyRPCs[method] {
		return nil
	}
	p, ok := a.id.panel(fp)
	if !ok {
		return status.Error(codes.PermissionDenied, "不在信任列表里的面板")
	}
	st := a.id.cluster(p.clusterID())
	if st.Primary == "" || st.Primary == fp {
		return nil
	}
	where := st.Primary
	if pp, ok := a.id.panel(st.Primary); ok {
		where = pp.Address
	}
	return status.Errorf(codes.FailedPrecondition, "这是备用面板，只能查询；修改请到主面板 %s", where)
}

// primaryGuard 返回只放行主面板修改的拦截器（每条面板连接一个）。
func (a *agent) primaryGuard(fp string) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := a.checkPrimary(fp, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := a.checkPrimary(fp, info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
	return unary, stream
}

// clusterOf 返回节点对面板 p 那一组记下的任期和主面板，随上报告诉面板。
func (a *agent) clusterOf(p trustedPanel) clusterState {
	return a.id.cluster(p.clusterID())
}

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// 顶替保护（见 docs/failover.md#租约与三方多数票）：顶替的三方（A、B、见证人）之间靠 mesh 投票，
// 而 mesh 只连通同一个面板下的成员。所以从节点的信任列表里移除一个面板之前要检查：
// 移除以后，本节点参与的每个顶替设置里，其他参与方还能不能经剩下的某个面板连上。
// 连不上的话三票只剩两票（甚至持有者自己续不上约而停掉），所以拒绝移除。
// 例如这个面板就是见证人；或者它是本节点和顶替节点之间唯一共同的面板。
// 节点本地命令可以用 --force 强制移除（旧面板已经挂了时只能这样）。

// failoverBreaks 返回移除指纹为 fingerprint 的面板后，本节点会联系不上的顶替参与方，形如"项目（参与方）"。
// 读的是磁盘上的文件（各面板告诉节点的成员列表、顶替设置），节点进程和本地命令都能用。
func failoverBreaks(dataDir, fingerprint string) []string {
	saved := map[string]savedPanel{}
	if b, err := os.ReadFile(filepath.Join(dataDir, "identity", "mesh-peers.json")); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	// 剩下的面板能连上的成员：面板自己和它告诉我们的所有成员。
	var reachable [][]byte
	for fp, sp := range saved {
		if fp == fingerprint {
			continue
		}
		reachable = append(reachable, sp.Key)
		for _, raw := range sp.Peers {
			p := &nagiv1.MeshPeer{}
			if protojson.Unmarshal(raw, p) == nil {
				reachable = append(reachable, p.GetPublicKey())
			}
		}
	}
	canReach := func(k []byte) bool {
		for _, r := range reachable {
			if bytes.Equal(r, k) {
				return true
			}
		}
		return false
	}
	self := selfMeshKey(dataDir)

	dir := filepath.Join(dataDir, "failover")
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var f failoverFile
		cfg := &nagiv1.FailoverConfig{}
		if json.Unmarshal(b, &f) != nil || protojson.Unmarshal(f.Config, cfg) != nil {
			continue
		}
		name := cfg.GetProjectName()
		if name == "" {
			name = cfg.GetProjectId()
		}
		for _, m := range []*nagiv1.FailoverParticipant{cfg.GetA(), cfg.GetB(), cfg.GetWitness()} {
			if len(m.GetKey()) != 32 || bytes.Equal(m.GetKey(), self) || canReach(m.GetKey()) {
				continue
			}
			who := m.GetName()
			if m == cfg.GetWitness() {
				who = "见证人 " + who
			}
			out = append(out, fmt.Sprintf("%s（%s）", name, who))
		}
	}
	return out
}

// selfMeshKey 返回本节点的 WireGuard 公钥。
func selfMeshKey(dataDir string) []byte {
	id, err := openIdentity(dataDir)
	if err != nil {
		return nil
	}
	k, err := id.wgPublicKey()
	if err != nil {
		return nil
	}
	return k[:]
}

// failoverGuard 是移除面板前的检查：移除后顶替的参与方之间联系不上时拒绝。
func failoverGuard(dataDir string) func(fingerprint string) error {
	return func(fingerprint string) error {
		if list := failoverBreaks(dataDir, fingerprint); len(list) > 0 {
			return fmt.Errorf("%w：%s。请先在项目的顶替设置里更换见证人，或者让相关节点都加入另一个面板（共同管理）后再移除，或者取消顶替",
				errFailoverPanel, strings.Join(list, "、"))
		}
		return nil
	}
}

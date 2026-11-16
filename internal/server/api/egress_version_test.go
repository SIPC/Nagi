package api

import (
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// L14：面板升级、节点没升级时，旧节点不认出站限制，也不报 egress_error，节点页之前不会提示「无法限制」。
// 现在按节点版本补上原因。
func TestEgressSupportOldNodes(t *testing.T) {
	old := &nagiv1.NodeInfo{Version: "v0.28.1"}
	got := egressSupport(old)
	if !strings.Contains(got.GetEgressError(), "v0.28.1") || old.GetEgressError() != "" {
		t.Fatalf("旧节点应提示无法限制（且不改上报的原件）：%q / %q", got.GetEgressError(), old.GetEgressError())
	}
	for _, v := range []string{"v0.29.0", "v0.29.1", "v1.0.0", "dev", ""} {
		if e := egressSupport(&nagiv1.NodeInfo{Version: v}).GetEgressError(); e != "" {
			t.Errorf("%s 不应提示：%q", v, e)
		}
	}
	// 节点自己报了原因的，用节点的。
	if e := egressSupport(&nagiv1.NodeInfo{Version: "v0.28.0", EgressError: "no nft"}).GetEgressError(); e != "no nft" {
		t.Errorf("节点报的原因被改了：%q", e)
	}
	if egressSupport(nil) != nil {
		t.Error("nil")
	}
}

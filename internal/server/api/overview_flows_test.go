package api

import (
	"testing"

	"github.com/SIPC/nagi/internal/server/store"
)

func TestAddPair(t *testing.T) {
	peers := map[string]string{"aa": "n1", "bb": "n2", "ff": flowPanel}
	pairs := map[string]float64{}
	addPair(pairs, "n1", "m:bb:tx", 100, peers, "ff")
	addPair(pairs, "n1", "m:bb:rx", 50, peers, "ff")    // 节点之间只取发送方的数
	addPair(pairs, "n1", "m:ff:rx", 30, peers, "ff")    // 面板发出的按节点的接收量算
	addPair(pairs, "n1", "m:ff:tx", 20, peers, "ff")    // 节点发给面板
	addPair(pairs, "n1", "m:aa:tx", 10, peers, "ff")    // 自己
	addPair(pairs, "n1", "m:relay:tx", 10, peers, "ff") // 中转量不是路径
	want := map[string]float64{"n1|n2": 100, flowPanel + "|n1": 30, "n1|" + flowPanel: 20}
	if len(pairs) != len(want) {
		t.Fatalf("pairs = %v", pairs)
	}
	for k, v := range want {
		if pairs[k] != v {
			t.Fatalf("pairs[%s] = %v, want %v", k, pairs[k], v)
		}
	}
}

func TestTrafficFlows(t *testing.T) {
	tr := newTrafficNow()
	tr.ready = true
	tr.extRx["n1"] = 2000
	tr.extTx["n1"] = 0.5 // 不到 1 B/s 不画
	tr.extRx["n3"] = 900 // 范围外
	tr.pairs["n1|n2"] = 300
	tr.pairs["n1|n3"] = 300 // 范围外
	tr.relayed["n1|n2"] = true
	rows := []store.Node{{ID: "n1", Name: "a"}, {ID: "n2", Name: "b"}, {ID: "n3", Name: "c"}, {ID: "n4", Name: "d", Pending: 1}}
	x := &access{nodes: map[string]bool{"n1": true, "n2": true}}
	flows, flowRows := trafficFlows(tr, rows, x, func(id string) bool { return id != "n2" })
	if len(flowRows) != 2 || flowRows[1].GetName() != "b" || !flowRows[1].GetOffline() {
		t.Fatalf("rows = %v", flowRows)
	}
	if len(flows) != 2 {
		t.Fatalf("flows = %v", flows)
	}
	if f := flows[0]; f.GetFrom() != flowExternal || f.GetTo() != "n1" || f.GetKind() != "ext" {
		t.Fatalf("flows[0] = %v", f)
	}
	if f := flows[1]; f.GetFrom() != "n1" || f.GetTo() != "n2" || f.GetKind() != "mesh" || !f.GetRelay() {
		t.Fatalf("flows[1] = %v", f)
	}
}

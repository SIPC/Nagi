package agent

import (
	"encoding/pem"
	"log/slog"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestClusterStandbys(t *testing.T) {
	id, err := loadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := tlsutil.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}))
	primary := trustedPanel{Address: "a:1", Fingerprint: "fp-a", NodeID: "n1", CertPEM: "cert", CAPEM: caPEM}
	if err := id.addPanel(primary); err != nil {
		t.Fatal(err)
	}
	a := &agent{id: id, log: slog.Default(), reload: make(chan struct{}, 1)}

	members := []*nagiv1.ClusterPanel{{Address: "a:1", Fingerprint: "fp-a"}, {Address: "b:1", Fingerprint: "fp-b"}}
	a.applyCluster(primary, &nagiv1.PanelCluster{Term: 1, Primary: "fp-a", Panels: members})
	b, ok := id.panel("fp-b")
	if !ok || b.Address != "b:1" || b.NodeID != "n1" || b.CAPEM != caPEM || b.Cluster == "" {
		t.Fatalf("备用面板没有加入信任列表: %+v", b)
	}
	if a.role(primary) != "primary" || a.role(b) != "standby" {
		t.Fatal("角色不对")
	}

	// 主面板能修改，备用面板只能查询。
	if err := a.checkPrimary("fp-a", nagiv1.AgentService_PutContainer_FullMethodName); err != nil {
		t.Fatal(err)
	}
	if err := a.checkPrimary("fp-b", nagiv1.AgentService_QueryMetrics_FullMethodName); err != nil {
		t.Fatal(err)
	}
	if err := a.checkPrimary("fp-b", nagiv1.AgentService_PutContainer_FullMethodName); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("备用面板的修改应被拒绝: %v", err)
	}

	// 备用面板说的成员列表不采用；任期一样时不改主面板。
	a.applyCluster(b, &nagiv1.PanelCluster{Term: 1, Primary: "fp-b", Panels: members[:1]})
	if _, ok := id.panel("fp-b"); !ok || id.cluster(primary.clusterID()).Primary != "fp-a" {
		t.Fatal("同一任期不应换主面板")
	}

	// 主面板的列表里没有了就移除。
	a.applyCluster(primary, &nagiv1.PanelCluster{Term: 1, Primary: "fp-a", Panels: members[:1]})
	if _, ok := id.panel("fp-b"); ok {
		t.Fatal("备用面板应已移除")
	}

	// 主面板不再信任时，它的备用面板一起移除。
	a.applyCluster(primary, &nagiv1.PanelCluster{Term: 1, Primary: "fp-a", Panels: members})
	if err := id.addPanel(trustedPanel{Address: "c:1", Fingerprint: "fp-c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := id.removePanel("fp-a", false, nil); err != nil {
		t.Fatal(err)
	}
	if pruned, err := id.pruneClusters(); err != nil || !pruned {
		t.Fatalf("pruned=%v err=%v", pruned, err)
	}
	if _, ok := id.panel("fp-b"); ok {
		t.Fatal("主面板移除后备用面板应一起移除")
	}
}

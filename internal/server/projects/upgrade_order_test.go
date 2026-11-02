package projects

import (
	"slices"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

func TestOrderUpgrades(t *testing.T) {
	failover, err := proto.Marshal(&nagiv1.FailoverConfig{
		A: &nagiv1.FailoverParticipant{NodeId: "host"},
		B: &nagiv1.FailoverParticipant{NodeId: "standby"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := []store.Container{
		{NodeID: "host", Failover: failover},
		{NodeID: "both", BackupNodeID: "backup"},
		{NodeID: "plain"},
		{NodeID: "host", BackupNodeID: "both"},
	}
	got := orderUpgrades([]string{"idle", "plain", "host", "both", "backup", "standby"}, rows)
	want := []string{"backup", "standby", "both", "plain", "host", "idle"}
	if !slices.Equal(got, want) {
		t.Fatalf("顺序 = %v，应为 %v", got, want)
	}
}

package projects

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// haTestService：一个项目、容器 web（在 n1 上），登记了节点 n1、n2、n3，mesh 公钥分别是 1、2、3。
func haTestService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, stmt := range []string{
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('aaaaaaaaaaaaaaaa', 'a', 'a', 1, 0, x'')`,
		`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
		 VALUES ('aaaaaaaaaaaaaaaa', 'web', 1, 'n1', '/srv/a', 1, 0, 'h1', x'', '')`,
		`INSERT INTO nodes (id, name, created_at, mesh_key) VALUES ('n1', 'n1', 0, x'01'), ('n2', 'n2', 0, x'02'), ('n3', 'n3', 0, x'03')`,
	} {
		if _, err := d.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	return &Service{Q: store.New(d), Log: slog.Default()}
}

func marshalPending(t *testing.T, p *nagiv1.PendingHA) []byte {
	t.Helper()
	b, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 从节点缓存拼回的顶替设置和备份节点只记成待确认；核对不过的（节点没登记、公钥对不上、上报的节点不是其中一方）直接丢掉。
func TestRebuiltHAPending(t *testing.T) {
	ctx := context.Background()
	s := haTestService(t)
	cfg := func(a, b string, ka, kb byte) *nagiv1.FailoverStatus {
		return &nagiv1.FailoverStatus{Role: "holder", Epoch: 4, Config: &nagiv1.FailoverConfig{
			A: &nagiv1.FailoverParticipant{NodeId: a, Key: []byte{ka}}, B: &nagiv1.FailoverParticipant{NodeId: b, Key: []byte{kb}}, WitnessIsPanel: true,
		}}
	}
	sp := &nagiv1.ContainerSpec{ProjectId: "aaaaaaaaaaaaaaaa", Name: "web"}
	cases := []struct {
		desc     string
		backup   string
		fo       *nagiv1.FailoverStatus
		failover bool
		backupOK bool
	}{
		{"顶替设置核对通过", "", cfg("n1", "n2", 1, 2), true, false},
		{"公钥对不上", "", cfg("n1", "n2", 1, 9), false, false},
		{"上报的节点不在其中", "", cfg("n2", "n3", 2, 3), false, false},
		{"节点没登记", "", cfg("n1", "nx", 1, 2), false, false},
		{"备份节点登记过", "n2", nil, false, true},
		{"备份节点没登记", "nx", nil, false, false},
	}
	for _, c := range cases {
		sp.Backup = nil
		if c.backup != "" {
			sp.Backup = &nagiv1.BackupTarget{NodeId: c.backup}
		}
		p := s.rebuiltHA(ctx, "n1", sp, c.fo)
		if (p.GetFailover() != nil) != c.failover || (p.GetBackupNodeId() != "") != c.backupOK {
			t.Errorf("%s：得到 %v", c.desc, p)
		}
		if p != nil && p.GetFailover() != nil && p.GetFailoverEpoch() != 4 {
			t.Errorf("%s：轮次没带上", c.desc)
		}
	}
}

// 待确认的设置存进数据库，在容器上显示；放弃后清掉，顶替和备份都没有生效。
func TestPendingHADiscard(t *testing.T) {
	ctx := context.Background()
	s := haTestService(t)
	p := &nagiv1.PendingHA{BackupNodeId: "n2"}
	raw := marshalPending(t, p)
	if err := s.Q.SetContainerPendingHA(ctx, store.SetContainerPendingHAParams{PendingHa: raw, ProjectID: "aaaaaaaaaaaaaaaa", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.container(ctx, "aaaaaaaaaaaaaaaa", "web")
	if err != nil || pendingHAOf(c).GetBackupNodeId() != "n2" || c.BackupNodeID != "" {
		t.Fatalf("待确认：%v %v", pendingHAOf(c), err)
	}
	if err := s.ConfirmPendingHA(ctx, "aaaaaaaaaaaaaaaa", "web", true); err != nil {
		t.Fatal(err)
	}
	c, _ = s.container(ctx, "aaaaaaaaaaaaaaaa", "web")
	if pendingHAOf(c) != nil || c.BackupNodeID != "" {
		t.Fatalf("放弃后：%v，备份节点 %q", pendingHAOf(c), c.BackupNodeID)
	}
	if err := s.ConfirmPendingHA(ctx, "aaaaaaaaaaaaaaaa", "web", false); err == nil {
		t.Fatal("没有待确认的设置时应该报错")
	}
}

// 拼回的顶替设置等确认期间（哪怕项目拼回已经超过 10 分钟），节点上的顶替设置要留着；放弃以后才让节点删掉。
func TestKeepNodeFailoverWhilePending(t *testing.T) {
	ctx := context.Background()
	s := haTestService(t)
	load := func() store.Container {
		c, err := s.container(ctx, "aaaaaaaaaaaaaaaa", "web")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if s.keepUnsetFailover(ctx, load()) {
		t.Fatal("没有待确认的设置、项目也不是刚拼回的：应该让节点删掉")
	}
	p := &nagiv1.PendingHA{FailoverEpoch: 3, Failover: &nagiv1.FailoverConfig{
		A: &nagiv1.FailoverParticipant{NodeId: "n1", Key: []byte{1}}, B: &nagiv1.FailoverParticipant{NodeId: "n2", Key: []byte{2}},
	}}
	if err := s.Q.SetContainerPendingHA(ctx, store.SetContainerPendingHAParams{PendingHa: marshalPending(t, p), ProjectID: "aaaaaaaaaaaaaaaa", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if !s.keepUnsetFailover(ctx, load()) {
		t.Fatal("等确认期间应该留着节点上的顶替设置")
	}
	if err := s.ConfirmPendingHA(ctx, "aaaaaaaaaaaaaaaa", "web", true); err != nil {
		t.Fatal(err)
	}
	if s.keepUnsetFailover(ctx, load()) {
		t.Fatal("放弃以后应该让节点删掉")
	}
}

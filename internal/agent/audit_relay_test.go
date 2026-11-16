package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/agent/auditlog"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// H2：旧版本的面板照旧下发带凭据的外送目标，新节点不保存（也删掉升级前保存的）、返回成功；
// 面板用 ReadAuditLog 读走本地审计。
func TestAuditSinkNotStored(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "identity"), 0o700)
	cfg := filepath.Join(dir, "identity", "auditsinks.json")
	_ = os.WriteFile(cfg, []byte(`{"sha256//p":[{"id":"s1","secret_key":"sk"}]}`), 0o600)
	a := &agent{cfg: Config{DataDir: dir}, log: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.openAudit(ctx)
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatal("打开本地审计时要删掉升级前保存的外送凭据")
	}
	s := &agentService{node: a}
	if _, err := s.SetAuditSink(ctx, &nagiv1.SetAuditSinkRequest{Sinks: []*nagiv1.NodeAuditSink{{Id: "s1", Type: "s3", SecretKey: "sk"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatal("旧面板下发的外送凭据不能保存")
	}
	a.audit.Record(auditlog.Event{Action: "node_exec"})
	start, err := s.ReadAuditLog(ctx, &nagiv1.ReadAuditLogRequest{After: -1})
	if err != nil || start.GetStart() != 1 || len(start.GetLines()) != 0 {
		t.Fatalf("起点：%v %v", start, err)
	}
	r, err := s.ReadAuditLog(ctx, &nagiv1.ReadAuditLogRequest{After: 0})
	if err != nil || len(r.GetLines()) != 1 || r.GetLast() != 1 {
		t.Fatalf("读本地审计：%v %v", r, err)
	}
}

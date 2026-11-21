package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/agent/alerts"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// L11：节点重启后 dockerd 先拉起容器，nagi 列完容器之前没有出站规则。现在启动时最先加载上次应用的规则。
// 用一个假的 nft 记下收到的规则。
func TestRestoreEgress(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	got := filepath.Join(dir, "got.nft")
	script := "#!/bin/sh\ncat > " + got + "\n"
	if err := os.WriteFile(filepath.Join(bin, "nft"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := slog.New(slog.DiscardHandler)

	// 没有缓存：什么也不做。
	restoreEgress(dir, log)
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatal("没有缓存时不应调用 nft")
	}
	rules := "table inet nagi\ndelete table inet nagi\ntable inet nagi {\n}\n"
	if err := writeFileAtomic(egressCacheFile(dir), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	restoreEgress(dir, log)
	b, err := os.ReadFile(got)
	if err != nil || string(b) != rules {
		t.Fatalf("启动时应加载上次的规则：%q %v", b, err)
	}
	if st, _ := os.Stat(egressCacheFile(dir)); st.Mode().Perm() != 0o600 {
		t.Fatalf("规则文件权限 %v", st.Mode().Perm())
	}
}

func TestEgressHistoryWithoutNotifications(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "logs.db")
	s, err := logstore.Open(ctx, path, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	a := &agent{cfg: Config{Logs: s}, log: slog.New(slog.DiscardHandler), alerts: &alerts.Engine{
		ContainerRules: func(string, string) (*nagiv1.EffectiveAlertRules, *nagiv1.AlertRules, bool) {
			return nil, &nagiv1.AlertRules{Enabled: map[string]bool{"egress": false}}, true
		},
	}}
	e := &egress{a: a, byIP: map[string]egressContainer{"172.20.0.2": {project: testProject, name: "instance", nagi: "web"}}, modes: map[string]string{testProject: "log"}}
	hit := egressHit{src: netip.MustParseAddr("172.20.0.2"), dst: netip.MustParseAddr("169.254.169.254"), port: 80}
	e.hit(ctx, hit)
	e.hit(ctx, hit) // The same observation remains deduplicated, even with alerts disabled.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = logstore.Open(ctx, path, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Query(ctx, logstore.Filter{Category: logstore.Security})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "egress.observed" || events[0].Object != "web" {
		t.Fatalf("events: %+v", events)
	}
	b, _ := json.Marshal(events[0].Data)
	var data struct {
		Target   string `json:"target"`
		Mode     string `json:"mode_at_observation"`
		Complete bool   `json:"complete_connections"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	if data.Target != "169.254.169.254:80" || data.Mode != "log" || data.Complete {
		t.Fatalf("data: %s", b)
	}
}

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// 改面板对外地址和域名之前，先用现在的告警渠道发一条通知并等它发完（和改告警渠道一样）：
// 拿到「管理面板」令牌的人改掉地址之前，原来的渠道已经收到了。
func TestPanelAddressNoticeBefore(t *testing.T) {
	h := newAuditHarness(t)
	alert.AllowLoopbackForTest(t) // 收告警的 httptest 服务在本机
	ctx := context.Background()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, slog.Default(), h.a.Sealer, nil)
	type got struct{ title, addrAtSend, domainAtSend string }
	var mu sync.Mutex
	var received []got
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Title string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		received = append(received, got{body.Title, h.a.Panel.PublicAddress(ctx), h.a.Panel.Domain(ctx)})
		mu.Unlock()
	}))
	defer hook.Close()
	if err := h.a.Nodes.SetAlertConfig(ctx, &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "c1", Name: "ops", Type: "webhook", Url: hook.URL}}}); err != nil {
		t.Fatal(err)
	}
	s := &settingsService{h.a}
	uctx := auth.WithUser(ctx, auth.User{ID: 1, Name: "admin", Role: auth.RoleAdmin, TokenID: 3})
	if _, err := s.UpdateSettings(uctx, connect.NewRequest(&nagiv1.UpdateSettingsRequest{PublicAddress: "203.0.113.9"})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPanelDomain(uctx, connect.NewRequest(&nagiv1.SetPanelDomainApiRequest{Domain: "panel.example.com"})); err != nil {
		t.Fatal(err)
	}
	// 没有变化时不通知。
	if _, err := s.UpdateSettings(uctx, connect.NewRequest(&nagiv1.UpdateSettingsRequest{PublicAddress: "203.0.113.9"})); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("应该收到两条通知：%+v", received)
	}
	if !strings.Contains(received[0].title, "203.0.113.9") || received[0].addrAtSend == "203.0.113.9" {
		t.Errorf("改地址的通知应在生效之前发出：%+v", received[0])
	}
	if !strings.Contains(received[1].title, "panel.example.com") || received[1].domainAtSend != "" {
		t.Errorf("改域名的通知应在生效之前发出：%+v", received[1])
	}
	if h.a.Panel.PublicAddress(ctx) != "203.0.113.9" || h.a.Panel.Domain(ctx) != "panel.example.com" {
		t.Error("通知以后照常生效")
	}
}

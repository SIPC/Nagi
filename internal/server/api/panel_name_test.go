package api

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// 面板名称：改了以后 Name 返回新的，为空或等于主机名时恢复默认；太长、有换行的拒绝。
func TestSetPanelName(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, slog.Default(), h.a.Sealer, nil)
	s := &settingsService{h.a}
	res, err := s.SetPanelName(ctx, connect.NewRequest(&nagiv1.SetPanelNameRequest{Name: "  家里的集群  "}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetSettings().GetName(); got != "家里的集群" || h.a.Panel.Name(ctx) != "家里的集群" {
		t.Fatalf("name = %q / %q", got, h.a.Panel.Name(ctx))
	}
	if _, err := s.SetPanelName(ctx, connect.NewRequest(&nagiv1.SetPanelNameRequest{Name: ""})); err != nil {
		t.Fatal(err)
	}
	if got := h.a.Panel.Name(ctx); got != DefaultPanelName() {
		t.Fatalf("reset name = %q, want %q", got, DefaultPanelName())
	}
	for _, bad := range []string{strings.Repeat("长", maxPanelName+1), "a\nb"} {
		if _, err := s.SetPanelName(ctx, connect.NewRequest(&nagiv1.SetPanelNameRequest{Name: bad})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("SetPanelName(%q) err = %v", bad, err)
		}
	}
}

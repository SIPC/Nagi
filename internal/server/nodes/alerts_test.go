package nodes

import (
	"context"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRelayAlertRequiresAlert(t *testing.T) {
	panel := newSessionPanel(t)
	p := &panelService{conn: &Conn{NodeID: panel.nodeID}, r: panel.r}
	_, err := p.RelayAlert(context.Background(), &nagiv1.RelayAlertRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty alert: %v", err)
	}
}

func TestAlertContainer(t *testing.T) {
	for key, want := range map[string][2]string{
		"health:p1/web/nginx": {"p1", "web"},
		"lease:p1-web":        {"p1", "web"},
		"disk":                {"", ""},
		"cert:example.com":    {"", ""},
		"errors:p1/web":       {"p1", "web"},
	} {
		p, c := alertContainer(key)
		if p != want[0] || c != want[1] {
			t.Errorf("%s = %s/%s，应为 %v", key, p, c, want)
		}
	}
}

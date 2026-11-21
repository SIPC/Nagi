package alerts

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestPendingNotificationsFollowCurrentStateAndConfiguration(t *testing.T) {
	for _, scenario := range []string{"latest text", "recovered", "new occurrence", "unknown", "removed channel", "changed destination", "revoked panel", "disabled rule"} {
		t.Run(scenario, func(t *testing.T) {
			r := &receiver{fail: true}
			srv := httptest.NewServer(r)
			defer srv.Close()
			r2 := &receiver{}
			srv2 := httptest.NewServer(r2)
			defer srv2.Close()
			e := newEngine(t, nil)
			ctx := context.Background()
			ch := &nagiv1.AlertChannel{Id: "channel", Type: "webhook", Url: srv.URL}
			if err := e.SetConfig("panel", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{ch}}); err != nil {
				t.Fatal(err)
			}
			e.Fire(ctx, "snapshot:p/web", "private original text", "body", "p")
			b, err := os.ReadFile(e.queueFile())
			if err != nil || strings.Contains(string(b), srv.URL) || strings.Contains(string(b), "private original text") {
				t.Fatalf("queue retained credentials or message: %s %v", b, err)
			}
			want, title, status := 1, "latest text", "firing"
			switch scenario {
			case "latest text":
				e.Fire(ctx, "snapshot:p/web", title, "updated", "p")
			case "recovered":
				e.Resolve(ctx, "snapshot:p/web")
				status = "resolved"
				title = "private original text"
			case "new occurrence":
				e.Resolve(ctx, "snapshot:p/web")
				e.Fire(ctx, "snapshot:p/web", title, "", "p")
			case "unknown":
				e.MarkUnavailableUnless("snapshot:", func(string) bool { return false })
				want = 0
			case "removed channel":
				e.SetConfig("panel", &nagiv1.AlertConfig{})
				want = 0
			case "changed destination":
				e.SetConfig("panel", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "channel", Type: "webhook", Url: srv2.URL}}})
				want = 0
			case "revoked panel":
				e.Retain(nil)
				want = 0
			case "disabled rule":
				e.SetConfig("panel", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{ch}, Rules: &nagiv1.AlertRules{Enabled: map[string]bool{"snapshot": false}}})
				want = 0
			}
			// Persisted event state and pending intent both survive restart.
			e = New(e.log, e.node, e.dir, e.certDir, nil, nil, e.projectName, nil)
			r.mu.Lock()
			r.fail = false
			r.mu.Unlock()
			e.flushQueue(ctx)
			if r.count() != want || r2.count() != 0 {
				t.Fatalf("stale or redirected delivery: old=%v new=%v", r.got, r2.got)
			}
			if want == 1 && (r.got[0]["title"] != title || r.got[0]["status"] != status) {
				t.Fatalf("replayed old message: %+v", r.got)
			}
			e.flushQueue(ctx)
			if r.count() != want {
				t.Fatalf("accepted delivery replayed: %+v", r.got)
			}
		})
	}
}

func TestLegacyNotificationQueueDoesNotReplaySavedPayloads(t *testing.T) {
	r := &receiver{}
	srv := httptest.NewServer(r)
	defer srv.Close()
	e := newEngine(t, nil)
	data := `[{"alert":{"key":"disk","status":"firing","title":"obsolete"},"channel":{"type":"webhook","url":"` + srv.URL + `"},"panel":"removed"}]`
	if err := os.WriteFile(e.queueFile(), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	e = New(e.log, e.node, e.dir, e.certDir, nil, nil, e.projectName, nil)
	e.flushQueue(context.Background())
	if r.count() != 0 {
		t.Fatal("legacy full message queue sent obsolete notification")
	}
	b, err := os.ReadFile(e.queueFile())
	if err != nil || strings.Contains(string(b), srv.URL) {
		t.Fatalf("legacy credential copy retained: %s %v", b, err)
	}
}

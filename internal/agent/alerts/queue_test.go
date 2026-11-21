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
			e = New(e.log, e.node, e.dir, e.certDir, nil, nil, e.projectName, nil, e.logs)
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

// v0.36 的 alerts-queue.json 是 [{alert, channel, panel}]：升级后迁到新格式并补发，
// 不保留其中的渠道地址；渠道已删除或换了地址的条目发不出去，不发到别处。
func TestLegacyNotificationQueueMigratesAndSends(t *testing.T) {
	r, gone := &receiver{fail: true}, &receiver{}
	srv, srvGone := httptest.NewServer(r), httptest.NewServer(gone)
	defer srv.Close()
	defer srvGone.Close()
	e := newEngine(t, nil)
	if err := e.SetConfig("panel", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "kept", Type: "webhook", Url: srv.URL}, {Id: "moved", Type: "webhook", Url: srv.URL + "/new"}}}); err != nil {
		t.Fatal(err)
	}
	data := `[{"alert":{"key":"disk","status":"firing","title":"disk full"},"channel":{"id":"kept","type":"webhook","url":"` + srv.URL + `"},"panel":"panel"},` +
		`{"alert":{"key":"cpu","status":"resolved","title":"cpu high"},"channel":{"id":"kept","type":"webhook","url":"` + srv.URL + `"},"panel":"panel"},` +
		`{"alert":{"key":"mem","status":"firing","title":"moved"},"channel":{"id":"moved","type":"webhook","url":"` + srvGone.URL + `"},"panel":"panel"},` +
		`{"alert":{"key":"x","status":"firing","title":"removed panel"},"channel":{"id":"kept","type":"webhook","url":"` + srv.URL + `"},"panel":"gone"}]`
	if err := os.WriteFile(e.queueFile(), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	e = New(e.log, e.node, e.dir, e.certDir, nil, nil, e.projectName, nil, e.logs)
	b, err := os.ReadFile(e.queueFile())
	if err != nil || strings.Contains(string(b), srv.URL) || !strings.Contains(string(b), `"version":2`) {
		t.Fatalf("迁移后应是新格式且不含渠道地址：%s %v", b, err)
	}
	// 第一次发送失败：留在队列里，重启后仍在。
	e.flushQueue(context.Background())
	e = New(e.log, e.node, e.dir, e.certDir, nil, nil, e.projectName, nil, e.logs)
	r.mu.Lock()
	r.fail = false
	r.mu.Unlock()
	e.flushQueue(context.Background())
	e.flushQueue(context.Background())
	if r.count() != 2 || gone.count() != 0 {
		t.Fatalf("应只发出 2 条且各一次：%v %v", r.got, gone.got)
	}
	if r.got[0]["title"] != "disk full" || r.got[0]["status"] != "firing" || r.got[1]["title"] != "cpu high" || r.got[1]["status"] != "resolved" {
		t.Fatalf("按原顺序发出原通知：%v", r.got)
	}
	if b, _ := os.ReadFile(e.queueFile()); strings.Contains(string(b), "legacy_alert") {
		t.Fatalf("发出后应从队列删除：%s", b)
	}
}

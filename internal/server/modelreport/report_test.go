package modelreport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestGenerate(t *testing.T) {
	for _, outcome := range []string{"ok", "failure", "timeout", "oversize", "incomplete"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			logs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 8<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer logs.Close()
			var calls atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				b, _ := io.ReadAll(req.Body)
				if strings.Contains(string(b), "super-secret") || strings.Contains(string(b), "raw-log-body") || !strings.Contains(string(b), "[已隐藏]") {
					t.Errorf("unsafe request: %s", b)
				}
				if req.Header.Get("Authorization") != "Bearer super-secret" {
					t.Error("authorization missing")
				}
				switch outcome {
				case "failure":
					w.WriteHeader(401)
					io.WriteString(w, "super-secret raw-log-body")
				case "timeout":
					<-req.Context().Done()
				case "oversize":
					io.WriteString(w, strings.Repeat("x", 65<<10))
				case "incomplete":
					io.WriteString(w, `{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`)
				default:
					io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"Check A1; password=hidden-value; super-secret","reasoning_content":"PRIVATE-REASONING"}}]}`)
				}
			}))
			defer fake.Close()
			e := &Engine{Logs: logs, client: fake.Client(), timeout: 100 * time.Millisecond}
			cfg := Config{Endpoint: fake.URL, Model: "local-fixture", APIKey: "super-secret"}
			in := Input{From: time.Now().Add(-30 * time.Minute), To: time.Now(), Alerts: []Alert{{Ref: "A1", Rule: "cpu"}}, Missing: []string{"token=super-secret"}}
			if _, err = e.Generate(ctx, cfg, 1, in); err == nil || calls.Load() != 0 {
				t.Fatal("disabled sent request")
			}
			cfg.Enabled = true
			r, err := e.Generate(ctx, cfg, 1, in)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"ok": "succeeded", "failure": "failed", "timeout": "timeout", "oversize": "failed", "incomplete": "failed"}[outcome]
			if r.Status != want {
				t.Fatalf("status=%s, want=%s; failure=%s", r.Status, want, r.Failure)
			}
			if outcome == "ok" && (!strings.Contains(r.Content, "Check A1") || strings.Contains(r.Content, "hidden-value")) {
				t.Fatalf("bad content: %s", r.Content)
			}
			// Recreate the engine: restart must not reset persisted cooldown.
			e = &Engine{Logs: logs, client: fake.Client()}
			if _, err = e.Generate(ctx, cfg, 1, in); err == nil || err.Error() != "cooldown" || calls.Load() != 1 {
				t.Fatalf("cooldown: %v, calls=%d", err, calls.Load())
			}
			list, err := e.List(ctx)
			if err != nil || len(list) != 1 || list[0].Status != want {
				t.Fatalf("list=%+v err=%v", list, err)
			}
			events, _ := logs.Query(ctx, logstore.Filter{Limit: 1000})
			data, _ := json.Marshal(events)
			for _, secret := range []string{"super-secret", "hidden-value", "PRIVATE-REASONING", "raw-log-body"} {
				if strings.Contains(string(data), secret) {
					t.Errorf("persisted secret %s", secret)
				}
			}
		})
	}
}

func TestQuotaAndInputLimits(t *testing.T) {
	ctx := context.Background()
	logs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	e := &Engine{Logs: logs}
	cfg := Config{Enabled: true}
	in := Input{Alerts: []Alert{{Ref: "A1"}}}
	lim := limiter{}
	for i := 0; i < 12; i++ {
		lim.Attempts = append(lim.Attempts, time.Now().Add(-time.Duration(i+1)*time.Hour))
	}
	if err := logs.RecordCheckpoint(ctx, logstore.Event{Category: logstore.Task, Kind: "quota", Object: Object, Data: map[string]any{}}, Object, lim); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Generate(ctx, cfg, 1, in); err == nil || err.Error() != "daily_limit" {
		t.Fatalf("daily limit: %v", err)
	}
	in.Missing = []string{strings.Repeat("x", MaxInput)}
	if _, err := e.Generate(ctx, cfg, 1, in); err == nil || err.Error() != "material_too_large" {
		t.Fatalf("input limit: %v", err)
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	in.Missing = nil
	if _, err := e.Generate(ctx, cfg, 1, in); err == nil {
		t.Fatal("sent without storage")
	}
}

func TestConfigRejectsCredentialURLs(t *testing.T) {
	for _, u := range []string{"http://example.com/v1/chat/completions", "https://user:pass@example.com/v1", "https://example.com/v1?k=secret", "https://example.com/v1#secret"} {
		if (Config{Endpoint: u, Model: "model", APIKey: "key"}).Validate() == nil {
			t.Errorf("accepted %s", u)
		}
	}
	if err := (Config{Endpoint: "https://example.com/v1/chat/completions", Model: "model", APIKey: "key"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

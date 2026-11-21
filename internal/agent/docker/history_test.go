package docker

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

type frameWriter struct {
	io.Writer
	stream byte
}

func (w frameWriter) Write(p []byte) (int, error) {
	var header [8]byte
	header[0] = w.stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(p)))
	if _, err := w.Writer.Write(header[:]); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}

func historyClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := client.New(client.WithHost(srv.URL), client.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &Client{c: c}
}

func TestHistoryReadsTTYAndMultiplexedOutputPastOversizedLine(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprint(tty), func(t *testing.T) {
			at := time.Date(2026, 11, 21, 1, 2, 3, 123, time.UTC)
			prefix := at.Format(time.RFC3339Nano) + " "
			d := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/json"):
					fmt.Fprintf(w, `{"Config":{"Tty":%t}}`, tty)
				case strings.HasSuffix(r.URL.Path, "/logs"):
					if r.URL.Query().Get("since") != fmt.Sprintf("%d.%09d", at.Unix(), at.Nanosecond()) || r.URL.Query().Get("follow") != "1" {
						http.Error(w, "wrong history boundary", 400)
						return
					}
					var out, errOut io.Writer = w, w
					if !tty {
						out, errOut = frameWriter{w, 1}, frameWriter{w, 2}
					}
					fmt.Fprint(out, prefix+"first\n")
					if !tty {
						fmt.Fprint(errOut, prefix+"warning\n")
					}
					fmt.Fprint(out, prefix+strings.Repeat("x", 2<<20)+"\n")
					fmt.Fprint(out, prefix+"last without newline")
				default:
					http.NotFound(w, r)
				}
			})
			var lines []LogLine
			err := d.LogsSince(t.Context(), Instance{ID: "instance-a", State: "running", Service: "web"}, at, 8192, func(l LogLine) error { lines = append(lines, l); return nil })
			want := 4
			if tty {
				want = 3
			}
			if err != nil || len(lines) != want || lines[len(lines)-1].Text != "last without newline" {
				t.Fatalf("日志在超长行或尾行处丢失: lines=%d err=%v", len(lines), err)
			}
			long := lines[len(lines)-2]
			if !long.Truncated || len(long.Text) > 8192 || long.Bytes != int64(len(prefix)+(2<<20)) || !long.Time.Equal(at) {
				t.Fatalf("长行没有有界保存/标记: %+v", long)
			}
			if !tty && lines[1].Stream != "stderr" {
				t.Fatal("stderr 被误记到 stdout")
			}
		})
	}
}

func TestHistoryStopsWhenPersistenceRejectsALine(t *testing.T) {
	d := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") {
			fmt.Fprint(w, `{"Config":{"Tty":true}}`)
			return
		}
		fmt.Fprint(w, "2026-11-21T00:00:00Z first\n2026-11-21T00:00:01Z second\n")
	})
	refused := errors.New("disk full")
	var got []string
	err := d.LogsSince(t.Context(), Instance{ID: "a"}, time.Time{}, 8192, func(l LogLine) error { got = append(got, l.Text); return refused })
	if !errors.Is(err, refused) || len(got) != 1 || got[0] != "first" {
		t.Fatalf("拒绝持久写后仍消费来源: %v %v", got, err)
	}
}

func TestEngineHistoryDoesNotCopyCommandsOrArbitraryLabels(t *testing.T) {
	d := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"Type":"container","Action":"exec_create: echo hidden-command","Actor":{"ID":"a","Attributes":{"nagi.project":"p","exitCode":"13","secret":"hidden-label"}},"timeNano":1795219200000000001}`)
		fmt.Fprintln(w, `{"Type":"container","Action":"health_status: hidden-health-output","Actor":{"ID":"a"},"timeNano":1795219200000000002}`)
	})
	var got []EngineEvent
	err := d.EventsSince(t.Context(), time.Time{}, func(e EngineEvent) error { got = append(got, e); return nil })
	if !errors.Is(err, io.EOF) || len(got) != 2 || got[0].Action != "exec_create" || got[0].Attributes["exitCode"] != "13" || got[0].Project != "p" {
		t.Fatalf("事件字段/结束边界不对: %+v %v", got, err)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "hidden-") {
		t.Fatalf("事件泄漏命令/检查输出/标签: %s", b)
	}
}

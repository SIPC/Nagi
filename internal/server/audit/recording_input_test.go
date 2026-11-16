package audit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// inputEvents 取出录制里的输入事件。
func inputEvents(t *testing.T, cast string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(cast, "\n") {
		if !strings.HasPrefix(line, "[") {
			continue
		}
		var ev []any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev[1] == "i" {
			out = append(out, ev[2].(string))
		}
	}
	return out
}

// 有回显的字符照录；没有回显的（密码）只记个数；控制键记名称；事件的时间不倒退。
func TestRecordingInput(t *testing.T) {
	r := testRecordings(t)
	rec := r.Start(context.Background(), 80, 24)
	// shell 里敲 ls 回车：每个字符都回显。
	for _, c := range []string{"l", "s"} {
		rec.Input([]byte(c))
		rec.Output([]byte(c))
	}
	rec.Input([]byte("\r"))
	rec.Output([]byte("\r\nfile.txt\r\n$ "))
	// sudo 问密码：不回显，回车后才有输出。
	rec.Output([]byte("[sudo] password: "))
	for _, c := range []string{"h", "u", "n", "t", "e", "r", "2"} {
		rec.Input([]byte(c))
	}
	rec.Input([]byte("\r"))
	rec.Output([]byte("\r\n"))
	// 方向键、Ctrl+C、带高亮的回显。
	rec.Input([]byte("\x1b[A"))
	rec.Input([]byte("\x03"))
	rec.Input([]byte("x"))
	rec.Output([]byte("\x1b[31mx\x1b[0m"))
	got := inputEvents(t, readAll(t, r, rec.Close().ID))
	joined := strings.Join(got, "|")
	if strings.ContainsAny(joined, "hunter2") {
		t.Fatalf("没有回显的密码不应录下：%q", got)
	}
	for _, want := range []string{"l", "s", "⏎", "•••••••", "↑", "^C", "x"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("缺少输入 %q：%q", want, got)
		}
	}
	if strings.Contains(joined, "•") && strings.Count(joined, "•") != 7 {
		t.Errorf("未回显的字符应记 7 个：%q", got)
	}
}

// 粘贴一整段并回车：回显在一次输出里，照录。
func TestRecordingInputPaste(t *testing.T) {
	r := testRecordings(t)
	rec := r.Start(context.Background(), 80, 24)
	rec.Input([]byte("\x1b[200~docker ps -a\x1b[201~\r"))
	rec.Output([]byte("docker ps -a\r\nCONTAINER ID\r\n"))
	got := inputEvents(t, readAll(t, r, rec.Close().ID))
	if strings.Join(got, "|") != "⏎|docker ps -a" && strings.Join(got, "|") != "docker ps -a|⏎" {
		t.Fatalf("粘贴的命令应照录：%q", got)
	}
}

// 回显超过 echoWait 才来：算没有回显。
func TestRecordingInputLateEcho(t *testing.T) {
	r := testRecordings(t)
	rec := r.Start(context.Background(), 80, 24)
	rec.Input([]byte("abc"))
	rec.mu.Lock()
	rec.typed[0].deadline = time.Now().Add(-time.Second)
	rec.mu.Unlock()
	rec.Output([]byte("abc"))
	got := inputEvents(t, readAll(t, r, rec.Close().ID))
	if len(got) != 1 || got[0] != "•••" {
		t.Fatalf("超时的回显应算没有回显：%q", got)
	}
}

// 输完密码按了回车：后面敲的、和密码里字符相同的回显，不会被当成密码的回显。
func TestRecordingInputAfterEnter(t *testing.T) {
	r := testRecordings(t)
	rec := r.Start(context.Background(), 80, 24)
	rec.Input([]byte("q"))
	rec.Input([]byte("\r"))
	rec.mu.Lock()
	for i := range rec.typed {
		rec.typed[i].deadline = time.Now().Add(-time.Millisecond) // 回车以后的等待已经过了
	}
	rec.mu.Unlock()
	rec.Input([]byte("q"))
	rec.Output([]byte("q"))
	got := inputEvents(t, readAll(t, r, rec.Close().ID))
	if strings.Join(got, "|") != "⏎|•|q" {
		t.Fatalf("密码应记成未回显，后面的 q 照录：%q", got)
	}
}

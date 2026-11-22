package project

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/SIPC/nagi/internal/agent/metrics"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// v0.35.0#21：git 服务器一直发不带换行的进度时，只转发截断的一行，缓冲不变大，耗时和数据量成正比（原来是平方）。
func TestLineWriterLongLine(t *testing.T) {
	var lines []string
	w := &lineWriter{onLine: func(l string) { lines = append(lines, l) }}
	chunk := []byte(strings.Repeat("x", 32<<10))
	start := time.Now()
	for range 1024 { // 32 MiB，原来要几十秒
		if n, err := w.Write(chunk); n != len(chunk) || err != nil {
			t.Fatal(n, err)
		}
		if len(w.buf) > maxProgressLine {
			t.Fatalf("缓冲不应该超过 %d：%d", maxProgressLine, len(w.buf))
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("32 MiB 不换行的数据用了 %v", d)
	}
	if len(lines) != 1 || len(lines[0]) != maxProgressLine+len("…") || !strings.HasSuffix(lines[0], "…") {
		t.Fatalf("超长的一行应该截断发出一次：%d 行", len(lines))
	}
	// 换行以后照常按行转发，\r 也算换行。
	w.Write([]byte("tail\nCounting objects: 50%\rCounting objects: 100%\r\n"))
	if want := []string{"Counting objects: 50%", "Counting objects: 100%"}; len(lines) != 3 || lines[1] != want[0] || lines[2] != want[1] {
		t.Fatalf("换行以后应该照常转发：%q", lines[1:])
	}
	// 一行分几次写；截断处落在多字节字符中间时不留下半个字符。
	lines = nil
	w.Write([]byte("Receiving "))
	w.Write([]byte("objects\n"))
	w.Write([]byte(strings.Repeat("a", maxProgressLine-1) + "中\n"))
	if len(lines) != 2 || lines[0] != "Receiving objects" || !utf8.ValidString(lines[1]) || lines[1] != strings.Repeat("a", maxProgressLine-1)+"…" {
		t.Fatalf("分几次写和截断：%q", lines)
	}
}

func TestLineWriterCarriageReturns(t *testing.T) {
	for _, row := range []string{"\r", "progress\r"} {
		t.Run(fmt.Sprintf("row-%d", len(row)), func(t *testing.T) {
			count := 0
			w := &lineWriter{onLine: func(line string) {
				if line != "progress" {
					t.Fatalf("unexpected progress: %q", line)
				}
				count++
			}}
			rows := (1 << 20) / len(row)
			input := []byte(strings.Repeat(row, rows))
			start := time.Now()
			if n, err := w.Write(input); n != len(input) || err != nil {
				t.Fatal(n, err)
			}
			elapsed := time.Since(start)
			t.Logf("%d bytes, %d CR rows: %v", len(input), rows, elapsed)
			if elapsed > 2*time.Second {
				t.Fatalf("1 MiB of CR progress took %v", elapsed)
			}
			want := 0
			if row != "\r" {
				want = rows
			}
			if count != want {
				t.Fatalf("progress count: %d, want %d", count, want)
			}
		})
	}
}

// 拉取中磁盘剩余空间不够了就取消，开始前不够就不拉取。
func TestGuardedCloneLowDisk(t *testing.T) {
	prevCheck, prevEvery := checkDisk, gitDiskEvery
	defer func() { checkDisk, gitDiskEvery = prevCheck, prevEvery }()
	gitDiskEvery = 10 * time.Millisecond
	var calls atomic.Int32
	full := &metrics.LowDiskError{Total: 100 << 30, Free: 1 << 30}
	checkDisk = func(string, int64) error {
		if calls.Add(1) > 3 {
			return full
		}
		return nil
	}
	err := guardedClone(context.Background(), t.TempDir(), func(ctx context.Context) error {
		select {
		case <-ctx.Done(): // 无穷大的 packfile：一直写到被取消
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	})
	if !errors.Is(err, full) {
		t.Fatalf("磁盘不够时应该取消并报磁盘不足：%v", err)
	}

	cloned := false
	err = guardedClone(context.Background(), t.TempDir(), func(context.Context) error { cloned = true; return nil })
	if !errors.Is(err, full) || cloned {
		t.Fatalf("开始前磁盘不够时不应该拉取：%v", err)
	}
}

// 拉取超过总时长就停下，报固定的文案。
func TestPullTimeout(t *testing.T) {
	prevCheck, prevTimeout := checkDisk, gitPullTimeout
	defer func() { checkDisk, gitPullTimeout = prevCheck, prevTimeout }()
	checkDisk = func(string, int64) error { return nil }
	gitPullTimeout = time.Nanosecond
	_, err := pull(context.Background(), containerPaths{root: t.TempDir()}, &nagiv1.Source{GitUrl: "https://git.invalid/a.git", GitRef: "v1"}, nil, func(string) {})
	if err == nil || err.Error() != "拉取超过 30 分钟，已停止" {
		t.Fatalf("应该报拉取超时：%v", err)
	}
}

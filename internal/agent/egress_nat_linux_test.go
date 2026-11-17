package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// S2：旧内核（5.2 以前）加载不了 nat 链时，之前 nat 链和过滤规则在同一张 inet 表里，整张表都加载不了，出站限制失效。
// 现在 nat 表单独应用：用一个对 nat 报错的假 nft，过滤规则照常加载，nat 失败只记日志、不报「无法限制」。
func TestApplyNATFailureKeepsFilter(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	loaded := filepath.Join(dir, "loaded")
	// 假的 nft：脚本里有 nat 链就报错（像 4.x 内核），否则记下来。
	script := "#!/bin/sh\nin=$(cat)\ncase \"$in\" in *'type nat'*) echo 'Error: Could not process rule: Operation not supported' >&2; exit 1;; esac\nprintf '%s\\n' \"$in\" >> " + loaded + "\n"
	if err := os.WriteFile(filepath.Join(bin, "nft"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	e := &egress{a: &agent{log: slog.New(slog.DiscardHandler)}}
	ctx := context.Background()
	e.applyNAT(ctx, "table ip nagi_nat {\n\tchain input {\n\t\ttype nat hook input priority srcnat; policy accept;\n\t}\n}\n")
	if e.appliedNAT != "" || !strings.Contains(e.natErr, "not supported") || e.Error() != "" {
		t.Fatalf("nat 失败只记下原因，不报无法限制：applied=%q natErr=%q err=%q", e.appliedNAT, e.natErr, e.Error())
	}
	if out, err := runNft(ctx, "table inet nagi {\n}\n"); err != nil {
		t.Fatalf("过滤规则应照常加载：%v %s", err, out)
	}
	if b, _ := os.ReadFile(loaded); !strings.Contains(string(b), "table inet nagi") {
		t.Fatal("过滤规则没有加载")
	}
}

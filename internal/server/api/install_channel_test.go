package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 面板安装脚本选哪个版本（见 docs/development.md#预发布版本）：默认跟随 releases/latest（只有正式版）；--channel pre 或
// NAGI_CHANNEL=pre 从发布列表里取版本号最大的（pre.10 比 pre.2 新，同号正式版比预发布版新）；--version 指定的版本优先。
// 整个脚本照常运行，PATH 里放假的 uname、id、curl、docker：curl 把要下载的二进制地址写进 asked 后失败，脚本在下载这一步停下。
func TestInstallScriptChannel(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	feed := func(tags ...string) string {
		var b strings.Builder
		b.WriteString(`<feed><link href="https://github.com/SIPC/Nagi/releases"/>`)
		for _, tag := range tags {
			b.WriteString(`<entry><link rel="alternate" type="text/html" href="https://github.com/SIPC/Nagi/releases/tag/` + tag + `"/></entry>`)
		}
		// 正文里提到的版本链接是转义过的，不算发布。
		b.WriteString(`<content type="html">&lt;a href=&quot;https://github.com/SIPC/Nagi/releases/tag/v9.9.9&quot;&gt;</content></feed>`)
		return b.String()
	}
	cases := []struct {
		name string
		args []string
		env  []string
		feed string
		want string // 下载的地址；以 "fail:" 开头时应失败并输出这段文字
	}{
		{"默认正式版", nil, nil, feed("v0.38.0-pre.1", "v0.37.0"), "https://github.com/SIPC/Nagi/releases/download/v0.37.0"},
		{"预发布渠道", []string{"--channel", "pre"}, nil, feed("v0.38.0-pre.2", "v0.38.0-pre.10", "v0.37.0"), "https://github.com/SIPC/Nagi/releases/download/v0.38.0-pre.10"},
		{"环境变量", nil, []string{"NAGI_CHANNEL=pre"}, feed("v0.38.0-pre.1", "v0.37.0"), "https://github.com/SIPC/Nagi/releases/download/v0.38.0-pre.1"},
		{"正式版发布后", []string{"--channel", "pre"}, nil, feed("v0.38.0", "v0.38.0-pre.10"), "https://github.com/SIPC/Nagi/releases/download/v0.38.0"},
		{"指定版本优先", []string{"--channel", "pre", "--version", "v0.36.0"}, nil, feed("v0.38.0-pre.1"), "https://github.com/SIPC/Nagi/releases/download/v0.36.0"},
		{"指定预发布版", []string{"--version", "v0.38.0-pre.1"}, nil, feed(), "https://github.com/SIPC/Nagi/releases/download/v0.38.0-pre.1"},
		{"取不到发布列表", []string{"--channel", "pre"}, nil, "", "fail:无法从 GitHub 获取发布列表"},
		{"认不出的渠道", []string{"--channel", "beta"}, nil, feed(), "fail:更新渠道须为 stable 或 pre"},
		{"版本号格式", []string{"--version", "0.38.0"}, nil, feed(), "fail:版本号须为"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			os.Mkdir(bin, 0o755)
			stub := func(name, body string) {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stub("uname", `case "$1" in -s) echo Linux ;; *) echo x86_64 ;; esac`)
			stub("id", `echo 0`)
			stub("docker", `case "$*" in "compose version --short") echo 2.30.0 ;; *) echo 27.0.0 ;; esac`)
			if c.feed != "" {
				os.WriteFile(filepath.Join(dir, "feed.xml"), []byte(c.feed), 0o644)
			}
			// 发布列表：有 feed.xml 就给它；releases/latest 重定向到 v0.37.0；cloudflare 查不到所在地（不走加速代理）；
			// 下载二进制时记下地址并失败。
			stub("curl", `out=""; url=""; w=""
while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift ;; -w) w=$2; shift ;; --max-time|--retry|--connect-timeout|--speed-limit|--speed-time) shift ;; -*) ;; *) url=$1 ;; esac; shift; done
case "$url" in
  https://www.cloudflare.com/*) exit 0 ;;
  https://github.com) exit 0 ;;
  */releases.atom) [ -f "$STUB_DIR/feed.xml" ] && cat "$STUB_DIR/feed.xml" || exit 22 ;;
  */releases/latest) printf 'https://github.com/SIPC/Nagi/releases/tag/v0.37.0' ;;
  */nagi-linux-amd64|*/SHA256SUMS) printf '%s\n' "${url%/*}" >> "$STUB_DIR/asked"; exit 22 ;;
  *) exit 22 ;;
esac`)
			// 数据目录在下载之后才用到，脚本在下载这一步就停下了。
			cmd := exec.Command(sh, append([]string{panelScript, "--no-agent", "--data-dir", "/nonexistent/nagi-test"}, c.args...)...)
			cmd.Env = append(append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_DIR="+dir), c.env...)
			out, err := cmd.CombinedOutput()
			if want, ok := strings.CutPrefix(c.want, "fail:"); ok {
				if err == nil || !strings.Contains(string(out), want) {
					t.Fatalf("应失败并提示 %q：%v\n%s", want, err, out)
				}
				return
			}
			asked, _ := os.ReadFile(filepath.Join(dir, "asked"))
			if got := strings.SplitN(strings.TrimSpace(string(asked)), "\n", 2)[0]; got != c.want {
				t.Fatalf("下载地址 %q，应为 %q\n%s", got, c.want, out)
			}
		})
	}
}

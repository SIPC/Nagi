package main

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 加入令牌优先用 --token（旧的安装脚本），没给时读环境变量 NAGI_TOKEN（新的安装脚本）。
func TestTokenFromEnv(t *testing.T) {
	t.Setenv(tokenEnv, " tok-env\n")
	if got := tokenFromEnv(""); got != "tok-env" {
		t.Errorf("没给 --token 时应该读环境变量，得到 %q", got)
	}
	if got := tokenFromEnv("tok-flag"); got != "tok-flag" {
		t.Errorf("给了 --token 时用它，得到 %q", got)
	}
	t.Setenv(tokenEnv, "")
	if got := tokenFromEnv(""); got != "" {
		t.Errorf("都没有时应该为空，得到 %q", got)
	}
}

// 备用面板的配对码也一样：没给 --standby 时读环境变量 NAGI_STANDBY，不必写在进程参数里。
func TestStandbyCodeFromEnv(t *testing.T) {
	t.Setenv(standbyEnv, " nagi-standby:abc\n")
	if got := flagOrEnv("", standbyEnv); got != "nagi-standby:abc" {
		t.Errorf("没给 --standby 时应该读环境变量，得到 %q", got)
	}
	if got := flagOrEnv("nagi-standby:flag", standbyEnv); got != "nagi-standby:flag" {
		t.Errorf("给了 --standby 时用它，得到 %q", got)
	}
}

// nagi server --standby <码> 仍然接受，但提示改用环境变量；用环境变量时不提示。读完都清掉环境变量。
func TestStandbyCodeFlagWarns(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	t.Setenv(standbyEnv, "nagi-standby:env")
	if got := standbyCodeFrom("nagi-standby:flag", log); got != "nagi-standby:flag" {
		t.Errorf("给了 --standby 时用它，得到 %q", got)
	}
	if !strings.Contains(buf.String(), standbyEnv) {
		t.Errorf("用 --standby 时应该提示改用环境变量：%s", buf.String())
	}
	if _, ok := os.LookupEnv(standbyEnv); ok {
		t.Error("读完应该清掉环境变量")
	}
	buf.Reset()
	t.Setenv(standbyEnv, "nagi-standby:env")
	if got := standbyCodeFrom("", log); got != "nagi-standby:env" {
		t.Errorf("没给 --standby 时应该读环境变量，得到 %q", got)
	}
	if buf.Len() != 0 {
		t.Errorf("用环境变量时不应该提示：%s", buf.String())
	}
}

// 安装脚本遇到 --standby <码> 时照常接受，但提示改用环境变量；用环境变量时不提示。
// 用一个不合法的端口让脚本在参数检查时停下，不会真的安装。
func TestInstallScriptStandbyArgWarns(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	run := func(env []string, args ...string) string {
		cmd := exec.Command(sh, append([]string{"../../scripts/install.sh"}, args...)...)
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "端口须为数字") {
			t.Fatalf("应该在端口检查时停下：%v %s", err, out)
		}
		return string(out)
	}
	if out := run(nil, "--standby", "nagi-standby:abc", "--port", "x"); !strings.Contains(out, "NAGI_STANDBY=<配对码>") {
		t.Errorf("用 --standby 时应该提示改用环境变量：%s", out)
	}
	if out := run([]string{standbyEnv + "=nagi-standby:abc"}, "--port", "x"); strings.Contains(out, "注意") {
		t.Errorf("用环境变量时不应该提示：%s", out)
	}
}

// 面板安装脚本不把配对码写进服务的启动参数和服务配置（谁都能读），而是放在只有 root 能读的环境变量文件里。
func TestInstallScriptStandbyCodeNotInArgs(t *testing.T) {
	b, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	if strings.Contains(script, `ARGS --standby`) || strings.Contains(script, "server --standby") {
		t.Error("安装脚本不应该把配对码放进 nagi server 的参数")
	}
	for _, want := range []string{`EnvironmentFile=`, standbyEnv + `=`, `umask 077`} {
		if !strings.Contains(script, want) {
			t.Errorf("安装脚本应该用只有 root 能读的环境变量文件传配对码，缺少 %q", want)
		}
	}
}

// 网页、App 给出的命令和使用文档默认用环境变量传配对码，不写成 --standby <码>。
func TestStandbyCommandsUseEnv(t *testing.T) {
	for _, f := range []string{
		"../../web/src/pages/standby.tsx",
		"../../android/feature/settings/src/main/kotlin/ink/sipc/nagi/settings/StandbyInviteScreens.kt",
		"../../docs/guide/standby.md",
		"../../docs/guide/standby.en.md",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if s := string(b); strings.Contains(s, "--standby") || !strings.Contains(s, standbyEnv+"=") {
			t.Errorf("%s 应该用环境变量 %s 传配对码，不用 --standby", f, standbyEnv)
		}
	}
}

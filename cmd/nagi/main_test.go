package main

import (
	"os"
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

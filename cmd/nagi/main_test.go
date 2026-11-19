package main

import "testing"

// v0.35.0#10：加入令牌优先用 --token（旧的安装脚本），没给时读环境变量 NAGI_TOKEN（新的安装脚本）。
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

package api

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 没有令牌时（多半是写成了 … | NAGI_TOKEN=x sudo sh -s --，sudo 不传环境变量），提示里要说明怎么做。
func TestInstallScriptMissingTokenHint(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	var buf bytes.Buffer
	if err := installScript.Execute(&buf, map[string]string{
		"Panel": "203.0.113.10:34567", "Fingerprint": "sha256//abc=", "Version": "v0.35.0",
		"Base": "https://github.com/SIPC/Nagi/releases/download/v0.35.0", "Shared": sharedScript,
	}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "-s", "--")
	cmd.Stdin = &buf
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")} // 没有 NAGI_TOKEN
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("没有令牌时应该失败：%s", out)
	}
	for _, want := range []string{"缺少加入令牌", "不要在 sh 前加 sudo", "sudo -i"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("提示里应该有 %q：%s", want, out)
		}
	}
}

// 下载到的 nagi 低于 v0.35.0（不读环境变量里的令牌）时停下并说明原因，其余版本照常继续。
func TestInstallScriptOldNagi(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	run := func(version string) (string, error) {
		script := `fail() { printf '%s\n' "$*"; exit 1; }` + "\n" + enrollVersionCheck + "\n"
		cmd := exec.Command(sh, "-c", script)
		cmd.Env = []string{"NEW_VERSION=" + version}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, v := range []string{"v0.1.0", "v0.9.3", "v0.10.0", "v0.28.0", "v0.34.2"} {
		if out, err := run(v); err == nil || !strings.Contains(out, v) || !strings.Contains(out, "--download-url") {
			t.Errorf("%s 应该停下并说明：%v %s", v, err, out)
		}
	}
	for _, v := range []string{"v0.35.0", "v0.35.1", "v0.40.0", "v0.100.0", "v1.0.0", "v1.3.4", "dev"} {
		if out, err := run(v); err != nil {
			t.Errorf("%s 应该继续：%v %s", v, err, out)
		}
	}
	var buf bytes.Buffer
	if err := installScript.Execute(&buf, map[string]string{"Shared": sharedScript}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "download_nagi\n"+enrollVersionCheck+"\n") {
		t.Error("节点安装脚本应该在下载 nagi 之后检查版本")
	}
}

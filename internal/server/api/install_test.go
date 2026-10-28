package api

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const panelScript = "../../../scripts/install.sh"

// 面板安装脚本里的共用代码要和 sharedScript 一字不差（节点安装脚本由面板生成，用的是 sharedScript）。
func TestSharedScript(t *testing.T) {
	b, err := os.ReadFile(panelScript)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	begin := strings.Index(text, "# >>> 共用代码开始")
	end := strings.Index(text, "# <<< 共用代码结束")
	if begin < 0 || end < 0 {
		t.Fatal("scripts/install.sh 里找不到共用代码的起止标记")
	}
	got := text[begin : end+len("# <<< 共用代码结束")]
	if got != sharedScript {
		t.Fatal("scripts/install.sh 里的共用代码和 install.go 的 sharedScript 不一致，请同步修改")
	}
}

// 生成的节点安装脚本和面板安装脚本都要能通过 sh -n 语法检查。
func TestInstallScriptsSyntax(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	var buf bytes.Buffer
	if err := installScript.Execute(&buf, map[string]string{
		"Panel": "203.0.113.10:34567", "Fingerprint": "sha256//abc=", "Version": "v0.1.0",
		"Base": "https://github.com/SIPC/Nagi/releases/download/v0.1.0", "Shared": sharedScript,
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"ensure_docker\n", "link_command\n", "download_nagi\n", `PANEL="203.0.113.10:34567"`,
		`BASE="https://github.com/SIPC/Nagi/releases/download/v0.1.0"`, `PANEL_VERSION="v0.1.0"`} {
		if !strings.Contains(buf.String(), s) {
			t.Errorf("节点安装脚本里没有 %q", s)
		}
	}
	// 二进制不再从面板下载。
	if strings.Contains(buf.String(), "/download/nagi-linux-") && strings.Contains(buf.String(), "$PANEL/download") {
		t.Error("节点安装脚本还在从面板下载 nagi")
	}
	node := filepath.Join(t.TempDir(), "node.sh")
	if err := os.WriteFile(node, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{node, panelScript} {
		if out, err := exec.Command(sh, "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s 语法错误：%v\n%s", f, err, out)
		}
	}
}
